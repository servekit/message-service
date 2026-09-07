// MessageAdminServiceServer shim over internal/service. Same
// one-line-delegation contract as message.go — no business logic here.
package handler

import (
	"context"

	pb "github.com/servekit/api/gen/go/messaging/v1"

	"google.golang.org/protobuf/types/known/emptypb"
)

// Compile-time assertion: Handler implements the admin server interface.
var _ pb.MessageAdminServiceServer = (*Handler)(nil)

// --- admin gRPC method delegations (platform resource management) ---

// CreateApp registers a calling app; plaintext app_secret returned exactly
// once in the response.
func (h *Handler) CreateApp(ctx context.Context, req *pb.CreateAppRequest) (*pb.CreateAppResponse, error) {
	return h.svc.CreateApp(ctx, req)
}

// GetApp returns one app by id.
func (h *Handler) GetApp(ctx context.Context, req *pb.GetAppRequest) (*pb.GetAppResponse, error) {
	return h.svc.GetApp(ctx, req)
}

// UpdateApp tweaks app metadata (name / disabled / daily limits).
func (h *Handler) UpdateApp(ctx context.Context, req *pb.UpdateAppRequest) (*pb.UpdateAppResponse, error) {
	return h.svc.UpdateApp(ctx, req)
}

// RotateAppSecret invalidates the current secret; new plaintext returned
// exactly once.
func (h *Handler) RotateAppSecret(ctx context.Context, req *pb.RotateAppSecretRequest) (*pb.RotateAppSecretResponse, error) {
	return h.svc.RotateAppSecret(ctx, req)
}

// ListApps returns all apps.
func (h *Handler) ListApps(ctx context.Context, req *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	return h.svc.ListApps(ctx, req)
}

// DeleteApp soft-deletes an app; its sends fail immediately.
func (h *Handler) DeleteApp(ctx context.Context, req *pb.DeleteAppRequest) (*emptypb.Empty, error) {
	return h.svc.DeleteApp(ctx, req)
}

// CreateChannelAccount adds a vendor account to the platform pool.
func (h *Handler) CreateChannelAccount(ctx context.Context, req *pb.CreateChannelAccountRequest) (*pb.CreateChannelAccountResponse, error) {
	return h.svc.CreateChannelAccount(ctx, req)
}

// UpdateChannelAccount edits remark/disabled or replaces credentials.
func (h *Handler) UpdateChannelAccount(ctx context.Context, req *pb.UpdateChannelAccountRequest) (*pb.UpdateChannelAccountResponse, error) {
	return h.svc.UpdateChannelAccount(ctx, req)
}

// DeleteChannelAccount soft-deletes an account from the pool.
func (h *Handler) DeleteChannelAccount(ctx context.Context, req *pb.DeleteChannelAccountRequest) (*emptypb.Empty, error) {
	return h.svc.DeleteChannelAccount(ctx, req)
}

// ListChannelAccounts returns the full pool (secrets masked).
func (h *Handler) ListChannelAccounts(ctx context.Context, req *pb.ListChannelAccountsRequest) (*pb.ListChannelAccountsResponse, error) {
	return h.svc.ListChannelAccounts(ctx, req)
}

// CreateSignature registers a signature with its account bindings.
func (h *Handler) CreateSignature(ctx context.Context, req *pb.CreateSignatureRequest) (*pb.CreateSignatureResponse, error) {
	return h.svc.CreateSignature(ctx, req)
}

// UpdateSignature replaces remark/disabled/bindings.
func (h *Handler) UpdateSignature(ctx context.Context, req *pb.UpdateSignatureRequest) (*pb.UpdateSignatureResponse, error) {
	return h.svc.UpdateSignature(ctx, req)
}

// DeleteSignature soft-deletes a signature.
func (h *Handler) DeleteSignature(ctx context.Context, req *pb.DeleteSignatureRequest) (*emptypb.Empty, error) {
	return h.svc.DeleteSignature(ctx, req)
}

// ListSignatures returns all signatures with bindings.
func (h *Handler) ListSignatures(ctx context.Context, req *pb.ListSignaturesRequest) (*pb.ListSignaturesResponse, error) {
	return h.svc.ListSignatures(ctx, req)
}

// CreateTemplate registers a template definition.
func (h *Handler) CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.CreateTemplateResponse, error) {
	return h.svc.CreateTemplate(ctx, req)
}

// UpdateTemplate fully replaces a template definition.
func (h *Handler) UpdateTemplate(ctx context.Context, req *pb.UpdateTemplateRequest) (*pb.UpdateTemplateResponse, error) {
	return h.svc.UpdateTemplate(ctx, req)
}

// DeleteTemplate soft-deletes a template.
func (h *Handler) DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	return h.svc.DeleteTemplate(ctx, req)
}

// ListTemplates filters by app (0 = all) and channel (0 = all).
func (h *Handler) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	return h.svc.ListTemplates(ctx, req)
}

// CreatePolicy binds (app, channel, scene) to a template + route chains.
func (h *Handler) CreatePolicy(ctx context.Context, req *pb.CreatePolicyRequest) (*pb.CreatePolicyResponse, error) {
	return h.svc.CreatePolicy(ctx, req)
}

// UpdatePolicy fully replaces template + route chains.
func (h *Handler) UpdatePolicy(ctx context.Context, req *pb.UpdatePolicyRequest) (*pb.UpdatePolicyResponse, error) {
	return h.svc.UpdatePolicy(ctx, req)
}

// DeletePolicy removes the binding; sends fail closed from then on.
func (h *Handler) DeletePolicy(ctx context.Context, req *pb.DeletePolicyRequest) (*emptypb.Empty, error) {
	return h.svc.DeletePolicy(ctx, req)
}

// ListPolicies filters by app (0 = all) and channel (0 = all).
func (h *Handler) ListPolicies(ctx context.Context, req *pb.ListPoliciesRequest) (*pb.ListPoliciesResponse, error) {
	return h.svc.ListPolicies(ctx, req)
}
