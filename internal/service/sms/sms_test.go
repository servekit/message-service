package sms

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"
	"github.com/servekit/message-service/internal/idempotency"
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

// --- mocks ---

type mockSMSProvider struct {
	vendor   pb.SmsVendor
	name     string
	err      error
	calls    int
	last     *provesms.Message
	lastIntl *provesms.InternationalMessage
}

func (m *mockSMSProvider) Vendor() pb.SmsVendor { return m.vendor }
func (m *mockSMSProvider) Account() string      { return m.name }
func (m *mockSMSProvider) Send(_ context.Context, msg *provesms.Message) error {
	m.calls++
	m.last = msg
	return m.err
}
func (m *mockSMSProvider) SendInternational(_ context.Context, msg *provesms.InternationalMessage) error {
	m.calls++
	m.lastIntl = msg
	return m.err
}

var (
	testGIDOnce    sync.Once
	testGIDHandler *gidservice.Handler
)

func getTestGID(t *testing.T) gidservice.Service {
	t.Helper()
	testGIDOnce.Do(func() {
		hdl, err := gidservice.NewModule(&gidconfig.Config{
			Snowflake: &gidconfig.SnowflakeConfig{MachineID: 2, StartTime: time.Now().Add(-time.Hour)},
		})
		require.NoError(t, err)
		testGIDHandler = hdl
	})
	return testGIDHandler
}

func setupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	return db
}

func newIdem(t *testing.T) idempotency.Checker {
	t.Helper()
	return idempotency.NewRedisChecker(redisx.NewTestClient(t), &idempotency.Config{
		KeyPrefix: "msg:idem:test-sms",
		EmailTTL:  5 * time.Minute,
		SMSTTL:    5 * time.Minute,
	})
}

// smsFixture wires: app + CN signature + two SMS accounts (aliyun ok,
// tencent ok) + vendor-code template (per-vendor codes) + policy with CN
// and intl chains.
type smsFixture struct {
	svc       *Service
	db        *gorm.DB
	app       *models.MessageApp
	aliyun    *mockSMSProvider
	tencent   *mockSMSProvider
	aliyunID  int64
	tencentID int64
	sigID     int64
}

func newSMSFixture(t *testing.T) *smsFixture {
	t.Helper()
	db := setupDB(t)
	ctx := context.Background()
	app := &models.MessageApp{ID: 111, AppKey: "sms-app", AppSecret: "s", Name: "SMS App"}
	require.NoError(t, dal.CreateApp(ctx, db, app))

	aliyunID, tencentID, sigID := int64(2001), int64(2002), int64(3001)
	for _, acc := range []*models.MessageChannelAccount{
		{ID: aliyunID, Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS), Vendor: int32(pb.SmsVendor_SMS_VENDOR_ALIYUN), Name: "aliyun-main", Config: models.RawJSON(`{}`)},
		{ID: tencentID, Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS), Vendor: int32(pb.SmsVendor_SMS_VENDOR_TENCENT), Name: "tencent-main", Config: models.RawJSON(`{}`)},
	} {
		require.NoError(t, dal.CreateChannelAccount(ctx, db, acc))
	}
	sig := &models.MessageSignature{ID: sigID, Name: "测试签名"}
	require.NoError(t, dal.CreateSignature(ctx, db, sig, []int64{aliyunID, tencentID}))

	params, err := send.MarshalParamSpecs([]models.TemplateParamSpec{{Name: "code", Required: true}})
	require.NoError(t, err)
	content, err := send.MarshalVendorCodes([]send.VendorCode{
		{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, TemplateCode: "SMS_ALIYUN_1"},
		{Vendor: pb.SmsVendor_SMS_VENDOR_TENCENT, TemplateCode: "SMS_TENCENT_1"},
	})
	require.NoError(t, err)
	template := &models.MessageTemplate{
		ID: 2101, AppID: app.ID, Name: "login-code-sms",
		Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Kind:    int32(pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES),
		Params:  params, Content: content,
	}
	require.NoError(t, dal.CreateTemplate(ctx, db, template))

	routes, err := send.MarshalRoutes([]send.Route{
		{AccountID: aliyunID, SignatureID: sigID, Weight: 1},
		{AccountID: tencentID, SignatureID: sigID, Weight: 1},
	})
	require.NoError(t, err)
	policy := &models.MessagePolicy{
		ID: 3101, AppID: app.ID,
		Channel:    int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS),
		Scene:      int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		TemplateID: template.ID,
		Routes:     routes,
		IntlRoutes: routes,
	}
	require.NoError(t, dal.CreatePolicy(ctx, db, policy))

	aliyun := &mockSMSProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, name: "aliyun-main"}
	tencent := &mockSMSProvider{vendor: pb.SmsVendor_SMS_VENDOR_TENCENT, name: "tencent-main"}
	reg := registry.New(db)
	reg.BuildSMS = func(a *models.MessageChannelAccount) (provesms.AccountProvider, error) {
		switch a.ID {
		case aliyunID:
			return aliyun, nil
		case tencentID:
			return tencent, nil
		}
		return nil, fmt.Errorf("no mock for account %d", a.ID)
	}
	require.NoError(t, reg.Refresh(ctx))

	return &smsFixture{
		svc: New(db, newIdem(t), getTestGID(t), reg,
			quota.NewChecker(redisx.NewTestClient(t), "msg:quota:test-sms"), true),
		db: db, app: app, aliyun: aliyun, tencent: tencent,
		aliyunID: aliyunID, tencentID: tencentID, sigID: sigID,
	}
}

