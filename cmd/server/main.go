// Command message-service is the message-service gRPC + HTTP entry point, and
// also hosts operational subcommands such as database migration.
//
// Usage:
//
//	message-service           # start the gRPC + HTTP server (default)
//	message-service serve     # same as above (explicit)
//	message-service migrate   # apply GORM AutoMigrate, then exit
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/joho/godotenv"
	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/signalx"

	pkg "github.com/servekit/message-service/pkg"
	"github.com/servekit/message-service/pkg/config"

	"github.com/servekit/message-service/internal/version"
)

func main() {
	// Load .env when present so local binary runs pick up the same values
	// docker-compose injects. Missing .env (docker/prod) is not an error.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "warning: failed to load .env:", err)
	}

	switch subcommand() {
	case "", "serve":
		if err := runServer(); err != nil {
			slog.Error("serve failed", "error", err)
			os.Exit(1)
		}
	case "migrate":
		if err := runMigrate(); err != nil {
			slog.Error("migrate failed", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [serve|migrate]\n", os.Args[0])
		os.Exit(2)
	}
}

// runServer loads config and starts the gRPC + HTTP server.
func runServer() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logging.Setup(cfg.Log)
	slog.Info("starting", "service", "message-service", "version", version.Get().String())

	srv, err := pkg.NewServer(cfg)
	if err != nil {
		return fmt.Errorf("init server: %w", err)
	}

	if err := signalx.RunWithForceQuit(srv); err != nil {
		return fmt.Errorf("run server: %w", err)
	}
	return nil
}

// --- internal helpers ---

// subcommand returns the first positional argument, or "" when none is given.
// An empty value means "start the server" (the default).
func subcommand() string {
	if len(os.Args) > 1 {
		return os.Args[1]
	}
	return ""
}
