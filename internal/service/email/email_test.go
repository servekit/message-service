package email

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/servekit/message-service/internal/idempotency"
	"github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	gid_service "github.com/servekit/message-service/internal/thirdcall/gid_service"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/xcodes"

	pb "github.com/servekit/message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/redisx"

	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// --- mocks (providers only; persistence goes through the real dal) ---

type mockEmailProvider struct {
	name  string
	err   error
	calls int
}

func (m *mockEmailProvider) Vendor() pb.EmailVendor { return pb.EmailVendor_EMAIL_VENDOR_ALIYUN }
func (m *mockEmailProvider) Account() string        { return m.name }
func (m *mockEmailProvider) Send(_ context.Context, _ *email.Message) error {
	m.calls++
	return m.err
}

// failingGID is a gid_service.GIDService that always errors. Used to exercise
// the gid.NextID error path in SendEmail / SendSMS.
type failingGID struct{}

func (failingGID) NextID(context.Context) (int64, error) {
	return 0, errors.New("gid unavailable")
}

func (failingGID) Close() error { return nil }

// --- helpers ---

var testGIDHandlerOnce sync.Once
var testGIDHandler *gidservice.Handler

// getTestGID returns a GIDService wrapping a real in-process gid-service
// Handler. The Handler is built once and shared across tests (the snowflake
// generator is the expensive part); NewModule only wraps. Module mode no
// longer builds from config — the raw Handler is constructed here, matching
// how a parent process injects option.WithGIDHandler in production.
func getTestGID(t *testing.T) gid_service.GIDService {
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
	return gid_service.NewModule(testGIDHandler)
}

func setupEmailTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(&models.MessageEmailRecord{}, &models.MessageEmailRecordAttachment{}), "auto-migrate should succeed")
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

// newTestHTTPClient returns an *http.Client configured the same way as
// production: a short timeout and CheckRedirect=ErrUseLastResponse (no
// redirect following) to mitigate SSRF on attachment fetches. Use this in
// tests instead of http.DefaultClient or a bare &http.Client{Timeout:...}.
func newTestHTTPClient() *http.Client {
	return &http.Client{
		Timeout: time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// newTestAttachmentConfig returns an AttachmentConfig for the common test
// path: 1MB hard cap, 2MB single inline, 5MB per-request total. Tests that
// need other values construct their own &config.AttachmentConfig{...}.
func newTestAttachmentConfig() *config.AttachmentConfig {
	return &config.AttachmentConfig{
		FetchTimeout:        "1s",
		MaxBytes:            1024 * 1024,
		MaxInlineBytes:      2 * 1024 * 1024,
		MaxTotalInlineBytes: 5 * 1024 * 1024,
	}
}

func newTestEmailService(t *testing.T, providers []email.AccountProvider) *Service {
	t.Helper()
	db := setupEmailTestDB(t)
	accounts := make(map[string]email.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	return New(
		db,
		newTestIdempotencyChecker(t),
		getTestGID(t),
		email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{pb.EmailVendor_EMAIL_VENDOR_ALIYUN: accounts}),
		true,
		newTestAttachmentConfig(),
		newTestHTTPClient(),
	)
}

func newTestEmailServiceNoPersist(t *testing.T, providers []email.AccountProvider) *Service {
	t.Helper()
	db := setupEmailTestDB(t)
	accounts := make(map[string]email.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	return New(
		db,
		newTestIdempotencyChecker(t),
		getTestGID(t),
		email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{pb.EmailVendor_EMAIL_VENDOR_ALIYUN: accounts}),
		false,
		newTestAttachmentConfig(),
		newTestHTTPClient(),
	)
}

// --- tests ---

func TestSendEmail_Success(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	resp, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)
	assert.Greater(t, resp.Id, int64(0))

	// Verify persistence: scene and sender_id recorded.
	record, err := svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: resp.Id})
	require.NoError(t, err)
	assert.Equal(t, pb.EmailScene_EMAIL_SCENE_LOGIN_CODE, record.Scene)
	assert.Equal(t, "user:42", record.SenderId)
	assert.Equal(t, "user@example.com", record.Target.GetEmail())

	// Verify vendor is correctly mapped (regression: previously always 0
	// because AccountProvider.Vendor uses enum.String() but the old switch
	// matched lowercase names).
	assert.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, record.Vendor,
		"record.Vendor must reflect the AccountProvider's enum, not UNSPECIFIED")
	assert.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, resp.GetEmailVendor(),
		"SendResponse.Vendor must reflect the AccountProvider's enum")
}

