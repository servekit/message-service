// Send path of the email domain: policy-driven SendEmail with ordered
// provider fallback. Query/persist/attachment helpers live in email.go.
package email

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	provemail "github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/service/utils"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"google.golang.org/protobuf/encoding/protojson"
)

// SendEmail sends a policy-driven email: resolves (app, EMAIL, scene) →
// policy → template + ordered provider routes, renders subject/body from
// the template with {{param}} substitution, and sends along the route chain
// with fallback. Idempotent on (app_key, idempotency_key) via Redis.
func (s *Service) SendEmail(ctx context.Context, app *models.MessageApp, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	if err := validateSendEmailRequest(req); err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	snap := s.reg.Current()
	policy := snap.Policy(app.ID, int32(pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL), int32(req.GetScene()))
	if policy == nil {
		return nil, xcodes.ErrPolicyNotFound.New(fmt.Sprintf(
			"no email policy for app %q scene %s; configured scenes: %s",
			app.AppKey, req.GetScene(), sceneList(snap, app.ID, pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL)))
	}

	// Daily quota (attempts counted). Runs before the idempotency
	// reservation so rejected sends never consume the reservation.
	if err := s.quota.Allow(ctx, app.AppKey, "email", app.EmailDailyLimit); err != nil {
		return nil, err
	}

	// Idempotency reservation (Redis-backed). Always runs regardless of
	// persistence toggle — Redis is the single source of dedup truth.
	var idemKey string
	if k := req.GetIdempotencyKey(); k != "" {
		idemKey = k
		acquired, payload, err := s.idem.Reserve(ctx, "email", app.AppKey, k)
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
	release := func(reason string) {
		if idemKey == "" {
			return
		}
		if releaseErr := s.idem.Release(context.Background(), "email", app.AppKey, idemKey); releaseErr != nil {
			slog.Error("idempotency release after "+reason, "key", idemKey, "error", releaseErr)
		}
	}

	// Resolve template + validate params before any provider call.
	template := snap.Template(policy.TemplateID)
	if template == nil || template.Disabled {
		release("template missing")
		return nil, xcodes.ErrTemplateNotFound.New(fmt.Sprintf("policy template %d not found or disabled", policy.TemplateID))
	}
	if pb.TemplateChannel(template.Channel) != pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL {
		release("template channel mismatch")
		return nil, xcodes.ErrInvalidTemplateContent.New(fmt.Sprintf("template %d is not an email template", template.ID))
	}
	specs, err := send.ParseParamSpecs(template.Params)
	if err != nil {
		release("template params corrupt")
		return nil, xcodes.ErrInvalidTemplateContent.Wrap(err)
	}
	if err := send.ValidateParams(specs, req.GetTemplateParams()); err != nil {
		release("template param missing")
		return nil, err
	}
	content, err := send.DecodeEmailContent(template.Content)
	if err != nil {
		release("template content corrupt")
		return nil, err
	}
	subject := send.Render(content.Subject, req.GetTemplateParams())
	textBody := send.Render(content.TextBody, req.GetTemplateParams())
	htmlBody := send.Render(content.HTMLBody, req.GetTemplateParams())

	// Provider chain from the policy routes (weighted start, list order
	// fallback). Accounts missing from the snapshot are skipped.
	routes, err := send.ParseRoutes(policy.Routes)
	if err != nil {
		release("routes corrupt")
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	var providers []provemail.AccountProvider
	for _, route := range send.OrderRoutes(routes) {
		if p := snap.EmailProvider(route.AccountID); p != nil {
			providers = append(providers, p)
		}
	}
	if len(providers) == 0 {
		release("no usable provider")
		return nil, xcodes.ErrMessageSendFailed.New(fmt.Sprintf("no usable email provider in policy route chain"))
	}
	sender := provemail.NewSender(providers)

	id, err := gidservice.NextID(ctx, s.gid)
	if err != nil {
		release("gid failure")
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	// Attachment processing: wrap inline-content attachments as MIME parts.
	// url-only attachments are pure references (caller-managed download
	// links) — not fetched, not embedded; their metadata is persisted for
	// record queries.
	mimeAtts, err := s.processAttachments(req.GetAttachments())
	if err != nil {
		release("attachment processing failure")
		return nil, err
	}

	msg := &provemail.Message{
		To:      pbToAddrs(req.GetTo()),
		Cc:      pbToAddrs(req.GetCc()),
		Bcc:     pbToAddrs(req.GetBcc()),
		Subject: subject,
		Body:    textBody,
		HTMLBody: htmlBody,
		ReplyTo: pbToAddr(req.GetReplyTo()),
		// Template carries the platform template ID as an audit label (SMTP
		// ignores it — rendering already happened platform-side).
		Template:       strconv.FormatInt(template.ID, 10),
		TemplateParams: req.GetTemplateParams(),
		Attachments:    mimeAtts,
	}

	result, sendErr := sender.Send(ctx, msg)

	// Pre-send failure (empty recipient / no provider): no result to
	// persist. Release the reservation so the caller can retry the key.
	if sendErr != nil && result == nil {
		release("pre-send failure")
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr, "stage=pre_send")
	}

	// result is guaranteed non-nil from here. Persist with an independent
	// context so request cancellation does not lose the record. Skipped
	// when persistence disabled — caller opted out of DB writes.
	if s.persistence {
		s.persistEmailRecordWithTimeout(id, app, req, subject, textBody, htmlBody, template.ID, result)
	}

	// Post-send failure: Release so the caller can retry — failures are not
	// cached. Must run BEFORE Complete to avoid a window where a fake
	// Status=SENT payload is observable to a concurrent caller with the
	// same idempotency_key.
	if sendErr != nil {
		release("post-send failure")
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
			"vendor=%s account=%s attempts=%d",
			result.Vendor.String(), result.Account, result.Attempts)
	}

	// Success: cache the response payload so a second call with the same
	// key returns the cached result. Errors are logged but don't affect
	// the response — the send already succeeded.
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
		} else if err := s.idem.Complete(context.Background(), "email", app.AppKey, idemKey, payload); err != nil {
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

// sceneList renders configured scene values for the POLICY_NOT_FOUND error.
func sceneList(snap *registry.Snapshot, appID int64, channel pb.TemplateChannel) string {
	scenes := snap.AppScenes(appID, int32(channel))
	if len(scenes) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(scenes))
	for _, sc := range scenes {
		parts = append(parts, pb.EmailScene(sc).String())
	}
	return strings.Join(parts, ", ")
}

// validateSendEmailRequest enforces required fields at the service layer.
// Defense-in-depth that runs even when the protovalidate interceptor is
// bypassed (e.g. module-mode direct calls).
func validateSendEmailRequest(req *pb.SendEmailRequest) error {
	if req.GetScene() == pb.EmailScene_EMAIL_SCENE_UNSPECIFIED {
		return fmt.Errorf("scene is required")
	}
	if len(req.GetTo()) == 0 {
		return fmt.Errorf("to is required")
	}
	if len(req.GetIdempotencyKey()) > utils.MaxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key too long (max %d)", utils.MaxIdempotencyKeyLen)
	}
	if err := validateAttachments(req.GetAttachments()); err != nil {
		return err
	}
	return nil
}
