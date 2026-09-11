package handler

import (
	"fmt"
	"log/slog"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	"github.com/servekit/message-service/internal/store/models"
)

// tableName resolves the physical table name for a logical (unprefixed)
// table on db, prefix-aware: convention-named tables carry the naming
// strategy's TablePrefix exactly as AutoMigrate creates them (message's
// models have no custom TableName() overrides). The post-migrate raw SQL
// must go through this so a prefixed database (dbx TablePrefix) converges
// identically to a bare one — the bare-name pattern this replaces had
// recurred in every migrate mirror.
func tableName(db *gorm.DB, name string) string {
	return db.NamingStrategy.TableName(name)
}

// Migrate applies the current schema to db via GORM AutoMigrate, then runs
// the phase ③ tenant_key post-migration (backfill + reconcile + guarded
// drop — the same procedure as deploy/phase3-message-tenant-key.sql, so
// `make migrate` alone re-keys a pre-③ database; fresh DBs no-op).
//
// Single migration entry point for message-service: the `migrate` subcommand
// (cmd/server) and embedders that inject a parent db (NewModule +
// option.WithDB) both call it, so tables are created regardless of how the
// service runs. pkg re-exports it as pkg.Migrate.
//
// AutoMigrate creates missing tables/columns/indexes but never drops unused
// ones — when a column is removed from a model, dev DBs are recreated via
// testcontainer rather than migrated in place.
//
// Embedders migrate on the parent db before constructing the module:
//
//	messageservice.Migrate(parentDB)
//	hdl, err := messageservice.NewModule(cfg, option.WithDB(parentDB))
func Migrate(db *gorm.DB) error {
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	if err := postMigrateTenantKey(db); err != nil {
		return fmt.Errorf("post-migrate tenant_key: %w", err)
	}
	if err := postMigrateDropLegacy(db); err != nil {
		return fmt.Errorf("post-migrate legacy drops: %w", err)
	}
	return nil
}

// postMigrateTenantKey mirrors deploy/phase3-message-tenant-key.sql after
// AutoMigrate has added the columns/indexes (D-③4: add → backfill →
// reconcile → guarded drop). AutoMigrate never drops the superseded
// (app_id, channel, scene) composite — the guarded drop here does, and only
// once the replacement index exists. Old code running against the migrated
// schema keeps working; rows it writes without tenant_key are healed by the
// next run's backfill (registry load also resolves them through the app
// mapping in the meantime).
func postMigrateTenantKey(db *gorm.DB) error {
	// QF1008 false positive: Dialector is an interface-typed field, Name is
	// its method — the selector cannot be removed.
	//nolint:staticcheck // gorm.DB.Dialector is an interface field, not embedding
	if db.Dialector.Name() != "postgres" {
		// Non-PG dev dialects (sqlite testcontainers are PG here; MySQL
		// deployments run the deploy SQL) — indexes already come from
		// AutoMigrate; nothing to backfill on a fresh DB.
		return nil
	}

	apps := tableName(db, "message_apps")
	templates := tableName(db, "message_templates")
	policies := tableName(db, "message_policies")

	// Backfill (idempotent: NULL rows only).
	if err := db.Exec(fmt.Sprintf(
		`UPDATE %s SET tenant_key = app_key WHERE tenant_key IS NULL`, apps)).Error; err != nil {
		return fmt.Errorf("backfill message_apps: %w", err)
	}
	// The pointer-driven backfills and reconciles can only run while the
	// legacy app_id column still exists (④ drops it after its own converge
	// check; fresh post-④ databases never carry it — nothing to backfill).
	hasAppID, err := columnExists(db, templates, "app_id")
	if err != nil {
		return err
	}
	if hasAppID {
		if err := db.Exec(fmt.Sprintf(`UPDATE %s t SET tenant_key = a.tenant_key
			FROM %s a
			WHERE t.app_id = a.id AND t.app_id <> 0 AND t.tenant_key IS NULL`, templates, apps)).Error; err != nil {
			return fmt.Errorf("backfill message_templates: %w", err)
		}
		if err := db.Exec(fmt.Sprintf(`UPDATE %s p SET tenant_key = a.tenant_key
			FROM %s a
			WHERE p.app_id = a.id AND p.tenant_key IS NULL`, policies, apps)).Error; err != nil {
			return fmt.Errorf("backfill message_policies: %w", err)
		}
	}

	// Reconcile: every row that must carry a tenant_key does.
	if err := reconcileTenantKey(db, "message_apps",
		fmt.Sprintf(`SELECT count(*), count(tenant_key) FROM %s`, apps)); err != nil {
		return err
	}
	if hasAppID {
		if err := reconcileTenantKey(db, "message_templates",
			fmt.Sprintf(`SELECT count(*) FILTER (WHERE app_id <> 0), count(tenant_key) FILTER (WHERE app_id <> 0) FROM %s`, templates)); err != nil {
			return err
		}
		if err := reconcileTenantKey(db, "message_policies",
			fmt.Sprintf(`SELECT count(*), count(tenant_key) FROM %s`, policies)); err != nil {
			return err
		}
	}

	// Guarded drop of the superseded composite (only when the replacement
	// index exists — AutoMigrate created it from the model tags).
	var hasNew int64
	if err := db.Raw(`SELECT count(*) FROM pg_indexes
		WHERE tablename = ? AND indexname = 'uniq_msg_policy_tenant_ch_scene'`, policies).
		Scan(&hasNew).Error; err != nil {
		return fmt.Errorf("check replacement index: %w", err)
	}
	if hasNew == 0 {
		return fmt.Errorf("refusing to drop uniq_msg_policy_app_ch_scene: replacement uniq_msg_policy_tenant_ch_scene missing")
	}
	if err := db.Exec(`DROP INDEX IF EXISTS uniq_msg_policy_app_ch_scene`).Error; err != nil {
		return fmt.Errorf("drop superseded policy index: %w", err)
	}
	slog.Info("migrate: phase3 tenant_key post-migration complete")
	return nil
}

