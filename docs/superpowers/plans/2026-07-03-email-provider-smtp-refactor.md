# Email Provider SMTP Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Flatten email provider config, drop `CUSTOM_SMTP` enum value (protocol masquerading as brand), move vendor parsing into the provider package, and remove the cook-config indirection — reflect SMTP-only reality.

**Architecture:** Each `AccountConfig` carries its own vendor (string) plus SMTP fields. `AccountRegistry` parses vendor at construction (fail-fast). The `pkg/config.EmailConfig` wrapper is removed; `service.New` passes `cfg.Email` directly to `email.NewAccountRegistry`. The cook step (`cookEmailConfig`) and `parseEmailVendorName` in `internal/service/setup.go` are deleted; their work moves into the email package as `parseVendorName`.

**Tech Stack:** Go 1.x, buf/protobuf, `github.com/wneessen/go-mail`, Postgres via testcontainers, testify.

**Spec:** `docs/superpowers/specs/2026-07-03-email-provider-smtp-refactor-design.md`

---

## File Structure

| File | Role | Operation |
|---|---|---|
| `api/proto/message/v1/message.proto` | Source of truth for `EmailVendor` enum | Drop `EMAIL_VENDOR_CUSTOM_SMTP`, renumber |
| `gen/message/v1/message.pb.go` | Generated from proto | Regenerate via `buf generate` |
| `internal/provider/email/smtp.go` | SMTP sender impl | Add `vendor` field, change `NewSMTPProvider` signature |
| `internal/provider/email/smtp_test.go` | Tests for SMTPProvider | Update all `NewSMTPProvider` calls; add vendor-propagation test |
| `internal/provider/email/registry.go` | `Config`, `AccountConfig`, registry, `buildProvider`, `parseVendorName` | Full rewrite: flat `Config`, drop `VendorConfig`, in-package `parseVendorName`, no switch in `buildProvider` |
| `internal/provider/email/registry_test.go` | Tests for registry | Update config-based tests to new shape; vendor enum value swaps |
| `internal/provider/email/sender_test.go` | Tests for `Sender` fallback chain | Replace `EMAIL_VENDOR_CUSTOM_SMTP` enum references |
| `pkg/config/config.go` | Top-level config struct | Drop `EmailConfig`, use `*email.Config` directly |
| `internal/service/setup.go` | Config cooking + resource resolution | Delete `cookEmailConfig`, `parseEmailVendorName` |
| `internal/service/service.go` | `service.New` constructor | Drop `cookEmailConfig` call; pass `cfg.Email` to registry |
| `internal/service/message/email_test.go` | Tests for SendEmail service | Replace `EMAIL_VENDOR_CUSTOM_SMTP` enum references |
| `internal/service/message/util_test.go` | Test helpers | Replace `EMAIL_VENDOR_CUSTOM_SMTP` enum references |
| `internal/store/dal/email_record_test.go` | Tests for email record DAL | Replace `EMAIL_VENDOR_CUSTOM_SMTP` enum references |
| `cmd/testclient/commands.go` | CLI test client | Drop `"custom_smtp"` case in `parseEmailVendor`, update help text |
| `config.yaml` | Local dev config | Flatten email section, drop `custom_smtp` vendor key |
| `config.docker.yaml` | Docker config template | Flatten email section |
| `docker-compose.yaml` | Docker compose env vars | Replace `MESSAGE_SERVICE_EMAIL_VENDORS_CUSTOM_SMTP_*` with new flat var names |
| `CLAUDE.md` | Project instructions | Update enum list; remove stale `emailVendorFromString` reference |

---

## Task 1: Add `vendor` field to `SMTPProvider`

**Goal:** Stop hardcoding `EMAIL_VENDOR_CUSTOM_SMTP` in `SMTPProvider.Vendor()`. Each provider carries its own vendor identity set at construction. Keeps all existing enum values valid — the actual `CUSTOM_SMTP` deletion happens in Task 3.

**Files:**
- Modify: `internal/provider/email/smtp.go`
- Modify: `internal/provider/email/smtp_test.go`
- Modify: `internal/provider/email/registry.go:144-163` (`buildProvider` call to `NewSMTPProvider`)

