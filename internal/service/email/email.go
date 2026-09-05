package email

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	gidservice "github.com/servekit/gid-service/pkg"
	pb "github.com/servekit/message-service/gen/message/v1"
	provemail "github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/internal/service/utils"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/internal/utils/pagination"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"google.golang.org/protobuf/encoding/protojson"
)

// =====================================================================
// Public RPC methods (one per proto RPC, delegates to internal helpers)
// =====================================================================

// SendEmail sends an email via the configured vendor/account, or the default
// fallback chain when both are unset. Idempotent on (sender_id, idempotency_key)
// via Redis: a second request with the same key returns the cached response
// without re-invoking the provider. Failures are not cached — the reservation
// is released so the caller can retry the same key.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	if err := validateSendEmailRequest(req); err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	// Idempotency reservation (Redis-backed). Always runs regardless of
	// persistence toggle — Redis is the single source of dedup truth.
	var idemKey string
	if k := req.GetIdempotencyKey(); k != "" {
		idemKey = k
		acquired, payload, err := s.idem.Reserve(ctx, "email", req.GetSenderId(), k)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		if !acquired {
			if payload == nil {
				return nil, xcodes.ErrIdempotencyConflict.New("idempotency_key in flight")
			}
			resp, err := deserializeIdempotentEmail(payload)
			if err != nil {
				return nil, xcodes.ErrInternal.Wrap(err)
			}
			return resp, nil
		}
	}

	sender, err := s.emailRegistry.SenderFor(req.GetVendor(), req.GetAccount())
	if err != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after sender lookup failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	id, err := gidservice.NextID(ctx, s.gid)
	if err != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after gid failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	// Attachment processing: wrap inline-content attachments as MIME parts.
	// url-only attachments are pure references (caller-managed download
	// links, e.g. OSS) — not fetched, not embedded; their metadata is
	// persisted for record queries. htmlBody is passed through unchanged —
	// message-service never injects attachment links into the body; callers
	// render their own links/images inline.
	htmlBody := req.GetHtmlBody()
	mimeAtts, err := s.processAttachments(req.GetAttachments())
	if err != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after attachment processing", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, err
	}

	msg := &provemail.Message{
		To:             pbToAddrs(req.GetTo()),
		Cc:             pbToAddrs(req.GetCc()),
		Bcc:            pbToAddrs(req.GetBcc()),
		Subject:        req.GetSubject(),
		Body:           req.GetBody(),
		HTMLBody:       htmlBody,
		ReplyTo:        pbToAddr(req.GetReplyTo()),
		From:           pbToAddr(req.GetFrom()),
		Template:       req.GetTemplateId(),
		TemplateParams: req.GetTemplateParams(),
		Attachments:    mimeAtts,
	}

	result, sendErr := sender.Send(ctx, msg)

	// Pre-send failure (empty recipient / no provider): no result to persist.
	// Release the idempotency reservation so the caller can retry the key.
	if sendErr != nil && result == nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after pre-send failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr, "stage=pre_send")
	}

	// result is guaranteed non-nil from here. Persist with an independent
	// context so request cancellation does not lose the record. Skipped when
	// persistence disabled — caller opted out of DB writes.
	if s.persistence {
		persistCtx, cancel := context.WithTimeout(context.Background(), utils.PersistTimeout)
		defer cancel()
		s.persistEmailRecord(persistCtx, id, req, htmlBody, result)
	}

	// Post-send failure (vendor returned a failed result, not a pre-send
	// error): Release the reservation so the caller can retry — failures are
	// not cached. Must run BEFORE Complete to avoid a window where a fake
	// Status=SENT payload is observable to a concurrent caller with the same
	// idempotency_key.
	if sendErr != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after post-send failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
			"vendor=%s account=%s attempts=%d",
			result.Vendor.String(), result.Account, result.Attempts)
	}

	// Success: cache the response payload so a second call with the same key
	// returns the cached result. Errors are logged but don't affect the
	// response — the send already succeeded.
	if idemKey != "" {
		resp := &pb.SendResponse{
			Id:     id,
			Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
			Vendor: &pb.SendResponse_EmailVendor{
				EmailVendor: result.Vendor,
			},
		}
		payload, err := protojson.Marshal(resp)
		if err != nil {
			slog.Error("idempotency marshal", "key", idemKey, "error", err)
		} else if err := s.idem.Complete(context.Background(), "email", req.GetSenderId(), idemKey, payload); err != nil {
			slog.Error("idempotency complete", "key", idemKey, "error", err)
		}
	}

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_EmailVendor{
			EmailVendor: result.Vendor,
		},
	}, nil
}

