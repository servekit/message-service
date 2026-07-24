# Redis 幂等实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 message-service 的发送幂等从 DB partial unique index 完全迁到 Redis SETNX-based reservation；DB 不再保留 `idempotency_key`；persistence toggle 范围收窄为只管 DB 写/查；Redis 成硬依赖。

**Architecture:** 新增 `internal/idempotency/` 包定义 `Checker` 接口（Reserve / Complete / Release 三段式）+ Redis 实现；`service.New` 启动时初始化 Redis client 并注入到 `message.Service`；`SendEmail` / `SendSMS` 把 DB 幂等块替换为 Redis 流程；删除 dal 层幂等查询函数与 GORM 模型字段；删除 partial unique index 的 raw SQL。失败不缓存（Release 后可重试），in-flight 返回新错误码 `ErrIdempotencyConflict`。

**Tech Stack:** Go 1.22+、PostgreSQL（dbx testcontainer）、Redis（redisx + miniredis for tests）、go-common/xerr、go-common/redisx、GORM、viper/configx。

**Spec:** `docs/superpowers/specs/2026-06-27-redis-idempotency-design.md`

---

## File Structure

| 文件 | 角色 | 改动 |
|------|------|------|
| `pkg/xcodes/message.go` | 错误码 | 新增 `ErrIdempotencyConflict` (409) |
| `pkg/config/config.go` | 配置类型 | `Config` 加 `Redis *redisx.Config` 与 `Idempotency *IdempotencyConfig`；新增 `IdempotencyConfig` + nil-safe duration 方法 |
| `pkg/config/config_test.go` | 配置测试 | 加 IdempotencyConfig 默认 + 显式覆盖测试 |
| `internal/idempotency/checker.go` | 新文件 | `Checker` 接口 + `buildKey` helper |
| `internal/idempotency/redis_checker.go` | 新文件 | `RedisChecker` 实现 |
| `internal/idempotency/redis_checker_test.go` | 新文件 | miniredis 驱动的 6 个测试 |
| `internal/service/message/service.go` | message subpackage 入口 | 重命名 `PersistenceConfig` → `ServiceConfig` 并加 `EmailIdemTTL` / `SMSIdemTTL`；`Service` 加 `idem` + TTL 字段；`New` 签名加 `idem` 参数 |
| `internal/service/message/email.go` | Email service | `SendEmail` 把 DB 幂等块替换为 Redis Reserve/Complete/Release；去掉 `persistEmailEnabled` 包裹幂等的 gate；删除 `respondIdempotentEmail` |
| `internal/service/message/email_test.go` | Email service 测试 | helper 加 miniredis Checker + TTL；改写 `TestSendEmail_PersistenceDisabled_IdempotencyNoOp` → `IdempotencyStillWorks`；加新 Redis 行为测试 |
| `internal/service/message/sms.go` | SMS service | 与 Email 对称 |
| `internal/service/message/sms_test.go` | SMS service 测试 | 与 Email 对称 |
| `internal/service/service.go` | service root | `New` 初始化 Redis client（Ping 验证），创建 `idempotency.RedisChecker`，解析 TTL，传给 `message.New` |
| `internal/store/models/email_record.go` | GORM model | 删 `IdempotencyKey` 字段 |
| `internal/store/models/sms_record.go` | GORM model | 同上 |
| `internal/store/generated/{email,sms}_record.go` | 生成代码 | 删除 `IdempotencyKey` 生成字段（重跑 gorm gen） |
| `internal/store/dal/email_record.go` | DAL | 删 `GetEmailRecordByIdempotencyKey` |
| `internal/store/dal/email_record_test.go` | DAL 测试 | 删 `TestGetEmailRecordByIdempotencyKey_*` |
| `internal/store/dal/sms_record.go` | DAL | 同上 |
| `internal/store/dal/sms_record_test.go` | DAL 测试 | 同上 |
| `cmd/migrate/main.go` | 迁移入口 | 删除创建 partial unique index 的 SQL；加 `DROP INDEX IF EXISTS` 兜底 |
| `config.yaml` | 部署配置 | 加 `redis:` 与 `idempotency:` 段 |
| `config.docker.yaml` | Docker 配置 | 加 redis 与 idempotency 环境变量 |
| `docker-compose.yaml` | Docker compose | 加 redis 服务 |
| `CLAUDE.md` | 项目文档 | 更新持久化开关段，加 Redis 段 |

---

## Phase 1: 基础类型（无破坏性变更）

### Task 1: 新增错误码 `ErrIdempotencyConflict`

**Files:**
- Modify: `pkg/xcodes/message.go`

- [ ] **Step 1: 在 `pkg/xcodes/message.go` 末尾追加（在 `ErrPersistenceDisabled` 之后）**

```go
// ErrIdempotencyConflict indicates a send with the same idempotency_key is
// currently in flight (another caller holds the Redis reservation). Caller
// can retry the same request after the in-flight call completes or its TTL
// expires.
var ErrIdempotencyConflict = xerr.New(
	"IDEMPOTENCY_CONFLICT",
	xerr.CategoryConflict,
	409,
	"idempotency_key is in flight",
)
```

- [ ] **Step 2: 编译验证**

Run: `go build ./pkg/xcodes/...`
Expected: 无输出（成功）。

- [ ] **Step 3: Commit**

```bash
git add pkg/xcodes/message.go
git commit -m "feat(xcodes): add ErrIdempotencyConflict"
```

---

### Task 2: config 包加 `IdempotencyConfig` + nil-safe duration 方法 + 测试

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [ ] **Step 1: 先写测试 — 默认 5m，nil-safe，显式覆盖**

在 `pkg/config/config_test.go` 末尾追加：

```go
// TestIdempotencyConfig_DefaultTTL verifies the default 5m TTL on nil
// receiver and zero value.
func TestIdempotencyConfig_DefaultTTL(t *testing.T) {
	var nilPtr *IdempotencyConfig
	require.Equal(t, 5*time.Minute, nilPtr.EmailTTLDuration(), "nil receiver must default to 5m")
	require.Equal(t, 5*time.Minute, nilPtr.SMSTTLDuration(), "nil receiver must default to 5m")

	var empty IdempotencyConfig
	require.Equal(t, 5*time.Minute, empty.EmailTTLDuration(), "empty EmailTTL must default to 5m")
	require.Equal(t, 5*time.Minute, empty.SMSTTLDuration(), "empty SMSTTL must default to 5m")

	// Explicit values honored.
	cfg := &IdempotencyConfig{EmailTTL: "10m", SMSTTL: "1h"}
	require.Equal(t, 10*time.Minute, cfg.EmailTTLDuration())
	require.Equal(t, time.Hour, cfg.SMSTTLDuration())

	// Unparseable falls back to default.
	bad := &IdempotencyConfig{EmailTTL: "not-a-duration"}
	require.Equal(t, 5*time.Minute, bad.EmailTTLDuration(), "unparseable must fall back to 5m")
}
```

