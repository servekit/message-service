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

// CreateTenantConfig registers a tenant's config row (no secret — the
// credential column was retired with the ④ window close).
func (h *Handler) CreateTenantConfig(ctx context.Context, req *pb.CreateTenantConfigRequest) (*pb.CreateTenantConfigResponse, error) {
	return h.svc.CreateTenantConfig(ctx, req)
}

// GetTenantConfig returns one tenant config by row id.
func (h *Handler) GetTenantConfig(ctx context.Context, req *pb.GetTenantConfigRequest) (*pb.GetTenantConfigResponse, error) {
	return h.svc.GetTenantConfig(ctx, req)
}

// UpdateTenantConfig tweaks config metadata (name / disabled / daily
// limits).
func (h *Handler) UpdateTenantConfig(ctx context.Context, req *pb.UpdateTenantConfigRequest) (*pb.UpdateTenantConfigResponse, error) {
	return h.svc.UpdateTenantConfig(ctx, req)
}

// RotateTenantConfigSecret invalidates the current secret; new plaintext
// returned exactly once.
func (h *Handler) RotateTenantConfigSecret(ctx context.Context, req *pb.RotateTenantConfigSecretRequest) (*pb.RotateTenantConfigSecretResponse, error) {
	return h.svc.RotateTenantConfigSecret(ctx, req)
}

// ListTenantConfigs returns the tenant configs in the caller's scope.
func (h *Handler) ListTenantConfigs(ctx context.Context, req *pb.ListTenantConfigsRequest) (*pb.ListTenantConfigsResponse, error) {
	return h.svc.ListTenantConfigs(ctx, req)
}

// DeleteTenantConfig soft-deletes a tenant config; its sends fail
// immediately.
func (h *Handler) DeleteTenantConfig(ctx context.Context, req *pb.DeleteTenantConfigRequest) (*emptypb.Empty, error) {
	return h.svc.DeleteTenantConfig(ctx, req)
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
