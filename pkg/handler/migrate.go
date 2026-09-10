package handler

import (
	"fmt"
	"log/slog"

	"github.com/servekit/go-common/dbx"
	"gorm.io/gorm"

	"github.com/servekit/message-service/internal/store/models"
)

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

	// Backfill (idempotent: NULL rows only).
	if err := db.Exec(`UPDATE message_apps SET tenant_key = app_key WHERE tenant_key IS NULL`).Error; err != nil {
		return fmt.Errorf("backfill message_apps: %w", err)
	}
	if err := db.Exec(`UPDATE message_templates t SET tenant_key = a.tenant_key
		FROM message_apps a
		WHERE t.app_id = a.id AND t.app_id <> 0 AND t.tenant_key IS NULL`).Error; err != nil {
		return fmt.Errorf("backfill message_templates: %w", err)
	}
	if err := db.Exec(`UPDATE message_policies p SET tenant_key = a.tenant_key
		FROM message_apps a
		WHERE p.app_id = a.id AND p.tenant_key IS NULL`).Error; err != nil {
		return fmt.Errorf("backfill message_policies: %w", err)
	}

	// Reconcile: every row that must carry a tenant_key does.
	if err := reconcileTenantKey(db, "message_apps",
		`SELECT count(*), count(tenant_key) FROM message_apps`); err != nil {
		return err
	}
	if err := reconcileTenantKey(db, "message_templates",
		`SELECT count(*) FILTER (WHERE app_id <> 0), count(tenant_key) FILTER (WHERE app_id <> 0) FROM message_templates`); err != nil {
		return err
	}
	if err := reconcileTenantKey(db, "message_policies",
		`SELECT count(*), count(tenant_key) FROM message_policies`); err != nil {
		return err
	}

	// Guarded drop of the superseded composite (only when the replacement
	// index exists — AutoMigrate created it from the model tags).
	var hasNew int64
	if err := db.Raw(`SELECT count(*) FROM pg_indexes
		WHERE tablename = 'message_policies' AND indexname = 'uniq_msg_policy_tenant_ch_scene'`).
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