// columnExists reports whether the physical table carries the column.
func columnExists(db *gorm.DB, table, column string) (bool, error) {
	var exists bool
	if err := db.Raw(
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = ? AND column_name = ?)`,
		table, column,
	).Scan(&exists).Error; err != nil {
		return false, fmt.Errorf("check column %s.%s: %w", table, column, err)
	}
	return exists, nil
}

// postMigrateDropLegacy closes the ④ window on the data side
// (deploy/phase4-drop-legacy.sql performs the identical procedure by hand):
// after the ③ tenant_key re-keying converged, drop the legacy app_id
// pointers on templates/policies and the retired message_apps.app_secret
// credential column. Policies reconcile (count(tenant_key) = count(*)) and
// their replacement index must exist before the pointer goes; templates
// keep NULL tenant_key rows (shared semantics), so their drop is guarded on
// the ③ backfill only. DROP … IF EXISTS keeps it idempotent on fresh
// databases (testcontainers never carry the columns).
func postMigrateDropLegacy(db *gorm.DB) error {
	//nolint:staticcheck // gorm.DB.Dialector is an interface field, not embedding
	if db.Dialector.Name() != "postgres" {
		return nil
	}
	templates := tableName(db, "message_templates")
	policies := tableName(db, "message_policies")
	apps := tableName(db, "message_apps")

	if hasPolicies, err := columnExists(db, policies, "app_id"); err != nil {
		return err
	} else if hasPolicies {
		var hasNew int64
		if err := db.Raw(`SELECT count(*) FROM pg_indexes
			WHERE tablename = ? AND indexname = 'uniq_msg_policy_tenant_ch_scene'`, policies).
			Scan(&hasNew).Error; err != nil {
			return fmt.Errorf("check replacement index: %w", err)
		}
		if hasNew == 0 {
			return fmt.Errorf("refusing to drop message_policies.app_id: replacement uniq_msg_policy_tenant_ch_scene missing")
		}
		var total, filled int64
		if err := db.Raw(fmt.Sprintf(
			`SELECT count(*), count(tenant_key) FROM %s`, policies)).Row().Scan(&total, &filled); err != nil {
			return fmt.Errorf("reconcile message_policies: %w", err)
		}
		if filled != total {
			return fmt.Errorf("refusing to drop message_policies.app_id: %d of %d rows carry tenant_key", filled, total)
		}
	}
	if err := db.Exec(`DROP INDEX IF EXISTS idx_message_templates_app_id`).Error; err != nil {
		return fmt.Errorf("drop superseded template index: %w", err)
	}
	if err := db.Exec(`DROP INDEX IF EXISTS uniq_msg_policy_app_ch_scene`).Error; err != nil {
		return fmt.Errorf("drop superseded policy index: %w", err)
	}
	if err := db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS app_id`, templates)).Error; err != nil {
		return fmt.Errorf("drop message_templates.app_id: %w", err)
	}
	if err := db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS app_id`, policies)).Error; err != nil {
		return fmt.Errorf("drop message_policies.app_id: %w", err)
	}
	if err := db.Exec(fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS app_secret`, apps)).Error; err != nil {
		return fmt.Errorf("drop message_apps.app_secret: %w", err)
	}
	slog.Info("migrate: phase4 legacy-column drops complete")
	return nil
}

// reconcileTenantKey fails when filled != total for the given probe query.
func reconcileTenantKey(db *gorm.DB, table, probe string) error {
	var total, filled int64
	if err := db.Raw(probe).Row().Scan(&total, &filled); err != nil {
		return fmt.Errorf("reconcile %s: %w", table, err)
	}
	if filled != total {
		return fmt.Errorf("reconcile %s: %d of %d rows carry tenant_key (rows referencing a missing message_apps.id keep NULL)",
			table, filled, total)
	}
	slog.Info("migrate: phase3 tenant_key reconcile ok", "table", table, "total", total, "filled", filled)
	return nil
}
