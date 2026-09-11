// Phase ③ dual-stack tenant-keying of the send path: policies, daily quota
// and the idempotency namespace hang off tenant_key, not app identity. The
// legacy ak/sk window keeps app rows as the credential but converts them to
// their mapped tenant (column empty → app_key literal, T10 clears those).
package sms

import (
	"context"
	"testing"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/quota"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// tenantSMSFixture wires a policy whose TenantKey deliberately
// disagree with the tenant the caller presents — the shape that distinguishes
// "keyed by app" from "keyed by tenant".
type tenantSMSFixture struct {
	svc  *Service
	db   *gorm.DB
	app  *models.MessageApp
	good *mockSMSProvider
}

func newTenantSMSFixture(t *testing.T, appTenant string, policyTenant string, smsLimit int64) *tenantSMSFixture {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	ctx := context.Background()

	app := &models.MessageApp{
		ID: 5001, AppKey: "tenant-app", Name: "Tenant App",
		SMSDailyLimit: smsLimit, TenantKey: models.TenantKeyPtr(appTenant),
	}
	require.NoError(t, dal.CreateApp(ctx, db, app))

	acc1 := &models.MessageChannelAccount{ID: 6001, Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS), Vendor: int32(pb.SmsVendor_SMS_VENDOR_ALIYUN), Name: "aliyun-tenant", Config: models.RawJSON(`{}`)}
	acc2 := &models.MessageChannelAccount{ID: 6002, Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS), Vendor: int32(pb.SmsVendor_SMS_VENDOR_TENCENT), Name: "tencent-tenant", Config: models.RawJSON(`{}`)}
	require.NoError(t, dal.CreateChannelAccount(ctx, db, acc1))
	require.NoError(t, dal.CreateChannelAccount(ctx, db, acc2))
	sig := &models.MessageSignature{ID: 7001, Name: "租户签名"}
	require.NoError(t, dal.CreateSignature(ctx, db, sig, []int64{6001, 6002}))

	params, err := send.MarshalParamSpecs([]models.TemplateParamSpec{{Name: "code", Required: true}})
	require.NoError(t, err)
	content, err := send.MarshalVendorCodes([]send.VendorCode{
		{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "T_ALIYUN"},
		{Vendor: pb.SmsVendor_SMS_VENDOR_TENCENT, TemplateCode: "T_TENCENT"},
	})
	require.NoError(t, err)
	template := &models.MessageTemplate{
		ID: 8001, Name: "tenant-tpl",
		Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Kind:    int32(pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES),
		Params:  params, Content: content,
	}
	require.NoError(t, dal.CreateTemplate(ctx, db, template))

	routes, err := send.MarshalRoutes([]send.Route{
		{AccountID: 6001, SignatureID: 7001, Weight: 1},
		{AccountID: 6002, SignatureID: 7001, Weight: 1},
	})
	require.NoError(t, err)
	policy := &models.MessagePolicy{
		ID: 9001, TenantKey: models.TenantKeyPtr(policyTenant),
		Channel:    int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Scene:      int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		TemplateID: template.ID,
		Routes:     routes, IntlRoutes: routes,
	}
	require.NoError(t, dal.CreatePolicy(ctx, db, policy))

	good := &mockSMSProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, name: "aliyun-tenant"}
	reg := registry.New(db)
	reg.BuildSMS = func(a *models.MessageChannelAccount) (provesms.AccountProvider, error) {
		return good, nil
	}
	require.NoError(t, reg.Refresh(ctx))

	return &tenantSMSFixture{
		svc: New(db, newIdem(t), getTestGID(t), reg,
			quota.NewChecker(redisx.NewTestClient(t), "msg:quota:test-tenant"), true),
		db: db, app: app, good: good,
	}
}

func tenantSendReq(idem string) *pb.SendSMSRequest {
	return &pb.SendSMSRequest{
		To: "+8613800138000", Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "1"}, IdempotencyKey: idem,
	}
}

