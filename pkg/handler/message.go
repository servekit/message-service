// Package handler implements message.v1.MessageServiceServer as a thin shim
// over internal/service. Each method is a one-line delegation — service
// takes the proto request directly (per project convention: avoid unnecessary
// struct allocation; convert at the store boundary instead).
//
// Handlers hold NO business logic and NO conversion logic. Anything beyond
// `return h.svc.X(ctx, req)` belongs in internal/service.
//
// Handler also implements signalx.Service (Start/Stop) by delegating to the
// underlying Service. This lets in-process module users manage lifecycle
// via the same object they call RPC methods on.
package handler

import (
	"context"

	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/servekit/message-service/internal/service"

	"google.golang.org/protobuf/types/known/emptypb"
)

// Handler implements message.v1.MessageServiceServer.
//
// It holds no mutable state — the embedded *service.Service owns all
// business state and lifecycle. Construction-time injection only.
type Handler struct {
	pb.UnimplementedMessageServiceServer

	svc *service.Service
}

// New constructs a Handler wrapping svc.
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// Compile-time assertion: Handler implements the gRPC server interface.
var _ pb.MessageServiceServer = (*Handler)(nil)

// Start starts service-internal components (background goroutines for owned
// resources). Safe to call from in-process module users before invoking RPCs.
func (h *Handler) Start() error { return h.svc.Start() }

// Stop releases resources owned by the service. After Stop, the Handler
// must not be used.
func (h *Handler) Stop() error { return h.svc.Stop() }

// Ping is a health-check RPC.
func (h *Handler) Ping(ctx context.Context, _ *emptypb.Empty) (*pb.Pong, error) {
	return h.svc.Ping(ctx)
}

// --- gRPC method delegations ---
// Each method delegates to internal/service. Comments below describe "how to
// use" (prerequisites, side effects, follow-up RPCs) for in-process module
// callers; for the full contract see message.proto.

// SendEmail sends an email via the configured vendor/account, or the default
// fallback chain when both are unset. Idempotent on (sender_id,
// idempotency_key) via Redis when idempotency_key is set: a second request
// with the same key returns the cached response. Failures are NOT cached —
// reservation is released so caller can retry the same key.
// Returns: record ID + MessageStatus (SENT = vendor accepted sync;
// FAILED is returned as ErrMessageSendFailed).
func (h *Handler) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return h.svc.SendEmail(ctx, req)
}

// SendSMS sends an SMS via the configured vendor/account, or routes by phone
// country code when both are unset. CN → domestic path (template-based,
// sign_name required); other regions → international path (raw content OR
// template, vendor-dependent). Idempotency semantics mirror SendEmail.
func (h *Handler) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return h.svc.SendSMS(ctx, req)
}

// GetEmail returns a single email record by ID. Requires email persistence to
// be enabled — returns ErrPersistenceDisabled otherwise.
func (h *Handler) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	return h.svc.GetEmail(ctx, req)
}

// ListEmails returns an offset-paginated email list with totals. For large
// datasets or streaming scans, prefer ListEmailsByCursor (no COUNT(*) by
// default).
func (h *Handler) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	return h.svc.ListEmails(ctx, req)
}

// ListEmailsByCursor returns a cursor-paginated email list. Stable under
// concurrent writes (id acts as tiebreaker). Set include_total=true only
// when you need the count — COUNT(*) is skipped otherwise.
func (h *Handler) ListEmailsByCursor(ctx context.Context, req *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error) {
	return h.svc.ListEmailsByCursor(ctx, req)
}

// GetEmailStats returns aggregated counts (total / sent / failed /
// success_rate) broken down per vendor. Empty filter returns all-time stats.
func (h *Handler) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	return h.svc.GetEmailStats(ctx, req)
}

// GetSMS returns a single SMS record by ID. Requires SMS persistence to be
// enabled — returns ErrPersistenceDisabled otherwise.
func (h *Handler) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	return h.svc.GetSMS(ctx, req)
}

// ListSMS returns an offset-paginated SMS list with totals. See ListEmails
// for when to prefer the cursor variant.
func (h *Handler) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	return h.svc.ListSMS(ctx, req)
}

// ListSMSByCursor returns a cursor-paginated SMS list. See
// ListEmailsByCursor for semantics.
func (h *Handler) ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	return h.svc.ListSMSByCursor(ctx, req)
}

// GetSMSStats returns aggregated SMS statistics. See GetEmailStats for shape.
func (h *Handler) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	return h.svc.GetSMSStats(ctx, req)
}

// ListSMSRegions returns distinct region_code values, for frontend SMS
// filter dropdowns. Cheap query (low cardinality) — callers filter
// client-side.
func (h *Handler) ListSMSRegions(ctx context.Context, req *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	return h.svc.ListSMSRegions(ctx, req)
}

// ListSMSSenders returns distinct sender_id values across SMS records, for
// frontend filter dropdowns.
func (h *Handler) ListSMSSenders(ctx context.Context, req *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
	return h.svc.ListSMSSenders(ctx, req)
}

// ListEmailSenders returns distinct sender_id values across email records,
// for frontend filter dropdowns.
func (h *Handler) ListEmailSenders(ctx context.Context, req *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error) {
	return h.svc.ListEmailSenders(ctx, req)
}