// GetEmail returns a single email record by ID.
func (s *Service) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("email persistence is disabled"))
	}
	record, err := dal.GetEmailRecord(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	// Single record is a degenerate batch — reuse the shared loader so the
	// attachment backfill logic lives in exactly one place.
	return s.loadEmailRecordProtos(ctx, []*models.MessageEmailRecord{record})[0], nil
}

// ListEmails returns a paginated list of email records matching the filter.
func (s *Service) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("email persistence is disabled"))
	}
	f := dal.EmailListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		Target:        req.GetTarget(),
		SenderID:      req.GetSenderId(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	result, err := dal.ListEmailRecords(ctx, s.db, f, dbx.PageParams{
		Page:     int(req.GetPage()),
		PageSize: int(req.GetPageSize()),
		Count:    true,
	})
	if err != nil {
		return nil, err
	}

	protoRecords := s.loadEmailRecordProtos(ctx, result.List)

	return &pb.ListEmailsResponse{
		Records:    protoRecords,
		Total:      int32(result.Total),
		TotalPages: int32(result.TotalPages),
		HasMore:    req.GetPage() < int32(result.TotalPages),
	}, nil
}

// ListEmailsByCursor returns a cursor-paginated list of email records.
// Prefer this over ListEmails for large datasets or when COUNT(*) is
// expensive — set include_total = true to opt in to a count query.
func (s *Service) ListEmailsByCursor(ctx context.Context, req *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("email persistence is disabled"))
	}
	f := dal.EmailListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		Target:        req.GetTarget(),
		SenderID:      req.GetSenderId(),
		SortField:     req.GetSortField(),
		SortDirection: req.GetSortDirection(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	pg := dbx.Pagination{PageSize: int(req.GetPageSize())}
	var afterCreatedAt time.Time
	if token := req.GetPageToken(); token != "" {
		cursor, err := pagination.DecodePageCursor(token)
		if err != nil {
			return nil, xcodes.ErrBadRequest.Wrap(err)
		}
		pg.AfterID = cursor.ID
		afterCreatedAt = pagination.CursorToCreatedAt(cursor.CreatedAt)
	}

	records, err := dal.ListEmailsByCursor(ctx, s.db, f, pg, afterCreatedAt)
	if err != nil {
		return nil, err
	}

	trimmed, hasNext := dbx.TrimPage(records, pg.PageSize)

	protoRecords := s.loadEmailRecordProtos(ctx, trimmed)

	var total int32
	if req.GetIncludeTotal() {
		// Cheap path: if this is the first page and it fit in one go,
		// total == len(trimmed). Otherwise run a real count.
		if !hasNext && pg.AfterID == 0 {
			total = int32(len(trimmed))
		} else {
			count, err := dal.CountEmailRecords(ctx, s.db, f)
			if err != nil {
				return nil, err
			}
			total = int32(count)
		}
	}

	var nextToken string
	if hasNext {
		last := trimmed[len(trimmed)-1]
		nextToken = pagination.EncodePageCursor(pagination.PageCursor{
			ID:        last.ID,
			CreatedAt: pagination.CursorFromCreatedAt(last.CreatedAt),
		})
	}

	return &pb.ListEmailsByCursorResponse{
		Records:       protoRecords,
		Total:         total,
		NextPageToken: nextToken,
	}, nil
}

// GetEmailStats returns aggregated statistics for emails matching the filter.
func (s *Service) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("email persistence is disabled"))
	}
	f := dal.EmailStatsFilter{
		Vendor: req.GetVendor(),
		Scene:  req.GetScene(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	stats, err := dal.CountEmailStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendorStats, err := dal.ListEmailVendorStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendors := make([]*pb.EmailVendorStats, len(vendorStats))
	for i, vs := range vendorStats {
		vendors[i] = &pb.EmailVendorStats{
			Vendor: vs.Vendor,
			Total:  vs.Total,
			Sent:   vs.Sent,
			Failed: vs.Failed,
		}
	}

	return &pb.EmailStatsResponse{
		Total:       stats.Total,
		Sent:        stats.Sent,
		Failed:      stats.Failed,
		SuccessRate: stats.SuccessRate,
		Vendors:     vendors,
	}, nil
}

// ListEmailSenders returns all distinct sender_id values, for frontend email
// list filter dropdowns.
func (s *Service) ListEmailSenders(ctx context.Context, _ *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("email persistence is disabled"))
	}
	senders, err := dal.ListEmailSenderIDs(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return &pb.ListEmailSendersResponse{SenderIds: senders}, nil
}

// =====================================================================
// Private methods (Service-owned state or RPC helpers)
// =====================================================================