// --- tests ---

func TestSendSMSCNUsesVendorCodeAndSignature(t *testing.T) {
	fx := newSMSFixture(t)
	resp, err := fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To:             "+8613800138000",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "998877"},
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.GetStatus())

	// exactly one provider handled it, with its OWN vendor template code
	var used *mockSMSProvider
	if fx.aliyun.calls == 1 {
		used = fx.aliyun
	} else {
		used = fx.tencent
	}
	require.NotNil(t, used.last)
	assert.Equal(t, "测试签名", used.last.SignName)
	assert.Equal(t, map[string]string{"code": "998877"}, used.last.TemplateParams)
	expectedCode := "SMS_ALIYUN_1"
	if used.vendor == pb.SmsVendor_SMS_VENDOR_TENCENT {
		expectedCode = "SMS_TENCENT_1"
	}
	assert.Equal(t, expectedCode, used.last.TemplateID)

	// record carries app/sign/template code
	records, err := dal.ListSMSRecords(context.Background(), fx.db, dal.SmsListFilter{
		AppKey: fx.app.AppKey,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	require.Len(t, records.List, 1)
	assert.Equal(t, "测试签名", records.List[0].SignName)
	assert.Equal(t, fx.app.AppKey, records.List[0].AppKey)
	assert.Equal(t, expectedCode, records.List[0].TemplateID)
}

func TestSendSMSCNFallbackAcrossVendors(t *testing.T) {
	fx := newSMSFixture(t)
	fx.aliyun.err = errors.New("aliyun down")
	// heavier weight on aliyun so the chain starts there deterministically
	// is not needed: with aliyun failing, tencent must be reached regardless
	// of start choice — run until aliyun was tried and tencent succeeded.
	resp, err := fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To:             "+8613800138000",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "1"},
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.GetStatus())
	assert.Equal(t, pb.SmsVendor_SMS_VENDOR_TENCENT, resp.GetSmsVendor())
	assert.Equal(t, 1, fx.tencent.calls)
}

func TestSendSMSIntlChain(t *testing.T) {
	fx := newSMSFixture(t)
	// intl route vendors take the intl path; vendor-code template gives each
	// its code via SendInternational
	resp, err := fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To:             "+14155550123",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "7"},
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.GetStatus())
	var intl *provesms.InternationalMessage
	if fx.aliyun.calls == 1 && fx.aliyun.lastIntl != nil {
		intl = fx.aliyun.lastIntl
	} else {
		intl = fx.tencent.lastIntl
	}
	require.NotNil(t, intl)
	assert.Equal(t, "+14155550123", intl.To)
}

func TestSendSMSPolicyNotFound(t *testing.T) {
	fx := newSMSFixture(t)
	_, err := fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To: "+8613800138000", Scene: pb.SmsScene_SMS_SCENE_REGISTER,
		TemplateParams: map[string]string{"code": "1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POLICY_NOT_FOUND")
}

func TestSendSMSMissingParam(t *testing.T) {
	fx := newSMSFixture(t)
	_, err := fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To: "+8613800138000", Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPLATE_PARAM_MISSING")
	assert.Zero(t, fx.aliyun.calls+fx.tencent.calls)
}

func TestSendSMSInvalidPhone(t *testing.T) {
	fx := newSMSFixture(t)
	_, err := fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To:             "+86101234567", // Beijing landline — not SMS-capable
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
}

func TestSendSMSIdempotency(t *testing.T) {
	fx := newSMSFixture(t)
	req := &pb.SendSMSRequest{
		To: "+8613800138000", Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "3"}, IdempotencyKey: "sms-idem-1",
	}
	first, err := fx.svc.SendSMS(context.Background(), fx.app, req)
	require.NoError(t, err)
	second, err := fx.svc.SendSMS(context.Background(), fx.app, req)
	require.NoError(t, err)
	assert.Equal(t, first.GetId(), second.GetId())
	assert.Equal(t, 1, fx.aliyun.calls+fx.tencent.calls)
}

func TestSendSMSQuota(t *testing.T) {
	fx := newSMSFixture(t)
	fx.app.SMSDailyLimit = 1
	_, err := fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To: "+8613800138000", Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "1"},
	})
	require.NoError(t, err)
	_, err = fx.svc.SendSMS(context.Background(), fx.app, &pb.SendSMSRequest{
		To: "+8613800138001", Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		TemplateParams: map[string]string{"code": "2"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DAILY_QUOTA_EXCEEDED")
}
