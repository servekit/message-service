package dal

import (
	"context"

	"github.com/servekit/message-service/internal/store/generated"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"gorm.io/gorm"
)

// CreateEmailRecordAttachments inserts all attachment rows in one call. Caller
// is responsible for ordering. Empty slice is a no-op (returns nil).
//
// Uses plain *gorm.DB.Create rather than gorm.G[T]().Create because the typed
// API only accepts a single *T (not a slice), while attachment counts per
// email are small (typically <10) so a single batched INSERT is sufficient.
func CreateEmailRecordAttachments(ctx context.Context, tx *gorm.DB, rows []*models.MessageEmailRecordAttachment) error {
	if len(rows) == 0 {
		return nil
	}
	if err := tx.WithContext(ctx).Create(&rows).Error; err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// ListEmailRecordAttachments returns all attachments for a given email record
// ordered by id ascending (preserves send-time ordering). Returns an empty
// (non-nil) slice when none exist.
func ListEmailRecordAttachments(ctx context.Context, tx *gorm.DB, emailRecordID int64) ([]*models.MessageEmailRecordAttachment, error) {
	rows, err := gorm.G[models.MessageEmailRecordAttachment](tx).
		Where(generated.MessageEmailRecordAttachment.EmailRecordID.Eq(emailRecordID)).
		Order(generated.MessageEmailRecordAttachment.ID.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	out := make([]*models.MessageEmailRecordAttachment, len(rows))
	for i := range rows {
		out[i] = &rows[i]
	}
	return out, nil
}

// ListEmailRecordAttachmentsByEmailRecordIDs batch-loads attachments for many
// email records in a single query. Returns a map keyed by email_record_id;
// each slice is ordered by id ascending. Missing IDs map to an empty slice
// (or no entry — callers should use `len(map[id]) == 0` to detect absence).
//
// Use this on List paths to avoid N+1 queries. Empty input returns an empty
// map without touching the DB.
func ListEmailRecordAttachmentsByEmailRecordIDs(ctx context.Context, tx *gorm.DB, emailRecordIDs []int64) (map[int64][]*models.MessageEmailRecordAttachment, error) {
	out := make(map[int64][]*models.MessageEmailRecordAttachment)
	if len(emailRecordIDs) == 0 {
		return out, nil
	}
	rows, err := gorm.G[models.MessageEmailRecordAttachment](tx).
		Where(generated.MessageEmailRecordAttachment.EmailRecordID.In(emailRecordIDs...)).
		Order(generated.MessageEmailRecordAttachment.EmailRecordID.Asc()).
		Order(generated.MessageEmailRecordAttachment.ID.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	for i := range rows {
		id := rows[i].EmailRecordID
		out[id] = append(out[id], &rows[i])
	}
	return out, nil
}
