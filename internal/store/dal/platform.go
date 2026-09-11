// Platform resource data access: apps, channel accounts, signatures,
// templates, policies. Consumed by the admin service (writes) and the
// registry snapshot loader (bulk reads).
package dal

import (
	"context"
	"errors"
	"time"

	"github.com/servekit/message-service/internal/store/generated"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// --- Apps ---

// CreateApp inserts a new app. record.ID is backfilled on success.
func CreateApp(ctx context.Context, tx *gorm.DB, record *models.MessageApp) error {
	if err := gorm.G[models.MessageApp](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// EnsureTenantApp idempotently inserts the first-sight tenant config row
// (trusted x-tenant-key path). ON CONFLICT DO NOTHING + caller re-read, so
// racing replicas converge on one row and operator edits are never
// clobbered.
//
// F2 hardening: the conflict target is now BROAD (no column list). The
// previous ON CONFLICT (app_key) left two edges where a soft-deleted
// occupant permanently 500'd the tenant's first send: an app_key-equal
// occupant suppressed the insert and the scoped re-read missed it ("row
// absent after insert"), while an occupant holding tenant_key under a
// DIFFERENT app_key raised a raw uniq_msg_apps_tenant_key violation the
// narrow target did not cover. After the suppressed insert the occupant is
// looked up UNSCOPED (by tenant_key, then the app_key-equal fallback
// mirroring GetAppForTenant) and revived in place — deleted_at cleared,
// tenant_key re-pointed — while its historic identity fields stay verbatim
// (app_key/secret/name/daily limits are the operator's row). The lookup is
// ordered live-first (③-review F2 corner): when a LIVE row and a
// soft-deleted one both match the OR, the live one wins and the dead one
// stays dead instead of being revived into a second claimant of the
// tenant's unique keys.
func EnsureTenantApp(ctx context.Context, tx *gorm.DB, record *models.MessageApp) error {
	if err := gorm.G[models.MessageApp](tx, clause.OnConflict{
		DoNothing: true,
	}).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}

	tk := models.TenantKeyOf(record.TenantKey)
	var occupant models.MessageApp
	err := tx.WithContext(ctx).Unscoped().
		Order(clause.Expr{SQL: "deleted_at IS NULL DESC"}).
		Order("id").
		Where("tenant_key = ? OR app_key = ?", tk, record.AppKey).
		Take(&occupant).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil // insert landed; the caller's re-read confirms
		}
		return xcodes.ErrInternal.Wrap(err)
	}
	if occupant.DeletedAt.Time.IsZero() && !occupant.DeletedAt.Valid {
		return nil // live occupant: race winner or existing row — never clobbered
	}
	res := tx.WithContext(ctx).Unscoped().
		Model(&models.MessageApp{}).
		Where("id = ?", occupant.ID).
		Updates(map[string]any{
			"deleted_at": nil,
			"tenant_key": tk,
			"updated_at": time.Now(),
		})
	if res.Error != nil {
		return xcodes.ErrInternal.Wrap(res.Error)
	}
	return nil
}

