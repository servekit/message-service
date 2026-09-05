package dal

import (
	"context"
	"errors"
	"time"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/message-service/internal/store/generated"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"gorm.io/gorm"
)

// SmsListFilter mirrors EmailListFilter for SMS records. See its doc comment
// for the rationale on splitting pagination fields out of the filter.
type SmsListFilter struct {
	Vendor        pb.SmsVendor
	Scene         pb.SmsScene
	Status        pb.MessageStatus
	RegionCode    string
	Phone         string
	SenderID      string
	StartTime     *time.Time
	EndTime       *time.Time
	SortField     pb.SortField
	SortDirection pb.SortDirection
}

// SmsStatsFilter holds parameters for querying SMS statistics.
type SmsStatsFilter struct {
	Vendor    pb.SmsVendor
	Scene     pb.SmsScene
	StartTime *time.Time
	EndTime   *time.Time
}

// SmsVendorStat contains per-vendor SMS statistics.
type SmsVendorStat struct {
	Vendor pb.SmsVendor
	Total  int64
	Sent   int64
	Failed int64
}

// CreateSMSRecord inserts a new SMS record. record.ID is backfilled on success.
func CreateSMSRecord(ctx context.Context, tx *gorm.DB, record *models.MessageSMSRecord) error {
	if err := gorm.G[models.MessageSMSRecord](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetSMSRecord returns the SMS record with the given ID, or
// xcodes.ErrSMSNotFound when no such record exists.
func GetSMSRecord(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageSMSRecord, error) {
	record, err := gorm.G[models.MessageSMSRecord](tx).
		Where(generated.MessageSMSRecord.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrSMSNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListSMSRecords returns a page of SMS records matching filter, with total
// count and derived total pages. See ListEmailRecords for ordering rationale.
func ListSMSRecords(ctx context.Context, tx *gorm.DB, filter SmsListFilter, p dbx.PageParams) (*dbx.PageResult[*models.MessageSMSRecord], error) {
	p = p.Normalize()

	// The query is built twice because gorm gen's typed Count(ctx, col)
	// consumes the chain (it selects the count column internally), so we
	// cannot reuse q for the Find. ChainInterface[T] has no Session()
	// clone, unlike *gorm.DB.
	//
	// ID.Gt(0) is a no-op predicate (snowflake IDs are always positive) used
	// as an anchor so subsequent .Where() clauses compose via AND on the
	// typed chain returned by applySMSListFilter.
	q := applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	var total int64
	if p.Count {
		count, err := q.Count(ctx, generated.MessageSMSRecord.ID.Column().Name)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		total = count
	}

	q = applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	q = applySmsOrderBy(q, filter)

	results, err := q.
		Offset((p.Page - 1) * p.PageSize).
		Limit(p.PageSize).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageSMSRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}

	var totalPages int
	if p.Count && p.PageSize > 0 {
		totalPages = int((total + int64(p.PageSize) - 1) / int64(p.PageSize))
	}

	return &dbx.PageResult[*models.MessageSMSRecord]{
		List:       records,
		Total:      total,
		TotalPages: totalPages,
	}, nil
}

// ListSMSByCursor returns the next page of SMS records past the
// (afterCreatedAt, afterID) cursor. See ListEmailsByCursor for cursor
// semantics.
func ListSMSByCursor(ctx context.Context, tx *gorm.DB, filter SmsListFilter, pg dbx.Pagination, afterCreatedAt time.Time) ([]*models.MessageSMSRecord, error) {
	pg = pg.Normalize()

	q := applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	q = applySmsOrderBy(q, filter)
	q = applySmsCursor(q, filter, pg.AfterID, afterCreatedAt)

	results, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageSMSRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}
	return records, nil
}

// CountSMSRecords returns the total number of SMS records matching filter,
// ignoring pagination. Used by ListSMSByCursor when callers opt in via
// include_total = true.
func CountSMSRecords(ctx context.Context, tx *gorm.DB, filter SmsListFilter) (int64, error) {
	q := applySMSListFilter(gorm.G[models.MessageSMSRecord](tx).Where(generated.MessageSMSRecord.ID.Gt(0)), filter)
	count, err := q.Count(ctx, generated.MessageSMSRecord.ID.Column().Name)
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return count, nil
}

// CountSMSStats returns aggregated SMS statistics matching filter.
// Single SQL query using COUNT(*) FILTER (faster than 3 separate COUNTs).
// SuccessRate is -1 when total == 0 (distinguishes "no data" from "0% success").
// See CountEmailStats for the table-name resolution rationale.
func CountSMSStats(ctx context.Context, tx *gorm.DB, filter SmsStatsFilter) (*Stats, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	q := tx.WithContext(ctx).Model(&models.MessageSMSRecord{}).
		Select("COUNT(*) AS total, COUNT(*) FILTER (WHERE status = ?) AS sent, COUNT(*) FILTER (WHERE status = ?) AS failed", sentStatus, failedStatus)
	q = applySmsStatsWhere(q, filter)

	var rows []models.SmsTotalStatsRow
	if err := q.Scan(&rows).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if len(rows) == 0 {
		return &Stats{SuccessRate: -1}, nil
	}
	r := rows[0]

	var successRate float64
	if r.Total > 0 {
		successRate = float64(r.Sent) / float64(r.Total) * 100
	} else {
		successRate = -1
	}

	return &Stats{
		Total:       r.Total,
		Sent:        r.Sent,
		Failed:      r.Failed,
		SuccessRate: successRate,
	}, nil
}

// ListSMSVendorStats returns per-vendor SMS statistics matching filter.
func ListSMSVendorStats(ctx context.Context, tx *gorm.DB, filter SmsStatsFilter) ([]SmsVendorStat, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	q := tx.WithContext(ctx).Model(&models.MessageSMSRecord{}).
		Select("vendor, COUNT(*) AS total, COUNT(*) FILTER (WHERE status = ?) AS sent, COUNT(*) FILTER (WHERE status = ?) AS failed", sentStatus, failedStatus).
		Group("vendor")
	q = applySmsStatsWhere(q, filter)

	var rows []models.SmsStatsRow
	if err := q.Scan(&rows).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	stats := make([]SmsVendorStat, len(rows))
	for i, r := range rows {
		stats[i] = SmsVendorStat{
			Vendor: pb.SmsVendor(r.Vendor),
			Total:  r.Total,
			Sent:   r.Sent,
			Failed: r.Failed,
		}
	}
	return stats, nil
}

// --- internal helpers ---

func applySmsStatsWhere(q *gorm.DB, f SmsStatsFilter) *gorm.DB {
	if f.Vendor != 0 {
		q = q.Where("vendor = ?", int32(f.Vendor))
	}
	if f.Scene != 0 {
		q = q.Where("scene = ?", int32(f.Scene))
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at <= ?", *f.EndTime)
	}
	return q
}

func applySMSListFilter(q gorm.ChainInterface[models.MessageSMSRecord], f SmsListFilter) gorm.ChainInterface[models.MessageSMSRecord] {
	if f.Vendor != 0 {
		q = q.Where(generated.MessageSMSRecord.Vendor.Eq(int32(f.Vendor)))
	}
	if f.Scene != 0 {
		q = q.Where(generated.MessageSMSRecord.Scene.Eq(int32(f.Scene)))
	}
	if f.Status != 0 {
		q = q.Where(generated.MessageSMSRecord.Status.Eq(int32(f.Status)))
	}
	if f.RegionCode != "" {
		q = q.Where(generated.MessageSMSRecord.RegionCode.Eq(f.RegionCode))
	}
	if f.Phone != "" {
		q = q.Where(generated.MessageSMSRecord.Phone.Eq(f.Phone))
	}
	if f.SenderID != "" {
		q = q.Where(generated.MessageSMSRecord.SenderID.Eq(f.SenderID))
	}
	if f.StartTime != nil {
		q = q.Where(generated.MessageSMSRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.MessageSMSRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}

// applySmsOrderBy mirrors applyEmailOrderBy for SMS records.
func applySmsOrderBy(q gorm.ChainInterface[models.MessageSMSRecord], f SmsListFilter) gorm.ChainInterface[models.MessageSMSRecord] {
	asc := f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC
	if asc {
		return q.Order(generated.MessageSMSRecord.CreatedAt.Asc()).Order(generated.MessageSMSRecord.ID.Asc())
	}
	return q.Order(generated.MessageSMSRecord.CreatedAt.Desc()).Order(generated.MessageSMSRecord.ID.Desc())
}

// applySmsCursor mirrors applyEmailCursor for SMS records.
func applySmsCursor(q gorm.ChainInterface[models.MessageSMSRecord], f SmsListFilter, afterID int64, afterCreatedAt time.Time) gorm.ChainInterface[models.MessageSMSRecord] {
	if afterID == 0 {
		return q
	}
	if f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC {
		return q.Where("created_at > ? OR (created_at = ? AND id > ?)", afterCreatedAt, afterCreatedAt, afterID)
	}
	return q.Where("created_at < ? OR (created_at = ? AND id < ?)", afterCreatedAt, afterCreatedAt, afterID)
}

// ListSMSRegions returns all distinct region_code values, ordered ascending.
// Used by the frontend to populate SMS list filter dropdowns. Region sets
// are low-cardinality so no filter or pagination is exposed.
func ListSMSRegions(ctx context.Context, tx *gorm.DB) ([]string, error) {
	q := tx.WithContext(ctx).Model(&models.MessageSMSRecord{}).
		Distinct("region_code").
		Where("region_code != ''").
		Order("region_code ASC")

	var regions []string
	if err := q.Scan(&regions).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return regions, nil
}

// ListSMSSenderIDs returns all distinct sender_id values, ordered ascending.
// Used by the frontend to populate SMS list filter dropdowns. Sender sets
// are low-cardinality so no filter or pagination is exposed.
func ListSMSSenderIDs(ctx context.Context, tx *gorm.DB) ([]string, error) {
	q := tx.WithContext(ctx).Model(&models.MessageSMSRecord{}).
		Distinct("sender_id").
		Where("sender_id != ''").
		Order("sender_id ASC")

	var senders []string
	if err := q.Scan(&senders).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return senders, nil
}