// persistEmailRecord writes the email record (and attachment metadata rows)
// to the DB. Synchronous but error-logged — send already succeeded, so a DB
// failure must not propagate to the caller. Uses an independent ctx so
// request cancellation does not lose the record.
func (s *Service) persistEmailRecord(ctx context.Context, id int64, req *pb.SendEmailRequest, htmlBody string, result *provemail.SendResult) {
	// DB stores bare emails only (no display_name) — query filtering targets
	// the email address, not the human-readable name. Display name lives in
	// the request, not the persisted record.
	toEmails := bareEmailsFromAddrs(req.GetTo())
	primaryTarget := ""
	if len(toEmails) > 0 {
		primaryTarget = toEmails[0]
	}

	record := &models.MessageEmailRecord{
		ID:             id,
		Vendor:         int32(result.Vendor),
		Account:        result.Account,
		Scene:          int32(req.GetScene()),
		Target:         primaryTarget,
		SenderID:       req.GetSenderId(),
		Cc:             models.StringSlice(bareEmailsFromAddrs(req.GetCc())),
		Bcc:            models.StringSlice(bareEmailsFromAddrs(req.GetBcc())),
		Subject:        req.GetSubject(),
		Content:        req.GetBody(),
		HTMLBody:       htmlBody,
		ReplyTo:        bareEmailFromAddr(req.GetReplyTo()),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
	}

	if result.Success {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		if result.Error != nil {
			record.ErrorMessage = utils.TruncateErrorMessage(result.Error.Error())
		}
	}

	if err := dal.CreateEmailRecord(ctx, s.db, record); err != nil {
		slog.Error("persist email record", "record_id", id, "error", err)
	}

	// Attachment metadata rows. Best-effort like the email record above —
	// errors are logged but NOT propagated (send already succeeded).
	if len(req.GetAttachments()) == 0 {
		return
	}
	attRows := make([]*models.MessageEmailRecordAttachment, 0, len(req.GetAttachments()))
	for _, a := range req.GetAttachments() {
		attRows = append(attRows, &models.MessageEmailRecordAttachment{
			EmailRecordID: id,
			Filename:      a.GetFilename(),
			URL:           a.GetUrl(),
			Inline:        a.GetInline(),
			MimeType:      a.GetMimeType(),
			SizeBytes:     a.GetSizeBytes(),
		})
	}
	if err := dal.CreateEmailRecordAttachments(ctx, s.db, attRows); err != nil {
		slog.Error("persist email attachments", "record_id", id, "error", err)
	}
}

// processAttachments wraps every inline-content attachment as a MIME part.
// url-only attachments are reference metadata (the "large attachment"
// pattern: the caller uploads to object storage and renders a download link
// into html_body itself) — they are neither fetched nor embedded, so this
// function has no HTTP surface at all.
//
// Any violation aborts the whole send — no partial-send semantics.
//
// Size contract (inline content only): per-attachment cap
// (attachmentCfg.MaxInlineBytes) and per-request sum cap
// (attachmentCfg.MaxTotalInlineBytes). Exceeding either returns
// ErrAttachmentTooLarge.
func (s *Service) processAttachments(atts []*pb.EmailAttachment) ([]*provemail.Attachment, error) {
	if len(atts) == 0 {
		return nil, nil
	}

	var totalInline int64
	out := make([]*provemail.Attachment, 0, len(atts))
	for i, a := range atts {
		if len(a.GetContent()) == 0 {
			// url-only reference — nothing to embed; metadata persistence in
			// persistEmailRecord covers it.
			continue
		}
		if n := int64(len(a.GetContent())); n > s.attachment.MaxInlineBytes {
			return nil, xcodes.ErrAttachmentTooLarge.Wrapf(nil,
				"attachment[%d] %s: content size %d exceeds inline limit %d (use a url reference instead)",
				i, a.GetFilename(), n, s.attachment.MaxInlineBytes)
		}
		totalInline += int64(len(a.GetContent()))
		if totalInline > s.attachment.MaxTotalInlineBytes {
			return nil, xcodes.ErrAttachmentTooLarge.Wrapf(nil,
				"total inline content %d exceeds per-request limit %d (use url references for some attachments)",
				totalInline, s.attachment.MaxTotalInlineBytes)
		}
		out = append(out, &provemail.Attachment{
			Filename:  a.GetFilename(),
			Content:   a.GetContent(),
			MimeType:  a.GetMimeType(),
			Inline:    a.GetInline(),
			ContentID: a.GetFilename(), // simple CID = filename; service layer owns naming
		})
	}
	return out, nil
}

