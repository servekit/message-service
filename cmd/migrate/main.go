// Package main runs message-service database migrations via GORM AutoMigrate.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"

	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/config"

	"gorm.io/gorm"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		os.Exit(1)
	}

	logging.Setup(cfg.Log)

	db, err := dbx.New(cfg.Database)
	if err != nil {
		slog.Error("init database", "error", err)
		os.Exit(1)
	}

	if err := runMigration(db); err != nil {
		slog.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}

// runMigration applies the current schema via GORM AutoMigrate.
//
// AutoMigrate creates missing tables/columns/indexes but never drops unused
// ones — when a column is removed from a model, dev DBs are recreated via
// testcontainer rather than migrated in place.
func runMigration(db *gorm.DB) error {
	if err := dbx.AutoMigrate(db, models.AllModels()...); err != nil {
		return fmt.Errorf("auto-migrate: %w", err)
	}
	return nil
}
