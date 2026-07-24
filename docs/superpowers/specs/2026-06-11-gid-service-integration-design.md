# GID Service Integration Design

Date: 2026-06-11

## Goal

Replace `time.Now().UnixNano()` ID generation with gid-service global IDs in message-service, following the same thirdcall/config/option pattern as storage-service.

## Scope

Only `MessageRecord.ID` needs global ID generation. No other tables.

## Architecture

Replicate storage-service's two-layer pattern:

### New Files

| File | Purpose |
|------|---------|
| `pkg/thirdcall/gid_service.go` | Public `GIDService` interface + `NewGIDService(cfg)` factory |
| `internal/thirdcall/gid_service/grpc.go` | gRPC client adapter (unexported `grpcGID`) |
| `internal/thirdcall/gid_service/module.go` | In-process snowflake adapter (unexported `moduleGID`) |

### Modified Files

| File | Change |
|------|--------|
| `pkg/config/config.go` | Add `ThirdParty *ThirdPartyConfig` with `GIDConfig`, `SnowflakeConfig`, validation |
| `pkg/option/option.go` | Add `GIDService thirdcall.GIDService` field + `WithGIDService()` option |
| `internal/service/service.go` | Add `gid` field, `resolveGID` helper, wire in constructor |
| `internal/service/send.go` | Replace `time.Now().UnixNano()` with `s.gid.NextID(ctx)` |
| `config.yaml` | Add `third_party.gid` section |

## Interface

```go
// pkg/thirdcall/gid_service.go
type GIDService interface {
    NextID(ctx context.Context) (int64, error)
}

func NewGIDService(cfg *config.GIDConfig) GIDService
```

## Config

```go
type ThirdPartyConfig struct {
    GID *GIDConfig
}

type GIDConfig struct {
    Mode      string           // "module" | "grpc"
    Target    string           // gRPC address (required when mode=grpc)
    Snowflake *SnowflakeConfig // required when mode=module
}

type SnowflakeConfig struct {
    MachineID uint16
    StartTime string // RFC3339
}
```

Validation:
- Mode must be "module" or "grpc"
- module: MachineID >= 1, StartTime must parse and not be in the future
- grpc: Target must be non-empty

## Option Injection

```go
// pkg/option/option.go
type Options struct {
    DB         *gorm.DB
    GIDService thirdcall.GIDService
}

func WithGIDService(svc thirdcall.GIDService) Option
```

## Service Integration

```go
// internal/service/service.go
type MessageService struct {
    db          *gorm.DB
    ownDB       bool
    repo        *repository.MessageRepository
    emailSender email.Sender
    smsSender   sms.Sender
    gid         thirdcall.GIDService
}
```

Constructor calls `resolveGID(cfg, opts.GIDService)` — same pattern as storage-service: prefer injected, fall back to config-based creation.

## ID Generation Changes

In `send.go`, both `sendEmail` and `sendSMS` replace:

```go
// before
id := time.Now().UnixNano()

// after
id, err := s.gid.NextID(ctx)
```

## Config YAML Addition

```yaml
third_party:
  gid:
    mode: module
    snowflake:
      machine_id: 1
      start_time: "2026-06-01T00:00:00Z"
```

## Testing

- `send_test.go`: inject a module-mode GIDService via `option.WithGIDService()` (real snowflake instance, no mock needed)
- `service_query_test.go`: same approach for test helper that creates records
- `message_repository_test.go`: unchanged (repository doesn't generate IDs)

## Dependency

Add `gid-service` to `go.mod` via replace directive pointing to `../gid-service`.
