package dal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/servekit/message-service/internal/store/models"

	pb "github.com/servekit/message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupEmailDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	err := db.AutoMigrate(&models.MessageEmailRecord{})
	require.NoError(t, err, "auto-migrate should succeed")
	return db
}

func newTestEmailRecord(status int32, scene int32, target string) *models.MessageEmailRecord {
	return &models.MessageEmailRecord{
		ID:       time.Now().UnixNano(),
		Vendor:   int32(pb.EmailVendor_EMAIL_VENDOR_ALIYUN),
		Scene:    scene,
		Status:   status,
		Target:   target,
		Subject:  "Test Subject",
		Content:  "Test content body",
		SenderID: "user:42",
		Attempts: 1,
	}
}

// newTestEmailRecordAt is newTestEmailRecord with an explicit CreatedAt,
// so multiple records can share the same timestamp to exercise the
// (created_at, id) tiebreaker.
func newTestEmailRecordAt(id int64, createdAt time.Time, target string) *models.MessageEmailRecord {
	r := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		target,
	)
	r.ID = id
	r.CreatedAt = createdAt
	r.UpdatedAt = createdAt
	return r
}

func TestCreateEmailRecord(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	record := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"user@example.com")
	require.NoError(t, CreateEmailRecord(ctx, db, record))

	found, err := GetEmailRecord(ctx, db, record.ID)
	require.NoError(t, err)
	assert.Equal(t, record.ID, found.ID)
	assert.Equal(t, int32(pb.EmailVendor_EMAIL_VENDOR_ALIYUN), found.Vendor)
	assert.Equal(t, int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE), found.Scene)
	assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), found.Status)
	assert.Equal(t, "user@example.com", found.Target)
	assert.Equal(t, "user:42", found.SenderID)
	assert.Equal(t, "Test Subject", found.Subject)
	assert.Equal(t, 1, found.Attempts)
	assert.False(t, found.CreatedAt.IsZero())
	assert.False(t, found.UpdatedAt.IsZero())
}

func TestGetEmailRecord_NotFound(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	_, err := GetEmailRecord(ctx, db, 99999999)
	assert.Error(t, err)
}

func TestListEmailRecords_ByScene(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com")))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_REGISTER),
		"c@d.com")))

	result, err := ListEmailRecords(ctx, db, EmailListFilter{
		Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Total)
	require.Len(t, result.List, 1)
	assert.Equal(t, "a@b.com", result.List[0].Target)
}

func TestListEmailRecords_ByVendor(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com")))
	// Second record uses a different vendor so the ByVendor filter below
	// actually narrows the result set (otherwise both match ALIYUN).
	other := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"c@d.com")
	other.Vendor = int32(pb.EmailVendor_EMAIL_VENDOR_TENCENT)
	require.NoError(t, CreateEmailRecord(ctx, db, other))

	result, err := ListEmailRecords(ctx, db, EmailListFilter{
		Vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Total)
	require.Len(t, result.List, 1)
}

func TestListEmailRecords_PageSizeClamped(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
			int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
			int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
			"user@example.com",
		)))
	}

	result, err := ListEmailRecords(ctx, db, EmailListFilter{}, dbx.PageParams{
		Page:     1,
		PageSize: 1000,
		Count:    true,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(result.List), 100)
	assert.Len(t, result.List, 5)
	assert.Equal(t, 1, result.TotalPages, "5 records / page_size=100 → 1 page")
}

func TestCountEmailStats(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com")))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"b@c.com")))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_FAILED),
		int32(pb.EmailScene_EMAIL_SCENE_REGISTER),
		"d@e.com")))

	stats, err := CountEmailStats(ctx, db, EmailStatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Total)
	assert.Equal(t, int64(2), stats.Sent)
	assert.Equal(t, int64(1), stats.Failed)
	assert.InDelta(t, 66.67, stats.SuccessRate, 0.1)
}

func TestCountEmailStats_EmptyReturnsMinusOne(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	stats, err := CountEmailStats(ctx, db, EmailStatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), stats.Total)
	assert.Equal(t, float64(-1), stats.SuccessRate,
		"empty filter should return -1 to distinguish 'no data' from '0% success'")
}

func TestListEmailVendorStats(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com")))
	// Second record uses a different vendor so GROUP BY vendor returns 2
	// rows (otherwise both collapse into one ALIYUN bucket).
	other := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_FAILED),
		int32(pb.EmailScene_EMAIL_SCENE_REGISTER),
		"c@d.com")
	other.Vendor = int32(pb.EmailVendor_EMAIL_VENDOR_TENCENT)
	require.NoError(t, CreateEmailRecord(ctx, db, other))

	stats, err := ListEmailVendorStats(ctx, db, EmailStatsFilter{})
	require.NoError(t, err)
	require.Len(t, stats, 2)
}

