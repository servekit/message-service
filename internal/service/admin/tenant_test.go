// Phase ③ resource domain tests: channel accounts / signatures / templates
// gain a nullable tenant_key (NULL = platform pool, behavior unchanged);
// tenant policies may reference the platform pool plus their OWN private
// resources only — cross-tenant references are rejected with BadRequest.
package admin

import (
	"context"
	"testing"
	"time"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var testGID gidservice.Service

func init() {
	hdl, err := gidservice.NewModule(&gidconfig.Config{
		Snowflake: &gidconfig.SnowflakeConfig{MachineID: 3, StartTime: time.Now().Add(-time.Hour)},
	})
	if err != nil {
		panic(err)
	}
	testGID = hdl
}

func newTenantAdminFixture(t *testing.T) (*Service, *gorm.DB) {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	reg := registry.New(db)
	return New(db, reg, testGID), db
}

func aliyunCreds() *pb.ChannelAccountCredentials {
	return &pb.ChannelAccountCredentials{Credentials: &pb.ChannelAccountCredentials_AliyunSms{
		AliyunSms: &pb.AliyunSmsCredentials{AccessKeyId: "ak", AccessKeySecret: "sk"},
	}}
}

// seedAppRow inserts an app row with a mapped tenant (raw column control).
func seedAppRow(t *testing.T, db *gorm.DB, id int64, appKey, tenant string) *models.MessageApp {
	t.Helper()
	app := &models.MessageApp{ID: id, AppKey: appKey, AppSecret: "s", Name: appKey, TenantKey: models.TenantKeyPtr(tenant)}
	require.NoError(t, dal.CreateApp(context.Background(), db, app))
	return app
}

// seedAccount inserts a channel account row with the given ownership.
func seedAccount(t *testing.T, db *gorm.DB, id int64, name string, tenant *string) *models.MessageChannelAccount {
	t.Helper()
	acc := &models.MessageChannelAccount{
		ID: id, Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Vendor: int32(pb.SmsVendor_SMS_VENDOR_ALIYUN), Name: name,
		Config: models.RawJSON(`{}`), TenantKey: tenant,
	}
	require.NoError(t, dal.CreateChannelAccount(context.Background(), db, acc))
	return acc
}

// seedSignature inserts a signature bound to accounts with the given ownership.
func seedSignature(t *testing.T, db *gorm.DB, id int64, name string, tenant *string, accountIDs []int64) *models.MessageSignature {
	t.Helper()
	sig := &models.MessageSignature{ID: id, Name: name, TenantKey: tenant}
	require.NoError(t, dal.CreateSignature(context.Background(), db, sig, accountIDs))
	return sig
}

// seedTemplate inserts a vendor-code SMS template with the given ownership.
func seedTemplate(t *testing.T, db *gorm.DB, id int64, name string, tenant *string) *models.MessageTemplate {
	t.Helper()
	content, err := send.MarshalVendorCodes([]send.VendorCode{
		{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "T_X"},
	})
	require.NoError(t, err)
	tpl := &models.MessageTemplate{
		ID: id, Name: name,
		Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Kind:    int32(pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES),
		Content: content, TenantKey: tenant,
	}
	require.NoError(t, dal.CreateTemplate(context.Background(), db, tpl))
	return tpl
}

func policyReq(appID, templateID, accountID, signatureID int64, scene pb.SmsScene) *pb.CreatePolicyRequest {
	return &pb.CreatePolicyRequest{
		AppId:      appID,
		Scene:      &pb.PolicyScene{Scene: &pb.PolicyScene_SmsScene{SmsScene: scene}},
		TemplateId: templateID,
		Routes:     []*pb.RouteRule{{AccountId: accountID, SignatureId: signatureID, Weight: 1}},
		IntlRoutes: []*pb.RouteRule{{AccountId: accountID, SignatureId: signatureID, Weight: 1}},
	}
}

// TestTenantPrivateResourceCRUD: accounts and signatures created with a
// tenant_key are tenant-private; created without one they stay in the
// platform pool; both echo their ownership on the admin surface.
func TestTenantPrivateResourceCRUD(t *testing.T) {
	svc, db := newTenantAdminFixture(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	accResp, err := svc.CreateChannelAccount(ctx, &pb.CreateChannelAccountRequest{
		Name: "alpha-private", Credentials: aliyunCreds(), TenantKey: "ten_alpha0000000",
	})
	require.NoError(t, err)
	assert.Equal(t, "ten_alpha0000000", accResp.GetAccount().GetTenantKey())

	poolResp, err := svc.CreateChannelAccount(ctx, &pb.CreateChannelAccountRequest{
		Name: "platform-pool", Credentials: aliyunCreds(),
	})
	require.NoError(t, err)
	assert.Empty(t, poolResp.GetAccount().GetTenantKey(), "absent tenant_key = platform pool")

	sigResp, err := svc.CreateSignature(ctx, &pb.CreateSignatureRequest{
		Name: "私有签名", AccountIds: []int64{poolResp.GetAccount().GetId()}, TenantKey: "ten_alpha0000000",
	})
	require.NoError(t, err)
	assert.Equal(t, "ten_alpha0000000", sigResp.GetSignature().GetTenantKey())

	// rows carry the columns
	acc, err := dal.GetChannelAccount(ctx, db, accResp.GetAccount().GetId())
	require.NoError(t, err)
	assert.Equal(t, "ten_alpha0000000", models.TenantKeyOf(acc.TenantKey))
	pool, err := dal.GetChannelAccount(ctx, db, poolResp.GetAccount().GetId())
	require.NoError(t, err)
	assert.Nil(t, pool.TenantKey, "platform pool row keeps NULL tenant_key")
}

// TestTenantPolicyResourceDomain: a tenant policy may reference platform-pool
// resources and its own private ones; referencing another tenant's private
// account, signature or template is a BadRequest.
func TestTenantPolicyResourceDomain(t *testing.T) {
	svc, db := newTenantAdminFixture(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	app := seedAppRow(t, db, 1001, "alpha-app", "ten_alpha0000000")
	poolAcc := seedAccount(t, db, 2001, "pool-acc", nil)
	alphaAcc := seedAccount(t, db, 2002, "alpha-acc", models.TenantKeyPtr("ten_alpha0000000"))
	betaAcc := seedAccount(t, db, 2003, "beta-acc", models.TenantKeyPtr("ten_beta0000000"))
	poolSig := seedSignature(t, db, 3001, "平台签名", nil, []int64{2001, 2002, 2003})
	alphaSig := seedSignature(t, db, 3002, "甲签名", models.TenantKeyPtr("ten_alpha0000000"), []int64{2001, 2002, 2003})
	betaSig := seedSignature(t, db, 3003, "乙签名", models.TenantKeyPtr("ten_beta0000000"), []int64{2001, 2002, 2003})
	sharedTpl := seedTemplate(t, db, 4001, "shared-tpl", nil)
	alphaTpl := seedTemplate(t, db, 4002, "alpha-tpl", models.TenantKeyPtr("ten_alpha0000000"))
	betaTpl := seedTemplate(t, db, 4003, "beta-tpl", models.TenantKeyPtr("ten_beta0000000"))

	require.NoError(t, svc.reg.Refresh(ctx))

	// platform pool + shared template: allowed
	_, err := svc.CreatePolicy(ctx, policyReq(app.ID, sharedTpl.ID, poolAcc.ID, poolSig.ID, pb.SmsScene_SMS_SCENE_LOGIN_CODE))
	require.NoError(t, err, "platform pool + shared template must stay usable by tenants")

	// own private account + own private signature + own template: allowed
	_, err = svc.CreatePolicy(ctx, policyReq(app.ID, alphaTpl.ID, alphaAcc.ID, alphaSig.ID, pb.SmsScene_SMS_SCENE_REGISTER))
	require.NoError(t, err, "tenant may reference its own private resources")

	// cross-tenant account
	_, err = svc.CreatePolicy(ctx, policyReq(app.ID, sharedTpl.ID, betaAcc.ID, poolSig.ID, pb.SmsScene_SMS_SCENE_FORGOT_PASSWORD))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
	assert.Contains(t, err.Error(), "ten_beta0000000")

	// cross-tenant signature
	_, err = svc.CreatePolicy(ctx, policyReq(app.ID, sharedTpl.ID, poolAcc.ID, betaSig.ID, pb.SmsScene_SMS_SCENE_CHANGE_PASSWORD))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")

	// cross-tenant template
	_, err = svc.CreatePolicy(ctx, policyReq(app.ID, betaTpl.ID, poolAcc.ID, poolSig.ID, pb.SmsScene_SMS_SCENE_BIND_ACCOUNT))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
}

// TestCreatePolicyDuplicateScopingByTenant: uniqueness moved to
// (tenant_key, channel, scene) — the same tenant cannot bind a scene twice,
// while a different tenant may bind the same scene. (Two app rows sharing
// one tenant are structurally impossible: tenant_key is unique on
// message_apps — one config row per tenant.)
func TestCreatePolicyDuplicateScopingByTenant(t *testing.T) {
	svc, db := newTenantAdminFixture(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	alpha := seedAppRow(t, db, 1101, "alpha-1", "ten_alpha0000000")
	beta := seedAppRow(t, db, 1103, "beta-1", "ten_beta0000000")
	acc := seedAccount(t, db, 2101, "pool-acc2", nil)
	sig := seedSignature(t, db, 3101, "签名2", nil, []int64{2101})
	tpl := seedTemplate(t, db, 4101, "tpl2", nil)
	require.NoError(t, svc.reg.Refresh(ctx))

	_, err := svc.CreatePolicy(ctx, policyReq(alpha.ID, tpl.ID, acc.ID, sig.ID, pb.SmsScene_SMS_SCENE_LOGIN_CODE))
	require.NoError(t, err)

	_, err = svc.CreatePolicy(ctx, policyReq(alpha.ID, tpl.ID, acc.ID, sig.ID, pb.SmsScene_SMS_SCENE_LOGIN_CODE))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST", "same tenant + scene collides")

	_, err = svc.CreatePolicy(ctx, policyReq(beta.ID, tpl.ID, acc.ID, sig.ID, pb.SmsScene_SMS_SCENE_LOGIN_CODE))
	require.NoError(t, err, "a different tenant may bind the same scene")
}

// TestSignatureBindingCrossTenantAccountRejected: a tenant-private signature
// may bind platform-pool accounts but not another tenant's private account.
func TestSignatureBindingCrossTenantAccountRejected(t *testing.T) {
	svc, db := newTenantAdminFixture(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	poolAcc := seedAccount(t, db, 2201, "pool-acc3", nil)
	betaAcc := seedAccount(t, db, 2202, "beta-acc2", models.TenantKeyPtr("ten_beta0000000"))
	require.NoError(t, svc.reg.Refresh(ctx))

	_, err := svc.CreateSignature(ctx, &pb.CreateSignatureRequest{
		Name: "绑定平台", AccountIds: []int64{poolAcc.ID}, TenantKey: "ten_alpha0000000",
	})
	require.NoError(t, err, "tenant signature binding a platform-pool account is allowed")

	_, err = svc.CreateSignature(ctx, &pb.CreateSignatureRequest{
		Name: "绑定他租", AccountIds: []int64{betaAcc.ID}, TenantKey: "ten_alpha0000000",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
}

// TestCreateAppStampsTenantKey: apps created with an explicit tenant_key keep
// it; without one the app_key literal is stamped (the legacy→tenant fallback
// value; T10 总装 remaps).
func TestCreateAppStampsTenantKey(t *testing.T) {
	svc, _ := newTenantAdminFixture(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	resp, err := svc.CreateApp(ctx, &pb.CreateAppRequest{AppKey: "explicit-app", Name: "n", TenantKey: "ten_explicit0000"})
	require.NoError(t, err)
	assert.Equal(t, "ten_explicit0000", resp.GetApp().GetTenantKey())

	resp, err = svc.CreateApp(ctx, &pb.CreateAppRequest{AppKey: "implicit-app", Name: "n"})
	require.NoError(t, err)
	assert.Equal(t, "implicit-app", resp.GetApp().GetTenantKey())

	// duplicate mapping refused
	_, err = svc.CreateApp(ctx, &pb.CreateAppRequest{AppKey: "another-app", Name: "n", TenantKey: "ten_explicit0000"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
}

// TestCreateTemplateTenantScoped: app_id=0 stays shared (NULL tenant_key);
// an app-owned template stamps the app's mapped tenant.
func TestCreateTemplateTenantScoped(t *testing.T) {
	svc, db := newTenantAdminFixture(t)
	ctx := platformCtx() // phase ④ T5: the admin surface requires a trusted identity

	app := seedAppRow(t, db, 1201, "mapped-app", "ten_mapped000000")
	require.NoError(t, svc.reg.Refresh(ctx))

	shared, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{AppId: 0, Template: &pb.TemplateInfo{
		Name: "shared", Channel: pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, Kind: pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES,
		Content: &pb.TemplateInfo_VendorCodes{VendorCodes: &pb.SmsVendorCodesContent{Codes: []*pb.VendorTemplateCode{
			{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "T_S"},
		}}},
	}})
	require.NoError(t, err)
	assert.Empty(t, shared.GetTemplate().GetTenantKey())

	owned, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{AppId: app.ID, Template: &pb.TemplateInfo{
		Name: "owned", Channel: pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, Kind: pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES,
		Content: &pb.TemplateInfo_VendorCodes{VendorCodes: &pb.SmsVendorCodesContent{Codes: []*pb.VendorTemplateCode{
			{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "T_O"},
		}}},
	}})
	require.NoError(t, err)
	assert.Equal(t, "ten_mapped000000", owned.GetTemplate().GetTenantKey())

	// unknown app refused (tenant cannot be derived)
	_, err = svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{AppId: 999999, Template: &pb.TemplateInfo{
		Name: "dangling", Channel: pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, Kind: pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES,
		Content: &pb.TemplateInfo_VendorCodes{VendorCodes: &pb.SmsVendorCodesContent{Codes: []*pb.VendorTemplateCode{
			{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "T_D"},
		}}},
	}})
	require.Error(t, err)
}
