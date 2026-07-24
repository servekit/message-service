// Package thirdcall wires message-service to external services via go-common
// clients (currently just gid-service).
package thirdcall

import (
	"context"

	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/servekit/message-service/internal/thirdcall/gid_service"
	"github.com/servekit/message-service/pkg/config"
)

// GIDService generates globally unique IDs via gid-service.
type GIDService interface {
	NextID(ctx context.Context) (int64, error)
}

// NewGIDService creates a GIDService based on config mode.
func NewGIDService(cfg *config.RemoteServiceConfig[*gidconfig.Config]) (GIDService, error) {
	switch cfg.Mode {
	case "grpc":
		return gid_service.NewGRPC(cfg.Target)
	default:
		return gid_service.NewModule(cfg.Config)
	}
}
