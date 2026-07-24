# GID Service Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Integrate gid-service into message-service for global ID generation, replacing `time.Now().UnixNano()`.

**Architecture:** Two-layer thirdcall pattern — public interface in `pkg/thirdcall/`, unexported implementations in `internal/thirdcall/gid_service/`. Config-driven mode switching (module/grpc). Option-based dependency injection.

**Tech Stack:** gid-service (module + gRPC client), go-common/configx, functional options pattern.

---

## Task 1: Add gid-service dependency to go.mod

**Files:**
- Modify: `go.mod`
- Modify: `go.sum` (auto-updated)

- [ ] **Step 1: Add replace directive and require for gid-service**

Add to `go.mod` after the existing `replace` line:

```
replace gid-service => ../gid-service
```

Add `gid-service` to the `require` block:

```
gid-service v0.0.0-00010101000000-000000000000
```

- [ ] **Step 2: Run go mod tidy to resolve dependencies**

Run: `go mod tidy`
Expected: success, go.sum updated

- [ ] **Step 3: Verify compilation**

Run: `go build ./...`
Expected: success (no import errors yet since no code references gid-service)

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore: add gid-service dependency"
```

---

## Task 2: Add ThirdParty config to pkg/config

**Files:**
- Modify: `pkg/config/config.go`

- [ ] **Step 1: Add config types and validation**

Add imports `"fmt"` and `"time"` to the import block.

Add `ThirdParty` field to `Config` struct:

```go
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Log        *logging.Config
	Email      *EmailConfig
	SMS        *SMSConfig
	ThirdParty *ThirdPartyConfig
}
```

Add the new config types after `SMSRouteConfig`:

```go
type ThirdPartyConfig struct {
	GID *GIDConfig
}

type GIDConfig struct {
	Mode      string          // "module" | "grpc"
	Target    string          // gRPC addr, e.g. "localhost:9000"
	Snowflake SnowflakeConfig // module mode
}