func TestSendEmail_ProviderError_PersistsFailedRecord(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock", err: fmt.Errorf("smtp timeout")},
	})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_REGISTER,
		SenderId: "user:42",
	})
	require.Error(t, err)

	// Verify a FAILED record was persisted. List by sender_id to find it.
	resp, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		SenderId: "user:42",
	})
	require.NoError(t, err)
	require.Len(t, resp.Records, 1)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, resp.Records[0].Status)
	assert.Equal(t, pb.EmailScene_EMAIL_SCENE_REGISTER, resp.Records[0].Scene)
}

func TestListEmails_ByScene(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	// Send two emails with different scenes.
	for _, scene := range []pb.EmailScene{
		pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		pb.EmailScene_EMAIL_SCENE_REGISTER,
	} {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       []*pb.EmailAddress{{Email: "user@example.com"}},
			Subject:  "Test",
			Body:     "Hello",
			Scene:    scene,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.Total)
	assert.Len(t, resp.Records, 1)
}

func TestListEmails_ASC_WithTotalPages(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	for i := 0; i < 3; i++ {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       []*pb.EmailAddress{{Email: fmt.Sprintf("u%d@x.com", i)}},
			Subject:  "T",
			Body:     "B",
			Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		Scene:         pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
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

func TestGetEmailStats(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	// Send 2 successful + 1 failed.
	for i := 0; i < 2; i++ {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       []*pb.EmailAddress{{Email: "user@example.com"}},
			Subject:  "Test",
			Body:     "Hello",
			Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}
	// Failed one
	_, _ = svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "fail@example.com"}},
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Vendor:   pb.EmailVendor_EMAIL_VENDOR_ALIYUN, // route to non-existent provider
		Account:  "nonexistent",
	})

	resp, err := svc.GetEmailStats(context.Background(), &pb.GetEmailStatsRequest{})
	require.NoError(t, err)
	// At minimum, the 2 successful sends are counted.
	assert.GreaterOrEqual(t, resp.Total, int64(2))
	assert.GreaterOrEqual(t, resp.Sent, int64(2))
}

func TestSendEmail_Idempotent_NoKey_DoesNotDedupe(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		// No idempotency_key
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)

	_, err = svc.SendEmail(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 2, provider.calls, "without key, both calls hit provider")
}

// TestSendEmail_Idempotent_FailureNotCached_RetriesProvider verifies the
// Redis idempotency contract on failure: a failed send releases the
// reservation, so a second call with the same key hits the provider again
// rather than returning a cached failure.
func TestSendEmail_Idempotent_FailureNotCached_RetriesProvider(t *testing.T) {
	provider := &mockEmailProvider{name: "mock", err: fmt.Errorf("smtp timeout")}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	// First call fails — reservation released, failure not cached.
	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, 1, provider.calls)

	// Second call with same key hits provider again (Redis reservation was
	// released after the failure, so dedup does not kick in).
	_, err = svc.SendEmail(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, 2, provider.calls, "failed send must release reservation so retry hits provider")
}

func TestSendEmail_PersistsEvenWhenContextCancelled(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before send

	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	}

	_, err := svc.SendEmail(ctx, req)
	// Provider's Send is called with cancelled ctx; the Sender wrapper
	// checks ctx.Err() and returns a failed result. Service persists it.
	require.Error(t, err)

	// The record must still be persisted (independent ctx).
	listResp, lerr := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		SenderId: "user:42",
	})
	require.NoError(t, lerr)
	require.Len(t, listResp.Records, 1, "record must be persisted even with cancelled ctx")
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, listResp.Records[0].Status)
}

func TestSendEmail_RejectsMissingScene(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		SenderId: "user:42",
		// No scene
	})
	require.Error(t, err)
	assert.Equal(t, 0, provider.calls, "validation must short-circuit before provider call")
}

func TestSendEmail_RejectsVendorWithoutAccount(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Vendor:   pb.EmailVendor_EMAIL_VENDOR_ALIYUN,
		// No account
	})
	require.Error(t, err)
	assert.Equal(t, 0, provider.calls)
}

func TestSendEmail_FailureIncludesVendorContext(t *testing.T) {
	provider := &mockEmailProvider{name: "aliyun", err: fmt.Errorf("connection refused")}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	})
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "vendor=")
	assert.Contains(t, msg, "account=")
	assert.Contains(t, msg, "attempts=")
	assert.Contains(t, msg, "connection refused")
}

