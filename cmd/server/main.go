// Package main is the message-service binary entrypoint: it loads config,
// constructs the server, and runs it under signalx until shutdown.
package main

import (
	"log/slog"
	"os"

	messageservice "github.com/servekit/message-service/pkg"
	"github.com/servekit/message-service/pkg/config"

	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/signalx"
)

func main() {
	os.Exit(run())
}

// run does the actual work and returns the process exit code.
//
// signalx.RunWithForceQuit panics when Server.Start fails (by design —
// see go-common/signalx/signalx.go). The deferred recover logs the panic
// and surfaces a non-zero exit code instead of a raw stack trace.
func run() (code int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("server panicked", "panic", r)
			code = 1
		}
	}()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", err)
		return 1
	}
	logging.Setup(cfg.Log)

	srv, err := messageservice.NewServer(cfg)
	if err != nil {
		slog.Error("init server", "error", err)
		return 1
	}

	if err := signalx.RunWithForceQuit(srv); err != nil {
		slog.Error("run server", "error", err)
		return 1
	}
	return 0
}
