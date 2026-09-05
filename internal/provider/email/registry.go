package email

import (
	"fmt"
	"sort"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// Config is the email provider configuration. Each account carries its own
// vendor (brand label, parsed at registry construction) plus SMTP fields.
//
// YAML loads directly into this type via pkg/config — no intermediate
// cooked form needed because Vendor is a string.
type Config struct {
	Accounts []*AccountConfig
}

// AccountConfig is one SMTP account plus its brand label. Vendor selects
// audit identity and (vendor, account) lookup; the underlying transport is
// always SMTP.
//
// Vendor name in YAML must match parseVendorName's accepted values
// ("aliyun", "tencent", "netease"). Unknown names rejected
// at registry construction.
type AccountConfig struct {
	Name     string
	Vendor   string
	Host     string
	Port     int `default:"587"`
	Username string
	Password string
	From     string
}

// AccountRegistry indexes AccountProviders by (vendor, account) and exposes
// both a default fallback sender and per-account senders.
type AccountRegistry struct {
	vendors map[pb.EmailVendor]map[string]AccountProvider
	def     *Sender
}

// NewAccountRegistryFromProviders builds a registry from a pre-built provider
// map keyed by proto enum. The default fallback chain is ordered by vendor
// enum value asc, then account name asc.
//
// Primarily for testing. Production code uses NewAccountRegistry.
func NewAccountRegistryFromProviders(vendors map[pb.EmailVendor]map[string]AccountProvider) *AccountRegistry {
	r := &AccountRegistry{vendors: vendors}
	r.def = NewSender(flattenProviders(vendors))
	return r
}

// NewAccountRegistry constructs a registry from Config. Each account's
// Vendor string is parsed to enum (fail-fast on unknown). Duplicate
// (vendor, account) pairs are rejected. Provider construction failures
// return an error.
//
// The default fallback chain is ordered by vendor enum value asc, then
// account name asc (guaranteed by NewAccountRegistryFromProviders).
func NewAccountRegistry(cfg *Config) (*AccountRegistry, error) {
	vendors := make(map[pb.EmailVendor]map[string]AccountProvider)
	if cfg == nil {
		return NewAccountRegistryFromProviders(vendors), nil
	}

	for _, ac := range cfg.Accounts {
		vendor, err := parseVendorName(ac.Vendor)
		if err != nil {
			return nil, fmt.Errorf("email: account %s: %w", ac.Name, err)
		}
		accounts := vendors[vendor]
		if accounts == nil {
			accounts = make(map[string]AccountProvider)
			vendors[vendor] = accounts
		}
		if _, dup := accounts[ac.Name]; dup {
			return nil, fmt.Errorf("email: duplicate account %q under vendor %s", ac.Name, ac.Vendor)
		}
		p, err := buildProvider(vendor, ac)
		if err != nil {
			return nil, fmt.Errorf("email: account %s/%s: %w", ac.Vendor, ac.Name, err)
		}
		accounts[ac.Name] = p
	}

	return NewAccountRegistryFromProviders(vendors), nil
}

// DefaultSender returns the fallback sender containing all providers in
// construction-determined order.
func (r *AccountRegistry) DefaultSender() *Sender { return r.def }

// SenderFor selects a sender based on vendor+account.
//
//   - vendor == UNSPECIFIED && account == "" → DefaultSender (fallback chain)
//   - both set → sender with only that provider (no fallback)
//   - only one set → error (vendor and account must be set together)
//   - unknown vendor → error
//   - unknown account → error
//
// Design tradeoff: no fallback when vendor+account is specified. The caller
// explicitly chose a specific account; semantically "use this account, fail
// if it fails" — easier to debug and audit.
func (r *AccountRegistry) SenderFor(vendor pb.EmailVendor, account string) (*Sender, error) {
	if vendor == pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED && account == "" {
		return r.def, nil
	}
	if vendor == pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED || account == "" {
		return nil, fmt.Errorf("email: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("unknown email vendor %q", vendor)
	}
	p, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("unknown email account %q under vendor %q", account, vendor)
	}
	return NewSender([]AccountProvider{p}), nil
}

// --- internal helpers ---

// parseVendorName converts a YAML vendor string (e.g. "aliyun") to its proto
// enum. Single source of truth for the string→enum mapping. Unknown strings
// fail-fast here at registry construction, before any provider is built.
func parseVendorName(s string) (pb.EmailVendor, error) {
	switch s {
	case "aliyun":
		return pb.EmailVendor_EMAIL_VENDOR_ALIYUN, nil
	case "tencent":
		return pb.EmailVendor_EMAIL_VENDOR_TENCENT, nil
	case "netease":
		return pb.EmailVendor_EMAIL_VENDOR_NETEASE, nil
	default:
		return 0, fmt.Errorf("unknown email vendor %q", s)
	}
}

// buildProvider constructs the AccountProvider for one account. All accounts
// use SMTP today — vendor is brand metadata only. When API support lands,
// this regains a switch (on protocol, not vendor).
func buildProvider(vendor pb.EmailVendor, ac *AccountConfig) (AccountProvider, error) {
	if ac.Host == "" {
		return nil, fmt.Errorf("host is required")
	}
	return NewSMTPProvider(vendor, ac.Name, &SMTPConfig{
		Host:     ac.Host,
		Port:     ac.Port,
		Username: ac.Username,
		Password: ac.Password,
		From:     ac.From,
	})
}

// flattenProviders expands the nested map into a flat slice ordered by vendor
// enum value asc, then account name asc. The sort is deterministic so the
// default fallback chain stays stable across runs.
func flattenProviders(vendors map[pb.EmailVendor]map[string]AccountProvider) []AccountProvider {
	vendorEnums := make([]pb.EmailVendor, 0, len(vendors))
	for v := range vendors {
		vendorEnums = append(vendorEnums, v)
	}
	sort.Slice(vendorEnums, func(i, j int) bool { return vendorEnums[i] < vendorEnums[j] })

	var out []AccountProvider
	for _, v := range vendorEnums {
		accounts := vendors[v]
		acctNames := make([]string, 0, len(accounts))
		for a := range accounts {
			acctNames = append(acctNames, a)
		}
		sort.Strings(acctNames)
		for _, a := range acctNames {
			out = append(out, accounts[a])
		}
	}
	return out
}
