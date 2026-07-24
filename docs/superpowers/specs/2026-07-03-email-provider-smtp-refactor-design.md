# Email Provider SMTP Refactor — Design

**Date**: 2026-07-03
**Status**: Approved
**Scope**: `internal/provider/email/`, `pkg/config/`, `internal/service/setup.go`, `api/proto/message/v1/message.proto`, `config.yaml`

## Background

The current email provider package carries an abandoned multi-vendor abstraction:

- `EmailVendor` enum has 4 values (`CUSTOM_SMTP`, `ALIYUN`, `TENCENT`, `NETEASE`) but `buildProvider` switches on all 4 cases to do the **exact same thing** — call `NewSMTPProvider`.
- `AccountConfig` is documented as a "fat struct" supporting multiple vendors, but only has SMTP fields.
- `VendorConfig` is a wrapper struct around `[]*AccountConfig` that carries no vendor-level config — pure nesting overhead.
- `parseEmailVendorName` in `internal/service/setup.go` is a 4-case identity-mapping switch.
- `cookEmailConfig` translates the string-keyed map to an enum-keyed map; the indirection exists only because vendor is the map key.
- `CUSTOM_SMTP` is a **protocol descriptor masquerading as a vendor brand** — it doesn't belong in a vendor enum.

The actual requirement is simpler: every email send goes through SMTP. Vendor is a brand label (Aliyun / Tencent / NetEase) used for audit and (vendor, account) selection — not protocol selection.

## Goals

- Remove the fake multi-vendor dispatch.
- Keep `(vendor, account)` selection semantics at the RPC level (hard requirement).
- Keep vendor as an audit label on each account.
- Reflect "SMTP only" honestly in the data model.
- Set up the code so adding API support later is a localized change, not a rewrite — but **do not** pre-design for it (YAGNI).

## Non-Goals

- Adding API-vendor support. Future work.
- Refactoring SMS (SMS does not have the same confusion — its vendors are already brand-only).
- Changing the public RPC contract beyond dropping one enum value.

## Design

### 1. Proto: drop `CUSTOM_SMTP`, renumber

```proto
enum EmailVendor {
  EMAIL_VENDOR_UNSPECIFIED = 0;
  EMAIL_VENDOR_ALIYUN = 1;
  EMAIL_VENDOR_TENCENT = 2;
  EMAIL_VENDOR_NETEASE = 3;
}
```

Renumbering is acceptable: memory records "no prod data yet — schema/proto changes can be destructive without migration scripts".

### 2. `internal/provider/email/registry.go` — flatten

Drop `VendorConfig` wrapper. `AccountConfig` gets a `Vendor` string field (validated at registry construction):

```go
type Config struct {
    Accounts []*AccountConfig
}

type AccountConfig struct {
    Name     string
    Vendor   string // "aliyun" / "tencent" / "netease"; validated by parseVendorName
    Host     string
    Port     int
    Username string
    Password string
    From     string
}
```

`parseVendorName` moves from `internal/service/setup.go` into this package (single source of truth for the string→enum mapping). Stays a 3-case explicit switch — more readable than reflection on proto enum names.

`buildProvider` loses its switch — every account is SMTP:

```go
func buildProvider(vendor pb.EmailVendor, ac *AccountConfig) (AccountProvider, error) {
    if ac.Host == "" {
        return nil, fmt.Errorf("host is required")
    }
    return NewSMTPProvider(vendor, ac.Name, &SMTPConfig{
        Host: ac.Host, Port: ac.Port, Username: ac.Username, Password: ac.Password, From: ac.From,
    })
}
```

`NewAccountRegistry` iterates `cfg.Accounts`, parses vendor (fail-fast on unknown), rejects duplicate `(vendor, account)` pairs, dispatches to `buildProvider`. The internal `vendors map[pb.EmailVendor]map[string]AccountProvider` stays — it's the O(1) lookup index for `SenderFor`, not exposed publicly.

`NewAccountRegistryFromProviders` stays for tests that want to inject mock providers.

`flattenProviders` (default fallback chain sort) stays — deterministic ordering is still needed.

### 3. `internal/provider/email/smtp.go` — vendor on provider

