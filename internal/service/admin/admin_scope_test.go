// Admin actor-scope matrix (tenant platform phase ④ T5): the three caller
// states every admin RPC must branch on —
//
//	injected key   → pinned to that tenant (two-layer list domain; create
//	                 clamp; ownership-checked mutations)
//	PLATFORM actor → cross-view full pool
//	no identity    → fail closed
//
// Cross-tenant reads/writes are refused; the platform pool stays visible but
// read-only for tenant-scoped callers. Fixtures deliberately disagree (the
// request body names beta while the injection says alpha) to prove the body
// is never trusted.
package admin

import (
	"context"
	"testing"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	pb "github.com/servekit/api/gen/go/messaging/v1"
	userv1 "github.com/servekit/api/gen/go/user/v1"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/tenantctx"
	"github.com/servekit/message-service/internal/store/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	scopeAlpha = "ten_alpha0000000"
	scopeBeta  = "ten_beta0000000"
)

// platformCtx is the PLATFORM cross-view (verified actor, no injection).
func platformCtx() context.Context {
	return grpcx.WithActor(context.Background(), &commonv1.RequestActor{
		UserId:   7,
		UserType: int32(userv1.UserType_USER_TYPE_PLATFORM),
	})
}

// tenantCtx is a tenant-scoped caller (the gate's injected key).
func tenantCtx(key string) context.Context {
	return tenantctx.WithTenantKey(context.Background(), key)
}

// anonCtx carries neither identity — the state that used to reach the admin
// surface via raw internal calls and must now fail closed.
func anonCtx() context.Context { return context.Background() }

// seedScopeWorld plants one row of each resource per ownership: platform
// pool, alpha-private, beta-private. Refreshes the registry snapshot.
func seedScopeWorld(t *testing.T, s *Service) (apps, accounts, sigs, tpls map[string]int64) {
	t.Helper()
	ctx := context.Background()
	apps = map[string]int64{
		"pool":  0, // no pool app: apps always map a tenant
		"alpha": seedAppRow(t, s.db, 6001, "alpha-app", scopeAlpha).ID,
		"beta":  seedAppRow(t, s.db, 6002, "beta-app", scopeBeta).ID,
	}
	accounts = map[string]int64{
		"pool":  seedAccount(t, s.db, 6101, "pool-acc", nil).ID,
		"alpha": seedAccount(t, s.db, 6102, "alpha-acc", models.TenantKeyPtr(scopeAlpha)).ID,
		"beta":  seedAccount(t, s.db, 6103, "beta-acc", models.TenantKeyPtr(scopeBeta)).ID,
	}
	sigs = map[string]int64{
		"pool":  seedSignature(t, s.db, 6201, "平台签名", nil, []int64{6101, 6102, 6103}).ID,
		"alpha": seedSignature(t, s.db, 6202, "甲签名", models.TenantKeyPtr(scopeAlpha), []int64{6101, 6102}).ID,
		"beta":  seedSignature(t, s.db, 6203, "乙签名", models.TenantKeyPtr(scopeBeta), []int64{6103}).ID,
	}
	tpls = map[string]int64{
		"pool":  seedTemplate(t, s.db, 6301, "shared-tpl", nil).ID,
		"alpha": seedTemplate(t, s.db, 6302, "alpha-tpl", models.TenantKeyPtr(scopeAlpha)).ID,
		"beta":  seedTemplate(t, s.db, 6303, "beta-tpl", models.TenantKeyPtr(scopeBeta)).ID,
	}
	require.NoError(t, s.reg.Refresh(ctx))
	return
}

