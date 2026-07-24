// Package dal provides type-safe data access for message-service tables.
//
// Each file in this package corresponds to one table; cross-table operations
// are composed at the service layer. Functions accept a *gorm.DB (or tx) and
// return raw errors; the service layer wraps them with xcodes.
package dal

import (
	"context"
	"errors"
	"time"

	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/servekit/message-service/internal/store/generated"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"gorm.io/gorm"
)

// EmailListFilter holds parameters for listing email records. Page / PageSize
// live on dbx.PageParams (offset mode) or dbx.Pagination (cursor mode) and
// are passed to ListEmailRecords / ListEmailsByCursor separately — keeping
// the two modes' pagination fields separate avoids the "which PageSize wins"
// ambiguity.
type EmailListFilter struct {
	Vendor        pb.EmailVendor
	Scene         pb.EmailScene
	Status        pb.MessageStatus
	Target        string
	SenderID      string
	StartTime     *time.Time
	EndTime       *time.Time
	SortField     pb.SortField
	SortDirection pb.SortDirection
}

// EmailStatsFilter holds parameters for querying email statistics.
type EmailStatsFilter struct {
	Vendor    pb.EmailVendor
	Scene     pb.EmailScene
	StartTime *time.Time
	EndTime   *time.Time
}

// EmailVendorStat contains per-vendor email statistics.
type EmailVendorStat struct {
	Vendor pb.EmailVendor
	Total  int64
	Sent   int64
	Failed int64
}