在文件顶部 import 块加 `"time"` 如果尚未导入（检查现有 import 块）。

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./pkg/config/ -run TestIdempotencyConfig_DefaultTTL -v`
Expected: 编译失败 — `IdempotencyConfig` 未定义。

- [ ] **Step 3: 实现 — 在 `pkg/config/config.go` 加新类型**

在 `PersistenceConfig` 之后追加：

```go
// IdempotencyConfig controls the Redis idempotency window per channel. TTLs
// are strings in time.ParseDuration format; viper doesn't decode "5m" into
// time.Duration directly.
type IdempotencyConfig struct {
	EmailTTL string `default:"5m"`
	SMSTTL   string `default:"5m"`
}

// EmailTTLDuration parses EmailTTL. Nil-receiver-safe and falls back to 5m
// on empty/unparseable values (defensive — module-mode callers may skip Load).
func (c *IdempotencyConfig) EmailTTLDuration() time.Duration {
	return parseIdempotencyTTL(c, true)
}

// SMSTTLDuration mirrors EmailTTLDuration.
func (c *IdempotencyConfig) SMSTTLDuration() time.Duration {
	return parseIdempotencyTTL(c, false)
}
```

在文件底部（或 `Validate` 之前的 helper 区）加内部 helper：

```go
// parseIdempotencyTTL resolves an idempotency TTL string. Returns the 5m
// default on nil receiver, empty string, or unparseable input.
func parseIdempotencyTTL(c *IdempotencyConfig, email bool) time.Duration {
	const defaultTTL = 5 * time.Minute
	if c == nil {
		return defaultTTL
	}
	raw := c.SMSTTL
	if email {
		raw = c.EmailTTL
	}
	if raw == "" {
		return defaultTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return defaultTTL
	}
	return d
}
```

在 `Config` struct 加字段（紧跟 `Persistence *PersistenceConfig` 之后）：

```go
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Log        *logging.Config
	Email      *EmailConfig
	SMS        *SMSConfig
	Cron       *cronx.Config
	ThirdParty *ThirdPartyConfig
	Persistence  *PersistenceConfig
	Idempotency  *IdempotencyConfig
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./pkg/config/ -run TestIdempotencyConfig_DefaultTTL -v`
Expected: PASS。

- [ ] **Step 5: 跑整个 config 包做回归**

Run: `go test ./pkg/config/ -v`
Expected: 全部 PASS（5 个旧用例 + 1 个新用例）。

- [ ] **Step 6: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go
git commit -m "feat(config): add IdempotencyConfig with per-channel TTL"
```

---

### Task 3: config 包加 `Redis` 字段（redisx.Config）+ 测试

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [ ] **Step 1: 在 `Config` struct 加 `Redis` 字段**

修改 `Config`：

```go
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Redis      *redisx.Config
	Log        *logging.Config
	Email      *EmailConfig
	SMS        *SMSConfig
	Cron       *cronx.Config
	ThirdParty *ThirdPartyConfig
	Persistence  *PersistenceConfig
	Idempotency  *IdempotencyConfig
}
```

在 import 块加 `"github.com/servekit/go-common/redisx"`（与其他 go-common import 同组）。

- [ ] **Step 2: 写测试 — yaml 加 redis 段可加载**

在 `pkg/config/config_test.go` 末尾追加：

```go
// TestLoad_RedisSection loads a yaml with the redis section and verifies it
// lands in cfg.Redis.
func TestLoad_RedisSection(t *testing.T) {
	dir := t.TempDir()
	writeTestConfig(t, dir, `
server:
  grpc_addr: ":9000"
database:
  host: localhost
  port: 5432
  user: postgres
  password: postgres
  dbname: message_service
  sslmode: disable
redis:
  addr: localhost:6379
  db: 0
third_party:
  gid:
    mode: module
    snowflake:
      machine_id: 1
      start_time: "2026-06-01T00:00:00Z"
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Redis, "Redis section must load")
	require.Equal(t, "localhost:6379", cfg.Redis.Addr)
	require.Equal(t, 0, cfg.Redis.DB)
}
```

- [ ] **Step 3: 运行测试**

Run: `go test ./pkg/config/ -run TestLoad_RedisSection -v`
Expected: PASS（cfg.Redis 由 viper 自动 unmarshal 成 `*redisx.Config`）。

- [ ] **Step 4: 跑整个 config 包做回归**

Run: `go test ./pkg/config/ -v`
Expected: 全部 PASS。

- [ ] **Step 5: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go
git commit -m "feat(config): add Redis section"
```

---

### Task 4: 新增 `internal/idempotency/` 包 + RedisChecker + 测试

**Files:**
- Create: `internal/idempotency/checker.go`
- Create: `internal/idempotency/redis_checker.go`
- Create: `internal/idempotency/redis_checker_test.go`

- [ ] **Step 1: 先写测试 — 6 个用例覆盖 Reserve/Complete/Release 行为**

创建 `internal/idempotency/redis_checker_test.go`：