// TestListEmailRecords_Tiebreaker_StablePagination verifies that rows sharing
// the same created_at are paged without duplication or loss. Pre-fix the
// query only had ORDER BY created_at; under tied values PG returns rows in
// an undefined order, so different OFFSETs could skip or repeat rows.
func TestListEmailRecords_Tiebreaker_StablePagination(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	// 5 records all sharing the same created_at.
	ts := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, ts, fmt.Sprintf("u%d@x.com", i))))
	}

	// Page through all 5 records with page_size = 2.
	seen := make(map[int64]struct{})
	for page := 1; page <= 5; page++ {
		result, err := ListEmailRecords(ctx, db, EmailListFilter{}, dbx.PageParams{
			Page:     page,
			PageSize: 2,
			Count:    true,
		})
		require.NoError(t, err)
		if len(result.List) == 0 {
			break
		}
		for _, r := range result.List {
			// Detect duplication across pages.
			_, ok := seen[r.ID]
			assert.False(t, ok, "id %d appeared on multiple pages", r.ID)
			seen[r.ID] = struct{}{}
		}
	}

	// Every record was seen exactly once.
	assert.Len(t, seen, 5, "expected to page through all 5 records without loss or duplication")
}

// TestListEmailRecords_ASC_Ordering verifies that SortDirection_ASC returns
// rows oldest-first, mirroring the default DESC behavior.
func TestListEmailRecords_ASC_Ordering(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("u%d@x.com", i))))
	}

	result, err := ListEmailRecords(ctx, db, EmailListFilter{
		SortDirection: pb.SortDirection_SORT_DIRECTION_ASC,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	require.Len(t, result.List, 3)

	// Oldest first: id=1 has the earliest created_at.
	assert.Equal(t, int64(1), result.List[0].ID)
	assert.Equal(t, int64(2), result.List[1].ID)
	assert.Equal(t, int64(3), result.List[2].ID)
}

// TestListEmailsByCursor_FullSweep pages through every record with cursor
// mode, including the tiebreaker case (same created_at), and verifies no
// loss or duplication.
func TestListEmailsByCursor_FullSweep(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, ts, fmt.Sprintf("u%d@x.com", i))))
	}

	seen := make(map[int64]struct{})
	pg := dbx.Pagination{PageSize: 2}
	var afterCreatedAt time.Time
	for i := 0; i < 10; i++ { // upper bound to prevent infinite loop on bug
		records, err := ListEmailsByCursor(ctx, db, EmailListFilter{}, pg, afterCreatedAt)
		require.NoError(t, err)
		if len(records) == 0 {
			break
		}
		for _, r := range records {
			_, ok := seen[r.ID]
			assert.False(t, ok, "id %d duplicated across cursor pages", r.ID)
			seen[r.ID] = struct{}{}
		}
		last := records[len(records)-1]
		pg.AfterID = last.ID
		afterCreatedAt = last.CreatedAt
		if len(records) < pg.PageSize {
			break
		}
	}
	assert.Len(t, seen, 5, "cursor sweep should cover all 5 records")
}

// TestListEmailsByCursor_ASC verifies the ASC branch of the cursor advance.
func TestListEmailsByCursor_ASC(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecordAt(i, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("u%d@x.com", i))))
	}

	filter := EmailListFilter{SortDirection: pb.SortDirection_SORT_DIRECTION_ASC}
	pg := dbx.Pagination{PageSize: 10}
	records, err := ListEmailsByCursor(ctx, db, filter, pg, time.Time{})
	require.NoError(t, err)
	require.Len(t, records, 3)
	// Oldest first.
	assert.Equal(t, int64(1), records[0].ID)
	assert.Equal(t, int64(3), records[2].ID)
}

func TestListEmailSenderIDs_Distinct(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	r1 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_NOTIFICATION), "user@example.com")
	r1.SenderID = "user-service"
	require.NoError(t, CreateEmailRecord(ctx, db, r1))

	r2 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_NOTIFICATION), "admin@example.com")
	r2.SenderID = "user-service" // same sender, different target
	require.NoError(t, CreateEmailRecord(ctx, db, r2))

	r3 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_NOTIFICATION), "biz@example.com")
	r3.SenderID = "pay-service"
	require.NoError(t, CreateEmailRecord(ctx, db, r3))

	senders, err := ListEmailSenderIDs(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, []string{"pay-service", "user-service"}, senders)
}