// TestAdminScope_NoIdentityFailsClosed: neither an injected key nor a
// verified actor → every admin surface refuses (the internal-network-only
// window closed with phase ④ T5).
func TestAdminScope_NoIdentityFailsClosed(t *testing.T) {
	svc, _ := newTenantAdminFixture(t)
	ctx := anonCtx()

	_, err := svc.ListChannelAccounts(ctx, &pb.ListChannelAccountsRequest{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNAUTHORIZED")

	_, err = svc.ListApps(ctx, &pb.ListAppsRequest{})
	require.Error(t, err)

	_, err = svc.CreateChannelAccount(ctx, &pb.CreateChannelAccountRequest{Name: "x", Credentials: aliyunCreds()})
	require.Error(t, err)

	_, err = svc.GetApp(ctx, &pb.GetAppRequest{Id: 1})
	require.Error(t, err)
}

// TestAdminScope_ListTwoLayerDomain: the five List* surfaces answer the
// two-layer domain for an injected key (platform pool + own rows — never
// another tenant's) and the full pool for the PLATFORM cross-view.
func TestAdminScope_ListTwoLayerDomain(t *testing.T) {
	svc, _ := newTenantAdminFixture(t)
	seedScopeWorld(t, svc)

	// alpha: pool + alpha, never beta
	acc, err := svc.ListChannelAccounts(tenantCtx(scopeAlpha), &pb.ListChannelAccountsRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"pool-acc", "alpha-acc"}, namesOfAccounts(acc))

	sigs, err := svc.ListSignatures(tenantCtx(scopeAlpha), &pb.ListSignaturesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"平台签名", "甲签名"}, namesOfSigs(sigs))

	tpls, err := svc.ListTemplates(tenantCtx(scopeAlpha), &pb.ListTemplatesRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"shared-tpl", "alpha-tpl"}, namesOfTpls(tpls))

	apps, err := svc.ListApps(tenantCtx(scopeAlpha), &pb.ListAppsRequest{})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"alpha-app"}, namesOfApps(apps))

	pols, err := svc.ListPolicies(tenantCtx(scopeAlpha), &pb.ListPoliciesRequest{})
	require.NoError(t, err)
	assert.Empty(t, pols.GetPolicies()) // none seeded

	// PLATFORM cross-view: everything
	accAll, err := svc.ListChannelAccounts(platformCtx(), &pb.ListChannelAccountsRequest{})
	require.NoError(t, err)
	assert.Len(t, accAll.GetAccounts(), 3)
}

// TestAdminScope_CreateClampsTenantKey: a scoped caller's creates are stamped
// with the injected key even when the body names a foreign tenant (or the
// platform pool) — the choice the door validated IS the tenant.
func TestAdminScope_CreateClampsTenantKey(t *testing.T) {
	svc, _ := newTenantAdminFixture(t)
	seedScopeWorld(t, svc)
	ctx := tenantCtx(scopeAlpha)

	// body forges beta
	resp, err := svc.CreateChannelAccount(ctx, &pb.CreateChannelAccountRequest{
		Name: "forged", Credentials: aliyunCreds(), TenantKey: scopeBeta,
	})
	require.NoError(t, err)
	assert.Equal(t, scopeAlpha, resp.GetAccount().GetTenantKey(), "injected key wins over the body")

	// body asks for the platform pool (empty)
	resp2, err := svc.CreateChannelAccount(ctx, &pb.CreateChannelAccountRequest{
		Name: "pool-ask", Credentials: aliyunCreds(),
	})
	require.NoError(t, err)
	assert.Equal(t, scopeAlpha, resp2.GetAccount().GetTenantKey(), "scoped creates cannot enter the platform pool")

	sig, err := svc.CreateSignature(ctx, &pb.CreateSignatureRequest{
		Name: "夹带", AccountIds: []int64{6101}, TenantKey: scopeBeta,
	})
	require.NoError(t, err)
	assert.Equal(t, scopeAlpha, sig.GetSignature().GetTenantKey())

	// app create clamps too: a fresh tenant's row is stamped with the
	// injected key, not the body's (alpha already maps alpha-app, so a
	// second alpha row would trip the one-config-row-per-tenant check).
	app, err := svc.CreateApp(tenantCtx("ten_gamma0000000"), &pb.CreateAppRequest{AppKey: "gamma-minted", Name: "n", TenantKey: scopeBeta})
	require.NoError(t, err)
	assert.Equal(t, "ten_gamma0000000", app.GetApp().GetTenantKey())

	_, err = svc.CreateApp(ctx, &pb.CreateAppRequest{AppKey: "alpha-2", Name: "n", TenantKey: scopeBeta})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST", "one config row per tenant stands under a scope too")
}