```go
package idempotency

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/servekit/go-common/redisx"
)

func newTestChecker(t *testing.T) (*RedisChecker, *redis.Client) {
	t.Helper()
	client := redisx.NewTestClient(t)
	return NewRedisChecker(client), client
}

func TestRedisChecker_Reserve_FirstCallAcquires(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	acquired, payload, err := c.Reserve(ctx, "email", "user:1", "key-1", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired, "first call must acquire")
	assert.Nil(t, payload, "first call has no payload")
}

func TestRedisChecker_Reserve_SecondCallAfterCompleteReturnsPayload(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "email", "user:1", "key-2", time.Minute)
	require.NoError(t, err)

	payload := []byte(`{"status":"SENT","id":42}`)
	require.NoError(t, c.Complete(ctx, "email", "user:1", "key-2", payload, time.Minute))

	acquired, got, err := c.Reserve(ctx, "email", "user:1", "key-2", time.Minute)
	require.NoError(t, err)
	assert.False(t, acquired, "second call after Complete must not acquire")
	assert.Equal(t, payload, got, "must return cached payload")
}

func TestRedisChecker_Reserve_SecondCallWhilePendingReturnsNil(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "sms", "user:1", "key-3", time.Minute)
	require.NoError(t, err)

	// Second call before Complete — should see PENDING marker.
	acquired, got, err := c.Reserve(ctx, "sms", "user:1", "key-3", time.Minute)
	require.NoError(t, err)
	assert.False(t, acquired, "in-flight call must not acquire")
	assert.Nil(t, got, "in-flight call returns nil payload (caller returns ErrIdempotencyConflict)")
}

func TestRedisChecker_Complete_OverwritesPending(t *testing.T) {
	c, client := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "email", "user:1", "key-4", time.Minute)
	require.NoError(t, err)

	pending, err := client.Get(ctx, "msg:idem:email:user:1:key-4").Result()
	require.NoError(t, err)
	assert.Equal(t, "PENDING", pending, "Reserve must write PENDING marker")

	payload := []byte(`{"status":"SENT"}`)
	require.NoError(t, c.Complete(ctx, "email", "user:1", "key-4", payload, time.Minute))

	after, err := client.Get(ctx, "msg:idem:email:user:1:key-4").Result()
	require.NoError(t, err)
	assert.Equal(t, string(payload), after, "Complete must overwrite PENDING with payload")
}

func TestRedisChecker_Release_DeletesKey(t *testing.T) {
	c, client := newTestChecker(t)
	ctx := context.Background()

	_, _, err := c.Reserve(ctx, "email", "user:1", "key-5", time.Minute)
	require.NoError(t, err)

	require.NoError(t, c.Release(ctx, "email", "user:1", "key-5"))

	exists, err := client.Exists(ctx, "msg:idem:email:user:1:key-5").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), exists, "Release must delete key")

	// Subsequent Reserve must succeed (acquired=true) — key was released.
	acquired, _, err := c.Reserve(ctx, "email", "user:1", "key-5", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired, "Reserve after Release must acquire")
}

func TestRedisChecker_Reserve_DifferentChannelsDoNotCollide(t *testing.T) {
	c, _ := newTestChecker(t)
	ctx := context.Background()

	// Same senderID + key, different channel — must NOT collide.
	acquired1, _, err := c.Reserve(ctx, "email", "user:1", "shared-key", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired1)

	acquired2, _, err := c.Reserve(ctx, "sms", "user:1", "shared-key", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired2, "different channel must not collide")
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/idempotency/ -v`
Expected: 编译失败 — 包不存在。

- [ ] **Step 3: 实现 `checker.go`（接口 + key helper）**

创建 `internal/idempotency/checker.go`：

```go
// Package idempotency provides a Redis-backed idempotency check for send RPCs.
//
// The check is a three-phase reservation:
//   - Reserve: atomic SETNX claims the key for one caller
//   - Complete: store the response payload (overwrites the PENDING marker)
//   - Release: delete the key so the next caller can retry (used on failure)
//
// Failures are not cached — vendor rejection or transient error releases the
// reservation so the caller can retry the same idempotency_key. The only
// state we keep is "in flight" (PENDING marker) and "completed" (JSON payload).
package idempotency

import (
	"context"
	"fmt"
	"time"
)

// Checker reserves an idempotency key and stores the outcome payload.
// Implementations must be safe for concurrent use.
type Checker interface {
	// Reserve atomically claims the key.
	//
	//   - acquired=true, payload=nil: reservation succeeded; caller MUST call
	//     Complete (success) or Release (failure) afterwards.
	//   - acquired=false, payload!=nil: key already completed within TTL;
	//     caller returns the cached payload as the response.
	//   - acquired=false, payload=nil: another caller holds the reservation
	//     (in-flight); caller returns ErrIdempotencyConflict (defined in
	//     pkg/xcodes — kept out of this package to avoid a cycle).
	//
	// channel is "email" or "sms", used in the Redis key namespace.
	Reserve(ctx context.Context, channel, senderID, key string, ttl time.Duration) (acquired bool, payload []byte, err error)

	// Complete stores the payload and overwrites the PENDING marker set by
	// Reserve. Safe to call only after a successful Reserve on the same key.
	Complete(ctx context.Context, channel, senderID, key string, payload []byte, ttl time.Duration) error

	// Release deletes the reservation. Used when the send fails so the next
	// caller can retry the same key. No-op if the key does not exist.
	Release(ctx context.Context, channel, senderID, key string) error
}

// buildKey constructs the Redis key for an idempotency reservation.
// Format: msg:idem:{channel}:{senderID}:{key}
//
// Channel is namespace-isolated so the same senderID+key pair on different
// channels (email vs sms) does not collide.
func buildKey(channel, senderID, key string) string {
	return fmt.Sprintf("msg:idem:%s:%s:%s", channel, senderID, key)
}
```

- [ ] **Step 4: 实现 `redis_checker.go`**

创建 `internal/idempotency/redis_checker.go`：

```go
package idempotency

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// pendingMarker is the placeholder value written by Reserve. Its presence
// (with empty payload return) signals "another caller is in flight".
const pendingMarker = "PENDING"

// RedisChecker implements Checker with *redis.Client.
type RedisChecker struct {
	client *redis.Client
}

// NewRedisChecker constructs a RedisChecker. client must be non-nil and
// already Ping'd by the caller (service.New does this via redisx.New).
func NewRedisChecker(client *redis.Client) *RedisChecker {
	return &RedisChecker{client: client}
}

// Reserve implements Checker.
func (c *RedisChecker) Reserve(ctx context.Context, channel, senderID, key string, ttl time.Duration) (bool, []byte, error) {
	redisKey := buildKey(channel, senderID, key)
	acquired, err := c.client.SetNX(ctx, redisKey, pendingMarker, ttl).Result()
	if err != nil {
		return false, nil, err
	}
	if acquired {
		return true, nil, nil
	}
	// Key exists — GET to see what's there. Has its own TTL left from the
	// original SETNX; we don't refresh it here.
	val, err := c.client.Get(ctx, redisKey).Result()
	if err != nil {
		return false, nil, err
	}
	if val == pendingMarker {
		return false, nil, nil // in-flight
	}
	return false, []byte(val), nil // completed
}

// Complete implements Checker.
func (c *RedisChecker) Complete(ctx context.Context, channel, senderID, key string, payload []byte, ttl time.Duration) error {
	redisKey := buildKey(channel, senderID, key)
	// SET overwrites the PENDING marker. TTL refreshes from the original
	// Reserve so the cached payload has the full window.
	return c.client.Set(ctx, redisKey, payload, ttl).Err()
}

// Release implements Checker.
func (c *RedisChecker) Release(ctx context.Context, channel, senderID, key string) error {
	redisKey := buildKey(channel, senderID, key)
	// DEL is a no-op if the key has already expired or been released.
	return c.client.Del(ctx, redisKey).Err()
}
```

- [ ] **Step 5: 运行测试验证通过**

Run: `go test ./internal/idempotency/ -v -count=1`
Expected: 6 个用例全部 PASS。

- [ ] **Step 6: 跑 lint 单独验证新包**

Run: `golangci-lint run ./internal/idempotency/...`
Expected: 0 issues.

- [ ] **Step 7: Commit**

```bash
git add internal/idempotency/
git commit -m "feat(idempotency): add Redis-backed Checker"
```

---

## Phase 2: 接线（build 仍绿，发送行为不变）

