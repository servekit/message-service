package sms

import (
	"fmt"
)

// BuildRouter constructs a Router from Config + AccountRegistry. Each RouteTarget
// must reference a (vendor, account) already defined in Config.Vendors; otherwise
// construction fails (fail-fast at startup).
//
// The country "*" is treated as the default fallback chain — its targets become
// the Router's defaultTargets. Other countries become Route entries. If multiple
// "*" entries exist, the last one wins (undefined behavior — treat as config error).
//
// Returns (nil, nil) if cfg is nil or cfg.Routes is empty — caller decides
// whether that's an error (sendSMS path treats empty routes as misconfiguration
// and rejects vendor/account-empty requests with BadRequest).
func BuildRouter(cfg *Config, reg *AccountRegistry) (*Router, error) {
	if cfg == nil || len(cfg.Routes) == 0 {
		return nil, nil
	}

	defaultCountry := cfg.DefaultCountry
	if defaultCountry == "" {
		defaultCountry = "CN"
	}

	var defaultTargets []AccountProvider
	routes := make([]Route, 0, len(cfg.Routes))

	for _, rc := range cfg.Routes {
		targets := make([]AccountProvider, 0, len(rc.Targets))
		for _, t := range rc.Targets {
			ap, err := reg.lookup(t.Vendor, t.Account)
			if err != nil {
				return nil, fmt.Errorf("sms: route %s: %w", rc.Country, err)
			}
			targets = append(targets, ap)
		}
		if rc.Country == "*" {
			defaultTargets = targets
			continue
		}
		routes = append(routes, Route{Country: rc.Country, Targets: targets})
	}

	return NewRouter(defaultCountry, defaultTargets, routes...), nil
}
