package handler

import (
	"testing"

	"github.com/servekit/go-common/dbx"

	"github.com/stretchr/testify/require"
)

// TestMigrate_Idempotent verifies a second run on an already-migrated DB
// is a no-op.
func TestMigrate_Idempotent(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)

	require.NoError(t, Migrate(db))
	require.NoError(t, Migrate(db),
		"re-running migrate on a clean DB must not error")
}

// TestMigrate_RekeysPrePhase3Database simulates a pre-③ database (no
// tenant_key columns, old (app_id,channel,scene) composite) and verifies
// Migrate alone re-keys it: columns re-added, rows backfilled (apps →
// app_key literal; app-owned templates/policies → the app's mapping;
// app_id=0 templates stay NULL/shared), superseded composite dropped.
func TestMigrate_RekeysPrePhase3Database(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, Migrate(db))

	// rewind to the pre-③ shape
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS uniq_msg_policy_tenant_ch_scene`,
		`DROP INDEX IF EXISTS uniq_msg_apps_tenant_key`,
		`ALTER TABLE message_policies DROP COLUMN IF EXISTS tenant_key`,
		`ALTER TABLE message_templates DROP COLUMN IF EXISTS tenant_key`,
		`ALTER TABLE message_apps DROP COLUMN IF EXISTS tenant_key`,
		`CREATE UNIQUE INDEX uniq_msg_policy_app_ch_scene ON message_policies (app_id, channel, scene)`,
		// legacy rows: one app, a shared template, an owned template, a policy
		`INSERT INTO message_apps (id, app_key, app_secret, name, disabled, sms_daily_limit, email_daily_limit)
		   VALUES (5001, 'legacy-app', 's', 'legacy-app', false, 0, 0)`,
		`INSERT INTO message_templates (id, app_id, name, channel, kind, disabled)
		   VALUES (8001, 0, 'shared', 1, 1, false), (8002, 5001, 'owned', 1, 1, false)`,
		`INSERT INTO message_policies (id, app_id, channel, scene, template_id, disabled)
		   VALUES (9001, 5001, 1, 1, 8002, false)`,
	} {
		require.NoError(t, db.Exec(stmt).Error, stmt)
	}

	require.NoError(t, Migrate(db))

	var appTenant *string
	require.NoError(t, db.Raw(`SELECT tenant_key FROM message_apps WHERE id = 5001`).Row().Scan(&appTenant))
	require.NotNil(t, appTenant)
	require.Equal(t, "legacy-app", *appTenant, "app rows backfill to the app_key literal")

	var shared, owned, policyTenant *string
	require.NoError(t, db.Raw(`SELECT tenant_key FROM message_templates WHERE id = 8001`).Row().Scan(&shared))
	require.Nil(t, shared, "app_id=0 template stays NULL (shared)")
	require.NoError(t, db.Raw(`SELECT tenant_key FROM message_templates WHERE id = 8002`).Row().Scan(&owned))
	require.NotNil(t, owned)
	require.Equal(t, "legacy-app", *owned)
	require.NoError(t, db.Raw(`SELECT tenant_key FROM message_policies WHERE id = 9001`).Row().Scan(&policyTenant))
	require.NotNil(t, policyTenant)
	require.Equal(t, "legacy-app", *policyTenant)

	var oldIdx, newIdx int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM pg_indexes WHERE tablename = 'message_policies' AND indexname = 'uniq_msg_policy_app_ch_scene'`).Scan(&oldIdx).Error)
	require.Zero(t, oldIdx, "superseded composite dropped")
	require.NoError(t, db.Raw(`SELECT count(*) FROM pg_indexes WHERE tablename = 'message_policies' AND indexname = 'uniq_msg_policy_tenant_ch_scene'`).Scan(&newIdx).Error)
	require.Equal(t, int64(1), newIdx)

	require.NoError(t, Migrate(db), "re-run after re-keying is a no-op")
}

// TestMigrate_ReconcileAbortsOnDanglingReference: a policy row whose app_id
// points at no app keeps NULL tenant_key; migrate must refuse (reconcile)
// rather than silently drop the old composite.
func TestMigrate_ReconcileAbortsOnDanglingReference(t *testing.T) {
	db := dbx.SetupTestDB(t, dbx.DriverPostgres)
	require.NoError(t, Migrate(db))

	require.NoError(t, db.Exec(`INSERT INTO message_policies (id, app_id, channel, scene, template_id, disabled)
		VALUES (9900, 424242, 1, 1, 0, false)`).Error)

	err := Migrate(db)
	require.Error(t, err)
	require.Contains(t, err.Error(), "reconcile message_policies")
}
