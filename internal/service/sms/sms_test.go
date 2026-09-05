package sms

import (
	gidv1 "github.com/servekit/api/gen/go/gid/v1"

	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/message-service/internal/idempotency"
	"github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	pb "github.com/servekit/api/gen/go/messaging/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"

	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// --- mocks (providers only; persistence goes through the real dal) ---

type mockSMSProvider struct {
	name      string
	err       error
	calls     int
	intlCalls int
}

func (m *mockSMSProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_ALIYUN }
func (m *mockSMSProvider) Account() string      { return m.name }
func (m *mockSMSProvider) Send(_ context.Context, _ *sms.Message) error {
	m.calls++
	return m.err
}
func (m *mockSMSProvider) SendInternational(_ context.Context, _ *sms.InternationalMessage) error {
	m.intlCalls++
	return m.err
}

// failingGID is a gidservice.Service that always errors. Used to exercise
// the gid.NextID error path in SendSMS.
type failingGID struct {
	gidv1.UnimplementedGidServiceServer
}

func (failingGID) NextID(context.Context, *gidv1.NextIDRequest) (*gidv1.NextIDResponse, error) {
	return nil, errors.New("gid unavailable")
}

// --- helpers ---

var testGIDHandlerOnce sync.Once
var testGIDHandler *gidservice.Handler

// getTestGID returns a GIDService wrapping a real in-process gid-service
// Handler. The Handler is built once and shared across tests (the snowflake
// generator is the expensive part); NewModule only wraps. Module mode no
// longer builds from config — the raw Handler is constructed here, matching
// how a parent process injects option.WithGIDHandler in production.
func getTestGID(t *testing.T) gidservice.Service {
	t.Helper()
	testGIDHandlerOnce.Do(func() {
		hdl, err := gidservice.NewModule(&gidconfig.Config{
			Snowflake: &gidconfig.SnowflakeConfig{
				MachineID: 1,
				StartTime: time.Now().Add(-time.Hour),
			},
		})
		require.NoError(t, err)
		testGIDHandler = hdl
	})
	return testGIDHandler
}

func setupSMSTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(&models.MessageSMSRecord{}), "auto-migrate should succeed")
	return db
}

func newTestIdempotencyChecker(t *testing.T) idempotency.Checker {
	t.Helper()
	client := redisx.NewTestClient(t)
	return idempotency.NewRedisChecker(client, &idempotency.Config{
		KeyPrefix: "msg:idem",
		EmailTTL:  5 * time.Minute,
		SMSTTL:    5 * time.Minute,
	})
}

func newTestSMSServiceWithRouter(t *testing.T, providers []sms.AccountProvider) *Service {
	t.Helper()
	db := setupSMSTestDB(t)
	accounts := make(map[string]sms.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	registry := sms.NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]sms.AccountProvider{pb.SmsVendor_SMS_VENDOR_ALIYUN: accounts})

	// Configure a wildcard route so BuildRouter returns a non-nil Router.
	// Without routes BuildRouter returns (nil, nil) and SendSMS rejects
	// vendor/account-empty requests with BadRequest.
	router, err := sms.BuildRouter(&sms.Config{
		DefaultCountry: "CN",
		Routes: []*sms.RouteConfig{{
			Country: "*",
			Targets: []*sms.RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "p0"}},
		}},
	}, registry)
	require.NoError(t, err)
	require.NotNil(t, router, "router must be non-nil for tests")

	return New(db, newTestIdempotencyChecker(t), getTestGID(t), registry, router,
		true)
}

// newTestSMSServiceNoPersist mirrors newTestSMSServiceWithRouter but with
// persistence disabled for both channels.
func newTestSMSServiceNoPersist(t *testing.T, providers []sms.AccountProvider) *Service {
	t.Helper()
	db := setupSMSTestDB(t)
	accounts := make(map[string]sms.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	registry := sms.NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]sms.AccountProvider{pb.SmsVendor_SMS_VENDOR_ALIYUN: accounts})

	router, err := sms.BuildRouter(&sms.Config{
		DefaultCountry: "CN",
		Routes: []*sms.RouteConfig{{
			Country: "*",
			Targets: []*sms.RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "p0"}},
		}},
	}, registry)
	require.NoError(t, err)
	require.NotNil(t, router)

	return New(db, newTestIdempotencyChecker(t), getTestGID(t), registry, router,
		false)
}

