package email

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	gidv1 "github.com/servekit/api/gen/go/gid/v1"
	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"
	"github.com/servekit/message-service/internal/idempotency"
	provemail "github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/internal/quota"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/jsonx"
	"github.com/servekit/go-common/redisx"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// --- mocks ---

type mockEmailProvider struct {
	name  string
	err   error
	calls int
	last  *provemail.Message
}

func (m *mockEmailProvider) Vendor() pb.EmailVendor { return pb.EmailVendor_EMAIL_VENDOR_ALIYUN }
func (m *mockEmailProvider) Account() string        { return m.name }
func (m *mockEmailProvider) Send(_ context.Context, msg *provemail.Message) error {
	m.calls++
	m.last = msg
	return m.err
}

type failingGID struct {
	gidv1.UnimplementedGidServiceServer
}

func (failingGID) NextID(context.Context, *gidv1.NextIDRequest) (*gidv1.NextIDResponse, error) {
	return nil, errors.New("gid unavailable")
}

// --- helpers ---

var (
	testGIDOnce    sync.Once
	testGIDHandler *gidservice.Handler
)

func getTestGID(t *testing.T) gidservice.Service {
	t.Helper()
	testGIDOnce.Do(func() {
		hdl, err := gidservice.NewModule(&gidconfig.Config{
			Snowflake: &gidconfig.SnowflakeConfig{MachineID: 1, StartTime: time.Now().Add(-time.Hour)},
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
		KeyPrefix: "msg:idem:test",
		EmailTTL:  5 * time.Minute,
		SMSTTL:    5 * time.Minute,
	})
}

// fixture is a fully wired policy-driven email service: one app, one
// template ({{code}} placeholder), one policy with the given providers as
// its route chain.
type fixture struct {
	svc       *Service
	db        *gorm.DB
	app       *models.MessageApp
	providers []*mockEmailProvider
}

func newFixture(t *testing.T, providers ...*mockEmailProvider) *fixture {
	t.Helper()
	db := setupDB(t)
	app := &models.MessageApp{ID: 101, AppKey: "test-app", AppSecret: "secret", Name: "Test App"}
	require.NoError(t, dal.CreateApp(context.Background(), db, app))

	// email channel accounts — config JSON is opaque to the mocked builder
	accountIDs := make([]int64, len(providers))
	for i := range providers {
		accountIDs[i] = int64(1000 + i)
		acc := &models.MessageChannelAccount{
			ID:      accountIDs[i],
			Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL),
			Vendor:  int32(pb.EmailVendor_EMAIL_VENDOR_ALIYUN),
			Name:    fmt.Sprintf("acct-%d", i),
			Config:  models.RawJSON(`{"Host":"h","Username":"u","Password":"p","From":"f@x.com"}`),
		}
		require.NoError(t, dal.CreateChannelAccount(context.Background(), db, acc))
	}

	params, err := send.MarshalParamSpecs([]models.TemplateParamSpec{{Name: "code", Required: true}})
	require.NoError(t, err)
	content, err := send.MarshalEmailContent(&send.EmailContent{
		Subject:  "Code {{code}}",
		TextBody: "your code is {{code}}",
		HTMLBody: "<p>{{code}}</p>",
	})
	require.NoError(t, err)
	template := &models.MessageTemplate{
		ID: 201, AppID: app.ID, Name: "login-code-email",
		Channel: int32(pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL),
		Kind:    int32(pb.TemplateKind_TEMPLATE_KIND_EMAIL_RENDER),
		Params:  params, Content: content,
	}
	require.NoError(t, dal.CreateTemplate(context.Background(), db, template))

	routes := make([]send.Route, len(providers))
	for i := range providers {
		routes[i] = send.Route{AccountID: accountIDs[i], Weight: 1}
	}
	routesJSON, err := send.MarshalRoutes(routes)
	require.NoError(t, err)
	policy := &models.MessagePolicy{
		ID: 301, AppID: app.ID,
		Channel:    int32(pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL),
		Scene:      int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		TemplateID: template.ID,
		Routes:     routesJSON,
	}
	require.NoError(t, dal.CreatePolicy(context.Background(), db, policy))

	byID := make(map[int64]*mockEmailProvider, len(providers))
	for i, p := range providers {
		byID[accountIDs[i]] = p
	}
	reg := registry.New(db)
	reg.BuildEmail = func(a *models.MessageChannelAccount) (provemail.AccountProvider, error) {
		if m, ok := byID[a.ID]; ok {
			return m, nil
		}
		return nil, fmt.Errorf("no mock for account %d", a.ID)
	}
	require.NoError(t, reg.Refresh(context.Background()))

	return &fixture{
		svc: New(db, newIdem(t), getTestGID(t), reg,
			quota.NewChecker(redisx.NewTestClient(t), "msg:quota:test"),
			true, &config.AttachmentConfig{MaxInlineBytes: 1 << 20, MaxTotalInlineBytes: 2 << 20}),
		db:        db,
		app:       app,
		providers: providers,
	}
}

func sendReq(params map[string]string) *pb.SendEmailRequest {
	return &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "to@example.com"}},
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		TemplateParams: params,
	}
}

// tenant is the fixture app's resolved tenant context (stamped column,
// app_key literal fallback — mirrors what tenantres hands the send path).
func (f *fixture) tenant() string { return models.AppTenantKey(f.app) }

// --- tests ---