// CreateEmailRecord inserts a new email record. record.ID is backfilled
// on success.
func CreateEmailRecord(ctx context.Context, tx *gorm.DB, record *models.MessageEmailRecord) error {
	if err := gorm.G[models.MessageEmailRecord](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetEmailRecord returns the email record with the given ID, or
// xcodes.ErrEmailNotFound when no such record exists.
func GetEmailRecord(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageEmailRecord, error) {
	record, err := gorm.G[models.MessageEmailRecord](tx).
		Where(generated.MessageEmailRecord.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrEmailNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListEmailRecords returns a page of email records matching filter, along
// with the total count and derived total pages. Page and PageSize come from
// dbx.PageParams; PageSize is clamped to dbx.MaxPageSize.
//
// Ordering is always (sort_field, id) — id is the tiebreaker that keeps
// pagination stable when sort_field has tied values (e.g. multiple records
// sharing a created_at timestamp).
func ListEmailRecords(ctx context.Context, tx *gorm.DB, filter EmailListFilter, p dbx.PageParams) (*dbx.PageResult[*models.MessageEmailRecord], error) {
	p = p.Normalize()

	// The query is built twice because gorm gen's typed Count(ctx, col)
	// consumes the chain (it selects the count column internally), so we
	// cannot reuse q for the Find. ChainInterface[T] has no Session()
	// clone, unlike *gorm.DB.
	//
	// ID.Gt(0) is a no-op predicate (snowflake IDs are always positive) used
	// as an anchor so subsequent .Where() clauses compose via AND on the
	// typed chain returned by applyEmailListFilter.
	q := applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	var total int64
	if p.Count {
		count, err := q.Count(ctx, generated.MessageEmailRecord.ID.Column().Name)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		total = count
	}

	q = applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	q = applyEmailOrderBy(q, filter)

	results, err := q.
		Offset((p.Page - 1) * p.PageSize).
		Limit(p.PageSize).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageEmailRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}

	var totalPages int
	if p.Count && p.PageSize > 0 {
		totalPages = int((total + int64(p.PageSize) - 1) / int64(p.PageSize))
	}

	return &dbx.PageResult[*models.MessageEmailRecord]{
		List:       records,
		Total:      total,
		TotalPages: totalPages,
	}, nil
}

// ListEmailsByCursor returns the next page of email records past the
// (afterCreatedAt, afterID) cursor. Caller passes pageSize on pg and the
// previous page's last (id, created_at) on subsequent calls. Pass a zero
// pg.AfterID + zero afterCreatedAt for the first page.
//
// The returned slice may be up to pageSize+1 long; use dbx.TrimPage to
// detect "has next page".
func ListEmailsByCursor(ctx context.Context, tx *gorm.DB, filter EmailListFilter, pg dbx.Pagination, afterCreatedAt time.Time) ([]*models.MessageEmailRecord, error) {
	pg = pg.Normalize()

	q := applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	q = applyEmailOrderBy(q, filter)
	q = applyEmailCursor(q, filter, pg.AfterID, afterCreatedAt)

	results, err := q.Limit(pg.FetchLimit()).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.MessageEmailRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}
	return records, nil
}

// CountEmailRecords returns the total number of email records matching
// filter, ignoring pagination. Used by ListEmailsByCursor when callers opt
// in via include_total = true.
func CountEmailRecords(ctx context.Context, tx *gorm.DB, filter EmailListFilter) (int64, error) {
	q := applyEmailListFilter(gorm.G[models.MessageEmailRecord](tx).Where(generated.MessageEmailRecord.ID.Gt(0)), filter)
	count, err := q.Count(ctx, generated.MessageEmailRecord.ID.Column().Name)
	if err != nil {
		return 0, xcodes.ErrInternal.Wrap(err)
	}
	return count, nil
}

// CountEmailStats returns aggregated email statistics matching filter.
// Single SQL query using COUNT(*) FILTER (faster than 3 separate COUNTs).
// SuccessRate is -1 when total == 0 (distinguishes "no data" from "0% success").
//
// Uses tx.Model(&MessageEmailRecord{}) so GORM's NamingStrategy auto-derives
// the table name from the struct — no hard-coded table name, no TableName()
// override.
func CountEmailStats(ctx context.Context, tx *gorm.DB, filter EmailStatsFilter) (*Stats, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	q := tx.WithContext(ctx).Model(&models.MessageEmailRecord{}).
		Select("COUNT(*) AS total, COUNT(*) FILTER (WHERE status = ?) AS sent, COUNT(*) FILTER (WHERE status = ?) AS failed", sentStatus, failedStatus)
	q = applyEmailStatsWhere(q, filter)

	var rows []models.EmailTotalStatsRow
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

// ListEmailVendorStats returns per-vendor email statistics matching filter.
func ListEmailVendorStats(ctx context.Context, tx *gorm.DB, filter EmailStatsFilter) ([]EmailVendorStat, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	q := tx.WithContext(ctx).Model(&models.MessageEmailRecord{}).
		Select("vendor, COUNT(*) AS total, COUNT(*) FILTER (WHERE status = ?) AS sent, COUNT(*) FILTER (WHERE status = ?) AS failed", sentStatus, failedStatus).
		Group("vendor")
	q = applyEmailStatsWhere(q, filter)

	var rows []models.EmailStatsRow
	if err := q.Scan(&rows).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	stats := make([]EmailVendorStat, len(rows))
	for i, r := range rows {
		stats[i] = EmailVendorStat{
			Vendor: pb.EmailVendor(r.Vendor),
			Total:  r.Total,
			Sent:   r.Sent,
			Failed: r.Failed,
		}
	}
	return stats, nil
}

// --- internal helpers ---

func applyEmailStatsWhere(q *gorm.DB, f EmailStatsFilter) *gorm.DB {
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

func applyEmailListFilter(q gorm.ChainInterface[models.MessageEmailRecord], f EmailListFilter) gorm.ChainInterface[models.MessageEmailRecord] {
	if f.Vendor != 0 {
		q = q.Where(generated.MessageEmailRecord.Vendor.Eq(int32(f.Vendor)))
	}
	if f.Scene != 0 {
		q = q.Where(generated.MessageEmailRecord.Scene.Eq(int32(f.Scene)))
	}
	if f.Status != 0 {
		q = q.Where(generated.MessageEmailRecord.Status.Eq(int32(f.Status)))
	}
	if f.Target != "" {
		q = q.Where(generated.MessageEmailRecord.Target.Eq(f.Target))
	}
	if f.SenderID != "" {
		q = q.Where(generated.MessageEmailRecord.SenderID.Eq(f.SenderID))
	}
	if f.StartTime != nil {
		q = q.Where(generated.MessageEmailRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.MessageEmailRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}

// applyEmailOrderBy attaches an (ORDER BY sort_field [dir], id [dir]) clause
// to q. The id column is always present as a tiebreaker so pagination stays
// stable under tied sort values. UNSPECIFIED sort_field/direction fall back
// to (created_at DESC, id DESC) — the historical default.
//
// sort_field currently has only CREATED_AT as a non-UNSPECIFIED value, so
// all inputs converge on created_at; future SENT_AT-style fields will branch
// here.
func applyEmailOrderBy(q gorm.ChainInterface[models.MessageEmailRecord], f EmailListFilter) gorm.ChainInterface[models.MessageEmailRecord] {
	asc := f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC
	if asc {
		return q.Order(generated.MessageEmailRecord.CreatedAt.Asc()).Order(generated.MessageEmailRecord.ID.Asc())
	}
	return q.Order(generated.MessageEmailRecord.CreatedAt.Desc()).Order(generated.MessageEmailRecord.ID.Desc())
}

// applyEmailCursor advances q past the (afterCreatedAt, afterID) tuple from
// the previous page's last row. Returns q unchanged when afterID == 0
// (first page).
//
// Callers MUST pass both afterID and afterCreatedAt; passing only afterID
// would degrade to a bare `id < ?` cursor, which drops rows whose id
// ordering disagrees with the sort column under tied created_at values.
func applyEmailCursor(q gorm.ChainInterface[models.MessageEmailRecord], f EmailListFilter, afterID int64, afterCreatedAt time.Time) gorm.ChainInterface[models.MessageEmailRecord] {
	if afterID == 0 {
		return q
	}
	if f.SortDirection == pb.SortDirection_SORT_DIRECTION_ASC {
		return q.Where("created_at > ? OR (created_at = ? AND id > ?)", afterCreatedAt, afterCreatedAt, afterID)
	}
	return q.Where("created_at < ? OR (created_at = ? AND id < ?)", afterCreatedAt, afterCreatedAt, afterID)
}

// ListEmailSenderIDs returns all distinct sender_id values, ordered ascending.
// Used by the frontend to populate email list filter dropdowns. Sender sets
// are low-cardinality so no filter or pagination is exposed.
func ListEmailSenderIDs(ctx context.Context, tx *gorm.DB) ([]string, error) {
	q := tx.WithContext(ctx).Model(&models.MessageEmailRecord{}).
		Distinct("sender_id").
		Where("sender_id != ''").
		Order("sender_id ASC")

	var senders []string
	if err := q.Scan(&senders).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return senders, nil
}