// TestAdminScope_MutationOwnership: update/delete/rotate load the row, then
// demand ownership — foreign rows answer not-found (anti-enumeration), the
// platform pool is read-only (Forbidden), own rows work.
func TestAdminScope_MutationOwnership(t *testing.T) {
	svc, _ := newTenantAdminFixture(t)
	seedScopeWorld(t, svc)
	ctx := tenantCtx(scopeAlpha)

	// foreign row → not-found style
	_, err := svc.UpdateChannelAccount(ctx, &pb.UpdateChannelAccountRequest{Id: 6103, Remark: strPtr("x")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHANNEL_ACCOUNT_NOT_FOUND")

	_, err = svc.DeleteChannelAccount(ctx, &pb.DeleteChannelAccountRequest{Id: 6103})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CHANNEL_ACCOUNT_NOT_FOUND")

	_, err = svc.UpdateSignature(ctx, &pb.UpdateSignatureRequest{Id: 6203, Remark: strPtr("x")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SIGNATURE_NOT_FOUND")

	_, err = svc.UpdateTemplate(ctx, &pb.UpdateTemplateRequest{Id: 6303, Template: &pb.TemplateInfo{Name: "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPLATE_NOT_FOUND")

	_, err = svc.RotateAppSecret(ctx, &pb.RotateAppSecretRequest{Id: 6002})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_NOT_FOUND")

	// platform pool → visible but read-only
	_, err = svc.UpdateChannelAccount(ctx, &pb.UpdateChannelAccountRequest{Id: 6101, Remark: strPtr("x")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FORBIDDEN")

	_, err = svc.DeleteChannelAccount(ctx, &pb.DeleteChannelAccountRequest{Id: 6101})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FORBIDDEN")

	_, err = svc.DeleteTemplate(ctx, &pb.DeleteTemplateRequest{Id: 6301})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FORBIDDEN")

	// own row → allowed
	upd, err := svc.UpdateChannelAccount(ctx, &pb.UpdateChannelAccountRequest{Id: 6102, Remark: strPtr("mine")})
	require.NoError(t, err)
	assert.Equal(t, "mine", upd.GetAccount().GetRemark())

	// PLATFORM cross-view manages everything (spot check on the beta row)
	_, err = svc.UpdateChannelAccount(platformCtx(), &pb.UpdateChannelAccountRequest{Id: 6103, Remark: strPtr("ops")})
	require.NoError(t, err)
}

// TestAdminScope_TemplatePolicyAppRef: CreateTemplate/CreatePolicy with a
// scope must reference an app mapped to the caller's tenant; app_id 0
// (shared) is PLATFORM-only; foreign apps answer not-found.
func TestAdminScope_TemplatePolicyAppRef(t *testing.T) {
	svc, _ := newTenantAdminFixture(t)
	seedScopeWorld(t, svc)
	ctx := tenantCtx(scopeAlpha)

	tpl := func(appID int64) *pb.CreateTemplateRequest {
		return &pb.CreateTemplateRequest{AppId: appID, Template: &pb.TemplateInfo{
			Name: "scoped", Channel: pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, Kind: pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES,
			Content: &pb.TemplateInfo_VendorCodes{VendorCodes: &pb.SmsVendorCodesContent{Codes: []*pb.VendorTemplateCode{
				{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "T_SCOPE"},
			}}},
		}}
	}

	// shared (app_id 0) is platform-only for scoped callers
	_, err := svc.CreateTemplate(ctx, tpl(0))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FORBIDDEN")

	// foreign app → not-found (no existence leak)
	_, err = svc.CreateTemplate(ctx, tpl(6002))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_NOT_FOUND")

	// own app → allowed and stamped with the caller's tenant
	owned, err := svc.CreateTemplate(ctx, tpl(6001))
	require.NoError(t, err)
	assert.Equal(t, scopeAlpha, owned.GetTemplate().GetTenantKey())

	// policy on a foreign app → not-found
	_, err = svc.CreatePolicy(ctx, policyReq(6002, 6301, 6101, 6201, pb.SmsScene_SMS_SCENE_LOGIN_CODE))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_NOT_FOUND")

	// policy on own app references the shared template + pool account: fine
	_, err = svc.CreatePolicy(ctx, policyReq(6001, 6301, 6101, 6201, pb.SmsScene_SMS_SCENE_REGISTER))
	require.NoError(t, err)

	// beta then cannot even see that policy
	pols, err := svc.ListPolicies(tenantCtx(scopeBeta), &pb.ListPoliciesRequest{})
	require.NoError(t, err)
	assert.Empty(t, pols.GetPolicies())
	// ...while alpha can
	pols, err = svc.ListPolicies(ctx, &pb.ListPoliciesRequest{})
	require.NoError(t, err)
	assert.Len(t, pols.GetPolicies(), 1)
	// ...and so can the cross-view
	pols, err = svc.ListPolicies(platformCtx(), &pb.ListPoliciesRequest{})
	require.NoError(t, err)
	assert.Len(t, pols.GetPolicies(), 1)

	// foreign policy mutation → not-found
	_, err = svc.UpdatePolicy(tenantCtx(scopeBeta), &pb.UpdatePolicyRequest{Id: pols.GetPolicies()[0].GetId(), TemplateId: 6301})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POLICY_NOT_FOUND")
}

// TestAdminScope_AppSurfaceReads: a scoped caller sees and touches only the
// app row mapped to their tenant.
func TestAdminScope_AppSurfaceReads(t *testing.T) {
	svc, _ := newTenantAdminFixture(t)
	seedScopeWorld(t, svc)
	ctx := tenantCtx(scopeAlpha)

	got, err := svc.GetApp(ctx, &pb.GetAppRequest{Id: 6001})
	require.NoError(t, err)
	assert.Equal(t, scopeAlpha, got.GetApp().GetTenantKey())

	_, err = svc.GetApp(ctx, &pb.GetAppRequest{Id: 6002})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_NOT_FOUND")

	_, err = svc.UpdateApp(ctx, &pb.UpdateAppRequest{Id: 6002, Name: strPtr("steal")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_NOT_FOUND")

	_, err = svc.DeleteApp(ctx, &pb.DeleteAppRequest{Id: 6002})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_NOT_FOUND")

	_, err = svc.UpdateApp(ctx, &pb.UpdateAppRequest{Id: 6001, Name: strPtr("mine")})
	require.NoError(t, err)
}

// --- small helpers ---

func strPtr(s string) *string { return &s }

func namesOfAccounts(resp *pb.ListChannelAccountsResponse) []string {
	out := make([]string, 0, len(resp.GetAccounts()))
	for _, a := range resp.GetAccounts() {
		out = append(out, a.GetName())
	}
	return out
}

func namesOfSigs(resp *pb.ListSignaturesResponse) []string {
	out := make([]string, 0, len(resp.GetSignatures()))
	for _, s := range resp.GetSignatures() {
		out = append(out, s.GetName())
	}
	return out
}

func namesOfTpls(resp *pb.ListTemplatesResponse) []string {
	out := make([]string, 0, len(resp.GetTemplates()))
	for _, x := range resp.GetTemplates() {
		out = append(out, x.GetName())
	}
	return out
}

func namesOfApps(resp *pb.ListAppsResponse) []string {
	out := make([]string, 0, len(resp.GetApps()))
	for _, a := range resp.GetApps() {
		out = append(out, a.GetAppKey())
	}
	return out
}