func TestListEmailsByCursor_TwoPageFlow(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	// 3 records, page_size = 2 → expect 2 pages.
	for i := 0; i < 3; i++ {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       []*pb.EmailAddress{{Email: fmt.Sprintf("u%d@x.com", i)}},
			Subject:  "T",
			Body:     "B",
			Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	// Page 1: empty token, page_size = 2.
	first, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize: 2,
	})
	require.NoError(t, err)
	assert.Len(t, first.Records, 2)
	assert.NotEmpty(t, first.NextPageToken, "must return a next-page token when more rows remain")
	assert.Equal(t, int32(0), first.Total, "include_total defaults to false → total stays 0")

	// Page 2: pass token.
	second, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:     pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize:  2,
		PageToken: first.NextPageToken,
	})
	require.NoError(t, err)
	assert.Len(t, second.Records, 1)
	assert.Empty(t, second.NextPageToken, "no third page expected")

	// No duplication across pages.
	ids := map[int64]struct{}{}
	for _, r := range append(first.Records, second.Records...) {
		ids[r.Id] = struct{}{}
	}
	assert.Len(t, ids, 3, "cursor flow must return each record exactly once")
}

func TestListEmailsByCursor_BadToken(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	_, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:     pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize:  2,
		PageToken: "garbage-token",
	})
	require.Error(t, err)
}

// TestListEmailsByCursor_IncludeTotal verifies the include_total=true path
// runs a real COUNT and surfaces it on the response (covers code path not
// exercised by TwoPageFlow which uses the default include_total=false).
func TestListEmailsByCursor_IncludeTotal(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	for i := 0; i < 3; i++ {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       []*pb.EmailAddress{{Email: fmt.Sprintf("u%d@x.com", i)}},
			Subject:  "T",
			Body:     "B",
			Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	// First page, page_size=2 → hasNext=true, must run COUNT.
	first, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:        pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize:     2,
		IncludeTotal: true,
	})
	require.NoError(t, err)
	assert.Len(t, first.Records, 2)
	assert.Equal(t, int32(3), first.Total, "include_total=true must run COUNT")
	assert.NotEmpty(t, first.NextPageToken)

	// Page with all records in one go → hasNext=false, must short-circuit (total = len).
	all, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{
		Scene:        pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		PageSize:     10,
		IncludeTotal: true,
	})
	require.NoError(t, err)
	assert.Len(t, all.Records, 3)
	assert.Equal(t, int32(3), all.Total, "short-circuit: first page + no next → total = len(records)")
	assert.Empty(t, all.NextPageToken)
}

func TestSendEmail_PersistenceDisabled_SkipsDB(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	resp, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)

	// DB must be empty: persistence disabled.
	_, err = svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: resp.Id})
	require.Error(t, err, "GetEmail must fail (persistence disabled returns ErrPersistenceDisabled)")
}

// TestSendEmail_PersistenceDisabled_IdempotencyStillWorks verifies that
// Redis idempotency is independent of the persistence toggle: even with
// persistence off, the same idempotency_key is deduped via Redis.
func TestSendEmail_PersistenceDisabled_IdempotencyStillWorks(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	_, err = svc.SendEmail(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 1, provider.calls, "provider must be called once (Redis dedup works even with persistence off)")
}

func TestGetEmail_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: 1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()),
		"err must wrap ErrPersistenceDisabled, got: %v", err)
}

func TestListEmails_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()))
}

func TestListEmailsByCursor_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()))
}

func TestGetEmailStats_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.GetEmailStats(context.Background(), &pb.GetEmailStatsRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled.New()))
}

func TestSendEmail_Idempotent_SecondCallReturnsCached(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	resp1, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp1.Status)

	resp2, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, resp1.Id, resp2.Id, "second call must return cached ID")
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp2.Status)
	assert.Equal(t, 1, provider.calls, "provider must be called once (second served from cache)")
}

func TestSendEmail_IdempotencyConflict_OnInFlight(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	// Manually plant a PENDING marker to simulate in-flight.
	require.NoError(t, svc.idem.Complete(context.Background(), "email", "user:42", "in-flight-key", []byte("PENDING")))

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "in-flight-key",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrIdempotencyConflict.New()),
		"err must wrap ErrIdempotencyConflict, got: %v", err)
}