// --- tests ---

func TestSendSMS_Success(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	resp, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)
	assert.Greater(t, resp.Id, int64(0))

	// Verify persistence: scene and sender_id recorded.
	record, err := svc.GetSMS(context.Background(), &pb.GetSMSRequest{Id: resp.Id})
	require.NoError(t, err)
	assert.Equal(t, pb.SmsScene_SMS_SCENE_LOGIN_CODE, record.Scene)
	assert.Equal(t, "user:42", record.SenderId)
	assert.Equal(t, "CN", record.RegionCode)
	assert.Equal(t, "13800000111", record.Phone)

	// Verify vendor is correctly mapped (regression: previously always 0
	// because AccountProvider.Vendor uses enum.String() but the old switch
	// matched lowercase names).
	assert.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, record.Vendor,
		"record.Vendor must reflect the AccountProvider's enum, not UNSPECIFIED")
	assert.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, resp.GetSmsVendor(),
		"SendResponse.Vendor must reflect the AccountProvider's enum")
}

func TestSendSMS_ProviderError_PersistsFailedRecord(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock", err: fmt.Errorf("aliyun timeout")},
	})

	_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_REGISTER,
		SenderId:       "user:42",
	})
	require.Error(t, err)

	// Verify a FAILED record was persisted. List by sender_id to find it.
	resp, err := svc.ListSMS(context.Background(), &pb.ListSMSRequest{
		SenderId: "user:42",
	})
	require.NoError(t, err)
	require.Len(t, resp.Records, 1)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, resp.Records[0].Status)
	assert.Equal(t, pb.SmsScene_SMS_SCENE_REGISTER, resp.Records[0].Scene)
}

func TestListSMS_ByScene(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	// Send two SMS with different scenes.
	for _, scene := range []pb.SmsScene{
		pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		pb.SmsScene_SMS_SCENE_REGISTER,
	} {
		_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
			RegionCode:     "CN",
			Phone:          "13800000111",
			TemplateId:     "SMS_123",
			TemplateParams: map[string]string{"code": "1234"},
			SignName:       "sign",
			Scene:          scene,
			SenderId:       "user:42",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListSMS(context.Background(), &pb.ListSMSRequest{
		Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.Total)
	assert.Len(t, resp.Records, 1)
}

func TestSendSMS_Idempotent_NoKey_DoesNotDedupe(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		// No idempotency_key
	}

	_, err := svc.SendSMS(context.Background(), req)
	require.NoError(t, err)

	_, err = svc.SendSMS(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 2, provider.calls, "without key, both calls hit provider")
}

// TestSendSMS_Idempotent_FailureNotCached_RetriesProvider verifies the
// Redis idempotency contract on failure: a failed send releases the
// reservation, so a second call with the same key hits the provider again
// rather than returning a cached failure.
func TestSendSMS_Idempotent_FailureNotCached_RetriesProvider(t *testing.T) {
	provider := &mockSMSProvider{name: "mock", err: fmt.Errorf("aliyun timeout")}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	// First call fails — reservation released, failure not cached.
	_, err := svc.SendSMS(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, 1, provider.calls)

	// Second call with same key hits provider again (Redis reservation was
	// released after the failure, so dedup does not kick in).
	_, err = svc.SendSMS(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, 2, provider.calls, "failed send must release reservation so retry hits provider")
}

func TestSendSMS_PersistsEvenWhenContextCancelled(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before send

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		// Use explicit vendor+account so we hit the Sender path (which mirrors
		// email.Sender): the Sender wrapper checks ctx.Err() *inside* its
		// retry loop and returns a non-nil failed result, triggering persist.
		Vendor:  pb.SmsVendor_SMS_VENDOR_ALIYUN,
		Account: "p0",
	}

	_, err := svc.SendSMS(ctx, req)
	// Sender's Send is called with cancelled ctx; the Sender wrapper
	// checks ctx.Err() inside the retry loop and returns a failed result.
	// Service persists it.
	require.Error(t, err)

	// The record must still be persisted (independent ctx).
	listResp, lerr := svc.ListSMS(context.Background(), &pb.ListSMSRequest{
		SenderId: "user:42",
	})
	require.NoError(t, lerr)
	require.Len(t, listResp.Records, 1, "record must be persisted even with cancelled ctx")
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, listResp.Records[0].Status)
}

