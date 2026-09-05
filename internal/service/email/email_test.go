package email

import (
	gidv1 "github.com/servekit/gid-service/gen/gid/v1"

	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/servekit/message-service/internal/idempotency"
	"github.com/servekit/message-service/internal/provider/email"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
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
	name    string
	err     error
	calls   int
	lastMsg *email.Message
}

func (m *mockEmailProvider) Vendor() pb.EmailVendor { return pb.EmailVendor_EMAIL_VENDOR_ALIYUN }
func (m *mockEmailProvider) Account() string        { return m.name }
func (m *mockEmailProvider) Send(_ context.Context, msg *email.Message) error {
	m.calls++
	m.lastMsg = msg
	return m.err
}

// failingGID is a gidservice.Service that always errors. Used to exercise
// the gid.NextID error path in SendEmail / SendSMS.
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

// newTestAttachmentConfig returns an AttachmentConfig for the common test
// path: 2MB single inline, 5MB per-request total. Tests that need other
// values construct their own &config.AttachmentConfig{...}.
func newTestAttachmentConfig() *config.AttachmentConfig {
	return &config.AttachmentConfig{
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
		newTestAttachmentConfig(),
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
		newTestAttachmentConfig())

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

// TestSendEmail_URLAttachment_ReferenceNotFetched verifies the pure-reference
// semantics of url-only attachments: the service must NOT fetch the url — a
// send pointing at a closed port still succeeds, no MIME part is produced,
// and the metadata row lands in the side table.
func TestSendEmail_URLAttachment_ReferenceNotFetched(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := newTestIdempotencyChecker(t)
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, true,
		newTestAttachmentConfig())

	// Closed port: under the old fetch semantics this failed the whole send.
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
	resp, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err, "url reference must not be fetched; send proceeds")

	// No MIME part reached the provider.
	require.NotNil(t, provider.lastMsg)
	assert.Empty(t, provider.lastMsg.Attachments, "url-only attachment must not become a MIME part")

	// Metadata is persisted for record queries.
	atts, err := dal.ListEmailRecordAttachments(context.Background(), db, resp.Id)
	require.NoError(t, err)
	require.Len(t, atts, 1)
	assert.Equal(t, "r.pdf", atts[0].Filename)
	assert.Equal(t, "http://127.0.0.1:1/r.pdf", atts[0].URL)
}

func TestGetEmail_returnsAttachments(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := newTestIdempotencyChecker(t)
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})
	svc := New(db, idem, getTestGID(t), reg, true,
		newTestAttachmentConfig())

	// One inline-content attachment, one url reference.
	sendReq := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "x",
		Body:     "y",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "svc",
		Attachments: []*pb.EmailAttachment{
			{Filename: "a.txt", Content: []byte("a-content")},
			{Filename: "b.txt", Url: "https://oss.example.com/b.txt"},
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
	assert.Equal(t, "https://oss.example.com/b.txt", got.GetAttachments()[1].GetUrl())
}

