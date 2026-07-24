# Proto Enum Unification Design

## Goal

Remove `internal/enum` package. Use proto-generated enums (`pb.MessageStatus`, `pb.Channel`, `pb.Provider`) as the single source of truth across all layers — DB, service, repository, and API. No hand-written switch conversion functions.

## Constraint

`go-common/message` returns `SendResult.Provider` as a string ("smtp", "mailgun", "aliyun"). This string→enum mapping is unavoidable and must live somewhere in our code.

## Decisions

1. **DB stores int32** — proto enum numeric values. No custom Scan/Value types. GORM natively handles int32.
2. **Provider string mapping split into email/sms packages** — each package owns its own provider name constants and `ProviderToProto` function.
3. **No production data** — drop and recreate tables via AutoMigrate.
4. **Remove DB-level defaults for enum columns** — Go code sets explicit values (e.g. `Status: int32(pb.MessageStatus_MESSAGE_STATUS_PENDING)`).

## Changes by File

### Delete

- `internal/enum/enum.go` — entire package removed

### models/message_record.go

| Field   | Before          | After  |
|---------|-----------------|--------|
| Channel | `string`        | `int32`|
| Provider| `string`        | `int32`|
| Status  | `string`        | `int32`|

- Remove `default:pending` from Status tag; use `default:0` or no default
- No custom Scan/Value — raw int32, GORM handles natively

### internal/email/provider.go (new file)

```go
const (
    ProviderSMTP    = "smtp"
    ProviderMailgun = "mailgun"
)

var providerToProto = map[string]pb.Provider{
    ProviderSMTP:    pb.Provider_PROVIDER_SMTP,
    ProviderMailgun: pb.Provider_PROVIDER_MAILGUN,
}

func ProviderToProto(name string) pb.Provider {
    if p, ok := providerToProto[name]; ok {
        return p
    }
    return pb.Provider_PROVIDER_UNSPECIFIED
}
```

### internal/email/email_service.go

- `SendResponse.Status`: `string` → `pb.MessageStatus`
- `SendResponse.Provider`: `string` → `pb.Provider`
- `hook()`: use `email.ProviderToProto(result.Provider)` and `int32(pb.MessageStatus_MESSAGE_STATUS_SENT)`
- `MessageRepo` interface: `UpdateStatus` params `status, provider string` → `status, provider int32`
- `MessageRepo` interface: `UpdateError` param `provider string` → `provider int32`

### internal/sms/provider.go (new file)

Same pattern as email — manages `"aliyun"` → `pb.Provider_PROVIDER_ALIYUN`.

### internal/sms/sms_service.go

Same changes as email_service.go.

### internal/store/repository/message_repository.go

- `ListFilter.Status`: `string` → `pb.MessageStatus`
- `ListFilter.Channel`: `string` → `pb.Channel`
- `ListFilter.Provider`: `string` → `pb.Provider`
- `StatsFilter.Channel`: `string` → `pb.Channel`
- `StatsFilter.Provider`: `string` → `pb.Provider`
- `ProviderStats.Provider`: `string` → `pb.Provider`
- `UpdateStatus(ctx, id, status, provider string, ...)` → `UpdateStatus(ctx, id, status, provider int32, ...)`
- `UpdateError(ctx, id, provider string, ...)` → `UpdateError(ctx, id, provider int32, ...)`
- `applyListFilter`: filter fields are now proto enums; cast to int32 for generated `.Eq()` calls
- `Stats` raw SQL: `status = 'sent'` → `status = 2` (use `int32(pb.MessageStatus_MESSAGE_STATUS_SENT)`)
- `ProviderStats` raw SQL: `status = 'sent'`/`'failed'` → numeric equivalents

### internal/service/message_service.go

- Delete 6 switch functions: `statusToProto`, `statusFromProto`, `channelToProto`, `channelFromProto`, `providerToProto`, `providerFromProto`
- `toProtoRecord()`: direct cast — `pb.MessageStatus(r.Status)`, `pb.Channel(r.Channel)`, `pb.Provider(r.Provider)`
- `ListMessages`/`Stats` filter construction: `req.GetChannel()` returns `pb.Channel`, use directly (0 = unspecified = skip filter)
- `buildEmailProviders`: `enum.ProviderSMTP` → `email.ProviderSMTP`
- Remove `import "message-service/internal/enum"`

### internal/store/generated/

Re-run `gorm.io/cli` code generation. Fields change from `field.String` to `field.Number[int32]`.

### YAML config (no change)

`EmailProviderConfig.Type` stays `string`. Service layer switch uses `email.ProviderSMTP` constants.

### AutoMigrate

Drop existing tables and recreate. No production data to preserve.

## Proto Enum Quick Reference

```go
// From number to enum
pb.MessageStatus(2)                           // → MESSAGE_STATUS_SENT

// From enum to number
int32(pb.MessageStatus_MESSAGE_STATUS_SENT)   // → 2

// From string name to enum
pb.MessageStatus_value["MESSAGE_STATUS_SENT"]  // → 2 (int32)

// From enum to string name
pb.MessageStatus_MESSAGE_STATUS_SENT.String()  // → "MESSAGE_STATUS_SENT"
```

## Files Touched

- `internal/enum/enum.go` (delete)
- `internal/store/models/message_record.go`
- `internal/store/generated/message_record.go` (regenerate)
- `internal/store/repository/message_repository.go`
- `internal/store/repository/base.go` (if it references enum)
- `internal/email/email_service.go`
- `internal/email/provider.go` (new)
- `internal/email/email_service_test.go`
- `internal/sms/sms_service.go`
- `internal/sms/provider.go` (new)
- `internal/sms/sms_service_test.go`
- `internal/query/query_service.go` (no logic change, but filter types change)
- `internal/query/query_service_test.go`
- `internal/service/message_service.go`
- `internal/store/repository/message_repository_test.go`
- `cmd/migrate/main.go`
- `CLAUDE.md` (update enum conventions)