// TestSendSMS_RouterPathPersistsEvenWhenContextCancelled exercises the Router
// path (no explicit vendor+account). Before the router ctx.Err() fix this
// would return (nil, ctx.Err()) pre-send and the service would skip persist;
// now the router returns Success=false and the record is persisted.
func TestSendSMS_RouterPathPersistsEvenWhenContextCancelled(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := svc.SendSMS(ctx, &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		// No vendor+account: request is routed through sms.Router.
	})
	require.Error(t, err)

	listResp, lerr := svc.ListSMS(context.Background(), &pb.ListSMSRequest{
		SenderId: "user:42",
	})
	require.NoError(t, lerr)
	require.Len(t, listResp.Records, 1, "router-path pre-cancel must still persist a FAILED record")
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, listResp.Records[0].Status)
}

func TestSendSMS_RejectsMissingScene(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		SenderId:       "user:42",
		// No scene
	})
	require.Error(t, err)
	assert.Equal(t, 0, provider.calls, "validation must short-circuit before provider call")
}

func TestSendSMS_RejectsVendorWithoutAccount(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		Vendor:         pb.SmsVendor_SMS_VENDOR_ALIYUN,
		// No account
	})
	require.Error(t, err)
	assert.Equal(t, 0, provider.calls)
}

func TestSendSMS_FailureIncludesVendorContext(t *testing.T) {
	provider := &mockSMSProvider{name: "aliyun", err: fmt.Errorf("connection refused")}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
	})
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "vendor=")
	assert.Contains(t, msg, "account=")
	assert.Contains(t, msg, "attempts=")
	assert.Contains(t, msg, "connection refused")
}

func TestListSMS_ASC_WithTotalPages(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	for i := 0; i < 3; i++ {
		_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
			RegionCode:     "CN",
			Phone:          fmt.Sprintf("1380000%04d", i),
			TemplateId:     "SMS_123",
			TemplateParams: map[string]string{"code": "1234"},
			SignName:       "sign",
			Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
			SenderId:       "user:42",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListSMS(context.Background(), &pb.ListSMSRequest{
		Scene:         pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SortDirection: pb.SortDirection_SORT_DIRECTION_ASC,
		Page:          1,
		PageSize:      2,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(3), resp.Total)
	assert.Equal(t, int32(2), resp.TotalPages)
	assert.True(t, resp.HasMore)
	assert.Len(t, resp.Records, 2)
}

func TestListSMSByCursor_TwoPageFlow(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	for i := 0; i < 3; i++ {
		_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
			RegionCode:     "CN",
			Phone:          fmt.Sprintf("1380000%04d", i),
			TemplateId:     "SMS_123",
			TemplateParams: map[string]string{"code": "1234"},
			SignName:       "sign",
			Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
			SenderId:       "user:42",
		})
		require.NoError(t, err)
	}

	first, err := svc.ListSMSByCursor(context.Background(), &pb.ListSMSByCursorRequest{
		Scene:    pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		PageSize: 2,
	})
	require.NoError(t, err)
	assert.Len(t, first.Records, 2)
	assert.NotEmpty(t, first.NextPageToken)
	assert.Equal(t, int32(0), first.Total)

	second, err := svc.ListSMSByCursor(context.Background(), &pb.ListSMSByCursorRequest{
		Scene:     pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		PageSize:  2,
		PageToken: first.NextPageToken,
	})
	require.NoError(t, err)
	assert.Len(t, second.Records, 1)
	assert.Empty(t, second.NextPageToken)

	ids := map[int64]struct{}{}
	for _, r := range append(first.Records, second.Records...) {
		ids[r.Id] = struct{}{}
	}
	assert.Len(t, ids, 3)
}

func TestListSMSByCursor_BadToken(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	_, err := svc.ListSMSByCursor(context.Background(), &pb.ListSMSByCursorRequest{
		Scene:     pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		PageSize:  2,
		PageToken: "garbage-token",
	})
	require.Error(t, err)
}