- [ ] **Step 1: Write the failing test for vendor propagation**

Append to `internal/provider/email/smtp_test.go`:

```go
func TestSMTPProvider_vendorPropagation(t *testing.T) {
	addr := fakeSMTPServer(t)
	host, port, _ := net.SplitHostPort(addr)

	p, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", &SMTPConfig{
		Host:     host,
		Port:     mustAtoi(port),
		Username: "user",
		Password: "pass",
		From:     "noreply@example.com",
	})
	require.NoError(t, err)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, p.Vendor(),
		"Vendor() must return the vendor passed to NewSMTPProvider")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/provider/email/ -run TestSMTPProvider_vendorPropagation -v`
Expected: FAIL with compile error — `NewSMTPProvider` does not yet take a vendor argument.

- [ ] **Step 3: Update `SMTPProvider` to carry vendor**

Edit `internal/provider/email/smtp.go`. Replace the `SMTPProvider` struct, `NewSMTPProvider` function, and `Vendor()` method with:

```go
// SMTPProvider sends emails via SMTP using a reusable client.
type SMTPProvider struct {
	vendor  pb.EmailVendor
	account string
	client  *mail.Client
	from    string
}

// NewSMTPProvider creates an SMTP provider. The mail.Client is initialized
// once and reused across Send calls (each call still dials a new connection
// via DialAndSendWithContext, which is safe for concurrent use).
//
// vendor carries the brand identity (e.g. ALIYUN, TENCENT) so audit and
// selection work without an external wrapper layer.
func NewSMTPProvider(vendor pb.EmailVendor, account string, config *SMTPConfig) (*SMTPProvider, error) {
	client, err := mail.NewClient(config.Host,
		mail.WithPort(config.Port),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithUsername(config.Username),
		mail.WithPassword(config.Password),
		mail.WithTLSPortPolicy(mail.TLSOpportunistic),
	)
	if err != nil {
		return nil, fmt.Errorf("smtp: create client: %w", err)
	}
	return &SMTPProvider{vendor: vendor, account: account, client: client, from: config.From}, nil
}

// Vendor returns the brand this account belongs to (set at construction).
func (p *SMTPProvider) Vendor() pb.EmailVendor { return p.vendor }
```

Also update the existing comment that referenced CUSTOM_SMTP — replace:
```go
// Vendor identifies this provider as CUSTOM_SMTP in the proto enum.
func (*SMTPProvider) Vendor() pb.EmailVendor { return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP }
```
with the new `Vendor()` method shown above (the comment block is removed; the method is now self-documenting).

- [ ] **Step 4: Update existing `smtp_test.go` helper and tests**

In `internal/provider/email/smtp_test.go`, update `newSMTPTestProvider`:

```go
func newSMTPTestProvider(t *testing.T, account, host string, port int) *SMTPProvider {
	t.Helper()
	p, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, account, &SMTPConfig{
		Host:     host,
		Port:     port,
		Username: "user",
		Password: "pass",
		From:     "noreply@example.com",
	})
	require.NoError(t, err)
	return p
}
```

The only change is the new first argument `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP`. Existing tests using `newSMTPTestProvider` work without modification.

`TestSMTPProvider_Vendor` (the existing test that asserted `CUSTOM_SMTP`) still passes — `newSMTPTestProvider` hardcodes `CUSTOM_SMTP`. The new `TestSMTPProvider_vendorPropagation` test exercises the propagation explicitly.

For `TestSMTPProvider_NewProvider_error`, update the direct constructor call to add the vendor arg:

```go
func TestSMTPProvider_NewProvider_error(t *testing.T) {
	_, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", &SMTPConfig{
		Host:     "",
		Port:     0,
		Username: "user",
		Password: "pass",
		From:     "noreply@example.com",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp: create client")
}
```

For `TestSMTPProvider_Send_invalidFromAddress`, update similarly:

```go
	p, err := NewSMTPProvider(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary", &SMTPConfig{
		Host:     host,
		Port:     mustAtoi(port),
		Username: "user",
		Password: "pass",
		From:     "not-valid-email",
	})
```

