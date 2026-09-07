package service

import (
	"fmt"

	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"

	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/option"

	"github.com/servekit/go-common/lifecycle"
)

// --- resource resolution (DI or build) ---

// resolveGID returns the gid dependency. Construction delegates to
// gidservice.Connect, which owns the mode switch and lifecycle registration;
// only the adoption of a parent-injected Handler stays here — it reads this
// service's own options and the parent owns that lifecycle.
func resolveGID(o *option.Options, cfg *config.RemoteServiceConfig[*gidconfig.Config], mgr *lifecycle.Manager) (gidservice.Service, error) {
	// Injected handler takes precedence (a parent shares its gid Handler),
	// even if cfg is nil (no ThirdParty.GID configured).
	if o.GIDHandler != nil {
		return o.GIDHandler, nil // injected → borrowed; parent owns lifecycle
	}
	if cfg == nil {
		return nil, fmt.Errorf("third_party.gid: not configured")
	}
	gid, _, err := gidservice.Connect(gidservice.ConnectConfig{
		Mode:   cfg.Mode,
		Target: cfg.Target,
		Config: cfg.Config,
	}, mgr)
	return gid, err
}
