package sms

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/service/utils"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/nyaruka/phonenumbers"
	"google.golang.org/protobuf/encoding/protojson"
)

// SendSMS sends a policy-driven SMS: resolves (tenant, SMS, scene) → policy
// → template + CN/intl route chains, picks the chain by the destination
// country parsed from the E.164 number, and sends with ordered fallback
// (weighted start, then list order). Idempotent on (tenant_key,
// idempotency_key) via Redis.
//
// Phase ③: tenantKey is the caller's resolved tenant context (tenantres);
// app is its config row (daily limits, audit labels).
func (s *Service) SendSMS(ctx context.Context, app *models.MessageApp, tenantKey string, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	if err := validateSendSMSRequest(req); err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	snap := s.reg.Current()
	policy := snap.Policy(tenantKey, int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS), int32(req.GetScene()))
	if policy == nil {
		return nil, xcodes.ErrPolicyNotFound.New(fmt.Sprintf(
			"no SMS policy for tenant %q scene %s; configured scenes: %s",
			tenantKey, req.GetScene(), sceneList(snap, tenantKey, pb.TemplateChannel_TEMPLATE_CHANNEL_SMS)))
	}

	// Daily quota (attempts counted), namespaced by tenant_key (③ re-keying;
	// for backfilled apps tenant_key == app_key so the counters carry over).
	// Runs before the idempotency reservation so rejected sends never
	// consume the reservation.
	if err := s.quota.Allow(ctx, tenantKey, "sms", app.SMSDailyLimit); err != nil {
		return nil, err
	}

	// Idempotency reservation (Redis-backed), namespaced by tenant_key —
	// ③ re-keying: a key replayed under a different tenant is a fresh
	// namespace (in-flight reservations under a pre-remap app_key are not
	// visible after T10 remaps the column; the TTL bounds the window).
	// Always runs regardless of persistence toggle — Redis is the single
	// source of dedup truth.
	var idemKey string
	if k := req.GetIdempotencyKey(); k != "" {
		idemKey = k
		acquired, payload, err := s.idem.Reserve(ctx, "sms", tenantKey, k)
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
	release := func(reason string) {
		if idemKey == "" {
			return
		}
		if releaseErr := s.idem.Release(context.Background(), "sms", tenantKey, idemKey); releaseErr != nil {
			slog.Error("idempotency release after "+reason, "key", idemKey, "error", releaseErr)
		}
	}

	// Resolve template + validate params before any provider call.
	template := snap.Template(policy.TemplateID)
	if template == nil || template.Disabled {
		release("template missing")
		return nil, xcodes.ErrTemplateNotFound.New(fmt.Sprintf("policy template %d not found or disabled", policy.TemplateID))
	}
	specs, err := send.ParseParamSpecs(template.Params)
	if err != nil {
		release("template params corrupt")
		return nil, xcodes.ErrInvalidTemplateContent.Wrap(err)
	}
	// Free-form mode skips required-param enforcement (the author owns the
	// content); template mode enforces the declared params.
	if req.GetContent() == "" {
		if err := send.ValidateParams(specs, req.GetTemplateParams()); err != nil {
			release("template param missing")
			return nil, err
		}
	}

	id, err := gidservice.NextID(ctx, s.gid)
	if err != nil {
		release("gid failure")
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	// Parse the E.164 destination; the destination country decides the CN
	// vs international chain. validateSendSMSRequest already verified the
	// parse succeeds — the error here is theoretical defense-in-depth.
	num, err := phonenumbers.Parse(req.GetTo(), "")
	if err != nil {
		release("phone parse failure")
		return nil, xcodes.ErrBadRequest.Wrapf(err, "parse phone %q", req.GetTo())
	}
	e164 := phonenumbers.Format(num, phonenumbers.E164)
	regionCode := phonenumbers.GetRegionCodeForNumber(num)

	// Route chain: CN destinations use Routes, others use IntlRoutes.
	routesRaw := policy.Routes
	if regionCode != "CN" {
		routesRaw = policy.IntlRoutes
	}
	routes, err := send.ParseRoutes(routesRaw)
	if err != nil {
		release("routes corrupt")
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if len(routes) == 0 {
		release("no intl route")
		return nil, xcodes.ErrPolicyNotFound.New(fmt.Sprintf(
			"no SMS route chain configured for tenant %q scene %s to region %s",
			tenantKey, req.GetScene(), regionCode))
	}

	// Vendor-code templates resolve per provider (fallback may cross
	// vendors, each with its own template code).
	var vendorCodes map[pb.SmsVendor]string
	var intlContent string
	switch pb.TemplateKind(template.Kind) {
	case pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES:
		vendorCodes, err = send.DecodeVendorCodes(template.Content)
		if err != nil {
			release("vendor codes corrupt")
			return nil, err
		}
	case pb.TemplateKind_TEMPLATE_KIND_SMS_CONTENT:
		if regionCode == "CN" {
			release("content template on CN path")
			return nil, xcodes.ErrInvalidTemplateContent.New(
				"sms-content templates cannot serve CN destinations (vendor template codes required)")
		}
		var c *send.SmsContent
		c, err = send.DecodeSmsContent(template.Content)
		if err != nil {
			release("sms content corrupt")
			return nil, err
		}
		intlContent = send.Render(c.Content, req.GetTemplateParams())
	default:
		release("template kind mismatch")
		return nil, xcodes.ErrInvalidTemplateContent.New(fmt.Sprintf(
			"template %d kind %s is not an SMS template", template.ID, pb.TemplateKind(template.Kind)))
	}
	// Free-form mode (request content set, intl only — validated above):
	// the request body IS the content ({{param}} rendered); template
	// required-params are not enforced (the author owns the text). Template-
	// based vendors in the chain still use their per-vendor codes, so a
	// mixed chain serves both camps.
	if req.GetContent() != "" {
		intlContent = send.Render(req.GetContent(), req.GetTemplateParams())
	}

	// Dispatch along the ordered chain: weighted start, then list order.
	// Providers/signatures that fail snapshot resolution are skipped.
	result := s.dispatchChain(ctx, snap, e164, regionCode == "CN", routes, vendorCodes, intlContent, req.GetTemplateParams())

	// Pre-send failure (no usable provider at all): no result to persist.
	if result.err != nil && result.vendor == pb.SmsVendor_SMS_VENDOR_UNSPECIFIED && result.attempts == 0 {
		release("pre-send failure")
		return nil, xcodes.ErrMessageSendFailed.Wrapf(result.err, "stage=pre_send")
	}

	if s.persistence {
		persistCtx, cancel := context.WithTimeout(context.Background(), utils.PersistTimeout)
		defer cancel()
		s.persistSMSRecord(persistCtx, id, app, req, e164, regionCode, intlContent, result)
	}

	if result.err != nil {
		// Post-send failure: release so the caller can retry — failures
		// are not cached.
		release("post-send failure")
		return nil, xcodes.ErrMessageSendFailed.Wrapf(result.err,
			"vendor=%s account=%s attempts=%d", result.vendor.String(), result.account, result.attempts)
	}

	if idemKey != "" {
		resp := &pb.SendResponse{
			Id:     id,
			Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
			Vendor: &pb.SendResponse_SmsVendor{SmsVendor: result.vendor},
		}
		payload, err := protojson.Marshal(resp)
		if err != nil {
			slog.Error("idempotency marshal", "key", idemKey, "error", err)
		} else if err := s.idem.Complete(context.Background(), "sms", tenantKey, idemKey, payload); err != nil {
			slog.Error("idempotency complete", "key", idemKey, "error", err)
		}
	}

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_SmsVendor{SmsVendor: result.vendor},
	}, nil
}

// intlContentUsed reports the audit content for a record: the rendered
// intl body, empty on the CN path.
func intlContentUsed(regionCode, intlContent string) string {
	if regionCode == "CN" {
		return ""
	}
	return intlContent
}

// smsOutcome summarizes a chain dispatch.
type smsOutcome struct {
	vendor       pb.SmsVendor
	account      string
	signName     string
	templateCode string
	attempts     int
	err          error
}

// dispatchChain walks the ordered route chain. For each route: resolve the
// live provider and bound signature from the snapshot (skip on miss), build
// the vendor-specific message (per-vendor template code for VENDOR_CODES
// templates; rendered content for SMS_CONTENT), and attempt the send.
// Falls through on failure; first success wins.
func (s *Service) dispatchChain(
	ctx context.Context,
	snap *registry.Snapshot,
	e164 string,
	domestic bool,
	routes []send.Route,
	vendorCodes map[pb.SmsVendor]string,
	intlContent string,
	params map[string]string,
) *smsOutcome {
	out := &smsOutcome{}
	var lastErr error
	for _, route := range send.OrderRoutes(routes) {
		if ctx.Err() != nil {
			out.err = ctx.Err()
			return out
		}
		provider := snap.SMSProvider(route.AccountID)
		if provider == nil {
			continue
		}
		account := snap.ChannelAccount(route.AccountID)
		sig := snap.Signature(route.SignatureID)
		if sig == nil || sig.Disabled || !snap.SignatureBound(route.SignatureID, route.AccountID) {
			continue
		}

		var sendErr error
		if domestic {
			code, ok := vendorCodes[provider.Vendor()]
			if !ok {
				lastErr = fmt.Errorf("template has no code for vendor %s", provider.Vendor())
				continue
			}
			out.attempts++
			out.vendor, out.account, out.signName, out.templateCode = provider.Vendor(), account.Name, sig.Name, code
			sendErr = provider.Send(ctx, &provesms.Message{
				To:             e164,
				SignName:       sig.Name,
				TemplateID:     code,
				TemplateParams: params,
			})
		} else {
			msg := &provesms.InternationalMessage{
				To:       e164,
				SignName: sig.Name,
				Content:  intlContent,
			}
			if vendorCodes != nil {
				code, ok := vendorCodes[provider.Vendor()]
				if !ok {
					lastErr = fmt.Errorf("template has no code for vendor %s", provider.Vendor())
					continue
				}
				msg.TemplateID = code
				msg.TemplateParams = params
			}
			out.attempts++
			out.vendor, out.account, out.signName, out.templateCode = provider.Vendor(), account.Name, sig.Name, msg.TemplateID
			sendErr = provider.SendInternational(ctx, msg)
		}
		if sendErr == nil {
			return out
		}
		lastErr = sendErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no usable provider in route chain")
	}
	out.err = lastErr
	return out
}

// sceneList renders configured scene values for the POLICY_NOT_FOUND error.
func sceneList(snap *registry.Snapshot, tenantKey string, channel pb.TemplateChannel) string {
	scenes := snap.AppScenes(tenantKey, int32(channel))
	if len(scenes) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(scenes))
	for _, sc := range scenes {
		parts = append(parts, pb.SmsScene(sc).String())
	}
	return strings.Join(parts, ", ")
}