func TestSendEmail_Failure_NotCached_ReleasesReservation(t *testing.T) {
	// mockEmailProvider.Send returns m.err and nil result, triggering the
	// pre-send failure path (Release + return ErrMessageSendFailed).
	//
	// Note on the Complete-then-Release race: the post-send failure path
	// (result != nil, sendErr != nil) had a window where a fake SENT
	// payload was written by Complete before Release deleted it. That race
	// is verified by code inspection of SendEmail (post-send failure check
	// runs BEFORE Complete) rather than by a deterministic test — the
	// window is too narrow to assert via timing without flakiness.
	provider := &mockEmailProvider{name: "mock", err: errors.New("vendor rejected")}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "fail-once",
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err, "first call must fail")

	// After Release, a direct Reserve on the same key must succeed (key gone).
	acquired, payload, err := svc.idem.Reserve(context.Background(), "email", "user:42", "fail-once")
	require.NoError(t, err)
	assert.True(t, acquired, "Reserve after failed send must acquire (key was Released)")
	assert.Nil(t, payload, "no cached payload (failure was not cached)")
}

// TestSendEmail_IdempotencyReleased_OnSenderForFailure verifies the
// post-Reserve / pre-send error path: when SenderFor rejects an unknown
// account, the reservation must be released so the caller can retry the key.
// Regression: previously this path returned without Release, leaving the key
// PENDING for the full TTL and forcing 409 on every retry.
func TestSendEmail_IdempotencyReleased_OnSenderForFailure(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	req := &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		Vendor:         pb.EmailVendor_EMAIL_VENDOR_ALIYUN,
		Account:        "nonexistent", // not in registry → SenderFor fails
		IdempotencyKey: "senderfor-fail",
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err, "SenderFor must fail on unknown account")

	acquired, payload, reserveErr := svc.idem.Reserve(context.Background(), "email", "user:42", "senderfor-fail")
	require.NoError(t, reserveErr)
	assert.True(t, acquired, "Reserve after SenderFor failure must acquire (key was Released)")
	assert.Nil(t, payload)
}

// TestSendEmail_IdempotencyReleased_OnGIDFailure verifies the post-Reserve /
// post-SenderFor error path: when gid.NextID fails, the reservation must be
// released. Regression: previously this path returned without Release.
func TestSendEmail_IdempotencyReleased_OnGIDFailure(t *testing.T) {
	db := setupEmailTestDB(t)
	svc := New(
		db,
		newTestIdempotencyChecker(t),
		failingGID{},
		email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
			pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"p0": &mockEmailProvider{name: "mock"}},
		}),
		true,
		newTestAttachmentConfig(), newTestHTTPClient(),
	)

	req := &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "gid-fail",
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err, "gid.NextID must fail")

	acquired, payload, reserveErr := svc.idem.Reserve(context.Background(), "email", "user:42", "gid-fail")
	require.NoError(t, reserveErr)
	assert.True(t, acquired, "Reserve after gid failure must acquire (key was Released)")
	assert.Nil(t, payload)
}

// TestSendEmail_BodyPreserved_VerifyNoLinkInjection confirms that
// processAttachments no longer modifies htmlBody — callers own their body
// layout entirely. Regression for the removed LINK-kind auto-render.
func TestSendEmail_BodyPreserved_VerifyNoLinkInjection(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := newTestIdempotencyChecker(t)
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, true,
		newTestAttachmentConfig(), newTestHTTPClient())

	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "with attachment",
		Body:     "see attachment",
		HtmlBody: "<p>base</p>",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "test-svc",
		Attachments: []*pb.EmailAttachment{
			{Filename: "r.pdf", Content: []byte("PDF-CONTENT"), MimeType: "application/pdf"},
		},
	}
	resp, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// htmlBody must be exactly what the caller supplied — no service-side
	// injection of <a>/<img>/<table> for attachments.
	record, err := dal.GetEmailRecord(context.Background(), db, resp.Id)
	require.NoError(t, err)
	assert.Equal(t, "<p>base</p>", record.HTMLBody)

	// Attachment metadata row is still written to the side table.
	atts, err := dal.ListEmailRecordAttachments(context.Background(), db, resp.Id)
	require.NoError(t, err)
	require.Len(t, atts, 1)
	assert.Equal(t, "r.pdf", atts[0].Filename)
	assert.Equal(t, "", atts[0].URL, "inline-content attachments have no URL")
}