- [ ] **Step 5: Update `buildProvider` in `registry.go`**

In `internal/provider/email/registry.go`, the `buildProvider` switch currently passes only `ac.Name` to `NewSMTPProvider`. Update it to also pass the `vendorEnum`:

Replace the `buildProvider` function:

```go
// buildProvider dispatches to the corresponding constructor based on vendor
// enum and returns the constructed AccountProvider. The vendor impl carries
// its own (vendor, account) identity. Add a case here when adding a new vendor,
// and add corresponding fields to AccountConfig.
//
// All current vendors use SMTP protocol and share NewSMTPProvider.
// config.Host is required for every vendor — no built-in host table to maintain.
func buildProvider(vendor pb.EmailVendor, ac *AccountConfig) (AccountProvider, error) {
	switch vendor {
	case pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP,
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN,
		pb.EmailVendor_EMAIL_VENDOR_TENCENT,
		pb.EmailVendor_EMAIL_VENDOR_NETEASE:
		if ac.Host == "" {
			return nil, fmt.Errorf("email vendor %s requires explicit host", vendor)
		}
		return NewSMTPProvider(vendor, ac.Name, &SMTPConfig{
			Host:     ac.Host,
			Port:     ac.Port,
			Username: ac.Username,
			Password: ac.Password,
			From:     ac.From,
		})
	default:
		return nil, fmt.Errorf("unknown vendor %s", vendor)
	}
}
```

And update the call site inside `NewAccountRegistry` (around `registry.go:87`):

```go
p, err := buildProvider(vendorEnum, ac)
```

(Adds `vendorEnum` as the first argument.)

- [ ] **Step 6: Run all email package tests**

Run: `go test ./internal/provider/email/... -v`
Expected: All tests PASS, including the new `TestSMTPProvider_vendorPropagation`.

- [ ] **Step 7: Run full build to catch other call sites**

Run: `go build ./...`
Expected: Builds cleanly (no other production code calls `NewSMTPProvider` directly).

- [ ] **Step 8: Commit**

```bash
git add internal/provider/email/smtp.go internal/provider/email/smtp_test.go internal/provider/email/registry.go
git commit -m "$(cat <<'EOF'
refactor(email): SMTPProvider carries vendor identity

Vendor() was hardcoded to CUSTOM_SMTP — wrong for any non-self-hosted
account. Constructor now takes vendor; buildProvider passes the parsed
enum through. Behavior-preserving: existing tests still pass with
CUSTOM_SMTP; the new propagation test pins the contract.
EOF
)"
```

---

## Task 2: Flatten `email.Config`, drop `EmailConfig` wrapper and cook layer

**Goal:** Move vendor parsing into the email package; eliminate the cook step and the `EmailConfig` intermediate type. After this task, `cfg.Email` is consumed directly by `email.NewAccountRegistry`. All enum values still valid.

**Files:**
- Modify: `internal/provider/email/registry.go` (full rewrite of types + `NewAccountRegistry`)
- Modify: `internal/provider/email/registry_test.go` (update config-based tests to new shape)
- Modify: `pkg/config/config.go` (drop `EmailConfig`, embed `*email.Config`)
- Modify: `internal/service/setup.go` (delete `cookEmailConfig`, `parseEmailVendorName`)
- Modify: `internal/service/service.go` (drop `cookEmailConfig` call)

- [ ] **Step 1: Rewrite the type definitions and constructors in `registry.go`**

In `internal/provider/email/registry.go`, replace the entire file content with:

```go
package email

import (
	"fmt"
	"sort"

	pb "message-service/gen/message/v1"
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
// ("aliyun", "tencent", "netease"). Unknown names rejected at registry
// construction.
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
	case "custom_smtp":
		return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, nil
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
```

Note: `parseVendorName` still accepts `"custom_smtp"` in this task — Task 3 will drop that case alongside the enum value.

- [ ] **Step 2: Update config-based registry tests to new shape**

In `internal/provider/email/registry_test.go`, four tests construct `Config` directly. Replace each.