// TestSendEmail_URLAttachment_PersistedToSideTable is the url-only happy
// path: the reference metadata is written to the side table while the
// provider receives no MIME part for it.
func TestSendEmail_URLAttachment_PersistedToSideTable(t *testing.T) {
	db := setupEmailTestDB(t)
	idem := newTestIdempotencyChecker(t)
	provider := &mockEmailProvider{name: "primary"}
	reg := email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": provider},
	})

	svc := New(db, idem, getTestGID(t), reg, true,
		newTestAttachmentConfig())

	req := &pb.SendEmailRequest{
		To:       []*pb.EmailAddress{{Email: "user@test.com"}},
		Subject:  "x",
		Body:     "y",
		Scene:    pb.EmailScene_EMAIL_SCENE_NOTIFICATION,
		SenderId: "svc",
		Attachments: []*pb.EmailAttachment{
			{Filename: "r.pdf", Url: "https://oss.example.com/r.pdf", MimeType: "application/pdf"},
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
	assert.Empty(t, provider.lastMsg.Attachments)
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
	assert.Contains(t, err.Error(), "at least one of url or content")

	// Both set — allowed: content embeds as MIME, url is kept as record metadata.
	err = validateAttachments([]*pb.EmailAttachment{
		{Filename: "f.txt", Url: "https://x.com/f.txt", Content: []byte("x")},
	})
	require.NoError(t, err)
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
func TestValidateAttachments_rejectsNonHTTPScheme(t *testing.T) {
	for _, scheme := range []string{"file:///etc/passwd", "ftp://x/y", "gopher://x/y"} {
		err := validateAttachments([]*pb.EmailAttachment{
			{Filename: "f", Url: scheme},
		})
		require.Error(t, err, "should reject scheme %q", scheme)
		assert.Contains(t, err.Error(), "scheme")
	}
}
func TestValidateAttachments_ReturnsErrInvalidAttachment(t *testing.T) {
	err := validateAttachments([]*pb.EmailAttachment{
		{Filename: "f"}, // neither url nor content set
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

// TestProcessAttachments_MultipleURLReferences verifies url-only attachments
// produce no MIME parts at all — they are pure references now.
func TestProcessAttachments_MultipleURLReferences(t *testing.T) {
	svc := &Service{attachment: &config.AttachmentConfig{MaxInlineBytes: 2 * 1024 * 1024, MaxTotalInlineBytes: 5 * 1024 * 1024}}
	atts := []*pb.EmailAttachment{
		{Filename: "1.bin", Url: "http://127.0.0.1:1/1.bin"},
		{Filename: "2.bin", Url: "http://127.0.0.1:1/2.bin"},
		{Filename: "3.bin", Url: "http://127.0.0.1:1/3.bin"},
	}
	mime, err := svc.processAttachments(atts)
	require.NoError(t, err)
	assert.Empty(t, mime, "url-only attachments must not become MIME parts")
}

// TestProcessAttachments_ContentPreferredOverURL confirms the content path
// is taken when both content and url are set — url is record metadata only.
func TestProcessAttachments_ContentPreferredOverURL(t *testing.T) {
	unreachable := "http://127.0.0.1:1/unreachable.bin"
	svc := &Service{
		attachment: &config.AttachmentConfig{
			MaxInlineBytes:      1024,
			MaxTotalInlineBytes: 5 * 1024,
		},
	}
	atts := []*pb.EmailAttachment{
		{Filename: "inline.bin", Content: []byte("INLINE-BYTES"), Url: unreachable},
	}
	mime, err := svc.processAttachments(atts)
	require.NoError(t, err)
	require.Len(t, mime, 1)
	assert.Equal(t, []byte("INLINE-BYTES"), mime[0].Content, "inline content must be used as-is")
}

// TestProcessAttachments_InlineContent_ExceedsSingleCap verifies the
// per-attachment inline cap returns ErrAttachmentTooLarge with a hint to
// switch to url.
func TestProcessAttachments_InlineContent_ExceedsSingleCap(t *testing.T) {
	svc := &Service{
		attachment: &config.AttachmentConfig{
			MaxInlineBytes:      4,
			MaxTotalInlineBytes: 5 * 1024,
		},
	}
	atts := []*pb.EmailAttachment{
		{Filename: "big.bin", Content: []byte("abcdefgh")}, // 8 bytes > 4-byte cap
	}
	_, err := svc.processAttachments(atts)
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrAttachmentTooLarge.New()),
		"single-attachment inline cap exceeded must be ErrAttachmentTooLarge, got %v", err)
	assert.Contains(t, err.Error(), "url reference instead")
}

// TestProcessAttachments_InlineContent_ExceedsRequestTotalCap verifies the
// per-request sum cap returns ErrAttachmentTooLarge with a hint to spread
// attachments across url.
func TestProcessAttachments_InlineContent_ExceedsRequestTotalCap(t *testing.T) {
	svc := &Service{
		attachment: &config.AttachmentConfig{
			MaxInlineBytes:      8,
			MaxTotalInlineBytes: 10, // tight total
		},
	}
	atts := []*pb.EmailAttachment{
		{Filename: "a.bin", Content: []byte("AAAA")},     // 4 bytes
		{Filename: "b.bin", Content: []byte("BBBBBBBB")}, // 8 bytes — total 12 > 10
	}
	_, err := svc.processAttachments(atts)
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
	assert.Contains(t, err.Error(), "at least one of url or content")
}