// TestSendSMSPolicyKeyedByTenant: the policy row is found through the
// caller's tenant context, and invisible under any other tenant — even
// even though it names the calling app row as its owner.
func TestSendSMSPolicyKeyedByTenant(t *testing.T) {
	fx := newTenantSMSFixture(t, "ten_alpha0000000", "ten_alpha0000000", 0)

	_, err := fx.svc.SendSMS(context.Background(), fx.app, "ten_alpha0000000", tenantSendReq(""))
	require.NoError(t, err)
	before := fx.good.calls

	_, err = fx.svc.SendSMS(context.Background(), fx.app, "ten_beta0000000", tenantSendReq(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POLICY_NOT_FOUND")
	assert.Equal(t, before, fx.good.calls, "no provider may be touched for a foreign tenant")
}

// TestSendSMSQuotaNamespacedByTenant: the daily counter hangs off the
// tenant, not the app row — a limit of 1 trips on the second send for the
// same tenant, while a different tenant's counter is untouched.
func TestSendSMSQuotaNamespacedByTenant(t *testing.T) {
	fx := newTenantSMSFixture(t, "ten_alpha0000000", "ten_alpha0000000", 1)

	_, err := fx.svc.SendSMS(context.Background(), fx.app, "ten_alpha0000000", tenantSendReq(""))
	require.NoError(t, err)

	_, err = fx.svc.SendSMS(context.Background(), fx.app, "ten_alpha0000000", tenantSendReq(""))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DAILY_QUOTA_EXCEEDED")

	// beta's own config row (unlimited) + policy: its counter is a
	// different namespace, unaffected by alpha's trips.
	beta := &models.MessageApp{ID: 5002, AppKey: "beta-cfg", Name: "beta", TenantKey: models.TenantKeyPtr("ten_beta0000000")}
	require.NoError(t, dal.CreateApp(context.Background(), fx.db, beta))
	betaPolicy := &models.MessagePolicy{
		ID: 9002, TenantKey: models.TenantKeyPtr("ten_beta0000000"),
		Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS), Scene: int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		TemplateID: 8001, Routes: mustRoutes(t), IntlRoutes: mustRoutes(t),
	}
	require.NoError(t, dal.CreatePolicy(context.Background(), fx.db, betaPolicy))
	require.NoError(t, fx.svc.reg.Refresh(context.Background()))

	_, err = fx.svc.SendSMS(context.Background(), beta, "ten_beta0000000", tenantSendReq(""))
	require.NoError(t, err, "beta's quota is a fresh namespace — alpha's exceeded counter does not leak")
}

// mustRoutes builds a single-route chain for fixture policies.
func mustRoutes(t *testing.T) models.RawJSON {
	t.Helper()
	routes, err := send.MarshalRoutes([]send.Route{{AccountID: 6001, SignatureID: 7001, Weight: 1}})
	require.NoError(t, err)
	return routes
}

// TestSendSMSIdempotencyNamespacedByTenant: the same idempotency_key dedups
// within a tenant and re-sends under a different tenant (namespace switch
// semantics: keys are tenant-scoped from ③ on).
func TestSendSMSIdempotencyNamespacedByTenant(t *testing.T) {
	fx := newTenantSMSFixture(t, "ten_alpha0000000", "ten_alpha0000000", 0)

	first, err := fx.svc.SendSMS(context.Background(), fx.app, "ten_alpha0000000", tenantSendReq("shared-key"))
	require.NoError(t, err)
	second, err := fx.svc.SendSMS(context.Background(), fx.app, "ten_alpha0000000", tenantSendReq("shared-key"))
	require.NoError(t, err)
	assert.Equal(t, first.GetId(), second.GetId(), "same tenant + same key dedups")
	assert.Equal(t, 1, fx.good.calls)

	// give beta its own config row + policy (same shape): the same key under
	// beta is a DIFFERENT namespace — it must send again, not replay alpha's
	// answer.
	beta := &models.MessageApp{ID: 5002, AppKey: "beta-cfg", Name: "beta", TenantKey: models.TenantKeyPtr("ten_beta0000000")}
	require.NoError(t, dal.CreateApp(context.Background(), fx.db, beta))
	betaPolicy := &models.MessagePolicy{
		ID: 9002, TenantKey: models.TenantKeyPtr("ten_beta0000000"),
		Channel:    int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Scene:      int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		TemplateID: 8001,
		Routes:     mustRoutes(t), IntlRoutes: mustRoutes(t),
	}
	require.NoError(t, dal.CreatePolicy(context.Background(), fx.db, betaPolicy))
	require.NoError(t, fx.svc.reg.Refresh(context.Background()))

	resp, err := fx.svc.SendSMS(context.Background(), beta, "ten_beta0000000", tenantSendReq("shared-key"))
	require.NoError(t, err)
	assert.NotEqual(t, first.GetId(), resp.GetId(), "tenant-scoped namespace: beta's key is fresh")
	assert.Equal(t, 2, fx.good.calls)
}
