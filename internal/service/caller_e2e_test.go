// Root-level data-plane wiring test: the credential entry
// (Handler.SendSMS → service.Service → tenantres) resolves the tenant
// context from the trusted x-tenant-key — the only stack since the ④
// window close. Vendor dispatch is intentionally left to fail with
// MESSAGE_SEND_FAILED ("no usable provider" — accounts carry no real
// credentials): reaching that error proves the metadata was parsed, the
// tenant resolved (config row lazily ensured), and the policy found BY
// TENANT — a wiring break surfaces as POLICY_NOT_FOUND or
// APP_UNAUTHORIZED instead.
package service_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"
	"github.com/servekit/go-common/tenantctx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	messageservice "github.com/servekit/message-service/pkg"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/option"
)

func newRootHandler(t *testing.T) (*messageservice.Handler, *gorm.DB) {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	return newRootHandlerOn(t, db), db
}

// newRootHandlerOn constructs the module handler on a pre-seeded DB (the
// registry snapshot loads at construction — legacy-path reads are
// snapshot-only, so app rows must predate the handler).
func newRootHandlerOn(t *testing.T, db *gorm.DB) *messageservice.Handler {
	t.Helper()
	gid, err := gidservice.NewModule(&gidconfig.Config{
		Snowflake: &gidconfig.SnowflakeConfig{MachineID: 4, StartTime: time.Now().Add(-time.Hour)},
	})
	require.NoError(t, err)
	hdl, err := messageservice.NewModule(&config.Config{
		Email: &config.EmailConfig{},
		SMS:   &config.SMSConfig{},
	},
		option.WithDB(db),
		option.WithRedis(redisx.NewTestClient(t)),
		option.WithGIDHandler(gid),
	)
	require.NoError(t, err)
	require.NoError(t, hdl.Start())
	t.Cleanup(func() { _ = hdl.Stop() })
	return hdl
}

// seedPolicyFor wires one tenant's LOGIN_CODE policy over a credential-less
// (never-buildable) SMS account: sends reach dispatch and fail with
// MESSAGE_SEND_FAILED rather than POLICY_NOT_FOUND.
func seedPolicyFor(t *testing.T, db *gorm.DB, tenantKey string) {
	t.Helper()
	ctx := context.Background()
	acc := &models.MessageChannelAccount{
		ID:      9101,
		Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Vendor:  int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
		Name:    "no-creds",
		Config:  models.RawJSON(`{}`), // empty config → provider build fails → skipped
	}
	require.NoError(t, dal.CreateChannelAccount(ctx, db, acc))
	content, err := send.MarshalVendorCodes([]send.VendorCode{
		{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "T"},
	})
	require.NoError(t, err)
	tpl := &models.MessageTemplate{
		ID: 9201, Name: "e2e",
		Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Kind:    int32(pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES),
		Content: content,
	}
	require.NoError(t, dal.CreateTemplate(ctx, db, tpl))
	routes, err := send.MarshalRoutes([]send.Route{{AccountID: 9101, Weight: 1}})
	require.NoError(t, err)
	require.NoError(t, dal.CreatePolicy(ctx, db, &models.MessagePolicy{
		ID:         9301,
		Channel:    int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Scene:      int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		TemplateID: 9201,
		TenantKey:  models.TenantKeyPtr(tenantKey), Routes: routes, IntlRoutes: routes,
	}))
}

// legacyCtx plants the deleted stack's wire shape — anti-regression only.
func legacyCtx(ctx context.Context, appKey, appSecret string) context.Context {
	return metadata.NewIncomingContext(ctx, metadata.Pairs(
		"x-app-key", appKey,
		"x-app-secret", appSecret,
	))
}

func e2eSendReq() *pb.SendSMSRequest {
	return &pb.SendSMSRequest{
		To: "+8613800138000", Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "1"},
	}
}

// TestSendSMSTrustedEndToEnd: a trusted x-tenant-key send lazily creates the
// tenant's config row and resolves its policy by tenant (dispatch reached).
func TestSendSMSTrustedEndToEnd(t *testing.T) {
	hdl, db := newRootHandler(t)
	seedPolicyFor(t, db, "ten_e2etrusted00")

	_, err := hdl.SendSMS(tenantctx.WithTenant(context.Background(), "ten_e2etrusted00"), e2eSendReq())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MESSAGE_SEND_FAILED", "dispatch reached: tenant + policy resolved, no usable provider is the expected terminal state")

	app, err := dal.GetAppForTenant(context.Background(), db, "ten_e2etrusted00")
	require.NoError(t, err)
	require.NotNil(t, app, "first-sight trusted tenant got its config row")
	assert.Equal(t, "ten_e2etrusted00", models.TenantKeyOf(app.TenantKey))
}

// TestSendSMSLegacyRejected (④ window close): the legacy ak/sk stack no
// longer authenticates — even a previously-valid pair (with a mapped config
// row and policy seeded) answers APP_UNAUTHORIZED before any dispatch.
func TestSendSMSLegacyRejected(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	require.NoError(t, dal.CreateApp(context.Background(), db, &models.MessageApp{
		ID: 9401, AppKey: "e2e-legacy", Name: "legacy",
		TenantKey: models.TenantKeyPtr("ten_e2elegacy000"),
	}))
	seedPolicyFor(t, db, "ten_e2elegacy000")
	hdl := newRootHandlerOn(t, db)

	_, err := hdl.SendSMS(legacyCtx(context.Background(), "e2e-legacy", "e2e-secret"), e2eSendReq())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_UNAUTHORIZED", "a previously-valid legacy pair must now be unauthenticated")

	_, err = hdl.SendSMS(legacyCtx(context.Background(), "e2e-legacy", "wrong"), e2eSendReq())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_UNAUTHORIZED")
}

// TestSendSMSMalformedTrustedKeyRejected (④ window close): a malformed
// x-tenant-key answers APP_UNAUTHORIZED before any lookup.
func TestSendSMSMalformedTrustedKeyRejected(t *testing.T) {
	hdl, _ := newRootHandler(t)
	_, err := hdl.SendSMS(tenantctx.WithTenant(context.Background(), "e2e-legacy"), e2eSendReq())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_UNAUTHORIZED", "a legacy app_key literal must not pass as a trusted key")
}

// TestSendSMSTrustedBeatsSmuggledLegacy: trusted key + a VALID legacy pair of
// a different tenant resolves the trusted tenant (D-③1 — the smuggled pair
// is discarded; the trusted tenant's policy is the one consulted).
func TestSendSMSTrustedBeatsSmuggledLegacy(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	require.NoError(t, dal.CreateApp(context.Background(), db, &models.MessageApp{
		ID: 9402, AppKey: "e2e-other", Name: "other",
		TenantKey: models.TenantKeyPtr("ten_e2eother0000"),
	}))
	seedPolicyFor(t, db, "ten_e2etrusted00")
	hdl := newRootHandlerOn(t, db)

	ctx := tenantctx.WithTenant(
		legacyCtx(context.Background(), "e2e-other", "other-secret"),
		"ten_e2etrusted00")
	_, err := hdl.SendSMS(ctx, e2eSendReq())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MESSAGE_SEND_FAILED",
		"the trusted tenant's policy was used — the smuggled legacy pair was discarded")
}

// TestSendSMSUnauthenticated: no credentials → fail closed.
func TestSendSMSUnauthenticated(t *testing.T) {
	hdl, _ := newRootHandler(t)
	_, err := hdl.SendSMS(context.Background(), e2eSendReq())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_UNAUTHORIZED")
}
