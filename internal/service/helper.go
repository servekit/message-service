package service

import (
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"
	"gorm.io/gorm"

	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/servekit/message-service/internal/provider/sms"
	gid_service "github.com/servekit/message-service/internal/thirdcall/gid_service"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/option"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/redisx"
)

// --- config cooking (string → enum) ---

// cookSMSConfig converts the string-keyed SMSConfig (YAML-friendly) into
// the enum-keyed sms.Config that the registry and router consume. Unknown
// vendor names in either the Vendors map or route targets fail-fast here.
func cookSMSConfig(raw *config.SMSConfig) (*sms.Config, error) {
	if raw == nil {
		return nil, nil
	}
	vendors, err := cookSMSVendors(raw.Vendors)
	if err != nil {
		return nil, err
	}
	routes, err := cookSMSRoutes(raw.Routes)
	if err != nil {
		return nil, err
	}
	return &sms.Config{
		DefaultCountry: raw.DefaultCountry,
		Vendors:        vendors,
		Routes:         routes,
	}, nil
}

func cookSMSVendors(raw map[string]*sms.VendorConfig) (map[pb.SmsVendor]*sms.VendorConfig, error) {
	out := make(map[pb.SmsVendor]*sms.VendorConfig, len(raw))
	for name, vc := range raw {
		vendor, err := parseSMSVendorName(name)
		if err != nil {
			return nil, err
		}
		out[vendor] = vc
	}
	return out, nil
}

func cookSMSRoutes(raw []*config.RouteConfig) ([]*sms.RouteConfig, error) {
	out := make([]*sms.RouteConfig, 0, len(raw))
	for _, rc := range raw {
		targets := make([]*sms.RouteTarget, 0, len(rc.Targets))
		for _, t := range rc.Targets {
			vendor, err := parseSMSVendorName(t.Vendor)
			if err != nil {
				return nil, fmt.Errorf("route %s: %w", rc.Country, err)
			}
			targets = append(targets, &sms.RouteTarget{Vendor: vendor, Account: t.Account})
		}
		out = append(out, &sms.RouteConfig{Country: rc.Country, Targets: targets})
	}
	return out, nil
}

// parseSMSVendorName converts a YAML vendor string (e.g. "aliyun") to its
// proto enum. Valid keys: "aliyun", "tencent", "volcengine", "byteplus",
// "huawei".
func parseSMSVendorName(s string) (pb.SmsVendor, error) {
	switch s {
	case "aliyun":
		return pb.SmsVendor_SMS_VENDOR_ALIYUN, nil
	case "tencent":
		return pb.SmsVendor_SMS_VENDOR_TENCENT, nil
	case "volcengine":
		return pb.SmsVendor_SMS_VENDOR_VOLCENGINE, nil
	case "byteplus":
		return pb.SmsVendor_SMS_VENDOR_BYTEPLUS, nil
	case "huawei":
		return pb.SmsVendor_SMS_VENDOR_HUAWEI, nil
	default:
		return 0, fmt.Errorf("unknown vendor %q", s)
	}
}

// --- resource resolution (DI or build) ---

// resolveRedis returns the *redis.Client to use. If the caller injected one
// via WithRedis, it's returned as-is (caller owns lifecycle). Otherwise a new
// one is built from cfg and registered with mgr as a Stopper.
func resolveRedis(cfg *config.Config, injected *redis.Client, mgr *lifecycle.Manager) (*redis.Client, error) {
	if injected != nil {
		return injected, nil
	}
	client, err := redisx.New(cfg.Redis)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	mgr.AddStopper("redis", lifecycle.StopFunc(func() {
		if err := client.Close(); err != nil {
			slog.Warn("close redis", "error", err)
		}
	}))
	return client, nil
}

// resolveDB returns the *gorm.DB to use. If the caller injected one via
// WithDB, it's returned as-is (caller owns lifecycle). Otherwise a new one
// is built from cfg and registered with mgr as a Stopper.
func resolveDB(cfg *config.Config, injected *gorm.DB, mgr *lifecycle.Manager) (*gorm.DB, error) {
	if injected != nil {
		return injected, nil
	}
	db, err := dbx.New(cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	mgr.AddStopper("db", lifecycle.StopFunc(func() {
		sqlDB, err := db.DB()
		if err != nil {
			slog.Warn("get sql db for close", "error", err)
			return
		}
		if err := sqlDB.Close(); err != nil {
			slog.Warn("close db", "error", err)
		}
	}))
	return db, nil
}

// resolveGID returns the GIDService. grpc mode dials cfg.Target and registers
// a stopper (the GIDService's Close drops the connection); module mode uses an
// injected raw *gidservice.Handler (option.WithGIDHandler) when a parent embeds
// this service (parent owns lifecycle, nothing registered), otherwise builds one
// from cfg.Config (standalone) and registers the raw Handler with the Manager
// via mgr.Add (it owns the Handler's Start/Stop). The GIDService interface is
// internal.
func resolveGID(o *option.Options, cfg *config.RemoteServiceConfig[*gidconfig.Config], mgr *lifecycle.Manager) (gid_service.GIDService, error) {
	// Injected handler takes precedence (a parent shares its gid Handler),
	// even if cfg is nil (no ThirdParty.GID configured).
	if o.GIDHandler != nil {
		return gid_service.NewModule(o.GIDHandler), nil // injected → borrowed; parent owns lifecycle
	}
	if cfg == nil {
		return nil, fmt.Errorf("third_party.gid: not configured")
	}
	switch cfg.Mode {
	case "grpc":
		gid, err := gid_service.NewGRPC(cfg.Target)
		if err != nil {
			return nil, fmt.Errorf("init gid-service: %w", err)
		}
		mgr.AddStopper("gid", lifecycle.StopFunc(func() { _ = gid.Close() }))
		return gid, nil
	case "module":
		if cfg.Config == nil {
			return nil, fmt.Errorf("third_party.gid: module config required when no handler injected")
		}
		hdl, err := gidservice.NewModule(cfg.Config)
		if err != nil {
			return nil, fmt.Errorf("init gid-service: %w", err)
		}
		gid := gid_service.NewModule(hdl)
		mgr.Add("gid", hdl)
		return gid, nil
	default:
		return nil, fmt.Errorf("third_party.gid: unknown mode %q", cfg.Mode)
	}
}