type SnowflakeConfig struct {
	MachineID int64
	StartTime time.Time
}
```

Add a `Validate()` method to `Config` after `Load()`:

```go
func (c *Config) Validate() error {
	if c.ThirdParty == nil || c.ThirdParty.GID == nil {
		return fmt.Errorf("third_party.gid is required")
	}
	gid := c.ThirdParty.GID
	if gid.Mode == "" {
		return fmt.Errorf("third_party.gid.mode is required (module or grpc)")
	}
	switch gid.Mode {
	case "module":
		if gid.Snowflake.MachineID < 1 {
			return fmt.Errorf("third_party.gid.snowflake.machine_id is required and must be >= 1")
		}
		if gid.Snowflake.StartTime.IsZero() {
			return fmt.Errorf("third_party.gid.snowflake.start_time is required")
		}
		if gid.Snowflake.StartTime.After(time.Now()) {
			return fmt.Errorf("third_party.gid.snowflake.start_time must not be in the future")
		}
	case "grpc":
		if gid.Target == "" {
			return fmt.Errorf("third_party.gid.target is required for grpc mode")
		}
	default:
		return fmt.Errorf("third_party.gid.mode must be module or grpc, got %q", gid.Mode)
	}
	return nil
}
```

Call `Validate()` in `Load()` after `configx.Load()`:

```go
func Load() (*Config, error) {
	var cfg Config
	if err := configx.Load(&cfg,
		configx.WithServiceName("message-service"),
		configx.WithEnvPrefix("MESSAGE_SERVICE"),
	); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
```

- [ ] **Step 2: Verify compilation**

Run: `go build ./...`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add pkg/config/config.go
git commit -m "feat(config): add ThirdParty/GID config with validation"
```

---

## Task 3: Create pkg/thirdcall/gid_service.go (public interface + factory)

**Files:**
- Create: `pkg/thirdcall/gid_service.go`

- [ ] **Step 1: Write the public interface and factory**

```go
package thirdcall

import (
	"context"

	"message-service/internal/thirdcall/gid_service"
	"message-service/pkg/config"
)

// GIDService generates globally unique IDs via gid-service.
type GIDService interface {
	NextID(ctx context.Context) (int64, error)
}

// NewGIDService creates a GIDService based on config mode.
func NewGIDService(cfg *config.GIDConfig) (GIDService, error) {
	switch cfg.Mode {
	case "grpc":
		return gid_service.NewGRPC(cfg.Target)
	default:
		return gid_service.NewModule(cfg.Snowflake)
	}
}
```

- [ ] **Step 2: Verify it does NOT compile yet (expected — internal implementations missing)**

Run: `go build ./pkg/thirdcall/...`
Expected: FAIL — `gid_service` package not found

- [ ] **Step 3: Commit**

```bash
git add pkg/thirdcall/gid_service.go
git commit -m "feat(thirdcall): add GIDService interface and factory"
```

---

## Task 4: Create internal/thirdcall/gid_service/module.go

**Files:**
- Create: `internal/thirdcall/gid_service/module.go`

- [ ] **Step 1: Write the in-process module adapter**

```go
package gid_service

import (
	"context"

	gidservice "gid-service/pkg"
	"message-service/pkg/config"
)

type moduleGID struct {
	*gidservice.GidService
}

// NewModule creates a GIDService backed by an in-process snowflake generator.
func NewModule(cfg config.SnowflakeConfig) (*moduleGID, error) {
	svc, err := gidservice.NewModule(cfg.MachineID, cfg.StartTime)
	if err != nil {
		return nil, err
	}
	return &moduleGID{GidService: svc}, nil
}

func (m *moduleGID) NextID(ctx context.Context) (int64, error) {
	resp, err := m.GidService.NextID(ctx, nil)
	if err != nil {
		return 0, err
	}
	return resp.Id, nil
}
```

- [ ] **Step 2: Verify compilation of this file**

Run: `go build ./internal/thirdcall/gid_service/...`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add internal/thirdcall/gid_service/module.go
git commit -m "feat(thirdcall): add in-process module adapter for gid-service"
```

---

## Task 5: Create internal/thirdcall/gid_service/grpc.go

**Files:**
- Create: `internal/thirdcall/gid_service/grpc.go`

- [ ] **Step 1: Write the gRPC client adapter**

```go
package gid_service

import (
	"context"
	"fmt"

	pb "gid-service/gen/gid/v1"
	gidservice "gid-service/pkg"
)

type grpcGID struct {
	client *gidservice.Client
}

// NewGRPC creates a GIDService backed by a gRPC connection to gid-service.
func NewGRPC(target string) (*grpcGID, error) {
	client, err := gidservice.NewClient(target)
	if err != nil {
		return nil, fmt.Errorf("dial gid-service: %w", err)
	}
	return &grpcGID{client: client}, nil
}

func (g *grpcGID) NextID(ctx context.Context) (int64, error) {
	resp, err := g.client.NextID(ctx, &pb.NextIDRequest{})
	if err != nil {
		return 0, err
	}
	return resp.Id, nil
}

func (g *grpcGID) Close() error {
	return g.client.Close()
}
```

- [ ] **Step 2: Verify full compilation**

Run: `go build ./...`
Expected: success (all three packages compile)

- [ ] **Step 3: Commit**

```bash
git add internal/thirdcall/gid_service/grpc.go
git commit -m "feat(thirdcall): add gRPC client adapter for gid-service"
```

---

## Task 6: Add GIDService to option package

**Files:**
- Modify: `pkg/option/option.go`

- [ ] **Step 1: Add GIDService field and WithGIDService option**

Update `pkg/option/option.go` to:

```go
// Package option defines functional options for configuring the message service.
package option

import (
	"message-service/pkg/thirdcall"

	"gorm.io/gorm"
)

// Option configures a MessageService instance.
type Option func(*Options)

// Options holds resolved option values.
type Options struct {
	DB         *gorm.DB
	GIDService thirdcall.GIDService
}

// WithDB injects an external database connection. MessageService will not close it.
func WithDB(db *gorm.DB) Option {
	return func(o *Options) { o.DB = db }
}

// WithGIDService provides a gid-service instance.
// If not set, the service creates one from config.ThirdParty.GID.
func WithGIDService(svc thirdcall.GIDService) Option {
	return func(o *Options) { o.GIDService = svc }
}

// Apply evaluates all options and returns the resolved Options.
func Apply(opts ...Option) Options {
	var o Options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}
```

- [ ] **Step 2: Verify compilation**

Run: `go build ./...`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add pkg/option/option.go
git commit -m "feat(option): add GIDService injection option"
```

---

## Task 7: Wire GIDService into service layer

**Files:**
- Modify: `internal/service/service.go`

- [ ] **Step 1: Add gid field to MessageService and resolveGID helper**

Add `"message-service/pkg/thirdcall"` to imports.

Add `gid thirdcall.GIDService` field to `MessageService` struct:

```go
type MessageService struct {
	pb.UnimplementedMessageServiceServer

	db          *gorm.DB
	ownDB       bool
	repo        messageRepo
	emailSender *smsemail.Sender
	smsSender   *smssms.Sender
	gid         thirdcall.GIDService
}
```

Update `New()` to resolve GID before constructing:

```go
func New(cfg *config.Config, opts ...option.Option) (*MessageService, error) {
	o := option.Apply(opts...)

	db, ownDB, err := resolveDB(&o, cfg)
	if err != nil {
		return nil, err
	}

	gid, err := resolveGID(cfg, o.GIDService)
	if err != nil {
		return nil, err
	}

	svc, err := newWithDeps(cfg, db, gid)
	if err != nil {
		if ownDB {
			if sqlDB, e := db.DB(); e == nil && sqlDB != nil {
				_ = sqlDB.Close()
			}
		}
		return nil, err
	}
	svc.ownDB = ownDB
	return svc, nil
}
```

Update `newWithDeps` to accept and assign gid:

```go
func newWithDeps(cfg *config.Config, db *gorm.DB, gid thirdcall.GIDService) (*MessageService, error) {
	msgRepo := newRepository(db)

	emailProviders, err := buildEmailProviders(cfg)
	if err != nil {
		return nil, fmt.Errorf("email providers: %w", err)
	}

	smsProviders, err := buildSMSProviders(cfg)
	if err != nil {
		return nil, fmt.Errorf("sms providers: %w", err)
	}

	svc := &MessageService{
		db:  db,
		repo: msgRepo,
		gid:  gid,
	}

	svc.emailSender = smsemail.NewSender(emailProviders, smsemail.WithHook(smsemail.HookFunc(svc.emailHook)))
	svc.smsSender = smssms.NewSender(smsProviders, smssms.WithHook(smssms.HookFunc(svc.smsHook)))

	return svc, nil
}
```

Add `resolveGID` helper in the internal helpers section:

```go
func resolveGID(cfg *config.Config, external thirdcall.GIDService) (thirdcall.GIDService, error) {
	if external != nil {
		return external, nil
	}
	return thirdcall.NewGIDService(cfg.ThirdParty.GID)
}
```

- [ ] **Step 2: Verify compilation**

Run: `go build ./...`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add internal/service/service.go
git commit -m "feat(service): wire GIDService into MessageService"
```

---

## Task 8: Replace time.Now().UnixNano() with gid.NextID()

**Files:**
- Modify: `internal/service/send.go`

- [ ] **Step 1: Update sendEmail**

In `sendEmail`, replace `id := time.Now().UnixNano()` (line 19) with:

```go
id, err := s.gid.NextID(ctx)
if err != nil {
	return nil, xcodes.ErrInternal.Wrap(err)
}
```

Remove `"time"` from imports if no longer needed in this file (it IS still needed by hooks, so keep it).

- [ ] **Step 2: Update sendSMS**

In `sendSMS`, replace `id := time.Now().UnixNano()` (line 57) with:

```go
id, err := s.gid.NextID(ctx)
if err != nil {
	return nil, xcodes.ErrInternal.Wrap(err)
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./...`
Expected: success

- [ ] **Step 4: Commit**

```bash
git add internal/service/send.go
git commit -m "feat(send): use gid-service for ID generation"
```

---

## Task 9: Update config.yaml

**Files:**
- Modify: `config.yaml`

- [ ] **Step 1: Add third_party section**

Append to `config.yaml`:

```yaml
third_party:
  gid:
    mode: module
    snowflake:
      machine_id: 1
      start_time: "2026-06-01T00:00:00Z"
```

- [ ] **Step 2: Commit**

```bash
git add config.yaml
git commit -m "chore: add third_party.gid config to config.yaml"
```

---

## Task 10: Update tests

**Files:**
- Modify: `internal/service/send_test.go`
- Modify: `internal/service/service_query_test.go`

- [ ] **Step 1: Update send_test.go test helpers**

The `newTestEmailService` and `newTestSMSService` functions construct `MessageService` directly. They need a `gid` field. Add a test helper that creates a module-mode GIDService for testing.

Add import `"message-service/pkg/thirdcall"` and `"message-service/pkg/config"`.

Create a shared test helper:

```go
func newTestGIDService(t *testing.T) thirdcall.GIDService {
	t.Helper()
	gid, err := thirdcall.NewGIDService(&config.GIDConfig{
		Mode: "module",
		Snowflake: config.SnowflakeConfig{
			MachineID: 1,
			StartTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)
	return gid
}
```

Update `newTestEmailService` to include gid:

```go
func newTestEmailService(t *testing.T, repo *mockRepo, providers []smsemail.Provider) *MessageService {
	t.Helper()
	svc := &MessageService{repo: repo, gid: newTestGIDService(t)}
	svc.emailSender = smsemail.NewSender(providers, smsemail.WithHook(smsemail.HookFunc(svc.emailHook)))
	return svc
}
```

Update `newTestSMSService` similarly:

```go
func newTestSMSService(t *testing.T, repo *mockRepo, providers []smssms.Provider) *MessageService {
	t.Helper()
	svc := &MessageService{repo: repo, gid: newTestGIDService(t)}
	svc.smsSender = smssms.NewSender(providers, smssms.WithHook(smssms.HookFunc(svc.smsHook)))
	return svc
}
```

Update all callers of `newTestEmailService` and `newTestSMSService` to pass `t` as first argument. There are 5 call sites:

1. `TestSendEmail_Success` (line 143)
2. `TestSendEmail_AllProvidersFail` (line 163)
3. `TestSendEmail_FallbackProvider` (line 192)
4. `TestSendSMS_Success` (line 219)
5. `TestSendSMS_Failed` (line 233)

Each changes from e.g.:
```go
svc := newTestEmailService(repo, []smsemail.Provider{...})
```
to:
```go
svc := newTestEmailService(t, repo, []smsemail.Provider{...})
```

- [ ] **Step 2: Update service_query_test.go**

Update `newTestRecord` to use a module-mode GIDService instead of `time.Now().UnixNano()`.

Add imports `"message-service/pkg/thirdcall"` and `"message-service/pkg/config"`.

Create a package-level test helper:

```go
var testGIDOnce sync.Once
var testGID thirdcall.GIDService

func getTestGID(t *testing.T) thirdcall.GIDService {
	t.Helper()
	testGIDOnce.Do(func() {
		var err error
		testGID, err = thirdcall.NewGIDService(&config.GIDConfig{
			Mode: "module",
			Snowflake: config.SnowflakeConfig{
				MachineID: 1,
				StartTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			},
		})
		require.NoError(t, err)
	})
	return testGID
}
```

Update `newTestRecord` to accept a GIDService:

```go
func newTestRecord(t *testing.T, channel, status int32, target string, provider int32) *models.MessageRecord {
	t.Helper()
	id, err := getTestGID(t).NextID(context.Background())
	require.NoError(t, err)
	return &models.MessageRecord{
		ID:       id,
		Channel:  channel,
		Provider: provider,
		Status:   status,
		Target:   target,
		Subject:  "Test Subject",
		Content:  "Test content body",
		Attempts: 1,
	}
}
```

Add `"sync"` to imports. Update all callers of `newTestRecord` (lines 45, 72-74, 98-101) to pass `t` as first argument.

- [ ] **Step 3: Run all tests**

Run: `go test -race ./...`
Expected: all tests pass

- [ ] **Step 4: Commit**

```bash
git add internal/service/send_test.go internal/service/service_query_test.go
git commit -m "test: update tests to use gid-service for ID generation"
```

---

## Task 11: Final verification

- [ ] **Step 1: Run linter**

Run: `golangci-lint run ./...`
Expected: no errors

- [ ] **Step 2: Run full test suite**

Run: `go test -race -coverprofile=coverage.out ./...`
Expected: all tests pass

- [ ] **Step 3: Verify final git log**

Run: `git log --oneline -10`
Expected: clean linear history with all commits from Tasks 1-10