func TestSendSMS_PersistenceDisabled_SkipsDB(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	resp, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		// No vendor/account — router picks aliyun/p0.
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)

	_, err = svc.GetSMS(context.Background(), &pb.GetSMSRequest{Id: resp.Id})
	require.Error(t, err, "GetSMS must fail when persistence disabled")
}

// TestSendSMS_PersistenceDisabled_IdempotencyStillWorks verifies that
// Redis idempotency is independent of the persistence toggle: even with
// persistence off, the same idempotency_key is deduped via Redis.
func TestSendSMS_PersistenceDisabled_IdempotencyStillWorks(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{provider})

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "sms-1",
	}

	_, err := svc.SendSMS(context.Background(), req)
	require.NoError(t, err)
	_, err = svc.SendSMS(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 1, provider.calls, "provider must be called once (Redis dedup works even with persistence off)")
}

func TestGetSMS_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.GetSMS(context.Background(), &pb.GetSMSRequest{Id: 1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()),
		"err must wrap ErrPersistenceDisabled, got: %v", err)
}

func TestListSMS_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.ListSMS(context.Background(), &pb.ListSMSRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()))
}

func TestListSMSByCursor_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.ListSMSByCursor(context.Background(), &pb.ListSMSByCursorRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()))
}

func TestGetSMSStats_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.GetSMSStats(context.Background(), &pb.GetSMSStatsRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()))
}

func TestSendSMS_Idempotent_SecondCallReturnsCached(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "sms-1",
	}

	resp1, err := svc.SendSMS(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp1.Status)

	resp2, err := svc.SendSMS(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, resp1.Id, resp2.Id)
	assert.Equal(t, 1, provider.calls, "provider must be called once")
}

func TestSendSMS_IdempotencyConflict_OnInFlight(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	// Plant a "PENDING" marker to simulate in-flight. Reserve sees the
	// literal "PENDING" string and treats it as in-flight (matches the
	// pendingMarker constant in redis_checker.go).
	require.NoError(t, svc.idem.Complete(context.Background(), "sms", "user:42", "in-flight", []byte("PENDING")))

	_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "in-flight",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrIdempotencyConflict.New()))
}

func TestSendSMS_Failure_NotCached_ReleasesReservation(t *testing.T) {
	provider := &mockSMSProvider{name: "mock", err: errors.New("vendor rejected")}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "fail-once",
	}

	_, err := svc.SendSMS(context.Background(), req)
	require.Error(t, err, "first call must fail")

	acquired, payload, err := svc.idem.Reserve(context.Background(), "sms", "user:42", "fail-once")
	require.NoError(t, err)
	assert.True(t, acquired, "Reserve after failed send must acquire (key was Released)")
	assert.Nil(t, payload, "no cached payload (failure was not cached)")
}

// TestSendSMS_IdempotencyReleased_OnGIDFailure verifies the post-Reserve /
// pre-vendor error path: when gid.NextID fails, the reservation must be
// released. Regression: previously this path returned without Release.
func TestSendSMS_IdempotencyReleased_OnGIDFailure(t *testing.T) {
	db := setupSMSTestDB(t)
	registry := sms.NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]sms.AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"p0": &mockSMSProvider{name: "mock"}},
	})
	router, err := sms.BuildRouter(&sms.Config{
		DefaultCountry: "CN",
		Routes: []*sms.RouteConfig{{
			Country: "*",
			Targets: []*sms.RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "p0"}},
		}},
	}, registry)
	require.NoError(t, err)

	svc := New(db, newTestIdempotencyChecker(t), failingGID{}, registry, router,
		true)

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "gid-fail",
	}

	_, err = svc.SendSMS(context.Background(), req)
	require.Error(t, err, "gid.NextID must fail")

	acquired, payload, reserveErr := svc.idem.Reserve(context.Background(), "sms", "user:42", "gid-fail")
	require.NoError(t, reserveErr)
	assert.True(t, acquired, "Reserve after gid failure must acquire (key was Released)")
	assert.Nil(t, payload)
}