Replace `TestNewAccountRegistry_indexesByVendorAndAccount`:

```go
func TestNewAccountRegistry_indexesByVendorAndAccount(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "custom_smtp", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	ap, ok := r.vendors[pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP]["primary"]
	require.True(t, ok, "vendors map should index by (vendor, account)")
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, ap.Vendor())
	require.Equal(t, "primary", ap.Account())
}
```

Replace `TestNewAccountRegistry_customSMTPSuccess`:

```go
func TestNewAccountRegistry_customSMTPSuccess(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "custom_smtp", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	sender, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, "primary")
	require.NoError(t, err)
	require.NotNil(t, sender)
}
```

Replace `TestNewAccountRegistry_requiresHost`:

```go
func TestNewAccountRegistry_requiresHost(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "custom_smtp", Port: 587, From: "noreply@example.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host is required")
}
```

Replace `TestNewAccountRegistry_smtpInvalidPort`:

```go
func TestNewAccountRegistry_smtpInvalidPort(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "custom_smtp", Host: "smtp.example.com", Port: 0, From: "noreply@example.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp")
}
```

Replace `TestNewAccountRegistry_duplicateAccountName`:

```go
func TestNewAccountRegistry_duplicateAccountName(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "custom_smtp", Host: "a.example.com", Port: 587, From: "noreply@x.com"},
			{Name: "primary", Vendor: "custom_smtp", Host: "b.example.com", Port: 587, From: "noreply@y.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}
```

Add a new test for unknown vendor fail-fast:

```go
func TestNewAccountRegistry_unknownVendor(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "mailgun", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email vendor")
}
```

Other tests in this file use `NewAccountRegistryFromProviders` directly with mock `fakeProvider`s — those do not need changes in this task.

- [ ] **Step 3: Run registry tests to verify the new shape compiles and passes**

Run: `go test ./internal/provider/email/ -run TestNewAccountRegistry -v`
Expected: All PASS.

- [ ] **Step 4: Drop `EmailConfig` wrapper in `pkg/config/config.go`**

Edit `pkg/config/config.go`. Remove the `EmailConfig` struct definition (currently around lines 42-48). Update the `Config` struct field:

Replace:
```go
type Config struct {
	Server      *ServerConfig
	Database    *dbx.Config
	Redis       *redisx.Config
	Log         *logging.Config
	Email       *EmailConfig
	SMS         *SMSConfig
	Cron        *cronx.Config
	ThirdParty  *ThirdPartyConfig
	Persistence *PersistenceConfig
	Idempotency *IdempotencyConfig
}

// EmailConfig is the YAML-friendly form of email vendor accounts.
type EmailConfig struct {
	// Vendors maps a vendor name to its accounts. Valid keys (parsed to
	// pb.EmailVendor by service.New): "custom_smtp", "aliyun", "tencent",
	// "netease".
	Vendors map[string]*email.VendorConfig
}
```

with:
```go
type Config struct {
	Server      *ServerConfig
	Database    *dbx.Config
	Redis       *redisx.Config
	Log         *logging.Config
	Email       *email.Config
	SMS         *SMSConfig
	Cron        *cronx.Config
	ThirdParty  *ThirdPartyConfig
	Persistence *PersistenceConfig
	Idempotency *IdempotencyConfig
}
```

The `// EmailConfig is the YAML-friendly form...` block is removed entirely. The `email.Config` type lives in the email provider package and is YAML-native (vendor is a string field).

- [ ] **Step 5: Delete `cookEmailConfig` and `parseEmailVendorName` in `internal/service/setup.go`**

Edit `internal/service/setup.go`. Remove the `cookEmailConfig` function (around lines 23-39) and `parseEmailVendorName` function (around lines 91-106). The SMS cooking functions stay.

After deletion, the `// --- config cooking (string → enum) ---` section header can stay (it still covers SMS), but the email-specific helpers are gone.

- [ ] **Step 6: Update `service.New` to skip cooking for email**

Edit `internal/service/service.go`. Replace the email cooking block (around lines 85-91):

