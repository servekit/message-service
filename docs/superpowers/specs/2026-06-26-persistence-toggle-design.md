# Persistence Toggle Design

Date: 2026-06-26

## Goal

Add a per-channel (email / sms) config switch to disable DB persistence of send records. Some callers only need the send capability (transient notifications, tests, etc.) and don't want every message written to PostgreSQL.

## Scope

- New top-level `persistence` config section.
- New option overrides for module-mode callers.
- Behavioral changes to `SendEmail` / `SendSMS` (skip idempotency check + skip DB write when disabled) and all query methods (`GetEmail` / `ListEmails` / `ListEmailsByCursor` / `GetEmailStats` / SMS equivalents) to return a clear error when disabled.
- New error code `ErrPersistenceDisabled`.

Out of scope: status-based toggles (sent-only, failed-only), in-memory idempotency cache, async queue. Only a binary on/off per channel.

## Architecture

### New config types

`pkg/config/config.go`:

```go
type Config struct {
    // ...existing fields...
    Persistence *PersistenceConfig
}

// PersistenceConfig controls whether send records are persisted per channel.
// `default:"true"` struct tag makes yaml-omitted default to enabled (preserves
// existing behavior). Explicit `email: false` in yaml overrides.
type PersistenceConfig struct {
    Email bool `default:"true"`
    SMS   bool `default:"true"`
}

// EmailEnabled reports whether email persistence is on. Safe on nil receiver
// (returns true — module-mode callers may construct Config without Load).
func (p *PersistenceConfig) EmailEnabled() bool {
    if p == nil {
        return true
    }
    return p.Email
}

// SMSEnabled mirrors EmailEnabled.
func (p *PersistenceConfig) SMSEnabled() bool {
    if p == nil {
        return true
    }
    return p.SMS
}
```

### Option overrides

`pkg/option/option.go`:

```go
type Options struct {
    // ...existing fields...
    EmailPersistence *bool  // nil = use yaml/default
    SMSPersistence   *bool
}

func WithEmailPersistence(enabled bool) Option {
    return func(o *Options) { o.EmailPersistence = &enabled }
}

func WithSMSPersistence(enabled bool) Option {
    return func(o *Options) { o.SMSPersistence = &enabled }
}
```

Option values override yaml when both are set.

### message.Service changes

`internal/service/message/service.go`:

```go
type PersistenceConfig struct {
    Email bool
    SMS   bool
}

type Service struct {
    // ...existing fields...
    persistEmailEnabled bool
    persistSMSEnabled   bool
}

func New(
    db *gorm.DB,
    gid thirdcall.GIDService,
    emailRegistry *email.AccountRegistry,
    smsRegistry *sms.AccountRegistry,
    smsRouter *sms.Router,
    persistence PersistenceConfig,
) *Service
```

### Effective value resolution

`internal/service/service.go` `New`:

```go
emailEnabled := cfg.Persistence.EmailEnabled()  // safe on nil receiver
if o.EmailPersistence != nil {
    emailEnabled = *o.EmailPersistence
}
// (same for sms: cfg.Persistence.SMSEnabled() → option override)

message.New(db, gid, emailRegistry, smsRegistry, smsRouter,
    message.PersistenceConfig{Email: emailEnabled, SMS: smsEnabled})
```

## Behavior Changes

### Send path (`SendEmail` / `SendSMS`)

- **Idempotency check**: gated by `s.persistXxxEnabled`. When disabled, the DB lookup is skipped — duplicate idempotency keys will be sent to the vendor each time.
- **DB write**: gated by `s.persistXxxEnabled`. When disabled, no record is written (including FAILED records).
- **ID generation**: `s.gid.NextID(ctx)` is still called. `SendResponse.Id` keeps the same shape whether or not persistence is on.
- **Vendor call**: unchanged.
- **Error path**: a failed vendor call still returns `ErrMessageSendFailed` to the caller, but no FAILED record is persisted.

### Query path

`GetEmail` / `ListEmails` / `ListEmailsByCursor` / `GetEmailStats` and SMS equivalents: at method entry, if persistence is disabled for that channel, return `xcodes.ErrPersistenceDisabled` immediately.

Rationale for error vs. empty result: the caller didn't do anything wrong; the operator explicitly disabled the feature. An error is honest about the state. An empty list would silently mislead the caller into thinking no records exist.

## Error Code

`pkg/xcodes/message.go`:

```go
// ErrPersistenceDisabled indicates the caller invoked a query method on a
// channel whose persistence has been disabled in config. The send path still
// works (vendor call only); only Get/List/Stats/Idempotency are unavailable.
var ErrPersistenceDisabled = xerr.New(
    "PERSISTENCE_DISABLED",
    xerr.CategoryServiceUnavailable,
    503,
    "persistence is disabled for this channel",
)
```

Choice rationale: caller made a valid request; the operator disabled the feature on the server. 503 (ServiceUnavailable) is more accurate than 4xx. The closest xerr category is `CategoryServiceUnavailable` — there's no `FailedPrecondition` category.

## YAML

```yaml
persistence:
  email: true   # default; can omit entire section
  sms: true     # same
```

## Disabled surface

| Method | Disabled behavior |
|--------|------------------|
| `SendEmail` | Skip idempotency check, skip DB write |
| `SendSMS` | Same |
| `GetEmail` / `GetSMS` | `ErrPersistenceDisabled` |
| `ListEmails` / `ListSMS` | `ErrPersistenceDisabled` |
| `ListEmailsByCursor` / `ListSMSByCursor` | `ErrPersistenceDisabled` |
| `GetEmailStats` / `GetSMSStats` | `ErrPersistenceDisabled` |

## Testing

New unit tests in `internal/service/message/email_test.go` and `sms_test.go` (existing tests unchanged; the default helper now passes `PersistenceConfig{Email: true, SMS: true}`):

1. `TestSendEmail_PersistenceDisabled_SkipsDB` — send succeeds, no record in DB.
2. `TestSendEmail_PersistenceDisabled_IdempotencyNoOp` — same idempotency_key twice, provider called twice.
3. `TestGetEmail_PersistenceDisabled_ReturnsError` — `errors.Is(err, xcodes.ErrPersistenceDisabled)`.
4. `TestListEmails_PersistenceDisabled_ReturnsError`.
5. `TestGetEmailStats_PersistenceDisabled_ReturnsError`.
6. SMS mirrors of 1–5.

`pkg/config/config_test.go`:

7. `TestPersistenceConfig_DefaultTrue` — nil receiver returns true; explicit `Email: false` / `SMS: true` honored. Same for SMSEnabled.
8. `TestLoad_PersistenceDefaultTrueWhenOmitted` — yaml without `persistence` section, both `cfg.Persistence.EmailEnabled()` and `SMSEnabled()` are true.

Manual: set `persistence.email: false` in `config.yaml`, run `cmd/testclient` to send, confirm send succeeds but DB has no row; call `GetEmail`, confirm 503.

## 关联

- 参考现有 spec 风格：[[2026-06-11-gid-service-integration-design]]
