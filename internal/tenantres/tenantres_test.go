package tenantres

import (
	"context"
	"testing"
	"time"

	"github.com/servekit/message-service/internal/appauth"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setup(t *testing.T) (*Resolver, *gorm.DB, *registry.Registry) {
	t.Helper()
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, db.AutoMigrate(models.AllModels()...))
	reg := registry.New(db)
	return New(db, reg), db, reg
}

// seedApp inserts an app row directly (raw column control) and converges the
// snapshot so the legacy path (snapshot-only reads) sees it.
func seedApp(t *testing.T, db *gorm.DB, reg *registry.Registry, id int64, appKey, secret string, tenantKey *string) {
	t.Helper()
	app := &models.MessageApp{ID: id, AppKey: appKey, AppSecret: secret, Name: appKey, TenantKey: tenantKey}
	require.NoError(t, dal.CreateApp(context.Background(), db, app))
	require.NoError(t, reg.Refresh(context.Background()))
}

// TestRequireTrustedLazilyCreatesConfigRow: a first-sight trusted tenant gets
// a default config row (unlimited daily limits, minted secret nobody holds)
// and the same row is reused on the second call.
func TestRequireTrustedLazilyCreatesConfigRow(t *testing.T) {
	r, db, _ := setup(t)
	ctx := appauth.WithTenant(context.Background(), "ten_abc123def456")

	c1, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ten_abc123def456", c1.TenantKey)
	require.NotNil(t, c1.App)
	assert.Equal(t, "ten_abc123def456", c1.App.AppKey)
	assert.Equal(t, "ten_abc123def456", models.TenantKeyOf(c1.App.TenantKey))
	assert.Zero(t, c1.App.SMSDailyLimit, "lazy config row defaults to unlimited SMS quota")
	assert.Zero(t, c1.App.EmailDailyLimit, "lazy config row defaults to unlimited email quota")

	c2, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, c1.App.ID, c2.App.ID, "second call must reuse the config row")

	apps, err := dal.ListApps(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, apps, 1)
}