// TestSendSMS_IdempotencyReleased_OnSenderForFailure verifies the post-Reserve
// / post-gid error path: when SenderFor rejects an unknown account, the
// reservation must be released. Regression: previously this path returned
// without Release.
func TestSendSMS_IdempotencyReleased_OnSenderForFailure(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		Vendor:         pb.SmsVendor_SMS_VENDOR_ALIYUN,
		Account:        "nonexistent", // not in registry → SenderFor fails
		IdempotencyKey: "senderfor-fail",
	}

	_, err := svc.SendSMS(context.Background(), req)
	require.Error(t, err, "SenderFor must fail on unknown account")

	acquired, payload, reserveErr := svc.idem.Reserve(context.Background(), "sms", "user:42", "senderfor-fail")
	require.NoError(t, reserveErr)
	assert.True(t, acquired, "Reserve after SenderFor failure must acquire (key was Released)")
	assert.Nil(t, payload)
}

// TestSendSMS_IdempotencyReleased_OnRouterNil verifies the post-Reserve error
// path when neither vendor nor router is configured: the reservation must be
// released. Regression: previously this path returned without Release.
func TestSendSMS_IdempotencyReleased_OnRouterNil(t *testing.T) {
	db := setupSMSTestDB(t)
	registry := sms.NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]sms.AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"p0": &mockSMSProvider{name: "mock"}},
	})

	// Service with nil router: a no-vendor request has nowhere to go.
	svc := New(db, newTestIdempotencyChecker(t), getTestGID(t), registry, nil,
		true)

	req := &pb.SendSMSRequest{
		RegionCode:     "CN",
		Phone:          "13800000111",
		TemplateId:     "SMS_123",
		TemplateParams: map[string]string{"code": "1234"},
		SignName:       "sign",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "router-nil",
	}

	_, err := svc.SendSMS(context.Background(), req)
	require.Error(t, err, "router nil with no vendor must fail")

	acquired, payload, reserveErr := svc.idem.Reserve(context.Background(), "sms", "user:42", "router-nil")
	require.NoError(t, reserveErr)
	assert.True(t, acquired, "Reserve after router-nil failure must acquire (key was Released)")
	assert.Nil(t, payload)
}

// --- validateSendSMSRequest (request-level defense-in-depth) ---

func TestValidateSendSMSRequest_VendorWithoutAccount(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "13800000111",
		TemplateId: "SMS_123",
		SignName:   "sign",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user:42",
		Vendor:     pb.SmsVendor_SMS_VENDOR_ALIYUN,
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vendor and account")
}

func TestValidateSendSMSRequest_RegionCodeInvalidFormat(t *testing.T) {
	cases := []string{"", "cn", "CHN", "C", "ABC", "12"}
	for _, rc := range cases {
		t.Run(rc, func(t *testing.T) {
			req := &pb.SendSMSRequest{
				RegionCode: rc,
				Phone:      "13800000111",
				Content:    "Your code is 1234",
				Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
				SenderId:   "user-service",
			}
			err := validateSendSMSRequest(req)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "region_code")
		})
	}
}

func TestValidateSendSMSRequest_PhoneStartsWithPlus(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "+8613800001111",
		TemplateId: "SMS_123",
		SignName:   "sign",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "phone")
}

func TestValidateSendSMSRequest_PhoneUnparsable(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "not-a-phone!!!",
		TemplateId: "SMS_123",
		SignName:   "sign",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestValidateSendSMSRequest_RegionMismatch(t *testing.T) {
	// Region CN but phone parses as RU number (00 international prefix routes
	// to a different country than the supplied defaultRegion).
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "0074951234567",
		TemplateId: "SMS_123",
		SignName:   "sign",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "region")
}

func TestValidateSendSMSRequest_ValidChineseNumber(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "13800000111",
		TemplateId: "SMS_123",
		SignName:   "sign",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	assert.NoError(t, validateSendSMSRequest(req))
}

func TestValidateSendSMSRequest_ValidUSNumber(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "US",
		Phone:      "4155552671",
		Content:    "Your code is 1234",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	assert.NoError(t, validateSendSMSRequest(req))
}

func TestValidateSendSMSRequest_ValidHKNumber(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "HK",
		Phone:      "91234567",
		Content:    "Your code is 1234",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	assert.NoError(t, validateSendSMSRequest(req))
}