// TestSendEmail_MIMEAttachment_FetchFailure verifies that when an attachment
// URL fetch fails, SendEmail returns ErrAttachmentFetchFailed and no record
// is persisted (the error short-circuits before persistEmailRecord).
func TestSendEmail_MIMEAttachment_FetchFailure(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := newTestIdempotencyChecker(t)
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, true,
		newTestAttachmentConfig(), newTestHTTPClient())

	// Point at a closed port to force fetch failure.
	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "with attachment",
		Body:     "see attached",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "test-svc",
		Attachments: []*pb.EmailAttachment{
			{Filename: "r.pdf", Url: "http://127.0.0.1:1/r.pdf"},
		},
	}
	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err)

	// Fetch failure short-circuits before persistence: the error must be
	// ErrAttachmentFetchFailed (send returned before persistEmailRecord ran).
	assert.True(t, errors.Is(err, xcodes.ErrAttachmentFetchFailed.New()),
		"expected ErrAttachmentFetchFailed, got %v", err)
}

func TestGetEmail_returnsAttachments(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := newTestIdempotencyChecker(t)
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, true,
		newTestAttachmentConfig(), newTestHTTPClient())

	// One attachment via inline content, one via URL fetch (httptest server).
	mimeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "b-content")
	}))
	defer mimeSrv.Close()

	sendReq := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "x",
		Body:     "y",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "svc",
		Attachments: []*pb.EmailAttachment{
			{Filename: "a.txt", Content: []byte("a-content")},
			{Filename: "b.txt", Url: mimeSrv.URL + "/b.txt"},
		},
	}
	resp, err := svc.SendEmail(context.Background(), sendReq)
	require.NoError(t, err)

	got, err := svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: resp.Id})
	require.NoError(t, err)
	require.Len(t, got.GetAttachments(), 2)
	assert.Equal(t, "a.txt", got.GetAttachments()[0].GetFilename())
	assert.Equal(t, "b.txt", got.GetAttachments()[1].GetFilename())
	assert.Equal(t, "", got.GetAttachments()[0].GetUrl(), "inline-content attachment has no url")
	assert.Equal(t, mimeSrv.URL+"/b.txt", got.GetAttachments()[1].GetUrl())
}

// TestSendEmail_MIMEAttachment_PersistedToSideTable is the MIME-only happy
// path: a single MIME attachment is downloaded and the send succeeds, with
// the attachment metadata written to the side table (regression for the
// MIME persist path which was previously only covered mixed with LINK).
func TestSendEmail_MIMEAttachment_PersistedToSideTable(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := newTestIdempotencyChecker(t)
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})

	// Serve the MIME attachment bytes.
	mimeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "PDF-CONTENT")
	}))
	defer mimeSrv.Close()

	svc := New(db, idem, getTestGID(t), reg, true,
		newTestAttachmentConfig(), newTestHTTPClient())

	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "x",
		Body:     "y",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "svc",
		Attachments: []*pb.EmailAttachment{
			{Filename: "r.pdf", Url: mimeSrv.URL, MimeType: "application/pdf"},
		},
	}
	resp, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)

	// Side table must contain the attachment row.
	atts, err := dal.ListEmailRecordAttachments(context.Background(), db, resp.Id)
	require.NoError(t, err)
	require.Len(t, atts, 1)
	assert.Equal(t, "r.pdf", atts[0].Filename)
	assert.Equal(t, "application/pdf", atts[0].MimeType)
}

// --- attachment validation ---

func TestValidateAttachments_emptyOK(t *testing.T) {
	require.NoError(t, validateAttachments(nil))
}

func TestValidateAttachments_urlOrContentRequired(t *testing.T) {
	// Neither url nor content set — must error.
	err := validateAttachments([]*pb.EmailAttachment{
		{Filename: "f.txt"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of url or content")

	// Both set — also an error (XOR violation).
	err = validateAttachments([]*pb.EmailAttachment{
		{Filename: "f.txt", Url: "https://x.com/f.txt", Content: []byte("x")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of url or content")
}

func TestValidateAttachments_filenameRequired(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Url: "https://x.com/f.txt"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "filename")
}

func TestValidateAttachments_validPasses(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Filename: "f.txt", Url: "https://x.com/f.txt"},
		{Filename: "g.txt", Content: []byte("g"), Inline: true},
	})
	require.NoError(t, err)
}

func TestFetchAttachmentBytes_success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello bytes")
	}))
	defer srv.Close()

	svc := &Service{httpClient: newTestHTTPClient(), attachment: &config.AttachmentConfig{MaxBytes: 1024, MaxInlineBytes: 2 * 1024 * 1024, MaxTotalInlineBytes: 5 * 1024 * 1024}}
	bytes, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 0)
	require.NoError(t, err)
	assert.Equal(t, []byte("hello bytes"), bytes)
}

func TestFetchAttachmentBytes_exceedsMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()

	svc := &Service{httpClient: newTestHTTPClient(), attachment: &config.AttachmentConfig{MaxBytes: 10, MaxInlineBytes: 2 * 1024 * 1024, MaxTotalInlineBytes: 5 * 1024 * 1024}}
	_, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "size limit")
}