```go
	emailCfg, err := cookEmailConfig(cfg.Email)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after email config cook failure", "error", cerr)
		}
		return nil, fmt.Errorf("email config: %w", err)
	}
```

with nothing — just delete the block. The next line (which uses `emailCfg`) becomes:

```go
	emailRegistry, err := email.NewAccountRegistry(cfg.Email)
```

(Was `email.NewAccountRegistry(emailCfg)`.)

- [ ] **Step 7: Run full build and all tests**

Run: `go build ./...`
Expected: Builds cleanly.

Run: `go test ./...`
Expected: All tests PASS. The `internal/service/message/email_test.go` tests still pass because they construct the registry via `NewAccountRegistryFromProviders` (not affected by Config shape change).

- [ ] **Step 8: Commit**

```bash
git add internal/provider/email/registry.go internal/provider/email/registry_test.go pkg/config/config.go internal/service/setup.go internal/service/service.go
git commit -m "$(cat <<'EOF'
refactor(email): flatten Config, drop cook step

Vendor moves from being the YAML map key to a string field on each
AccountConfig. EmailConfig wrapper in pkg/config is gone — cfg.Email is
consumed directly. cookEmailConfig and parseEmailVendorName deleted from
setup.go; parseVendorName now lives in the email package as the single
source of truth for the string→enum mapping.
EOF
)"
```

---

## Task 3: Drop `CUSTOM_SMTP` enum value, sweep all references

**Goal:** Remove `EMAIL_VENDOR_CUSTOM_SMTP` from the proto enum (renumber the rest) and update every reference. After this task the only valid vendor values are `UNSPECIFIED`, `ALIYUN`, `TENCENT`, `NETEASE`.

**Files:**
- Modify: `api/proto/message/v1/message.proto`
- Regenerate: `gen/message/v1/*.pb.go` via `buf generate`
- Modify: `internal/provider/email/registry.go` (`parseVendorName` drops `"custom_smtp"` case)
- Modify: `internal/provider/email/registry_test.go` (replace enum references)
- Modify: `internal/provider/email/sender_test.go` (replace enum references)
- Modify: `internal/provider/email/smtp_test.go` (replace enum references in helper + new test)
- Modify: `internal/service/message/email_test.go` (replace enum references)
- Modify: `internal/service/message/util_test.go` (replace enum references)
- Modify: `internal/store/dal/email_record_test.go` (replace enum references)
- Modify: `cmd/testclient/commands.go` (drop `"custom_smtp"` case, fix help text)
- Modify: `config.yaml` (flatten email section, drop `custom_smtp` vendor)
- Modify: `config.docker.yaml` (flatten email section)
- Modify: `docker-compose.yaml` (update env var names)

- [ ] **Step 1: Edit the proto enum**

In `api/proto/message/v1/message.proto`, replace the `EmailVendor` enum block:

```proto
// EmailVendor represents the email service brand. Each vendor offers SMTP
// access today; an API path may be added per-vendor in the future. Vendor
// is an audit label and (vendor, account) selection key — the underlying
// transport is always SMTP. Vendors are configured in config.yaml; selecting
// one with an unconfigured account returns ErrBadRequest.
enum EmailVendor {
  EMAIL_VENDOR_UNSPECIFIED = 0;
  EMAIL_VENDOR_ALIYUN = 1;      // Aliyun DirectMail.
  EMAIL_VENDOR_TENCENT = 2;     // Tencent SES.
  EMAIL_VENDOR_NETEASE = 3;     // NetEase EasyMail.
}
```

- [ ] **Step 2: Regenerate protobuf code**

Run: `make proto` (or `buf generate`)
Expected: `gen/message/v1/message.pb.go` is regenerated. The `EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP` constant and its number mappings are gone; `EmailVendor_EMAIL_VENDOR_ALIYUN = 1`, `_TENCENT = 2`, `_NETEASE = 3`.

- [ ] **Step 3: Drop `"custom_smtp"` case from `parseVendorName`**

In `internal/provider/email/registry.go`, update `parseVendorName`:

```go
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
```

