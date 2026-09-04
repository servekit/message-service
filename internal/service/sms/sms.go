package sms

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	pb "github.com/servekit/message-service/gen/message/v1"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/service/common"
	"github.com/servekit/message-service/internal/service/utils"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/internal/utils/pagination"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"github.com/nyaruka/phonenumbers"
	"google.golang.org/protobuf/encoding/protojson"
)

// SendSMS sends an SMS via the configured vendor/account, or routes by phone
// country code when both are unset. Idempotent on (sender_id, idempotency_key)
// via Redis: a second request with the same key returns the cached response
// without re-invoking the provider. Failures are not cached — the reservation
// is released so the caller can retry the same key.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	if err := validateSendSMSRequest(req); err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	// Idempotency reservation (Redis-backed). Always runs regardless of
	// persistence toggle — Redis is the single source of dedup truth.
	var idemKey string
	if k := req.GetIdempotencyKey(); k != "" {
		idemKey = k
		acquired, payload, err := s.idem.Reserve(ctx, "sms", req.GetSenderId(), k)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		if !acquired {
			if payload == nil {
				return nil, xcodes.ErrIdempotencyConflict.New("idempotency_key in flight")
			}
			resp, err := deserializeIdempotentSMS(payload)
			if err != nil {
				return nil, xcodes.ErrInternal.Wrap(err)
			}
			return resp, nil
		}
	}

	id, err := common.NextID(ctx, s.gid)
	if err != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after gid failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	// Parse + format to E.164 so router/providers receive an unambiguous
	// input. validateSendSMSRequest already verified parse succeeds and
	// region matches; the error here is theoretical defense-in-depth.
	num, err := phonenumbers.Parse(req.GetPhone(), req.GetRegionCode())
	if err != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after phone parse failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrBadRequest.Wrapf(err, "parse phone %q", req.GetPhone())
	}
	e164 := phonenumbers.Format(num, phonenumbers.E164)

	// Path selection by destination region: CN → domestic (template-based),
	// anything else → international (raw content). The two paths are strictly
	// separated at the AccountProvider interface; the service picks one based
	// on phone region and never crosses over.
	regionCode := req.GetRegionCode()
	var result *provesms.SendResult
	var sendErr error
	if regionCode == "CN" {
		domesticMsg := &provesms.Message{
			To:             e164,
			SignName:       req.GetSignName(),
			TemplateID:     req.GetTemplateId(),
			TemplateParams: models.MapStringString(req.GetTemplateParams()),
		}
		if req.GetVendor() != pb.SmsVendor_SMS_VENDOR_UNSPECIFIED {
			sender, err := s.smsRegistry.SenderFor(req.GetVendor(), req.GetAccount())
			if err != nil {
				if idemKey != "" {
					releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
					if releaseErr != nil {
						slog.Error("idempotency release after sender lookup failure", "key", idemKey, "error", releaseErr)
					}
				}
				return nil, xcodes.ErrBadRequest.Wrap(err)
			}
			result, sendErr = sender.Send(ctx, domesticMsg)
		} else {
			if s.smsRouter == nil {
				if idemKey != "" {
					releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
					if releaseErr != nil {
						slog.Error("idempotency release after router missing", "key", idemKey, "error", releaseErr)
					}
				}
				return nil, xcodes.ErrBadRequest.New("sms routes not configured; specify vendor and account explicitly")
			}
			result, sendErr = s.smsRouter.Send(ctx, domesticMsg)
		}
	} else {
		// International path. Two vendor camps here: raw-content vendors
		// (Aliyun SendMessageToGlobe, Twilio, AWS SNS) consume Content;
		// template-based vendors (Byteplus, Tencent with intl SdkAppid)
		// consume TemplateID + TemplateParams. Pass whichever the caller
		// set; vendor adapters pick based on their SDK capability.
		intlMsg := &provesms.InternationalMessage{
			To:             e164,
			SignName:       req.GetSignName(),
			Content:        req.GetContent(),
			TemplateID:     req.GetTemplateId(),
			TemplateParams: models.MapStringString(req.GetTemplateParams()),
		}
		if req.GetVendor() != pb.SmsVendor_SMS_VENDOR_UNSPECIFIED {
			sender, err := s.smsRegistry.SenderFor(req.GetVendor(), req.GetAccount())
			if err != nil {
				if idemKey != "" {
					releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
					if releaseErr != nil {
						slog.Error("idempotency release after sender lookup failure", "key", idemKey, "error", releaseErr)
					}
				}
				return nil, xcodes.ErrBadRequest.Wrap(err)
			}
			result, sendErr = sender.SendInternational(ctx, intlMsg)
		} else {
			if s.smsRouter == nil {
				if idemKey != "" {
					releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
					if releaseErr != nil {
						slog.Error("idempotency release after router missing", "key", idemKey, "error", releaseErr)
					}
				}
				return nil, xcodes.ErrBadRequest.New("sms routes not configured; specify vendor and account explicitly")
			}
			result, sendErr = s.smsRouter.SendInternational(ctx, intlMsg)
		}
	}

	// Pre-send failure (empty recipient / no provider / SMS router top-level
	// cancel short-circuit): no result to persist.
	// Release the idempotency reservation so the caller can retry the key.
	if sendErr != nil && result == nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
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
		s.persistSMSRecord(persistCtx, id, req, result)
	}

	// Post-send failure (vendor returned a failed result, not a pre-send
	// error): Release the reservation so the caller can retry — failures are
	// not cached. Must run BEFORE Complete to avoid a window where a fake
	// Status=SENT payload is observable to a concurrent caller with the same
	// idempotency_key.
	if sendErr != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
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
			Vendor: &pb.SendResponse_SmsVendor{
				SmsVendor: result.Vendor,
			},
		}
		payload, err := protojson.Marshal(resp)
		if err != nil {
			slog.Error("idempotency marshal", "key", idemKey, "error", err)
		} else if err := s.idem.Complete(context.Background(), "sms", req.GetSenderId(), idemKey, payload); err != nil {
			slog.Error("idempotency complete", "key", idemKey, "error", err)
		}
	}

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_SmsVendor{
			SmsVendor: result.Vendor,
		},
	}, nil
}