func TestFetchAttachmentBytes_httpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	svc := &Service{httpClient: newTestHTTPClient(), attachment: &config.AttachmentConfig{MaxBytes: 1024, MaxInlineBytes: 2 * 1024 * 1024, MaxTotalInlineBytes: 5 * 1024 * 1024}}
	_, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404")
}

func TestProcessAttachments_PreservesSizeErrorCategory(t *testing.T) {
	// Server returns a body larger than maxBytes; fetchAttachmentBytes returns
	// ErrAttachmentTooLarge (413). processAttachments must NOT re-wrap it as
	// ErrAttachmentFetchFailed (502).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()

	svc := &Service{httpClient: newTestHTTPClient(), attachment: &config.AttachmentConfig{MaxBytes: 10, MaxInlineBytes: 2 * 1024 * 1024, MaxTotalInlineBytes: 5 * 1024 * 1024}}
	_, err := svc.processAttachments(context.Background(),
		[]*pb.EmailAttachment{
			{Filename: "big.bin", Url: srv.URL},
		})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrAttachmentTooLarge.New()),
		"size violation must remain ErrAttachmentTooLarge, got %v", err)
}

func TestFetchAttachmentBytes_UnconfiguredMaxBytes(t *testing.T) {
	svc := &Service{httpClient: newTestHTTPClient(), attachment: &config.AttachmentConfig{MaxBytes: 0, MaxInlineBytes: 2 * 1024 * 1024, MaxTotalInlineBytes: 5 * 1024 * 1024}}
	_, err := svc.fetchAttachmentBytes(context.Background(), "https://example.com/x", 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrInternal.New()),
		"unconfigured maxBytes is a server config error (500), got %v", err)
}

// TestValidateAttachments_rejectsNonHTTPScheme verifies that file://, ftp://,
// gopher:// (and any non-http(s) scheme) are rejected at validation time to
// mitigate SSRF.
func TestValidateAttachments_rejectsNonHTTPScheme(t *testing.T) {
	for _, scheme := range []string{"file:///etc/passwd", "ftp://x/y", "gopher://x/y"} {
		err := validateAttachments([]*pb.EmailAttachment{
			{Filename: "f", Url: scheme},
		})
		require.Error(t, err, "should reject scheme %q", scheme)
		assert.Contains(t, err.Error(), "scheme")
	}
}

// TestFetchAttachmentBytes_DoesNotFollowRedirects verifies the production
// http.Client config (CheckRedirect=ErrUseLastResponse) prevents redirect
// following, mitigating SSRF via redirect to internal addresses.
func TestFetchAttachmentBytes_DoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "target-body")
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	svc := &Service{
		httpClient: newTestHTTPClient(),
		attachment: &config.AttachmentConfig{MaxBytes: 1024},
	}
	// 302 response is returned as-is; body is the redirect HTML, not target-body.
	// Note: fetchAttachmentBytes treats non-2xx as fetch failure, so this errors,
	// but the important assertion is that target-body is never fetched.
	_, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 0)
	require.Error(t, err, "302 must be treated as non-2xx fetch failure")
	assert.NotContains(t, err.Error(), "target-body")
}

// TestFetchAttachmentBytes_SizeHintExceedsMax verifies that a size_hint
// greater than maxBytes is rejected without hitting the network (the check
// runs before the body read, but after the request is issued — the
// size_hint branch is a defense-in-depth bound on callers that know the
// size upfront).
func TestFetchAttachmentBytes_SizeHintExceedsMax(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "x")
	}))
	defer srv.Close()

	svc := &Service{
		httpClient: newTestHTTPClient(),
		attachment: &config.AttachmentConfig{MaxBytes: 100},
	}
	// sizeHint 200 > max 100 — must reject as ErrAttachmentTooLarge.
	_, err := svc.fetchAttachmentBytes(context.Background(), srv.URL, 200)
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrAttachmentTooLarge.New()),
		"size_hint exceeding max must be ErrAttachmentTooLarge, got %v", err)
}

// TestValidateAttachments_ReturnsErrInvalidAttachment verifies that
// validateAttachments returns an error that errors.Is-matches
// xcodes.ErrInvalidAttachment (previously it returned a bare fmt.Errorf,
// making the declared ErrInvalidAttachment unused and un-checkable).
func TestValidateAttachments_ReturnsErrInvalidAttachment(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Filename: "f", Content: []byte("x"), Url: "https://x.com/f"}, // both url and content set (XOR violation)
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrInvalidAttachment.New()),
		"expected ErrInvalidAttachment, got %v", err)
}