// GetAppForTenant resolves the tenant's config row: prefer the tenant_key
// mapping, fall back to app_key = tenantKey (pre-backfill window rows whose
// column is still NULL). nil when neither matches.
func GetAppForTenant(ctx context.Context, tx *gorm.DB, tenantKey string) (*models.MessageApp, error) {
	record, err := gorm.G[models.MessageApp](tx).
		Where(generated.MessageApp.TenantKey.Eq(tenantKey)).
		Take(ctx)
	if err == nil {
		return &record, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	record, err = gorm.G[models.MessageApp](tx).
		Where(generated.MessageApp.AppKey.Eq(tenantKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// GetApp returns the app with the given ID, or ErrAppNotFound.
func GetApp(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageApp, error) {
	record, err := gorm.G[models.MessageApp](tx).
		Where(generated.MessageApp.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrAppNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// GetAppByKey returns the app with the given app_key, or ErrAppNotFound.
func GetAppByKey(ctx context.Context, tx *gorm.DB, appKey string) (*models.MessageApp, error) {
	record, err := gorm.G[models.MessageApp](tx).
		Where(generated.MessageApp.AppKey.Eq(appKey)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrAppNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListApps returns all apps ordered by id ascending (low cardinality — no
// pagination).
func ListApps(ctx context.Context, tx *gorm.DB) ([]*models.MessageApp, error) {
	results, err := gorm.G[models.MessageApp](tx).
		Order(generated.MessageApp.ID.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	apps := make([]*models.MessageApp, len(results))
	for i := range results {
		apps[i] = &results[i]
	}
	return apps, nil
}

// UpdateApp replaces the mutable app fields (name, disabled, daily
// limits). app_key is immutable; the credential column is gone (④).
func UpdateApp(ctx context.Context, tx *gorm.DB, app *models.MessageApp) error {
	_, err := gorm.G[models.MessageApp](tx).
		Where(generated.MessageApp.ID.Eq(app.ID)).
		Set(
			generated.MessageApp.Name.Set(app.Name),
			generated.MessageApp.Disabled.Set(app.Disabled),
			generated.MessageApp.SMSDailyLimit.Set(app.SMSDailyLimit),
			generated.MessageApp.EmailDailyLimit.Set(app.EmailDailyLimit),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// DeleteApp soft-deletes the app.
func DeleteApp(ctx context.Context, tx *gorm.DB, id int64) error {
	if _, err := gorm.G[models.MessageApp](tx).
		Where(generated.MessageApp.ID.Eq(id)).
		Delete(ctx); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// --- Channel accounts ---

// CreateChannelAccount inserts a new channel account.
func CreateChannelAccount(ctx context.Context, tx *gorm.DB, record *models.MessageChannelAccount) error {
	if err := gorm.G[models.MessageChannelAccount](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetChannelAccount returns the account with the given ID, or
// ErrChannelAccountNotFound.
func GetChannelAccount(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageChannelAccount, error) {
	record, err := gorm.G[models.MessageChannelAccount](tx).
		Where(generated.MessageChannelAccount.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrChannelAccountNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListChannelAccounts returns the full pool ordered by name (low
// cardinality — no pagination).
func ListChannelAccounts(ctx context.Context, tx *gorm.DB) ([]*models.MessageChannelAccount, error) {
	results, err := gorm.G[models.MessageChannelAccount](tx).
		Order(generated.MessageChannelAccount.Name.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	accounts := make([]*models.MessageChannelAccount, len(results))
	for i := range results {
		accounts[i] = &results[i]
	}
	return accounts, nil
}

// UpdateChannelAccount replaces the mutable account fields. Name, channel,
// and vendor are immutable.
func UpdateChannelAccount(ctx context.Context, tx *gorm.DB, account *models.MessageChannelAccount) error {
	_, err := gorm.G[models.MessageChannelAccount](tx).
		Where(generated.MessageChannelAccount.ID.Eq(account.ID)).
		Set(
			generated.MessageChannelAccount.Disabled.Set(account.Disabled),
			generated.MessageChannelAccount.Remark.Set(account.Remark),
			generated.MessageChannelAccount.Config.Set(account.Config),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// DeleteChannelAccount soft-deletes the account (its signature bindings and
// policy references are resolved against the live snapshot, which simply
// stops including it).
func DeleteChannelAccount(ctx context.Context, tx *gorm.DB, id int64) error {
	if _, err := gorm.G[models.MessageChannelAccount](tx).
		Where(generated.MessageChannelAccount.ID.Eq(id)).
		Delete(ctx); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// --- Signatures ---

// CreateSignature inserts a signature and its account bindings in one
// transaction.
func CreateSignature(ctx context.Context, tx *gorm.DB, sig *models.MessageSignature, accountIDs []int64) error {
	return tx.WithContext(ctx).Transaction(func(t *gorm.DB) error {
		if err := gorm.G[models.MessageSignature](t).Create(ctx, sig); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return createSignatureAccounts(ctx, t, sig.ID, accountIDs)
	})
}

// createSignatureAccounts inserts binding rows for sigID.
func createSignatureAccounts(ctx context.Context, tx *gorm.DB, sigID int64, accountIDs []int64) error {
	if len(accountIDs) == 0 {
		return nil
	}
	rows := make([]*models.MessageSignatureAccount, 0, len(accountIDs))
	for _, id := range accountIDs {
		rows = append(rows, &models.MessageSignatureAccount{SignatureID: sigID, AccountID: id})
	}
	// Plain *gorm.DB batch insert — the typed Create takes a single entity
	// (same rationale as CreateEmailRecordAttachments).
	if err := tx.WithContext(ctx).Create(&rows).Error; err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetSignature returns the signature with the given ID, or
// ErrSignatureNotFound.
func GetSignature(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageSignature, error) {
	record, err := gorm.G[models.MessageSignature](tx).
		Where(generated.MessageSignature.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrSignatureNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListSignatures returns all signatures ordered by name.
func ListSignatures(ctx context.Context, tx *gorm.DB) ([]*models.MessageSignature, error) {
	results, err := gorm.G[models.MessageSignature](tx).
		Order(generated.MessageSignature.Name.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	sigs := make([]*models.MessageSignature, len(results))
	for i := range results {
		sigs[i] = &results[i]
	}
	return sigs, nil
}

// ListAllSignatureAccounts returns every binding row (used to assemble
// signature → accounts views and the registry snapshot in one query).
func ListAllSignatureAccounts(ctx context.Context, tx *gorm.DB) ([]*models.MessageSignatureAccount, error) {
	results, err := gorm.G[models.MessageSignatureAccount](tx).
		Order(generated.MessageSignatureAccount.SignatureID.Asc()).
		Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	rows := make([]*models.MessageSignatureAccount, len(results))
	for i := range results {
		rows[i] = &results[i]
	}
	return rows, nil
}

// UpdateSignature replaces remark/disabled and the full account binding
// list in one transaction.
func UpdateSignature(ctx context.Context, tx *gorm.DB, sig *models.MessageSignature, accountIDs []int64) error {
	return tx.WithContext(ctx).Transaction(func(t *gorm.DB) error {
		_, err := gorm.G[models.MessageSignature](t).
			Where(generated.MessageSignature.ID.Eq(sig.ID)).
			Set(
				generated.MessageSignature.Remark.Set(sig.Remark),
				generated.MessageSignature.Disabled.Set(sig.Disabled),
			).
			Update(ctx)
		if err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		// Full-replace bindings: soft-delete current rows, insert the new
		// set. Unique index (signature_id, account_id) would collide with
		// re-inserted live rows otherwise.
		if _, err := gorm.G[models.MessageSignatureAccount](t).
			Where(generated.MessageSignatureAccount.SignatureID.Eq(sig.ID)).
			Delete(ctx); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return createSignatureAccounts(ctx, t, sig.ID, accountIDs)
	})
}

// DeleteSignature soft-deletes the signature and its bindings.
func DeleteSignature(ctx context.Context, tx *gorm.DB, id int64) error {
	return tx.WithContext(ctx).Transaction(func(t *gorm.DB) error {
		if _, err := gorm.G[models.MessageSignatureAccount](t).
			Where(generated.MessageSignatureAccount.SignatureID.Eq(id)).
			Delete(ctx); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		if _, err := gorm.G[models.MessageSignature](t).
			Where(generated.MessageSignature.ID.Eq(id)).
			Delete(ctx); err != nil {
			return xcodes.ErrInternal.Wrap(err)
		}
		return nil
	})
}

// --- Templates ---

// CreateTemplate inserts a new template.
func CreateTemplate(ctx context.Context, tx *gorm.DB, record *models.MessageTemplate) error {
	if err := gorm.G[models.MessageTemplate](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetTemplate returns the template with the given ID, or
// ErrTemplateNotFound.
func GetTemplate(ctx context.Context, tx *gorm.DB, id int64) (*models.MessageTemplate, error) {
	record, err := gorm.G[models.MessageTemplate](tx).
		Where(generated.MessageTemplate.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrTemplateNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListTemplates filters by channel (0 = all), ordered by id ascending.
// (The legacy app_id filter went with the ④ column drop; callers filter by
// tenant in memory — the registry Snapshot.)
func ListTemplates(ctx context.Context, tx *gorm.DB, channel int32) ([]*models.MessageTemplate, error) {
	q := gorm.G[models.MessageTemplate](tx).Where(generated.MessageTemplate.ID.Gt(0))
	if channel != 0 {
		q = q.Where(generated.MessageTemplate.Channel.Eq(channel))
	}
	results, err := q.Order(generated.MessageTemplate.ID.Asc()).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	templates := make([]*models.MessageTemplate, len(results))
	for i := range results {
		templates[i] = &results[i]
	}
	return templates, nil
}

// UpdateTemplate replaces the mutable template fields (name, kind, params,
// content, disabled). App ownership is immutable.
func UpdateTemplate(ctx context.Context, tx *gorm.DB, t *models.MessageTemplate) error {
	_, err := gorm.G[models.MessageTemplate](tx).
		Where(generated.MessageTemplate.ID.Eq(t.ID)).
		Set(
			generated.MessageTemplate.Name.Set(t.Name),
			generated.MessageTemplate.Kind.Set(t.Kind),
			generated.MessageTemplate.Params.Set(t.Params),
			generated.MessageTemplate.Content.Set(t.Content),
			generated.MessageTemplate.Disabled.Set(t.Disabled),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// DeleteTemplate soft-deletes the template.
func DeleteTemplate(ctx context.Context, tx *gorm.DB, id int64) error {
	if _, err := gorm.G[models.MessageTemplate](tx).
		Where(generated.MessageTemplate.ID.Eq(id)).
		Delete(ctx); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// --- Policies ---

// CreatePolicy inserts a new policy.
func CreatePolicy(ctx context.Context, tx *gorm.DB, record *models.MessagePolicy) error {
	if err := gorm.G[models.MessagePolicy](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetPolicy returns the policy with the given ID, or ErrPolicyNotFound.
func GetPolicy(ctx context.Context, tx *gorm.DB, id int64) (*models.MessagePolicy, error) {
	record, err := gorm.G[models.MessagePolicy](tx).
		Where(generated.MessagePolicy.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrPolicyNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListPolicies filters by channel (0 = all), ordered by id ascending.
// (The legacy app_id filter went with the ④ column drop; callers filter by
// tenant in memory — the registry Snapshot.)
func ListPolicies(ctx context.Context, tx *gorm.DB, channel int32) ([]*models.MessagePolicy, error) {
	q := gorm.G[models.MessagePolicy](tx).Where(generated.MessagePolicy.ID.Gt(0))
	if channel != 0 {
		q = q.Where(generated.MessagePolicy.Channel.Eq(channel))
	}
	results, err := q.Order(generated.MessagePolicy.ID.Asc()).Find(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	policies := make([]*models.MessagePolicy, len(results))
	for i := range results {
		policies[i] = &results[i]
	}
	return policies, nil
}

// UpdatePolicy replaces the mutable policy fields (template, route chains,
// disabled). App/channel/scene are immutable — delete + recreate to move a
// policy.
func UpdatePolicy(ctx context.Context, tx *gorm.DB, p *models.MessagePolicy) error {
	_, err := gorm.G[models.MessagePolicy](tx).
		Where(generated.MessagePolicy.ID.Eq(p.ID)).
		Set(
			generated.MessagePolicy.TemplateID.Set(p.TemplateID),
			generated.MessagePolicy.Disabled.Set(p.Disabled),
			generated.MessagePolicy.Routes.Set(p.Routes),
			generated.MessagePolicy.IntlRoutes.Set(p.IntlRoutes),
		).
		Update(ctx)
	if err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// DeletePolicy soft-deletes the policy; sends for its (app, scene) fail
// closed from the next snapshot refresh.
func DeletePolicy(ctx context.Context, tx *gorm.DB, id int64) error {
	if _, err := gorm.G[models.MessagePolicy](tx).
		Where(generated.MessagePolicy.ID.Eq(id)).
		Delete(ctx); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}