// GetSMS returns a single SMS record by ID.
func (s *Service) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	record, err := dal.GetSMSRecord(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	return toProtoSMSRecord(record), nil
}

// ListSMS returns a paginated list of SMS records matching the filter.
func (s *Service) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		RegionCode:    req.GetRegionCode(),
		Phone:         req.GetPhone(),
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

	result, err := dal.ListSMSRecords(ctx, s.db, f, dbx.PageParams{
		Page:     int(req.GetPage()),
		PageSize: int(req.GetPageSize()),
		Count:    true,
	})
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.SMSRecord, len(result.List))
	for i, r := range result.List {
		protoRecords[i] = toProtoSMSRecord(r)
	}

	return &pb.ListSMSResponse{
		Records:    protoRecords,
		Total:      int32(result.Total),
		TotalPages: int32(result.TotalPages),
		HasMore:    req.GetPage() < int32(result.TotalPages),
	}, nil
}

// ListSMSByCursor is the cursor-paginated counterpart of ListSMS.
// Prefer this over ListSMS for large datasets or when COUNT(*) is expensive —
// set include_total = true to opt in to a count query.
func (s *Service) ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsListFilter{
		Vendor:        req.GetVendor(),
		Scene:         req.GetScene(),
		Status:        req.GetStatus(),
		RegionCode:    req.GetRegionCode(),
		Phone:         req.GetPhone(),
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

	records, err := dal.ListSMSByCursor(ctx, s.db, f, pg, afterCreatedAt)
	if err != nil {
		return nil, err
	}

	trimmed, hasNext := dbx.TrimPage(records, pg.PageSize)

	protoRecords := make([]*pb.SMSRecord, len(trimmed))
	for i, r := range trimmed {
		protoRecords[i] = toProtoSMSRecord(r)
	}

	var total int32
	if req.GetIncludeTotal() {
		// Cheap path: if this is the first page and it fit in one go,
		// total == len(trimmed). Otherwise run a real count.
		if !hasNext && pg.AfterID == 0 {
			total = int32(len(trimmed))
		} else {
			count, err := dal.CountSMSRecords(ctx, s.db, f)
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

	return &pb.ListSMSByCursorResponse{
		Records:       protoRecords,
		Total:         total,
		NextPageToken: nextToken,
	}, nil
}

// GetSMSStats returns aggregated statistics for SMS messages matching the filter.
func (s *Service) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsStatsFilter{
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

	stats, err := dal.CountSMSStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendorStats, err := dal.ListSMSVendorStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendors := make([]*pb.SmsVendorStats, len(vendorStats))
	for i, vs := range vendorStats {
		vendors[i] = &pb.SmsVendorStats{
			Vendor: vs.Vendor,
			Total:  vs.Total,
			Sent:   vs.Sent,
			Failed: vs.Failed,
		}
	}

	return &pb.SMSStatsResponse{
		Total:       stats.Total,
		Sent:        stats.Sent,
		Failed:      stats.Failed,
		SuccessRate: stats.SuccessRate,
		Vendors:     vendors,
	}, nil
}

// ListSMSRegions returns all distinct region_code values, for frontend SMS
// list filter dropdowns.
func (s *Service) ListSMSRegions(ctx context.Context, _ *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	regions, err := dal.ListSMSRegions(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return &pb.ListSMSRegionsResponse{RegionCodes: regions}, nil
}

// ListSMSSenders returns all distinct sender_id values, for frontend SMS
// list filter dropdowns.
func (s *Service) ListSMSSenders(ctx context.Context, _ *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
	if !s.persistence {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	senders, err := dal.ListSMSSenderIDs(ctx, s.db)
	if err != nil {
		return nil, err
	}
	return &pb.ListSMSSendersResponse{SenderIds: senders}, nil
}

// --- record persistence (synchronous, error-logged) ---

func (s *Service) persistSMSRecord(ctx context.Context, id int64, req *pb.SendSMSRequest, result *provesms.SendResult) {
	record := &models.MessageSMSRecord{
		ID:             id,
		Vendor:         int32(result.Vendor),
		Account:        result.Account,
		Scene:          int32(req.GetScene()),
		RegionCode:     req.GetRegionCode(),
		Phone:          req.GetPhone(),
		SenderID:       req.GetSenderId(),
		Content:        req.GetContent(),
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

	if err := dal.CreateSMSRecord(ctx, s.db, record); err != nil {
		slog.Error("persist sms record", "record_id", id, "error", err)
	}
}

// --- idempotency ---

// deserializeIdempotentSMS rebuilds the SendResponse from a cached
// idempotency payload (protojson-serialized pb.SendResponse written by a
// prior successful send).
func deserializeIdempotentSMS(payload []byte) (*pb.SendResponse, error) {
	var resp pb.SendResponse
	if err := protojson.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal cached idempotent response: %w", err)
	}
	return &resp, nil
}

// --- proto ↔ model conversion ---

func toProtoSMSRecord(r *models.MessageSMSRecord) *pb.SMSRecord {
	rec := &pb.SMSRecord{
		Id:             r.ID,
		Vendor:         pb.SmsVendor(r.Vendor),
		Account:        r.Account,
		Scene:          pb.SmsScene(r.Scene),
		Status:         pb.MessageStatus(r.Status),
		RegionCode:     r.RegionCode,
		Phone:          r.Phone,
		SenderId:       r.SenderID,
		Content:        r.Content,
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

// --- validation ---

// regionCodePattern matches ISO 3166-1 alpha-2 codes: exactly two uppercase
// ASCII letters. phonenumbers.Parse accepts this as the defaultRegion arg.
// This duplicates the proto CEL pattern ^[A-Z]{2}$ as defense-in-depth for
// module-mode calls that bypass the protovalidate interceptor.
var regionCodePattern = regexp.MustCompile(`^[A-Z]{2}$`)

// validateSendSMSRequest enforces required fields + phone/region invariants
// at the service layer. Defense-in-depth that runs even when the
// protovalidate interceptor is bypassed (e.g. module-mode direct calls).
//
// Phone is expected in local format (no '+' prefix); region_code is the
// defaultRegion hint for phonenumbers.Parse.
//
// Path-specific requirements (region-based):
//   - region_code == "CN" (domestic): template_id and sign_name required,
//     content ignored (domestic vendors reject raw content for regulatory
//     reasons — every SMS must use a pre-registered template).
//   - other region_code (international): content required, template_id and
//     template_params ignored (international path is raw-content-based).
//
// Shared constants (MaxIdempotencyKeyLen, PersistTimeout) and helpers
// (TruncateErrorMessage) live in internal/service/utils — see utils.go.
func validateSendSMSRequest(req *pb.SendSMSRequest) error {
	vendorSet := req.GetVendor() != pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
	accountSet := req.GetAccount() != ""
	if vendorSet != accountSet {
		return fmt.Errorf("vendor and account must be set together")
	}
	if req.GetScene() == pb.SmsScene_SMS_SCENE_UNSPECIFIED {
		return fmt.Errorf("scene is required")
	}
	if req.GetSenderId() == "" {
		return fmt.Errorf("sender_id is required")
	}
	if len(req.GetIdempotencyKey()) > utils.MaxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key too long (max %d)", utils.MaxIdempotencyKeyLen)
	}

	// Cheap syntactic checks before the more expensive phonenumbers.Parse.
	rc := req.GetRegionCode()
	if !regionCodePattern.MatchString(rc) {
		return fmt.Errorf("region_code must be 2 uppercase letters (ISO 3166-1 alpha-2), got %q", rc)
	}
	phone := req.GetPhone()
	if phone == "" {
		return fmt.Errorf("phone is required")
	}
	// Local-format phone must not carry an international prefix; the caller
	// supplies region_code instead.
	if phone[0] == '+' {
		return fmt.Errorf("phone must not start with '+' — provide local number only; region_code disambiguates the country")
	}

	// Path-specific field requirements. CN is template-only (regulatory);
	// international vendors split into two camps — raw-content (Aliyun intl,
	// Twilio) and template-based (Byteplus, Tencent intl) — so we require
	// either Content or TemplateID, not both, not neither.
	if rc == "CN" {
		if req.GetTemplateId() == "" {
			return fmt.Errorf("template_id is required for CN (domestic) SMS — vendors reject raw content")
		}
		if req.GetSignName() == "" {
			return fmt.Errorf("sign_name is required for CN (domestic) SMS")
		}
	} else {
		hasContent := req.GetContent() != ""
		hasTemplate := req.GetTemplateId() != ""
		if hasContent == hasTemplate {
			return fmt.Errorf("international (non-CN) SMS requires exactly one of content or template_id")
		}
	}

	num, err := phonenumbers.Parse(phone, rc)
	if err != nil {
		return fmt.Errorf("parse phone %q with region %q: %w", phone, rc, err)
	}
	if !phonenumbers.IsValidNumber(num) {
		return fmt.Errorf("phone %q is not a valid number in region %q", phone, rc)
	}
	if got := phonenumbers.GetRegionCodeForNumber(num); got != rc {
		return fmt.Errorf("phone %q parses as region %q, not %q", phone, got, rc)
	}
	return nil
}