```go
type SMTPProvider struct {
    vendor  pb.EmailVendor // was hardcoded CUSTOM_SMTP
    account string
    client  *mail.Client
    from    string
}

func NewSMTPProvider(vendor pb.EmailVendor, account string, cfg *SMTPConfig) (*SMTPProvider, error)

func (p *SMTPProvider) Vendor() pb.EmailVendor { return p.vendor }
```

### 4. `pkg/config/config.go` — drop `EmailConfig` wrapper

```go
type Config struct {
    // ... other fields ...
    Email *email.Config // was *EmailConfig
    // ...
}
```

`EmailConfig` wrapper is removed. YAML loads directly into `email.Config.Accounts`. The `Vendor` field on `AccountConfig` is `string`, which is YAML-native — no intermediate type needed.

### 5. `internal/service/setup.go` — drop cook step

- Delete `cookEmailConfig` (vendor parsing moved into `email.NewAccountRegistry`).
- Delete `parseEmailVendorName` (moved to `internal/provider/email/registry.go` as `parseVendorName`).
- `service.New` calls `email.NewAccountRegistry(cfg.Email)` directly (was `email.NewAccountRegistry(cooked)`).

### 6. `config.yaml` — flat

```yaml
email:
  accounts:
    - name: default
      vendor: aliyun             # required, one of aliyun/tencent/netease
      host: smtp.aliyun.com
      port: 587
      username: ""
      password: ""
      from: noreply@aliyun.com
```

## Public Contract

Unchanged external surface:

- `SenderFor(vendor, account)` signature and semantics.
- Selection logic: `(UNSPECIFIED, "")` → default fallback chain; both set → single provider, no fallback; only one set → error.
- Audit: `SendResult.Vendor` reflects the account's brand.
- `SendEmailRequest.vendor / .account` proto fields unchanged.

Changed:

- `EmailVendor` enum loses `CUSTOM_SMTP`. Clients that sent `vendor=CUSTOM_SMTP` must update. Acceptable per "no prod data yet".

## Testing

- `registry_test.go`: rewrite tests using `Config{Vendors: map[...]}` to `Config{Accounts: [...]}` with `Vendor` field per account.
- `smtp_test.go`: `NewSMTPProvider(account, cfg)` → `NewSMTPProvider(vendor, account, cfg)`. Add a test asserting vendor is propagated (e.g. construct with ALIYUN, verify `p.Vendor() == EMAIL_VENDOR_ALIYUN`).
- `sender_test.go`: unaffected (uses mock `testProvider`).
- New test: end-to-end vendor propagation — build registry from config with `vendor: "aliyun"`, send, assert `SendResult.Vendor == EMAIL_VENDOR_ALIYUN`.

## File Change List

| File | Operation |
|---|---|
| `api/proto/message/v1/message.proto` | Drop `EMAIL_VENDOR_CUSTOM_SMTP`, renumber remaining |
| `internal/provider/email/registry.go` | Flatten `Config`/`AccountConfig`, drop `VendorConfig`, move `parseVendorName` in, drop `buildProvider` switch |
| `internal/provider/email/smtp.go` | Add `vendor` field, change `NewSMTPProvider` signature |
| `internal/provider/email/registry_test.go` | Update to new `Config` shape |
| `internal/provider/email/smtp_test.go` | Update `NewSMTPProvider` calls |
| `pkg/config/config.go` | Drop `EmailConfig`, use `*email.Config` directly |
| `internal/service/setup.go` | Drop `cookEmailConfig`, `parseEmailVendorName` |
| `internal/service/service.go` | Update `service.New` call site |
| `config.yaml` | Rewrite email section |

Untouched: `internal/provider/email/sender.go`, `message.go`, `sender_test.go`, all SMS code, `SendEmail` business logic in `internal/service/message/email.go`.

## Future: Adding API Vendor Support

When API support lands, the localized change is:

1. Add API-specific fields to `AccountConfig` (or split into `SMTPConfig` / `APIConfig` sub-structs).
2. Decide whether to add a `Protocol` field per account or to introduce a new `api/` subpackage.
3. `buildProvider` regains a switch — but on protocol, not vendor.

The current refactor does not pre-empt any of these designs. The simplification now is honest about what the code does today; future API work reintroduces complexity only when justified by an actual second protocol.