// loadEmailRecordProtos converts a slice of model records to proto records,
// batch-loading attachments in ONE query and backfilling each. Use this on
// List paths to avoid N+1. Errors are logged but do not fail the request —
// an attachment-table miss should not 500 the whole page.
func (s *Service) loadEmailRecordProtos(ctx context.Context, records []*models.MessageEmailRecord) []*pb.EmailRecord {
	ids := make([]int64, len(records))
	for i, r := range records {
		ids[i] = r.ID
	}
	attMap, err := dal.ListEmailRecordAttachmentsByEmailRecordIDs(ctx, s.db, ids)
	if err != nil {
		slog.Error("batch load email attachments", "error", err)
		attMap = nil // proceed without attachments rather than failing the page
	}
	out := make([]*pb.EmailRecord, len(records))
	for i, r := range records {
		p := toProtoEmailRecord(r)
		p.Attachments = emailAttachmentsToProto(attMap[r.ID])
		out[i] = p
	}
	return out
}

// =====================================================================
// Package-level helpers (no Service state — pure functions)
// =====================================================================

// --- idempotency ---

// deserializeIdempotentEmail rebuilds the SendResponse from a cached
// idempotency payload (protojson-serialized pb.SendResponse written by a
// prior successful send).
func deserializeIdempotentEmail(payload []byte) (*pb.SendResponse, error) {
	var resp pb.SendResponse
	if err := protojson.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal cached idempotent response: %w", err)
	}
	return &resp, nil
}

// --- proto ↔ model conversion ---