- [ ] **Step 4: Sweep `CUSTOM_SMTP` references in `internal/provider/email/registry_test.go`**

Every `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP` in this file becomes `pb.EmailVendor_EMAIL_VENDOR_ALIYUN`. Also rewrite config-based tests to use `Vendor: "aliyun"` instead of `Vendor: "custom_smtp"`.

For the mock-provider tests that simply need *some* vendor enum value, mechanical replacement is enough — they aren't asserting on the specific enum value:

- All `wrap(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, ...)` → `wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, ...)`
- All `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: {...}` map keys → `pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {...}`
- All `r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, ...)` → `r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, ...)`
- All `require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, ...)` → `require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, ...)`
- Config-based tests: `Vendor: "custom_smtp"` → `Vendor: "aliyun"`; `r.vendors[pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP]` → `r.vendors[pb.EmailVendor_EMAIL_VENDOR_ALIYUN]`.

The sort-order test needs more substantial rewriting because it deliberately uses two distinct vendor enums to verify the sort. Rewrite `TestNewAccountRegistryFromProviders_sortOrder` in full:

```go
// TestNewAccountRegistryFromProviders_sortOrder verifies that the default
// sender's fallback chain is ordered by vendor enum value asc, then account
// name asc.
//
// Map is intentionally unordered: tencent/zzz, aliyun/aaa, aliyun/bbb, tencent/aaa
// Enum values: ALIYUN=1, TENCENT=2 — so aliyun comes first.
// Account sort asc within each vendor: aaa < bbb, aaa < zzz.
// Expected fallback chain: aliyun/aaa → aliyun/bbb → tencent/aaa → tencent/zzz
func TestNewAccountRegistryFromProviders_sortOrder(t *testing.T) {
	aliyunA := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "aaa", errors.New("aliyunA down"))
	aliyunB := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "bbb", nil) // first to succeed
	tencentA := wrap(pb.EmailVendor_EMAIL_VENDOR_TENCENT, "aaa", errors.New("tencentA down"))
	tencentZ := wrap(pb.EmailVendor_EMAIL_VENDOR_TENCENT, "zzz", errors.New("tencentZ down"))

	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_TENCENT: {"zzz": tencentZ, "aaa": tencentA},
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN:  {"bbb": aliyunB, "aaa": aliyunA},
	})

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "u@x.com"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor, "should fall through to aliyun/bbb")
	require.Equal(t, "bbb", result.Account)
	require.Equal(t, 2, result.Attempts, "should try aliyunA then aliyunB (tencent not reached)")

	require.Equal(t, 1, aliyunA.sentCount)
	require.Equal(t, 1, aliyunB.sentCount)
	require.Equal(t, 0, tencentA.sentCount, "tencent comes after aliyun; never reached")
	require.Equal(t, 0, tencentZ.sentCount, "tencent comes after aliyun; never reached")
}
```

After changes, run: `go test ./internal/provider/email/ -v`
Expected: All PASS.

- [ ] **Step 5: Sweep `CUSTOM_SMTP` references in `internal/provider/email/sender_test.go`**

Replace all `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP` with `pb.EmailVendor_EMAIL_VENDOR_ALIYUN` in `internal/provider/email/sender_test.go`. There are 7 occurrences across `testProvider` constructions and assertions.

After changes, run: `go test ./internal/provider/email/ -run TestSender -v`
Expected: PASS.

- [ ] **Step 6: Sweep `CUSTOM_SMTP` references in `internal/provider/email/smtp_test.go`**

Update `newSMTPTestProvider` to use `pb.EmailVendor_EMAIL_VENDOR_ALIYUN` (was `CUSTOM_SMTP`). Update `TestSMTPProvider_Vendor` assertion to `pb.EmailVendor_EMAIL_VENDOR_ALIYUN`. Update `TestSMTPProvider_NewProvider_error` and `TestSMTPProvider_Send_invalidFromAddress` constructor calls.

`TestSMTPProvider_vendorPropagation` (added in Task 1) already uses `ALIYUN` — no change.

After changes, run: `go test ./internal/provider/email/ -v`
Expected: All PASS.