// --- record persistence (synchronous, error-logged) ---

func (s *Service) persistSMSRecord(ctx context.Context, id int64, app *models.MessageApp, req *pb.SendSMSRequest, e164, regionCode, intlContent string, result *smsOutcome) {
	record := &models.MessageSMSRecord{
		ID:         id,
		Vendor:     int32(result.vendor),
		Account:    result.account,
		Scene:      int32(req.GetScene()),
		RegionCode: regionCode,
		Phone:      e164,
		AppKey:     app.AppKey,
		SignName:   result.signName,
		// CN content lives at the vendor (TemplateID carries the code);
		// intl carries the rendered body actually sent.
		Content:        intlContentUsed(regionCode, intlContent),
		TemplateID:     result.templateCode,
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.attempts,
	}

	if result.err == nil {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		record.ErrorMessage = utils.TruncateErrorMessage(result.err.Error())
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
		AppKey:         r.AppKey,
		Content:        r.Content,
		TemplateId:     r.TemplateID,
		TemplateParams: map[string]string(r.TemplateParams),
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		SignName:       r.SignName,
		CreatedAt:      r.CreatedAt.Unix(),
		UpdatedAt:      r.UpdatedAt.Unix(),
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	return rec
}

// --- validation ---

// validateSendSMSRequest enforces required fields + phone invariants at the
// service layer. Defense-in-depth that runs even when the protovalidate
// interceptor is bypassed (e.g. module-mode direct calls).
func validateSendSMSRequest(req *pb.SendSMSRequest) error {
	if req.GetScene() == pb.SmsScene_SMS_SCENE_UNSPECIFIED {
		return fmt.Errorf("scene is required")
	}
	if len(req.GetIdempotencyKey()) > utils.MaxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key too long (max %d)", utils.MaxIdempotencyKeyLen)
	}

	// Cheap syntactic checks before the more expensive phonenumbers.Parse.
	to := req.GetTo()
	if to == "" {
		return fmt.Errorf("to is required")
	}
	if to[0] != '+' {
		return fmt.Errorf("to must be E.164 international format (\"+<countrycode><national>\", e.g. \"+8613800138000\")")
	}

	// The destination country decides the route chain — derived from the
	// E.164 number itself, never supplied separately.
	num, err := phonenumbers.Parse(to, "")
	if err != nil {
		return fmt.Errorf("parse phone %q: %w", to, err)
	}
	if !phonenumbers.IsValidNumber(num) {
		return fmt.Errorf("phone %q is not a valid number", to)
	}
	if nt := phonenumbers.GetNumberType(num); !smsCapableNumberType(nt) {
		return fmt.Errorf("phone %q is a %s number; SMS requires a mobile/SMS-capable number", to, numberTypeName(nt))
	}
	rc := phonenumbers.GetRegionCodeForNumber(num)
	if rc == "" || rc == "ZZ" {
		return fmt.Errorf("phone %q has no resolvable destination country", to)
	}
	// Free-form content is international-only: CN destinations must use
	// vendor pre-registered templates (regulatory).
	if req.GetContent() != "" && rc == "CN" {
		return fmt.Errorf("content is not allowed for CN (domestic) SMS — vendors require pre-registered templates")
	}
	return nil
}