// toProtoEmailRecord is a PURE protocol conversion — no DB access. Callers
// that need attachments populated must load them separately and assign to
// rec.Attachments. This keeps the function cheap to call inside loops
// (ListEmails / ListEmailsByCursor) without triggering N+1 queries.
func toProtoEmailRecord(r *models.MessageEmailRecord) *pb.EmailRecord {
	rec := &pb.EmailRecord{
		Id:             r.ID,
		Vendor:         pb.EmailVendor(r.Vendor),
		Account:        r.Account,
		Scene:          pb.EmailScene(r.Scene),
		Status:         pb.MessageStatus(r.Status),
		Target:         addrFromBareEmail(r.Target),
		SenderId:       r.SenderID,
		Cc:             addrsFromBareEmails(r.Cc),
		Bcc:            addrsFromBareEmails(r.Bcc),
		Subject:        r.Subject,
		Content:        r.Content,
		HtmlBody:       r.HTMLBody,
		ReplyTo:        addrFromBareEmail(r.ReplyTo),
		TemplateId:     r.TemplateID,
		TemplateParams: map[string]string(r.TemplateParams),
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		CreatedAt:      r.CreatedAt.Unix(),
		UpdatedAt:      r.UpdatedAt.Unix(),
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	return rec
}

// emailAttachmentsToProto converts model rows to proto EmailAttachment. Returns
// nil for nil/empty input so the proto repeated field stays unset (proto3
// semantics: empty repeated is indistinguishable from unset on the wire).
func emailAttachmentsToProto(rows []*models.MessageEmailRecordAttachment) []*pb.EmailAttachment {
	if len(rows) == 0 {
		return nil
	}
	out := make([]*pb.EmailAttachment, 0, len(rows))
	for _, a := range rows {
		out = append(out, &pb.EmailAttachment{
			Filename:  a.Filename,
			Url:       a.URL,
			Inline:    a.Inline,
			MimeType:  a.MimeType,
			SizeBytes: a.SizeBytes,
		})
	}
	return out
}

// --- attachment validation ---

// validateAttachments is a structural pre-check that runs in
// validateSendEmailRequest (defense-in-depth for module-mode calls that bypass
// the protovalidate interceptor). It verifies, per attachment:
//   - filename is set
//   - at least one of url / content is set (url-only = caller-managed
//     download reference; content-only = inline MIME part; both is allowed —
//     content embeds while url is kept as record metadata)
//   - when url is set, scheme is http(s) — the url ends up in caller-rendered
//     download links and stored records, so non-web schemes are rejected
//     defensively even though the service never fetches it
//
// Byte-size caps are NOT enforced here — they live in processAttachments
// where the Service config is available.
//
// Returns xcodes.ErrInvalidAttachment wrapping the first violation so callers
// can errors.Is-check for the specific reason.
func validateAttachments(atts []*pb.EmailAttachment) error {
	for i, a := range atts {
		if a == nil {
			return xcodes.ErrInvalidAttachment.Wrapf(nil, "attachment[%d]: nil", i)
		}
		if a.GetFilename() == "" {
			return xcodes.ErrInvalidAttachment.Wrapf(nil, "attachment[%d]: filename is required", i)
		}
		hasURL := a.GetUrl() != ""
		hasContent := len(a.GetContent()) > 0
		if !hasURL && !hasContent {
			return xcodes.ErrInvalidAttachment.Wrapf(nil, "attachment[%d]: at least one of url or content must be set", i)
		}
		if hasURL {
			parsed, err := url.Parse(a.GetUrl())
			if err != nil {
				return xcodes.ErrInvalidAttachment.Wrapf(nil, "attachment[%d]: invalid url: %v", i, err)
			}
			if parsed.Scheme != "http" && parsed.Scheme != "https" {
				return xcodes.ErrInvalidAttachment.Wrapf(nil, "attachment[%d]: url scheme must be http or https, got %q", i, parsed.Scheme)
			}
		}
	}
	return nil
}

// --- address conversion (proto ↔ provider / bare-email string) ---

// pbToAddr converts a pb.EmailAddress into the provider's value type — pure
// field copy, no formatting. Returns nil for nil input.
func pbToAddr(addr *pb.EmailAddress) *provemail.Address {
	if addr == nil {
		return nil
	}
	return &provemail.Address{Email: addr.GetEmail(), DisplayName: addr.GetDisplayName()}
}

// pbToAddrs applies pbToAddr across a slice, skipping nil entries. Returns
// a non-nil empty slice for nil input so downstream code can range safely.
func pbToAddrs(addrs []*pb.EmailAddress) []*provemail.Address {
	out := make([]*provemail.Address, 0, len(addrs))
	for _, a := range addrs {
		if a == nil {
			continue
		}
		out = append(out, &provemail.Address{Email: a.GetEmail(), DisplayName: a.GetDisplayName()})
	}
	return out
}

// bareEmailFromAddr returns the email portion only — display_name is dropped.
// Used when persisting to DB columns that store bare emails for query
// filtering. Returns "" for nil.
func bareEmailFromAddr(addr *pb.EmailAddress) string {
	if addr == nil {
		return ""
	}
	return addr.GetEmail()
}

// bareEmailsFromAddrs extracts bare emails from a slice, skipping nil
// entries. Returns non-nil empty slice for nil input.
func bareEmailsFromAddrs(addrs []*pb.EmailAddress) []string {
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if e := bareEmailFromAddr(a); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// addrFromBareEmail wraps a bare email string into a pb.EmailAddress with no
// display_name. Returns nil for empty input — proto message fields are
// pointer-typed, nil means "not set".
func addrFromBareEmail(s string) *pb.EmailAddress {
	if s == "" {
		return nil
	}
	return &pb.EmailAddress{Email: s}
}

// addrsFromBareEmails wraps a bare-email slice into pb.EmailAddress slice,
// skipping empty entries.
func addrsFromBareEmails(ss []string) []*pb.EmailAddress {
	out := make([]*pb.EmailAddress, 0, len(ss))
	for _, s := range ss {
		if a := addrFromBareEmail(s); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// --- validation ---

// validateSendEmailRequest enforces required fields and cross-field invariants
// at the service layer. This is a defense-in-depth check that runs even when
// the protovalidate interceptor is bypassed (e.g. module-mode direct calls).
// The proto CEL rules are the primary check; this is the fallback.
//
// Shared constants (MaxIdempotencyKeyLen, PersistTimeout) and helpers
// (TruncateErrorMessage) live in internal/service/utils — see utils.go.
func validateSendEmailRequest(req *pb.SendEmailRequest) error {
	vendorSet := req.GetVendor() != pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	accountSet := req.GetAccount() != ""
	if vendorSet != accountSet {
		return fmt.Errorf("vendor and account must be set together")
	}
	if req.GetScene() == pb.EmailScene_EMAIL_SCENE_UNSPECIFIED {
		return fmt.Errorf("scene is required")
	}
	if req.GetSenderId() == "" {
		return fmt.Errorf("sender_id is required")
	}
	if len(req.GetTo()) == 0 {
		return fmt.Errorf("at least one to recipient is required")
	}
	if len(req.GetIdempotencyKey()) > utils.MaxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key too long (max %d)", utils.MaxIdempotencyKeyLen)
	}
	if err := validateAttachments(req.GetAttachments()); err != nil {
		// validateAttachments already returns xcodes.ErrInvalidAttachment
		// (category BadRequest, HTTP 400) wrapping the specific violation.
		// Do not re-wrap with fmt.Errorf — that would hide the typed error
		// from errors.Is callers. SendEmail wraps with ErrBadRequest below,
		// which still allows errors.Is(err, ErrInvalidAttachment) via Unwrap.
		return err
	}
	return nil
}