### Task 5: 把 Checker 接入 `message.Service`（签名变更 + service.New 初始化 + 测试 helper）

**Files:**
- Modify: `internal/service/message/service.go`
- Modify: `internal/service/service.go`
- Modify: `internal/service/message/email_test.go`
- Modify: `internal/service/message/sms_test.go`

- [ ] **Step 1: 改 `internal/service/message/service.go` — 重命名 + 加字段 + 新签名**

把 `PersistenceConfig` 重命名为 `ServiceConfig` 并加 TTL 字段；`Service` 加 `idem` 与 TTL 字段；`New` 加 `idem` 参数。

完整替换文件内容（保留 godoc 注释风格）：

```go
// Package message contains the message domain business logic.
package message

import (
	"time"

	"message-service/internal/idempotency"
	"message-service/internal/provider/email"
	"message-service/internal/provider/sms"
	"message-service/pkg/thirdcall"

	"gorm.io/gorm"
)

// ServiceConfig is the resolved (yaml + option) form consumed by the
// message subpackage. Persistence toggles are orthogonal to idempotency
// TTLs — Redis idempotency always runs; persistence controls DB only.
type ServiceConfig struct {
	PersistEmail bool
	PersistSMS   bool

	EmailIdemTTL time.Duration
	SMSIdemTTL   time.Duration
}

// Service is the message domain service. Resources are injected at
// construction; the subpackage does not manage their lifecycle.
type Service struct {
	db            *gorm.DB
	idem          idempotency.Checker
	gid           thirdcall.GIDService
	emailRegistry *email.AccountRegistry
	smsRegistry   *sms.AccountRegistry
	smsRouter     *sms.Router // nil when no routes configured

	persistEmailEnabled bool
	persistSMSEnabled   bool

	emailIdemTTL time.Duration
	smsIdemTTL   time.Duration
}

// New constructs a message domain service with injected resources. idem
// must be non-nil — service-level idempotency is mandatory.
func New(
	db *gorm.DB,
	idem idempotency.Checker,
	gid thirdcall.GIDService,
	emailRegistry *email.AccountRegistry,
	smsRegistry *sms.AccountRegistry,
	smsRouter *sms.Router,
	cfg ServiceConfig,
) *Service {
	return &Service{
		db:                  db,
		idem:                idem,
		gid:                 gid,
		emailRegistry:       emailRegistry,
		smsRegistry:         smsRegistry,
		smsRouter:           smsRouter,
		persistEmailEnabled: cfg.PersistEmail,
		persistSMSEnabled:   cfg.PersistSMS,
		emailIdemTTL:        cfg.EmailIdemTTL,
		smsIdemTTL:          cfg.SMSIdemTTL,
	}
}
```

- [ ] **Step 2: 改 `internal/service/service.go` — 加 Redis 初始化 + 解析 TTL + 调用 message.New**

在 `service.New` 函数体内，**在 `db, err := resolveDB(...)` 之前**加 Redis 初始化：

```go
	redisClient, err := redisx.New(cfg.Redis)
	if err != nil {
		return nil, fmt.Errorf("redis: %w", err)
	}
	mgr.AddStopper("redis", lifecycle.StopFunc(func() {
		_ = redisClient.Close()
	}))
	idemChecker := idempotency.NewRedisChecker(redisClient)
```

import 块加：

```go
"message-service/internal/idempotency"

"github.com/servekit/go-common/redisx"
"github.com/redis/go-redis/v9"
```

> **lifecycle API 说明**：go-common 的 `lifecycle.Manager` 用 `AddStopper(name string, s lifecycle.Stopper)`。`lifecycle.StopFunc(func() { ... })` 把普通函数适配成 `Stopper`（`Start` 是 no-op，`Stop` 调用函数）。参考 `internal/service/setup.go:139` 注册 db 关闭的写法，pattern 完全一致。

修改 `svc := &Service{...}` 中的 `message.New(...)` 调用为：

```go
		message: message.New(db, idemChecker, gid, emailRegistry, smsRegistry, smsRouter, message.ServiceConfig{
			PersistEmail: emailEnabled,
			PersistSMS:   smsEnabled,
			EmailIdemTTL: cfg.Idempotency.EmailTTLDuration(),
			SMSIdemTTL:   cfg.Idempotency.SMSTTLDuration(),
		}),
```

`emailEnabled` / `smsEnabled` 是 Task 5（persistence-toggle plan）里已有的解析变量。

- [ ] **Step 3: 改 `internal/service/message/email_test.go` — 加 miniredis helper 与 TTL**

在 helper 区（`newTestEmailService` 之前）加：

```go
func newTestIdempotencyChecker(t *testing.T) idempotency.Checker {
	t.Helper()
	client := redisx.NewTestClient(t)
	return idempotency.NewRedisChecker(client)
}
```

import 块加：

```go
"message-service/internal/idempotency"
"github.com/servekit/go-common/redisx"
"time"
```

修改 `newTestEmailService`：

```go
func newTestEmailService(t *testing.T, providers []email.AccountProvider) *Service {
	t.Helper()
	db := setupEmailTestDB(t)
	accounts := make(map[string]email.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	return New(
		db,
		newTestIdempotencyChecker(t),
		getTestGID(t),
		email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: accounts}),
		nil, // smsRegistry
		nil, // smsRouter
		ServiceConfig{
			PersistEmail: true,
			PersistSMS:   true,
			EmailIdemTTL: 5 * time.Minute,
			SMSIdemTTL:   5 * time.Minute,
		},
	)
}
```

同样改 `newTestEmailServiceNoPersist`：把 `PersistenceConfig{Email: false, SMS: false}` 替换为：

```go
		ServiceConfig{
			PersistEmail: false,
			PersistSMS:   false,
			EmailIdemTTL: 5 * time.Minute,
			SMSIdemTTL:   5 * time.Minute,
		},
```

并把 `New(...)` 调用插入 `newTestIdempotencyChecker(t)` 作为第二个参数。

- [ ] **Step 4: 改 `internal/service/message/sms_test.go` — 同样改 helper**

import 块加同样的三个包。

修改 `newTestSMSServiceWithRouter` 末尾的 `New(...)` 调用：

```go
	return New(db, newTestIdempotencyChecker(t), getTestGID(t), nil, registry, router, ServiceConfig{
		PersistEmail: true,
		PersistSMS:   true,
		EmailIdemTTL: 5 * time.Minute,
		SMSIdemTTL:   5 * time.Minute,
	})
```

修改 `newTestSMSServiceNoPersist` 的 `New(...)` 调用：同样加 `newTestIdempotencyChecker(t)` 与 `ServiceConfig{PersistEmail: false, PersistSMS: false, EmailIdemTTL: 5*time.Minute, SMSIdemTTL: 5*time.Minute}`。

- [ ] **Step 5: 编译整个项目**

