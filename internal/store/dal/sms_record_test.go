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

func setupSMSDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	err := db.AutoMigrate(&models.MessageSMSRecord{})
	require.NoError(t, err, "auto-migrate should succeed")
	return db
}

func newTestSMSRecord(status int32, scene int32, regionCode, phone string) *models.MessageSMSRecord {
	return &models.MessageSMSRecord{
		ID:         time.Now().UnixNano(),
		Vendor:     int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
		Scene:      scene,
		Status:     status,
		RegionCode: regionCode,
		Phone:      phone,
		Content:    "Your code: 1234",
		SenderID:   "user:42",
		Attempts:   1,
	}
}

// newTestSMSRecordAt is newTestSMSRecord with an explicit CreatedAt,
// so multiple records can share the same timestamp to exercise the
// (created_at, id) tiebreaker.
func newTestSMSRecordAt(id int64, createdAt time.Time, phone string) *models.MessageSMSRecord {
	r := newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN",
		phone,
	)
	r.ID = id
	r.CreatedAt = createdAt
	r.UpdatedAt = createdAt
	return r
}

func TestCreateSMSRecord(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	record := newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN",
		"13800000111",
	)
	require.NoError(t, CreateSMSRecord(ctx, db, record))

	found, err := GetSMSRecord(ctx, db, record.ID)
	require.NoError(t, err)
	assert.Equal(t, record.ID, found.ID)
	assert.Equal(t, int32(pb.SmsVendor_SMS_VENDOR_ALIYUN), found.Vendor)
	assert.Equal(t, int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), found.Scene)
	assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), found.Status)
	assert.Equal(t, "CN", found.RegionCode)
	assert.Equal(t, "13800000111", found.Phone)
	assert.Equal(t, "user:42", found.SenderID)
	assert.Equal(t, "Your code: 1234", found.Content)
	assert.Equal(t, 1, found.Attempts)
}

func TestGetSMSRecord_NotFound(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	_, err := GetSMSRecord(ctx, db, 99999999)
	assert.Error(t, err)
}

func TestListSMSRecords_ByScene(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN",
		"13800000111",
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_REGISTER),
		"CN",
		"13800000222",
	)))

	result, err := ListSMSRecords(ctx, db, SmsListFilter{
		Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.Total)
	assert.Len(t, result.List, 1)
	assert.Equal(t, "13800000111", result.List[0].Phone)
}

func TestCountSMSStats(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN",
		"13800000111",
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_FAILED),
		int32(pb.SmsScene_SMS_SCENE_REGISTER),
		"CN",
		"13800000222",
	)))

	stats, err := CountSMSStats(ctx, db, SmsStatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Total)
	assert.Equal(t, int64(1), stats.Sent)
	assert.Equal(t, int64(1), stats.Failed)
	assert.InDelta(t, 50.0, stats.SuccessRate, 0.1)
}

func TestCountSMSStats_EmptyReturnsMinusOne(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	stats, err := CountSMSStats(ctx, db, SmsStatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), stats.Total)
	assert.Equal(t, float64(-1), stats.SuccessRate,
		"empty filter should return -1 to distinguish 'no data' from '0% success'")
}

func TestListSMSVendorStats(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN",
		"13800000111",
	)))

	stats, err := ListSMSVendorStats(ctx, db, SmsStatsFilter{})
	require.NoError(t, err)
	require.Len(t, stats, 1)
	assert.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, stats[0].Vendor)
}

// TestListSMSRecords_Tiebreaker_StablePagination verifies that rows sharing
// the same created_at are paged without duplication or loss.
func TestListSMSRecords_Tiebreaker_StablePagination(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecordAt(i, ts, fmt.Sprintf("1380000%04d", i))))
	}

	seen := make(map[int64]struct{})
	for page := 1; page <= 5; page++ {
		result, err := ListSMSRecords(ctx, db, SmsListFilter{}, dbx.PageParams{
			Page:     page,
			PageSize: 2,
			Count:    true,
		})
		require.NoError(t, err)
		if len(result.List) == 0 {
			break
		}
		for _, r := range result.List {
			_, ok := seen[r.ID]
			assert.False(t, ok, "id %d appeared on multiple pages", r.ID)
			seen[r.ID] = struct{}{}
		}
	}

	assert.Len(t, seen, 5, "expected to page through all 5 records without loss or duplication")
}

// TestListSMSRecords_ASC_Ordering verifies that SortDirection_ASC returns
// rows oldest-first, mirroring the default DESC behavior.
func TestListSMSRecords_ASC_Ordering(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecordAt(i, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("1380000%04d", i))))
	}

	result, err := ListSMSRecords(ctx, db, SmsListFilter{
		SortDirection: pb.SortDirection_SORT_DIRECTION_ASC,
	}, dbx.PageParams{Page: 1, PageSize: 10, Count: true})
	require.NoError(t, err)
	require.Len(t, result.List, 3)

	assert.Equal(t, int64(1), result.List[0].ID)
	assert.Equal(t, int64(2), result.List[1].ID)
	assert.Equal(t, int64(3), result.List[2].ID)
}

// TestListSMSByCursor_FullSweep pages through every record with cursor mode.
func TestListSMSByCursor_FullSweep(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	ts := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecordAt(i, ts, fmt.Sprintf("1380000%04d", i))))
	}

	seen := make(map[int64]struct{})
	pg := dbx.Pagination{PageSize: 2}
	var afterCreatedAt time.Time
	for i := 0; i < 10; i++ {
		records, err := ListSMSByCursor(ctx, db, SmsListFilter{}, pg, afterCreatedAt)
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

func TestListSMSByCursor_ASC(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 3; i++ {
		require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecordAt(i, base.Add(time.Duration(i)*time.Minute), fmt.Sprintf("1380000%04d", i))))
	}

	filter := SmsListFilter{SortDirection: pb.SortDirection_SORT_DIRECTION_ASC}
	pg := dbx.Pagination{PageSize: 10}
	records, err := ListSMSByCursor(ctx, db, filter, pg, time.Time{})
	require.NoError(t, err)
	require.Len(t, records, 3)
	assert.Equal(t, int64(1), records[0].ID)
	assert.Equal(t, int64(3), records[2].ID)
}

func TestListSMSRegions_Distinct(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN", "13800000111",
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN", "13800000222",
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"HK", "91234567",
	)))

	regions, err := ListSMSRegions(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, []string{"CN", "HK"}, regions)
}

func TestListSMSRegions_Empty(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	regions, err := ListSMSRegions(ctx, db)
	require.NoError(t, err)
	assert.Empty(t, regions)
}

func TestListSMSSenderIDs_Distinct(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	r1 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "CN", "13800000111")
	r1.SenderID = "user-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r1))

	r2 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "CN", "13800000222")
	r2.SenderID = "user-service" // same sender, different phone
	require.NoError(t, CreateSMSRecord(ctx, db, r2))

	r3 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "HK", "91234567")
	r3.SenderID = "pay-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r3))

	senders, err := ListSMSSenderIDs(ctx, db)
	require.NoError(t, err)
	assert.Equal(t, []string{"pay-service", "user-service"}, senders)
}