// TestRequireTrustedAuthoritativeOverSmuggledLegacyCreds pins D-③1: when
// x-tenant-key rides together with a legacy ak/sk pair (of a DIFFERENT
// tenant's app, valid secret), the trusted key wins and the legacy pair is
// discarded — a proxy-forwarded caller cannot impersonate another tenant.
func TestRequireTrustedAuthoritativeOverSmuggledLegacyCreds(t *testing.T) {
	r, db, reg := setup(t)
	seedApp(t, db, reg, 10, "beta-app", "beta-secret", models.TenantKeyPtr("ten_beta0000000"))

	ctx := appauth.WithTenant(appauth.WithApp(context.Background(), "beta-app", "beta-secret"), "ten_alpha0000000")
	c, err := r.Require(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ten_alpha0000000", c.TenantKey)
	assert.NotEqual(t, int64(10), c.App.ID, "the smuggled app row must not be the caller")
}

// TestRequireLegacyValidatesAndConvertsToTenant: the legacy ak/sk path keeps
// its verification and converts the app to its mapped tenant_key; an empty
// column falls back to the app_key literal (T10 总装 clears the empties).
func TestRequireLegacyValidatesAndConvertsToTenant(t *testing.T) {
	r, db, reg := setup(t)
	seedApp(t, db, reg, 20, "legacy-mapped", "s1", models.TenantKeyPtr("ten_mapped000000"))
	seedApp(t, db, reg, 21, "legacy-unmapped", "s2", nil)

	c, err := r.Require(appauth.WithApp(context.Background(), "legacy-mapped", "s1"))
	require.NoError(t, err)
	assert.Equal(t, "ten_mapped000000", c.TenantKey)

	c, err = r.Require(appauth.WithApp(context.Background(), "legacy-unmapped", "s2"))
	require.NoError(t, err)
	assert.Equal(t, "legacy-unmapped", c.TenantKey, "empty column falls back to the app_key literal")
}

// TestRequireLegacyBackedByUnbackfilledColumnReusesRow: a trusted key equal to
// an existing app_key whose tenant_key column is still NULL (SQL not yet run)
// must reuse that row instead of creating a duplicate.
func TestRequireTrustedReusesUnbackfilledRow(t *testing.T) {
	r, db, reg := setup(t)
	seedApp(t, db, reg, 30, "prebackfill", "s3", nil)

	c, err := r.Require(appauth.WithTenant(context.Background(), "prebackfill"))
	require.NoError(t, err)
	assert.Equal(t, int64(30), c.App.ID)

	apps, err := dal.ListApps(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, apps, 1, "no duplicate row for an app_key-equal trusted key")
}

// TestRequireTrustedRevivesSoftDeletedOccupant pins the F2 hardening: a
// soft-deleted config row occupying the tenant's unique keys hit the narrow
// ON CONFLICT (app_key) DO NOTHING edge — an app_key-equal occupant made the
// insert no-op and the scoped re-read miss ("row absent after insert"
// INTERNAL), while a tenant_key-holding occupant under a DIFFERENT app_key
// raised a raw unique-violation 500 (the conflict target did not cover
// uniq_msg_apps_tenant_key). Ensure semantics now revive the occupant in
// place: un-delete, re-point tenant_key, keep the historic identity fields
// (app_key, secret, name, daily limits).
func TestRequireTrustedRevivesSoftDeletedOccupant(t *testing.T) {
	r, db, _ := setup(t)
	const tenantKey = "ten_dead0000000"

	// Occupant shape 1: app_key = tenant_key (the conflict-target shape the
	// narrow ON CONFLICT suppressed).
	dead := &models.MessageApp{
		AppKey: tenantKey, AppSecret: "old-secret", Name: "old name",
		TenantKey: models.TenantKeyPtr(tenantKey), SMSDailyLimit: 7, EmailDailyLimit: 9,
	}
	require.NoError(t, db.Create(dead).Error)
	require.NoError(t, db.Model(&models.MessageApp{}).Where("id = ?", dead.ID).
		Update("deleted_at", time.Now()).Error)

	c, err := r.Require(appauth.WithTenant(context.Background(), tenantKey))
	require.NoError(t, err, "a soft-deleted occupant must be revived, not 500")
	require.Equal(t, tenantKey, c.TenantKey)
	require.Equal(t, dead.ID, c.App.ID, "the occupant row is revived in place")
	require.Equal(t, "old-secret", c.App.AppSecret, "historic secret kept (revive ≠ re-mint)")
	require.Equal(t, int64(7), c.App.SMSDailyLimit, "historic daily limits kept")

	var deletedCount int64
	require.NoError(t, db.Unscoped().Model(&models.MessageApp{}).
		Where("id = ? AND deleted_at IS NOT NULL", dead.ID).Count(&deletedCount).Error)
	require.Zero(t, deletedCount, "row must be live again")
}

// TestRequireTrustedRevivesSoftDeletedMappedOccupant: occupant shape 2 — a
// soft-deleted row holding tenant_key under a DIFFERENT app_key (the shape
// the narrow conflict target turned into a unique-violation 500).
func TestRequireTrustedRevivesSoftDeletedMappedOccupant(t *testing.T) {
	r, db, _ := setup(t)
	const tenantKey = "ten_mapped000000"

	dead := &models.MessageApp{
		AppKey: "msg_oldalias1", AppSecret: "s", Name: "old",
		TenantKey: models.TenantKeyPtr(tenantKey),
	}
	require.NoError(t, db.Create(dead).Error)
	require.NoError(t, db.Model(&models.MessageApp{}).Where("id = ?", dead.ID).
		Update("deleted_at", time.Now()).Error)

	c, err := r.Require(appauth.WithTenant(context.Background(), tenantKey))
	require.NoError(t, err, "the tenant_key unique-violation edge must revive, not 500")
	require.Equal(t, dead.ID, c.App.ID)
	require.Equal(t, tenantKey, models.TenantKeyOf(c.App.TenantKey))
}

// TestRequireFailureModes: bad secret / unknown app / no credentials /
// disabled config row all fail closed with ErrAppUnauthorized.
func TestRequireFailureModes(t *testing.T) {
	r, db, reg := setup(t)
	seedApp(t, db, reg, 40, "known", "right", nil)

	_, err := r.Require(appauth.WithApp(context.Background(), "known", "wrong"))
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	_, err = r.Require(appauth.WithApp(context.Background(), "ghost", "whatever"))
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	_, err = r.Require(context.Background())
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New())

	ctx := appauth.WithTenant(context.Background(), "ten_dead0000000")
	c, err := r.Require(ctx)
	require.NoError(t, err)
	c.App.Disabled = true
	require.NoError(t, dal.UpdateApp(context.Background(), db, c.App))
	require.NoError(t, r.reg.Refresh(context.Background()))
	_, err = r.Require(ctx)
	assert.ErrorIs(t, err, xcodes.ErrAppUnauthorized.New(), "a disabled config row fails closed")
}
