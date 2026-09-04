package service

import (
	"fmt"

	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"

	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/option"

	"github.com/servekit/go-common/lifecycle"
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