Run: `go build ./...`
Expected: 无输出（成功）。如果 `service.New` 中 `lifecycle.Named` API 不对，按 grep 的结果调整。

- [ ] **Step 6: 跑现有 service/message 测试做回归**

Run: `go test ./internal/service/message/... -count=1`
Expected: 全部 PASS（行为没变 — DB 幂等仍跑，Redis Checker 注入了但还没用上）。

- [ ] **Step 7: Commit**

```bash
git add internal/service/message/service.go internal/service/service.go \
        internal/service/message/email_test.go internal/service/message/sms_test.go
git commit -m "refactor(message): wire idempotency.Checker into Service (no behavior change)"
```

---

## Phase 3: 发送路径切到 Redis（行为变更）

### Task 6: SendEmail 切到 Redis 幂等 + 拆掉 persistence 幂等 gate + 改写测试

**Files:**
- Modify: `internal/service/message/email.go`
- Modify: `internal/service/message/email_test.go`

- [ ] **Step 1: 先写测试 — 同 key 第二次返回缓存，provider 只调一次**

在 `internal/service/message/email_test.go` 末尾追加：

```go
// TestSendEmail_Idempotent_SecondCallReturnsCached verifies that a second
// send with the same idempotency_key returns the cached response without
// re-invoking the provider.
func TestSendEmail_Idempotent_SecondCallReturnsCached(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:             "user@example.com",
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	resp1, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp1.Status)

	resp2, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, resp1.Id, resp2.Id, "second call must return cached ID")
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp2.Status)
	assert.Equal(t, 1, provider.calls, "provider must be called once (second served from cache)")
}

// TestSendEmail_IdempotencyConflict_OnInFlight verifies that a second send
// while the first is still PENDING returns ErrIdempotencyConflict.
func TestSendEmail_IdempotencyConflict_OnInFlight(t *testing.T) {
	svc := newTestEmailService(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	// Manually plant a PENDING marker to simulate in-flight.
	require.NoError(t, svc.idem.Complete(context.Background(), "email", "user:42", "in-flight-key", []byte("PENDING"), 5*time.Minute))

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:             "user@example.com",
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "in-flight-key",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrIdempotencyConflict.New()),
		"err must wrap ErrIdempotencyConflict, got: %v", err)
}

// TestSendEmail_Failure_NotCached_ReleasesReservation verifies that a failed
// send releases the Redis reservation so the caller can retry the same key.
func TestSendEmail_Failure_NotCached_ReleasesReservation(t *testing.T) {
	// mockEmailProvider.Send returns m.err and nil result, triggering the
	// pre-send failure path (Release + return ErrMessageSendFailed).
	provider := &mockEmailProvider{name: "mock", err: errors.New("vendor rejected")}
	svc := newTestEmailService(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:             "user@example.com",
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "fail-once",
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.Error(t, err, "first call must fail")

	// After Release, a direct Reserve on the same key must succeed (key gone).
	// This proves the reservation was released, not left as PENDING.
	acquired, payload, err := svc.idem.Reserve(context.Background(), "email", "user:42", "fail-once", time.Minute)
	require.NoError(t, err)
	assert.True(t, acquired, "Reserve after failed send must acquire (key was Released)")
	assert.Nil(t, payload, "no cached payload (failure was not cached)")
}
```

需要 import `errors`、`time`、`xcodes`：检查 email_test.go 现有 import，缺则补。

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/service/message/ -run 'TestSendEmail_(Idempotent_SecondCallReturnsCached|IdempotencyConflict_OnInFlight|Failure_NotCached_ReleasesReservation)' -v`
Expected: 至少前两个 FAIL（当前 DB 幂等行为：第二次调用返回缓存但调用了 dal，不是 Redis；in-flight 概念不存在）。第三个可能也 FAIL（当前失败也写 DB）。

同时 `TestSendEmail_PersistenceDisabled_IdempotencyNoOp` 会继续 PASS（DB gate 仍在）。这个测试在 Step 4 一起改。

- [ ] **Step 3: 实现 — `internal/service/message/email.go` `SendEmail` 替换 DB 幂等块**

定位现有幂等块（被 `if s.persistEmailEnabled { ... }` 包裹）：

```go
	// Idempotency check (skipped when persistence disabled — no DB to consult).
	if s.persistEmailEnabled {
		if key := req.GetIdempotencyKey(); key != "" {
			existing, err := dal.GetEmailRecordByIdempotencyKey(ctx, s.db, req.GetSenderId(), key)
			if err != nil {
				return nil, xcodes.ErrInternal.Wrap(err)
			}
			if existing != nil {
				return respondIdempotentEmail(existing)
			}
		}
	}
```

替换为（无 persistence gate — Redis 永远跑）：

```go
	// Idempotency reservation (Redis-backed). Always runs regardless of
	// persistence toggle — Redis is the single source of dedup truth.
	var idemKey string
	if k := req.GetIdempotencyKey(); k != "" {
		idemKey = k
		acquired, payload, err := s.idem.Reserve(ctx, "email", req.GetSenderId(), k, s.emailIdemTTL)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		if !acquired {
			if payload == nil {
				return nil, xcodes.ErrIdempotencyConflict.New("idempotency_key in flight")
			}
			resp, err := deserializeIdempotentEmail(payload)
			if err != nil {
				return nil, xcodes.ErrInternal.Wrap(err)
			}
			return resp, nil
		}
	}
```

定位发送后的持久化块（保留 persistence gate）：

```go
	if s.persistEmailEnabled {
		persistCtx, cancel := context.WithTimeout(context.Background(), persistTimeout)
		defer cancel()
		s.persistEmailRecord(persistCtx, id, req, result)
	}
```

**保留**，不动。但是在这块**之前**（vendor Send 之后、pre-send 失败检查之后）加 Redis Complete / Release：

定位 pre-send 失败检查：

```go
	if sendErr != nil && result == nil {
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr, "stage=pre_send")
	}
```

改为（先 Release 再 return）：

```go
	if sendErr != nil && result == nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after pre-send failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr, "stage=pre_send")
	}
```

在持久化块之后、最后的 `if sendErr != nil` 之前，加 Complete：

```go
	// Idempotency Complete: cache the response payload so a second call with
	// the same key returns the cached result. Failures logged but don't
	// affect the response — the send already succeeded.
	if idemKey != "" {
		resp := &pb.SendResponse{
			Id:     id,
			Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
			Vendor: &pb.SendResponse_EmailVendor{
				EmailVendor: result.Vendor,
			},
		}
		payload, err := json.Marshal(resp)
		if err != nil {
			slog.Error("idempotency marshal", "key", idemKey, "error", err)
		} else if err := s.idem.Complete(context.Background(), "email", req.GetSenderId(), idemKey, payload, s.emailIdemTTL); err != nil {
			slog.Error("idempotency complete", "key", idemKey, "error", err)
		}
	}