- [ ] **Step 7: Sweep `CUSTOM_SMTP` references in `internal/service/message/email_test.go`**

In `internal/service/message/email_test.go`, replace every `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP` with `pb.EmailVendor_EMAIL_VENDOR_ALIYUN`. There are 9 occurrences across the file (mock provider's `Vendor()` method, registry construction, request vendor fields, and assertions on records/responses).

After changes, run: `go test ./internal/service/message/ -v`
Expected: PASS.

- [ ] **Step 8: Sweep `CUSTOM_SMTP` references in `internal/service/message/util_test.go`**

Replace `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP` with `pb.EmailVendor_EMAIL_VENDOR_ALIYUN` in `internal/service/message/util_test.go`. Single occurrence.

- [ ] **Step 9: Sweep `CUSTOM_SMTP` references in `internal/store/dal/email_record_test.go`**

Replace every `pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP` with `pb.EmailVendor_EMAIL_VENDOR_ALIYUN` in `internal/store/dal/email_record_test.go`. There are 14 occurrences.

After changes, run: `go test ./internal/store/dal/ -v`
Expected: PASS.

- [ ] **Step 10: Drop `"custom_smtp"` case and fix help text in `cmd/testclient/commands.go`**

In `cmd/testclient/commands.go`, update `parseEmailVendor`:

```go
func parseEmailVendor(s string) pb.EmailVendor {
	switch s {
	case "", "unspecified":
		return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	case "aliyun":
		return pb.EmailVendor_EMAIL_VENDOR_ALIYUN
	case "tencent":
		return pb.EmailVendor_EMAIL_VENDOR_TENCENT
	case "netease":
		return pb.EmailVendor_EMAIL_VENDOR_NETEASE
	}
	return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
}
```

Update the help text for the `--vendor` flag (around line 91):

```go
	vendor := fs.String("vendor", "", "email vendor: aliyun|tencent|netease (empty = default fallback)")
```

After changes, run: `go build ./cmd/testclient/`
Expected: Builds cleanly.

- [ ] **Step 11: Rewrite `config.yaml` email section**

In `config.yaml`, replace the email section (the `email:` block, around lines 22-37):

```yaml
email:
  accounts:
    - name: default
      vendor: aliyun               # required, one of aliyun/tencent/netease
      host: smtp.example.com
      port: 587
      username: ""
      password: ""
      from: noreply@example.com
```

- [ ] **Step 12: Rewrite `config.docker.yaml` email section**

In `config.docker.yaml`, replace the email section (around lines 28-36):

```yaml
email:
  accounts:
    - name: ${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_NAME}
      vendor: ${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_VENDOR}
      host: ${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_HOST}
      port: ${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_PORT}
      username: ${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_USERNAME}
      password: ${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_PASSWORD}
      from: ${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_FROM}
```

- [ ] **Step 13: Update `docker-compose.yaml` env var block**

In `docker-compose.yaml`, replace the email env var block (around lines 67-73):

```yaml
      # Email — replace placeholders before sending real email
      - MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_NAME=default
      - MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_VENDOR=${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_VENDOR:-aliyun}
      - MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_HOST=${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_HOST:-smtp.example.com}
      - MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_PORT=${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_PORT:-587}
      - MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_USERNAME=${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_USERNAME:-replace-me}
      - MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_PASSWORD=${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_PASSWORD:-replace-me}
      - MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_FROM=${MESSAGE_SERVICE_EMAIL_ACCOUNTS_0_FROM:-noreply@example.com}
```

- [ ] **Step 14: Run the entire test suite and build**

Run: `go build ./...`
Expected: Builds cleanly.

Run: `go test -race ./...`
Expected: All tests PASS.

- [ ] **Step 15: Commit**

```bash
git add api/proto/message/v1/message.proto gen/ internal/provider/email/ internal/service/message/ internal/store/dal/ cmd/testclient/commands.go config.yaml config.docker.yaml docker-compose.yaml
git commit -m "$(cat <<'EOF'
refactor(email): drop CUSTOM_SMTP enum value

CUSTOM_SMTP was a protocol descriptor masquerading as a vendor brand.
Every account is SMTP; vendor is brand only (Aliyun/Tencent/NetEase).
All test references and config files updated to the new flat shape.
EOF
)"
```

---

## Task 4: Update `CLAUDE.md` and run final verification

**Goal:** Update project docs to reflect the new enum and remove stale references. Confirm fmt, lint, test, build all clean.

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update `EmailVendor` enum reference in `CLAUDE.md`**

In `CLAUDE.md`, find the line:

```
  - `EmailVendor`（CUSTOM_SMTP / ALIYUN / TENCENT / NETEASE）
```

Replace with:

```
  - `EmailVendor`（UNSPECIFIED / ALIYUN / TENCENT / NETEASE）
```

- [ ] **Step 2: Remove stale `emailVendorFromString` reference in `CLAUDE.md`**

In `CLAUDE.md`, find the line:

```
  - `internal/service/message/email.go`：`emailVendorFromString(s string) pb.EmailVendor`
```

This function does not exist in the codebase (the vendor mapping is one-way: `parseVendorName` lives in the email provider package). Delete the bullet entirely.

Also delete the surrounding sub-bullets if they describe `emailVendorFromString` behavior. The CLAUDE.md section currently reads (around the proto enum/GORM section):

```
- go-common/message 返回的 Provider 是字符串（如 `"aliyun"`），反向映射在 service 层：
  - `internal/service/message/email.go`：`emailVendorFromString(s string) pb.EmailVendor`
  - `internal/service/message/sms.go`：`smsVendorFromString(s string) pb.SmsVendor`
  - 未知 vendor 名字会 `slog.Warn` 并落到 UNSPECIFIED，便于监控发现 go-common 升级带来的新 vendor
```

Replace with:

```
- go-common/message 返回的 Provider 是字符串（如 `"aliyun"`）；反向映射（string → enum）的唯一真相源是 `parseVendorName`，位于 `internal/provider/email/registry.go`（SMS 在 `internal/provider/sms/`）。未知 vendor 名字 fail-fast。
```

(The `smsVendorFromString` reference also does not exist in current code — same stale-documentation issue. The replacement above reflects actual code shape.)

- [ ] **Step 3: Format and vet**

Run: `make fmt && make vet`
Expected: No changes from `gofmt` (idempotent); `go vet` clean.

- [ ] **Step 4: Lint**

Run: `make lint`
Expected: golangci-lint clean.

- [ ] **Step 5: Full test suite with race detector**

Run: `go test -race -coverprofile=coverage.out ./...`
Expected: All packages PASS.

- [ ] **Step 6: Build server and migrate binaries**

Run: `make build`
Expected: `bin/server` and `bin/migrate` produced cleanly.

- [ ] **Step 7: Build test client**

Run: `make build-client`
Expected: `bin/msgclient` produced cleanly.

- [ ] **Step 8: Commit**

```bash
git add CLAUDE.md
git commit -m "$(cat <<'EOF'
docs(claude): update EmailVendor list, drop stale emailVendorFromString

Reflects actual code shape after SMTP refactor: vendor parsing lives in
parseVendorName inside the email provider package.
EOF
)"
```

---

## Verification Checklist

After all tasks complete:

- [ ] `go build ./...` clean
- [ ] `go test -race ./...` all PASS
- [ ] `make lint` clean
- [ ] `make fmt` no-op (idempotent)
- [ ] `grep -r "EMAIL_VENDOR_CUSTOM_SMTP" --include="*.go" --include="*.proto" --include="*.yaml" .` returns no matches outside `docs/superpowers/` historical specs/plans
- [ ] `grep -r "custom_smtp" --include="*.yaml" .` returns no matches
- [ ] `config.yaml` loads cleanly: `go run ./cmd/server/` starts without config errors (smoke test, can be skipped if Redis/Postgres not running)

## Out of Scope

- Future API vendor support — design lives in the spec's "Future" section; no code changes for it now.
- SMS provider changes (SMS does not have the protocol/brand confusion).
- Backwards-compatibility shims (acceptable to break: memory records "no prod data yet").
