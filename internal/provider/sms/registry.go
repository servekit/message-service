package sms

import (
	"fmt"
	"sort"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// Config is the cooked config: vendor keys and route target vendors are proto
// enums, parsed from YAML strings by ParseVendorName during config.Load
// (fail-fast on unknown vendor).
//
// DefaultCountry is used by BuildRouter as the default country when parsing
// phone numbers without an international prefix (e.g., "13800138000" with
// DefaultCountry "CN" parses as China).
type Config struct {
	DefaultCountry string `default:"CN"` // ISO 3166-1 alpha-2, default "CN"
	Vendors        map[pb.SmsVendor]*VendorConfig
	Routes         []*RouteConfig
}

// VendorConfig holds accounts under one vendor (e.g., ALIYUN).
type VendorConfig struct {
	Accounts []*AccountConfig
}

// RouteConfig is one country's route targets.
type RouteConfig struct {
	Country string // ISO 3166-1 alpha-2, or "*" for fallback
	Targets []*RouteTarget
}

// RouteTarget references a (vendor, account) pair already defined in Vendors.
type RouteTarget struct {
	Vendor  pb.SmsVendor
	Account string
}

// AccountConfig is a single named SMS account. Each vendor's config lives in
// its own struct (defined in the vendor impl file, e.g. AliyunConfig in
// aliyun.go). AccountConfig holds pointers to whichever vendor configs are
// relevant; buildProvider picks the right one based on the parent vendor enum.
//
// Adding a new vendor = add a `<Vendor>Config` field here + a case in
// buildProvider + the vendor impl file. No fat-struct field bloat.
type AccountConfig struct {
	Name       string
	Aliyun     *AliyunConfig
	Tencent    *TencentConfig
	Volcengine *VolcengineConfig
	Byteplus   *ByteplusConfig
	Huawei     *HuaweiConfig
}

// AccountRegistry indexes AccountProviders by (vendor, account) and exposes
// both a default fallback sender and per-account senders.
type AccountRegistry struct {
	vendors map[pb.SmsVendor]map[string]AccountProvider
	def     *Sender
}

// NewAccountRegistryFromProviders builds a registry from a pre-built provider
// map keyed by proto enum. The default fallback chain is ordered by vendor
// enum value asc, then account name asc.
func NewAccountRegistryFromProviders(vendors map[pb.SmsVendor]map[string]AccountProvider) *AccountRegistry {
	r := &AccountRegistry{
		vendors: vendors,
	}
	r.def = NewSender(flattenProviders(vendors))
	return r
}

// NewAccountRegistry constructs a registry from cooked config (enum-keyed
// Vendors map). Vendor name strings have already been validated and converted
// to enums by config.Load via ParseVendorName.
//
// Behavior:
//   - Iterates each vendor, calling the corresponding constructor.
//   - Each constructed AccountProvider is stored directly (vendor impls
//     carry their own Vendor/Account identity — no external wrapper).
//   - Duplicate account names within the same vendor are rejected.
//   - Provider construction failures return an error.
//
// DefaultCountry field is preserved for BuildRouter (see Config comment).
func NewAccountRegistry(cfg *Config) (*AccountRegistry, error) {
	vendors := make(map[pb.SmsVendor]map[string]AccountProvider)
	if cfg == nil {
		return NewAccountRegistryFromProviders(vendors), nil
	}

	for vendorEnum, vc := range cfg.Vendors {
		accounts := make(map[string]AccountProvider)
		for _, ac := range vc.Accounts {
			if _, dup := accounts[ac.Name]; dup {
				return nil, fmt.Errorf("sms: duplicate account name %q under vendor %s", ac.Name, vendorEnum)
			}
			p, err := buildProvider(vendorEnum, ac)
			if err != nil {
				return nil, fmt.Errorf("sms: account %s/%s: %w", vendorEnum, ac.Name, err)
			}
			accounts[ac.Name] = p
		}
		vendors[vendorEnum] = accounts
	}

	return NewAccountRegistryFromProviders(vendors), nil
}

// DefaultSender returns the fallback sender containing all providers in the
// order determined at construction time.
func (r *AccountRegistry) DefaultSender() *Sender {
	return r.def
}

// SenderFor selects a sender based on vendor+account.
func (r *AccountRegistry) SenderFor(vendor pb.SmsVendor, account string) (*Sender, error) {
	if vendor == pb.SmsVendor_SMS_VENDOR_UNSPECIFIED && account == "" {
		return r.def, nil
	}
	if vendor == pb.SmsVendor_SMS_VENDOR_UNSPECIFIED || account == "" {
		return nil, fmt.Errorf("sms: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("unknown sms vendor %s", vendor)
	}
	p, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("unknown sms account %q under vendor %s", account, vendor)
	}
	return NewSender([]AccountProvider{p}), nil
}

// --- internal helpers ---

// lookup returns the AccountProvider for (vendor, account). Used by BuildRouter
// to resolve route targets at startup.
func (r *AccountRegistry) lookup(vendor pb.SmsVendor, account string) (AccountProvider, error) {
	if vendor == pb.SmsVendor_SMS_VENDOR_UNSPECIFIED || account == "" {
		return nil, fmt.Errorf("sms: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("unknown sms vendor %s", vendor)
	}
	ap, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("unknown sms account %q under vendor %s", account, vendor)
	}
	return ap, nil
}

// buildProvider dispatches to the corresponding constructor based on vendor
// enum and returns the constructed AccountProvider.
func buildProvider(vendor pb.SmsVendor, ac *AccountConfig) (AccountProvider, error) {
	switch vendor {
	case pb.SmsVendor_SMS_VENDOR_ALIYUN:
		if ac.Aliyun == nil {
			return nil, fmt.Errorf("sms vendor %s: aliyun config missing", vendor)
		}
		return NewAliyunProvider(ac.Name, ac.Aliyun)
	case pb.SmsVendor_SMS_VENDOR_TENCENT:
		if ac.Tencent == nil {
			return nil, fmt.Errorf("sms vendor %s: tencent config missing", vendor)
		}
		return NewTencentProvider(ac.Name, ac.Tencent)
	case pb.SmsVendor_SMS_VENDOR_VOLCENGINE:
		if ac.Volcengine == nil {
			return nil, fmt.Errorf("sms vendor %s: volcengine config missing", vendor)
		}
		return NewVolcengineProvider(ac.Name, ac.Volcengine)
	case pb.SmsVendor_SMS_VENDOR_BYTEPLUS:
		if ac.Byteplus == nil {
			return nil, fmt.Errorf("sms vendor %s: byteplus config missing", vendor)
		}
		return NewByteplusProvider(ac.Name, ac.Byteplus)
	case pb.SmsVendor_SMS_VENDOR_HUAWEI:
		if ac.Huawei == nil {
			return nil, fmt.Errorf("sms vendor %s: huawei config missing", vendor)
		}
		return NewHuaweiProvider(ac.Name, ac.Huawei)
	default:
		return nil, fmt.Errorf("unknown vendor %s", vendor)
	}
}

// flattenProviders expands the nested map into a flat slice ordered by vendor
// enum value asc, then account name asc.
func flattenProviders(vendors map[pb.SmsVendor]map[string]AccountProvider) []AccountProvider {
	vendorEnums := make([]pb.SmsVendor, 0, len(vendors))
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