```

定位最后的 post-send 失败检查：

```go
	if sendErr != nil {
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
			"vendor=%s account=%s attempts=%d",
			result.Vendor.String(), result.Account, result.Attempts)
	}
```

改为（先 Release 再 return —— 失败不缓存）：

```go
	if sendErr != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "email", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after post-send failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
			"vendor=%s account=%s attempts=%d",
			result.Vendor.String(), result.Account, result.Attempts)
	}
```

import 块加 `"encoding/json"`。

- [ ] **Step 4: 替换 `respondIdempotentEmail` 为 `deserializeIdempotentEmail`**

定位 `respondIdempotentEmail`（在 email.go 底部）：

```go
func respondIdempotentEmail(existing *models.MessageEmailRecord) (*pb.SendResponse, error) { ... }
```

**整个函数删除**。替换为：

```go
// deserializeIdempotentEmail rebuilds the SendResponse from a cached
// idempotency payload (JSON-serialized pb.SendResponse written by a prior
// successful send).
func deserializeIdempotentEmail(payload []byte) (*pb.SendResponse, error) {
	var resp pb.SendResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal cached idempotent response: %w", err)
	}
	return &resp, nil
}
```

如果 `models` 包因此变成未使用 import，删掉。

- [ ] **Step 5: 改写 `TestSendEmail_PersistenceDisabled_IdempotencyNoOp` 为 `IdempotencyStillWorks`**

在 `email_test.go` 定位：

```go
func TestSendEmail_PersistenceDisabled_IdempotencyNoOp(t *testing.T) {
	...
	assert.Equal(t, 2, provider.calls, "provider must be called twice when persistence disabled")
}
```

整个函数替换为：

```go
// TestSendEmail_PersistenceDisabled_IdempotencyStillWorks verifies that
// Redis idempotency is independent of the persistence toggle: even with
// persistence off, the same idempotency_key is deduped via Redis.
func TestSendEmail_PersistenceDisabled_IdempotencyStillWorks(t *testing.T) {
	provider := &mockEmailProvider{name: "mock"}
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{provider})

	req := &pb.SendEmailRequest{
		To:             "user@example.com",
		Subject:        "Test",
		Body:           "Hello",
		Scene:          pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "abc-123",
	}

	_, err := svc.SendEmail(context.Background(), req)
	require.NoError(t, err)
	_, err = svc.SendEmail(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 1, provider.calls, "provider must be called once (Redis dedup works even with persistence off)")
}
```

- [ ] **Step 6: 运行新测试验证通过**

Run: `go test ./internal/service/message/ -run 'TestSendEmail_(Idempotent_SecondCallReturnsCached|IdempotencyConflict_OnInFlight|Failure_NotCached_ReleasesReservation|PersistenceDisabled_IdempotencyStillWorks|PersistenceDisabled_SkipsDB)' -v -count=1`
Expected: 全部 PASS。

- [ ] **Step 7: 跑所有 email 测试做回归**

Run: `go test ./internal/service/message/ -run TestSendEmail -v -count=1`
Expected: 全部 PASS。

- [ ] **Step 8: Commit**

```bash
git add internal/service/message/email.go internal/service/message/email_test.go
git commit -m "feat(message/email): switch idempotency to Redis, drop persistence gate on dedup"
```

---

### Task 7: SendSMS 切到 Redis 幂等（与 Task 6 对称）

**Files:**
- Modify: `internal/service/message/sms.go`
- Modify: `internal/service/message/sms_test.go`

- [ ] **Step 1: 先写测试**

在 `internal/service/message/sms_test.go` 末尾追加（与 email 对称，注意 vendor 字段是 `SmsVendor`）：

```go
func TestSendSMS_Idempotent_SecondCallReturnsCached(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{provider})

	req := &pb.SendSMSRequest{
		To:             "+8613800001111",
		Content:        "code",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "sms-1",
	}

	resp1, err := svc.SendSMS(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp1.Status)

	resp2, err := svc.SendSMS(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, resp1.Id, resp2.Id)
	assert.Equal(t, 1, provider.calls, "provider must be called once")
}

func TestSendSMS_IdempotencyConflict_OnInFlight(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	require.NoError(t, svc.idem.Complete(context.Background(), "sms", "user:42", "in-flight", []byte("PENDING"), 5*time.Minute))

	_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		To:             "+8613800001111",
		Content:        "code",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "in-flight",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrIdempotencyConflict.New()))
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/service/message/ -run 'TestSendSMS_(Idempotent_SecondCallReturnsCached|IdempotencyConflict_OnInFlight)' -v`
Expected: FAIL（当前仍是 DB 幂等）。

- [ ] **Step 3: 实现 — `internal/service/message/sms.go`**

3a. 把开头的 DB 幂等块：

```go
	if s.persistSMSEnabled {
		if key := req.GetIdempotencyKey(); key != "" {
			existing, err := dal.GetSMSRecordByIdempotencyKey(ctx, s.db, req.GetSenderId(), key)
			if err != nil {
				return nil, xcodes.ErrInternal.Wrap(err)
			}
			if existing != nil {
				return respondIdempotentSMS(existing)
			}
		}
	}
```

替换为：

```go
	var idemKey string
	if k := req.GetIdempotencyKey(); k != "" {
		idemKey = k
		acquired, payload, err := s.idem.Reserve(ctx, "sms", req.GetSenderId(), k, s.smsIdemTTL)
		if err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		if !acquired {
			if payload == nil {
				return nil, xcodes.ErrIdempotencyConflict.New("idempotency_key in flight")
			}
			resp, err := deserializeIdempotentSMS(payload)
			if err != nil {
				return nil, xcodes.ErrInternal.Wrap(err)
			}
			return resp, nil
		}
	}
```

3b. pre-send 失败处加 Release：

```go
	if sendErr != nil && result == nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after pre-send failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr, "stage=pre_send")
	}
```

3c. 持久化块之后加 Complete：

```go
	if idemKey != "" {
		resp := &pb.SendResponse{
			Id:     id,
			Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
			Vendor: &pb.SendResponse_SmsVendor{
				SmsVendor: result.Vendor,
			},
		}
		payload, err := json.Marshal(resp)
		if err != nil {
			slog.Error("idempotency marshal", "key", idemKey, "error", err)
		} else if err := s.idem.Complete(context.Background(), "sms", req.GetSenderId(), idemKey, payload, s.smsIdemTTL); err != nil {
			slog.Error("idempotency complete", "key", idemKey, "error", err)
		}
	}