// TestValidateAttachments_SchemeViolation_ReturnsErrInvalidAttachment
// verifies the URL scheme check also wraps with ErrInvalidAttachment so
// callers can distinguish attachment-validation failures from generic
// bad-request errors.
func TestValidateAttachments_SchemeViolation_ReturnsErrInvalidAttachment(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Filename: "f", Url: "file:///etc/passwd"},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrInvalidAttachment.New()),
		"expected ErrInvalidAttachment for bad scheme, got %v", err)
}

// TestProcessAttachments_MultipleViaURL verifies processAttachments fetches
// multiple URL-based attachments in a single pass, returning one
// *email.Attachment per input with the correct bytes.
func TestProcessAttachments_MultipleViaURL(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ONE")
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "TWO")
	}))
	defer srv2.Close()
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "THREE")
	}))
	defer srv3.Close()

	svc := &Service{httpClient: newTestHTTPClient(), attachment: &config.AttachmentConfig{MaxBytes: 1024, MaxInlineBytes: 2 * 1024 * 1024, MaxTotalInlineBytes: 5 * 1024 * 1024}}
	atts := []*pb.EmailAttachment{
		{Filename: "1.bin", Url: srv1.URL},
		{Filename: "2.bin", Url: srv2.URL},
		{Filename: "3.bin", Url: srv3.URL},
	}
	mime, err := svc.processAttachments(context.Background(), atts)
	require.NoError(t, err)
	require.Len(t, mime, 3, "all 3 attachments must be fetched")
	assert.Equal(t, []byte("ONE"), mime[0].Content)
	assert.Equal(t, []byte("TWO"), mime[1].Content)
	assert.Equal(t, []byte("THREE"), mime[2].Content)
	assert.Equal(t, "1.bin", mime[0].Filename)
	assert.Equal(t, "3.bin", mime[2].Filename)
}

// TestProcessAttachments_InlineContentPreferredOverURL confirms that when
// both content and url are... no — they're XOR by validation. This test
// confirms the content path is taken (no HTTP call) when only content is set.
func TestProcessAttachments_InlineContent_NoFetchPerformed(t *testing.T) {
	// URL that would fail if hit — proves we never fetch when content is set.
	unreachable := "http://127.0.0.1:1/unreachable.bin"
	svc := &Service{
		httpClient: newTestHTTPClient(),
		attachment: &config.AttachmentConfig{
			MaxBytes:            1024,
			MaxInlineBytes:      1024,
			MaxTotalInlineBytes: 5 * 1024,
		},
	}
	atts := []*pb.EmailAttachment{
		{Filename: "inline.bin", Content: []byte("INLINE-BYTES"), Url: unreachable},
	}
	// Note: validateAttachments would reject this (XOR). Bypass it to test
	// processAttachments directly — it must prefer content over url.
	mime, err := svc.processAttachments(context.Background(), atts)
	require.NoError(t, err)
	require.Len(t, mime, 1)
	assert.Equal(t, []byte("INLINE-BYTES"), mime[0].Content, "inline content must be used as-is, no fetch")
}

// TestProcessAttachments_InlineContent_ExceedsSingleCap verifies the
// per-attachment inline cap returns ErrAttachmentTooLarge with a hint to
// switch to url.
func TestProcessAttachments_InlineContent_ExceedsSingleCap(t *testing.T) {
	svc := &Service{
		httpClient: newTestHTTPClient(),
		attachment: &config.AttachmentConfig{
			MaxBytes:            1024,
			MaxInlineBytes:      4,
			MaxTotalInlineBytes: 5 * 1024,
		},
	}
	atts := []*pb.EmailAttachment{
		{Filename: "big.bin", Content: []byte("abcdefgh")}, // 8 bytes > 4-byte cap
	}
	_, err := svc.processAttachments(context.Background(), atts)
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrAttachmentTooLarge.New()),
		"single-attachment inline cap exceeded must be ErrAttachmentTooLarge, got %v", err)
	assert.Contains(t, err.Error(), "use url instead")
}