func TestSendEmailPolicyDriven(t *testing.T) {
	ok := &mockEmailProvider{name: "primary"}
	fx := newFixture(t, ok)

	resp, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), sendReq(map[string]string{"code": "424242"}))
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.GetStatus())
	assert.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, resp.GetEmailVendor())

	// template rendered platform-side
	require.NotNil(t, ok.last)
	assert.Equal(t, "Code 424242", ok.last.Subject)
	assert.Equal(t, "your code is 424242", ok.last.Body)
	assert.Equal(t, "<p>424242</p>", ok.last.HTMLBody)

	// record persisted with app identity
	records, err := dal.ListEmailRecords(context.Background(), fx.db, dal.EmailListFilter{
		AppKey: fx.app.AppKey,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	require.Len(t, records.List, 1)
	assert.Equal(t, "Code 424242", records.List[0].Subject)
	assert.Equal(t, fx.app.AppKey, records.List[0].AppKey)
}

func TestSendEmailPolicyNotFound(t *testing.T) {
	fx := newFixture(t, &mockEmailProvider{name: "p"})
	_, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "to@example.com"}},
		Scene:          pb.EmailScene_EMAIL_SCENE_REGISTER, // no policy configured
		TemplateParams: map[string]string{"code": "1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "POLICY_NOT_FOUND")
	assert.Contains(t, err.Error(), "EMAIL_SCENE_LOGIN_CODE") // configured scenes listed
}

func TestSendEmailMissingRequiredParam(t *testing.T) {
	fx := newFixture(t, &mockEmailProvider{name: "p"})
	_, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), sendReq(map[string]string{}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TEMPLATE_PARAM_MISSING")
	assert.Zero(t, fx.providers[0].calls, "provider must not be called on param validation failure")
}

func TestSendEmailFallback(t *testing.T) {
	primary := &mockEmailProvider{name: "primary", err: errors.New("smtp rejected")}
	secondary := &mockEmailProvider{name: "secondary"}
	fx := newFixture(t, primary, secondary)

	resp, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), sendReq(map[string]string{"code": "1"}))
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.GetStatus())
	assert.Equal(t, 1, primary.calls)
	assert.Equal(t, 1, secondary.calls, "fallback must reach the second provider")
}

func TestSendEmailAllProvidersFail(t *testing.T) {
	fx := newFixture(t,
		&mockEmailProvider{name: "a", err: errors.New("x")},
		&mockEmailProvider{name: "b", err: errors.New("y")},
	)
	_, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), sendReq(map[string]string{"code": "1"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MESSAGE_SEND_FAILED")

	// failure record persisted
	records, err := dal.ListEmailRecords(context.Background(), fx.db, dal.EmailListFilter{
		AppKey: fx.app.AppKey, Status: pb.MessageStatus_MESSAGE_STATUS_FAILED,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	require.Len(t, records.List, 1)
	assert.Equal(t, 2, records.List[0].Attempts)
}

func TestSendEmailIdempotency(t *testing.T) {
	ok := &mockEmailProvider{name: "p"}
	fx := newFixture(t, ok)
	req := sendReq(map[string]string{"code": "9"})
	req.IdempotencyKey = "idem-1"

	first, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), req)
	require.NoError(t, err)
	second, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), req)
	require.NoError(t, err)
	assert.Equal(t, first.GetId(), second.GetId(), "replay returns the cached response")
	assert.Equal(t, 1, ok.calls, "provider must be called exactly once")
}

func TestQuotaEnforced(t *testing.T) {
	ok := &mockEmailProvider{name: "p"}
	fx := newFixture(t, ok)
	fx.app.EmailDailyLimit = 1

	_, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), sendReq(map[string]string{"code": "1"}))
	require.NoError(t, err)

	_, err = fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), sendReq(map[string]string{"code": "2"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DAILY_QUOTA_EXCEEDED")
	assert.Equal(t, 1, ok.calls)
}

// config shape guard: jsonx round trip of the stored account config matches
// the provider AccountConfig the registry decodes.
func TestAccountConfigJSONRoundTrip(t *testing.T) {
	src := provemail.AccountConfig{Name: "n", Vendor: "aliyun", Host: "h", Port: 465, Username: "u", Password: "p", From: "f@x.com"}
	b, err := jsonx.Marshal(&src)
	require.NoError(t, err)
	var dst provemail.AccountConfig
	require.NoError(t, jsonx.Unmarshal(b, &dst))
	assert.Equal(t, src, dst)
}

var _ = xcodes.ErrPolicyNotFound // keep import anchored

func TestSendEmailFreeFormContentWithParams(t *testing.T) {
	ok := &mockEmailProvider{name: "p"}
	fx := newFixture(t, ok)

	_, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), &pb.SendEmailRequest{
		To:    []*pb.EmailAddress{{Email: "to@example.com"}},
		Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		// free-form content wins over the template; {{param}} still rendered
		Subject:        "Hi {{nickname}}",
		Body:           "custom body for {{nickname}}, code {{code}}",
		TemplateParams: map[string]string{"nickname": "Ada", "code": "424242"},
	})
	require.NoError(t, err)
	require.NotNil(t, ok.last)
	assert.Equal(t, "Hi Ada", ok.last.Subject)
	assert.Equal(t, "custom body for Ada, code 424242", ok.last.Body)
	assert.Empty(t, ok.last.Template, "free-form sends carry no template label")

	// record persisted with the free-form content and empty template id
	records, err := dal.ListEmailRecords(context.Background(), fx.db, dal.EmailListFilter{
		AppKey: fx.app.AppKey,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	require.Len(t, records.List, 1)
	assert.Equal(t, "Hi Ada", records.List[0].Subject)
	assert.Empty(t, records.List[0].TemplateID)
}

func TestSendEmailFreeFormRequiresBody(t *testing.T) {
	fx := newFixture(t, &mockEmailProvider{name: "p"})
	_, err := fx.svc.SendEmail(context.Background(), fx.app, fx.tenant(), &pb.SendEmailRequest{
		To:      []*pb.EmailAddress{{Email: "to@example.com"}},
		Scene:   pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		Subject: "subject without body",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "BAD_REQUEST")
}