```

3d. post-send 失败处加 Release（与 pre-send 对称，channel="sms"）。

3e. 删 `respondIdempotentSMS`，加：

```go
func deserializeIdempotentSMS(payload []byte) (*pb.SendResponse, error) {
	var resp pb.SendResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal cached idempotent response: %w", err)
	}
	return &resp, nil
}
```

3f. import 加 `"encoding/json"`。

- [ ] **Step 4: 改写 `TestSendSMS_PersistenceDisabled_IdempotencyNoOp` → `IdempotencyStillWorks`**

参照 Task 6 Step 5 模式，断言 `provider.calls == 1`。

- [ ] **Step 5: 运行新测试 + 回归**

Run: `go test ./internal/service/message/ -v -count=1`
Expected: 全部 PASS（email + sms）。

- [ ] **Step 6: Commit**

```bash
git add internal/service/message/sms.go internal/service/message/sms_test.go
git commit -m "feat(message/sms): switch idempotency to Redis, drop persistence gate on dedup"
```

---

## Phase 4: DB 清理（删 idempotency_key 列、索引、dal 函数）

### Task 8: DB 删除 idempotency_key（GORM model + dal + cmd/migrate + tests）

**Files:**
- Modify: `internal/store/models/email_record.go`
- Modify: `internal/store/models/sms_record.go`
- Modify: `internal/store/generated/email_record.go`
- Modify: `internal/store/generated/sms_record.go`
- Modify: `internal/store/dal/email_record.go`
- Modify: `internal/store/dal/email_record_test.go`
- Modify: `internal/store/dal/sms_record.go`
- Modify: `internal/store/dal/sms_record_test.go`
- Modify: `cmd/migrate/main.go`

- [ ] **Step 1: 删 GORM model 的 IdempotencyKey 字段**

`internal/store/models/email_record.go`：定位 `IdempotencyKey string \`gorm:"size:64;column:idempotency_key"\``，整行删除。同样改 `internal/store/models/sms_record.go`。

- [ ] **Step 2: 重新生成 GORM gen 代码**

Run: `make generate`（实际执行 `gorm gen -i ./internal/store/models -o ./internal/store/generated`）

Expected: `internal/store/generated/email_record.go` 和 `sms_record.go` 不再含 `IdempotencyKey field.String`。

如果生成失败或与现有代码 diff 过大（gorm 版本问题），**手动**编辑 `internal/store/generated/email_record.go` 与 `sms_record.go`：
- 删 `IdempotencyKey field.String` 行（struct 字段，大约第 18-20 行附近）
- 删 `IdempotencyKey: field.String{}.WithColumn("idempotency_key"),` 行（在 struct 初始化块里）

- [ ] **Step 3: 删 dal 函数**

`internal/store/dal/email_record.go`：删 `GetEmailRecordByIdempotencyKey` 函数（含 godoc 注释）。
`internal/store/dal/sms_record.go`：删 `GetSMSRecordByIdempotencyKey` 函数。

- [ ] **Step 4: 删 dal 测试**

`internal/store/dal/email_record_test.go`：删 `TestGetEmailRecordByIdempotencyKey_Hit` 和 `TestGetEmailRecordByIdempotencyKey_NotFound`（含 setup 代码中创建带 IdempotencyKey 的 record 的部分，要相应调整）。
`internal/store/dal/sms_record_test.go`：删 `TestGetSMSRecordByIdempotencyKey_Hit` 和 `TestGetSMSRecordByIdempotencyKey_NotFound`。

如果有其他测试用例设置了 `record.IdempotencyKey = "..."`，把这些赋值也删掉（字段不存在了，编译会失败）。

- [ ] **Step 5: 改 `cmd/migrate/main.go` — 删 partial unique index SQL，加 DROP INDEX 兜底**

定位现有 index 创建代码：

```go
	indexes := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_email_records_sender_idempotency
		   ON ` + emailTable + ` (sender_id, idempotency_key)
		   WHERE idempotency_key != ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_sms_records_sender_idempotency
		   ON ` + smsTable + ` (sender_id, idempotency_key)
		   WHERE idempotency_key != ''`,
	}
	for _, ddl := range indexes {
		if err := db.Exec(ddl).Error; err != nil {
			slog.Error("create index failed", "ddl", ddl, "error", err)
			os.Exit(1)
		}
	}
```

整段替换为：

```go
	// Drop legacy idempotency indexes from the DB-based idempotency era.
	// AutoMigrate dropped the idempotency_key column above; these indexes
	// reference that column and must be removed explicitly (Postgres does
	// not auto-drop indexes when the underlying column is dropped).
	drops := []string{
		`DROP INDEX IF EXISTS idx_email_records_sender_idempotency`,
		`DROP INDEX IF EXISTS idx_sms_records_sender_idempotency`,
	}
	for _, ddl := range drops {
		if err := db.Exec(ddl).Error; err != nil {
			slog.Error("drop legacy index failed", "ddl", ddl, "error", err)
			os.Exit(1)
		}
	}
```

删除现已不需要的 `emailTable` / `smsTable` 解析代码（如果只被 indexes 用）—— **但 tableName helper 本身可能仍被引用，先 grep 确认**：

Run: `grep -n "tableName" /Users/moss/code/base/message-service/cmd/migrate/main.go`

如果只有 indexes 段用，连同 helper 与变量一起删。

- [ ] **Step 6: 编译验证**

Run: `go build ./...`
Expected: 无输出。

- [ ] **Step 7: 跑 dal 测试做回归**

Run: `go test ./internal/store/dal/... -count=1`
Expected: 全部 PASS。

- [ ] **Step 8: 跑全量测试**

Run: `go test ./... -count=1`
Expected: 全部 PASS。

- [ ] **Step 9: Commit**

```bash
git add internal/store/models/ internal/store/generated/ internal/store/dal/ cmd/migrate/main.go
git commit -m "refactor(store): drop idempotency_key from DB schema and dal"
```

---

## Phase 5: 配置文件 + Docker

### Task 9: 更新 `config.yaml` / `config.docker.yaml` / `docker-compose.yaml`

**Files:**
- Modify: `config.yaml`
- Modify: `config.docker.yaml`
- Modify: `docker-compose.yaml`

- [ ] **Step 1: `config.yaml` 加 redis 与 idempotency 段**

在 `database:` 段之后插入：

```yaml
redis:
  addr: localhost:6379
  password: ""
  db: 0
```

在 `persistence:` 段之后插入：

```yaml
idempotency:
  email_ttl: 5m   # per-channel idempotency window; time.ParseDuration format
  sms_ttl: 5m
```

- [ ] **Step 2: `config.docker.yaml` 加对应环境变量段**

先 `cat /Users/moss/code/base/message-service/config.docker.yaml` 看现有结构。然后在 `database:` 段之后插入（用 ${VAR} 形式）：

```yaml
redis:
  addr: ${MESSAGE_SERVICE_REDIS_ADDR}
  password: ${MESSAGE_SERVICE_REDIS_PASSWORD}
  db: 0