// TestProcessAttachments_InlineContent_ExceedsRequestTotalCap verifies the
// per-request sum cap returns ErrAttachmentTooLarge with a hint to spread
// attachments across url.
func TestProcessAttachments_InlineContent_ExceedsRequestTotalCap(t *testing.T) {
	svc := &Service{
		httpClient: newTestHTTPClient(),
		attachment: &config.AttachmentConfig{
			MaxBytes:            1024,
			MaxInlineBytes:      8,
			MaxTotalInlineBytes: 10, // tight total
		},
	}
	atts := []*pb.EmailAttachment{
		{Filename: "a.bin", Content: []byte("AAAA")},     // 4 bytes
		{Filename: "b.bin", Content: []byte("BBBBBBBB")}, // 8 bytes — total 12 > 10
	}
	_, err := svc.processAttachments(context.Background(), atts)
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrAttachmentTooLarge.New()),
		"per-request inline cap exceeded must be ErrAttachmentTooLarge, got %v", err)
	assert.Contains(t, err.Error(), "per-request limit")
}

// --- address conversion ---

func TestPbToAddr(t *testing.T) {
	require.Nil(t, pbToAddr(nil))
	require.Equal(t, &email.Address{Email: "alice@x.com"}, pbToAddr(&pb.EmailAddress{Email: "alice@x.com"}))
	require.Equal(t,
		&email.Address{Email: "alice@x.com", DisplayName: "Alice"},
		pbToAddr(&pb.EmailAddress{Email: "alice@x.com", DisplayName: "Alice"}))
}

func TestPbToAddrs(t *testing.T) {
	require.Equal(t, []*email.Address{}, pbToAddrs(nil),
		"nil input should yield non-nil empty slice")

	in := []*pb.EmailAddress{
		{Email: "alice@x.com", DisplayName: "Alice"},
		nil, // skipped
		{Email: "bob@x.com"},
	}
	require.Equal(t,
		[]*email.Address{
			{Email: "alice@x.com", DisplayName: "Alice"},
			{Email: "bob@x.com"},
		},
		pbToAddrs(in))
}

func TestBareEmailFromAddr(t *testing.T) {
	require.Equal(t, "", bareEmailFromAddr(nil))
	require.Equal(t, "alice@x.com",
		bareEmailFromAddr(&pb.EmailAddress{Email: "alice@x.com", DisplayName: "Alice"}))
}

func TestBareEmailsFromAddrs(t *testing.T) {
	require.Equal(t, []string{}, bareEmailsFromAddrs(nil))

	in := []*pb.EmailAddress{
		{Email: "alice@x.com", DisplayName: "Alice"},
		nil,
		{Email: "bob@x.com"},
		{Email: ""},
	}
	require.Equal(t, []string{"alice@x.com", "bob@x.com"}, bareEmailsFromAddrs(in))
}

func TestAddrFromBareEmail(t *testing.T) {
	require.Nil(t, addrFromBareEmail(""))
	require.Equal(t, &pb.EmailAddress{Email: "alice@x.com"}, addrFromBareEmail("alice@x.com"))
}

func TestAddrsFromBareEmails(t *testing.T) {
	require.Equal(t, []*pb.EmailAddress{}, addrsFromBareEmails(nil))

	in := []string{"alice@x.com", "", "bob@x.com"}
	require.Equal(t,
		[]*pb.EmailAddress{{Email: "alice@x.com"}, {Email: "bob@x.com"}},
		addrsFromBareEmails(in))
}

// --- validateSendEmailRequest (request-level defense-in-depth) ---

func TestValidateSendEmailRequest_AllValid(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Subject:  "Test",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	}
	assert.NoError(t, validateSendEmailRequest(req))
}

func TestValidateSendEmailRequest_VendorWithoutAccount(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Vendor:   pb.EmailVendor_EMAIL_VENDOR_ALIYUN,
		// Account empty
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vendor and account")
}

func TestValidateSendEmailRequest_AccountWithoutVendor(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Account:  "primary",
		// Vendor empty
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vendor and account")
}

func TestValidateSendEmailRequest_MissingScene(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@example.com"}},
		SenderId: "user:42",
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scene")
}

func TestValidateSendEmailRequest_MissingSenderID(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:    []*pb.EmailAddress{{Email: "user@example.com"}},
		Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "sender_id")
}

func TestValidateSendEmailRequest_IdempotencyKeyTooLong(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:             []*pb.EmailAddress{{Email: "user@example.com"}},
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: strings.Repeat("a", 65),
	}
	err := validateSendEmailRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency_key")
}

func TestValidateSendEmailRequest_AttachmentsURLOrContentRequired(t *testing.T) {
	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "x",
		Body:     "y",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "svc",
		Attachments: []*pb.EmailAttachment{
			{Filename: "f.txt"}, // neither url nor content
		},
	}
	err := validateSendEmailRequest(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one of url or content")
}