// smsCapableNumberType reports whether a number type can receive SMS.
// MOBILE is the obvious yes; FIXED_LINE_OR_MOBILE covers regions whose
// metadata cannot distinguish (US NANP); VOIP numbers (Google Voice and
// similar) receive SMS in practice; UNKNOWN is admitted because the number
// already passed IsValidNumber — the region's metadata simply lacks type
// patterns. Everything else (FIXED_LINE, TOLL_FREE, PREMIUM_RATE, PAGER,
// UAN, VOICEMAIL, ...) cannot receive SMS and is rejected before any
// vendor call.
func smsCapableNumberType(nt phonenumbers.PhoneNumberType) bool {
	switch nt {
	case phonenumbers.MOBILE,
		phonenumbers.FIXED_LINE_OR_MOBILE,
		phonenumbers.VOIP,
		phonenumbers.PERSONAL_NUMBER,
		phonenumbers.UNKNOWN:
		return true
	default:
		return false
	}
}

// numberTypeName renders a PhoneNumberType for error messages.
func numberTypeName(nt phonenumbers.PhoneNumberType) string {
	switch nt {
	case phonenumbers.FIXED_LINE:
		return "fixed-line"
	case phonenumbers.MOBILE:
		return "mobile"
	case phonenumbers.FIXED_LINE_OR_MOBILE:
		return "fixed-line-or-mobile"
	case phonenumbers.TOLL_FREE:
		return "toll-free"
	case phonenumbers.PREMIUM_RATE:
		return "premium-rate"
	case phonenumbers.SHARED_COST:
		return "shared-cost"
	case phonenumbers.VOIP:
		return "voip"
	case phonenumbers.PERSONAL_NUMBER:
		return "personal-number"
	case phonenumbers.PAGER:
		return "pager"
	case phonenumbers.UAN:
		return "uan"
	case phonenumbers.VOICEMAIL:
		return "voicemail"
	default:
		return "unknown"
	}
}