```

在 `persistence:` 段之后插入：

```yaml
idempotency:
  email_ttl: ${MESSAGE_SERVICE_EMAIL_IDEM_TTL}
  sms_ttl: ${MESSAGE_SERVICE_SMS_IDEM_TTL}
```

- [ ] **Step 3: `docker-compose.yaml` 加 redis 服务 + 环境变量**

先 `cat /Users/moss/code/base/message-service/docker-compose.yaml` 看现有 services。在 `services:` 下加（与 postgres 同级）：

```yaml
  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis-data:/data
    command: ["redis-server", "--appendonly", "yes"]
```

在 `volumes:` 段加（如果尚不存在 `volumes:` 段，新增）：

```yaml
volumes:
  postgres-data:
  redis-data:
```

（如果 `postgres-data` 已存在，只加 `redis-data:` 一行。）

在 message-service 服务的 `environment:` 段加（参考现有 `MESSAGE_SERVICE_DB_*`）：

```yaml
      MESSAGE_SERVICE_REDIS_ADDR: redis:6379
      MESSAGE_SERVICE_REDIS_PASSWORD: ""
      MESSAGE_SERVICE_EMAIL_IDEM_TTL: "5m"
      MESSAGE_SERVICE_SMS_IDEM_TTL: "5m"
```

确保 message-service 服务有 `depends_on:` 包含 `redis`（参考 postgres 的写法）。

- [ ] **Step 4: 验证 config 加载**

Run: `go test ./pkg/config/ -v -count=1`
Expected: 全部 PASS（包括 Task 3 加的 `TestLoad_RedisSection`）。

- [ ] **Step 5: Commit**

```bash
git add config.yaml config.docker.yaml docker-compose.yaml
git commit -m "docs(config): add redis + idempotency sections to deploy configs"
```

---

## Phase 6: 最终验证 + 文档

### Task 10: 最终验证（全量测试 + lint + 格式化）

**Files:** 无修改

- [ ] **Step 1: 跑全量测试 + race**

Run: `go test -race -count=1 ./...`
Expected: 全部 PASS。SKIP 不算失败（testcontainer 在无 docker 时）。

- [ ] **Step 2: 跑 lint**

Run: `golangci-lint run ./...`
Expected: 0 issues。

- [ ] **Step 3: 跑格式化检查**

Run: `gofmt -l .` 与 `goimports -l .`（忽略 `gen/` 生成文件）
Expected: 无输出。

如果列出了源文件，跑 `gofmt -w <files>` 后回到 Step 1。

- [ ] **Step 4: 手动 smoke（可选，需 docker）**

```bash
docker compose up -d postgres redis
go run ./cmd/migrate/
go run ./cmd/server/ &
# 用 cmd/testclient 发送一封带 idempotency_key 的邮件
# 重复同 key → 第二次返回相同 id
# 改 idempotency.email_ttl: 10s，重启，10s 后再发同 key → 视为新请求
```

如果 docker 不可用，跳过此步，标记为手动验证。

---

### Task 11: 更新 `CLAUDE.md` + Obsidian 笔记

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: 更新 CLAUDE.md**

定位刚加的"持久化开关（per-channel）"段，**改写为**（注意幂等永远走 Redis，persistence 只管 DB）：

```markdown
### 持久化开关（per-channel）与幂等（Redis）

- **幂等**：永远走 Redis，与持久化无关。`internal/idempotency.Checker` 接口（RedisChecker 实现）三段式：Reserve（SETNX PENDING）/ Complete（SET payload）/ Release（DEL）。失败不缓存，可重试。TTL per-channel：`idempotency.email_ttl` / `sms_ttl`（默认 5m）。
- **持久化**：`persistence.email/sms.enabled`（默认 true）只控制 DB 写入与查询。关闭后发送路径不写 DB（仍走 vendor），查询返回 `xcodes.ErrPersistenceDisabled`（503）。
- **Redis 是硬依赖**：启动时 `redisx.New` Ping 验证，连不上服务起不来。运行期 Redis 报错 → SendEmail/SendSMS 返回 ErrInternal，绝不放过重复发送。
- **DB schema**：`message_*_records` 不再有 `idempotency_key` 列（已删）。幂等键只在 Redis 里。
- **新错误码**：`xcodes.ErrIdempotencyConflict`（409）—— 同 idempotency_key 正在被另一个调用方处理。
- `pkg/option.WithEmailPersistence` / `WithSMSPersistence` 用于 module 模式从代码覆盖 persistence（不影响 Redis 幂等）。
```

定位"### Redis"段（如果存在），确认是否需要补充说明。如果不存在，在"### 数据库 / GORM"段之后插入简短指引：

```markdown
### Redis

- 使用 `go-common/redisx` 统一初始化：`redisx.New(cfg)` 创建客户端（含 Ping 验证）
- 客户端通过构造函数注入到 `idempotency.NewRedisChecker`
- key 命名：`msg:idem:<channel>:<senderID>:<key>`（幂等键）、`<module>:<purpose>:<identifier>` 通用约定
- 测试用 `redisx.NewTestClient(t)` 做 miniredis
```

- [ ] **Step 2: 更新 Obsidian changes.md**

```bash
obsidian vault=only append file="services/message-service/changes" content="
- 2026-06-27: 完成服务 Redis 幂等实施（11 task TDD）— 替换 DB 幂等为 Redis SETNX；新增 ErrIdempotencyConflict/409；persistence toggle 范围收窄；失败不缓存；TTL per-channel 默认 5m；DB 删除 idempotency_key 列与索引"
```

更新 `services/message-service/index.md` 中相关行（如有）。

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: mark Redis idempotency implementation complete"
```

---

## 完成标准

- 所有现有测试 PASS（向后兼容，幂等窗口从 DB forever 缩短到 Redis TTL）。
- 新增 6 个 idempotency 包测试 + 4+ 个 service 测试 PASS。
- `go build ./...` 无错。
- `golangci-lint run ./...` 无错。
- `message_*_records` 表无 `idempotency_key` 列。
- `persistence.email.enabled: false` 时：发送仍 dedup（Redis）、查询返回 `ErrPersistenceDisabled`、DB 不写。
- 同 idempotency_key 在 TTL 内重复发送：返回首次的缓存响应（status=SENT）。
- 同 idempotency_key 在 in-flight 期间发送：返回 `ErrIdempotencyConflict`（409）。
- 失败发送释放 reservation，同 key 立即可重试。
- TTL 过期后同 key：视为新请求。

## 关联

- 设计文档：[[services/message-service/design/v5/2026-06-27-redis-idempotency-design|redis-idempotency-design]]
- 项目内 spec：`docs/superpowers/specs/2026-06-27-redis-idempotency-design.md`
- 前置 spec（范围收窄）：[[services/message-service/design/v5/2026-06-26-persistence-toggle|persistence-toggle]] —— 其幂等 gate 在本计划 Task 6/7 中拆除
