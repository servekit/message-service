# Redis 幂等设计：替换 DB 幂等层

**Date:** 2026-06-27
**Status:** Design（待实施）

## Goal

把 message-service 的发送幂等检查从 Postgres 完全迁移到 Redis。DB 不再存储 `idempotency_key`，幂等窗口由 Redis TTL 决定。同时把刚做的 persistence toggle 与幂等解耦 —— persistence 只管 DB 写入/查询，幂等永远走 Redis。

## Background

当前实现（`2026-06-26-persistence-toggle` 的产物）：

- DB 层幂等：`message_email_records.idempotency_key` + partial unique index `(sender_id, idempotency_key) WHERE idempotency_key != ''`
- `dal.GetEmailRecordByIdempotencyKey` / `dal.GetSMSRecordByIdempotencyKey` 在发送前查 DB
- persistence toggle 同时 gate 幂等检查和 DB 写入（关掉就既不 dedup 也不落库）
- 问题：persistence-off 时调用方完全失去去重能力，同一 `idempotency_key` 会反复打 vendor

新设计：

- 幂等检查搬到 Redis，TTL 控制 dedup 窗口
- DB 不再保留 `idempotency_key` 列与索引
- persistence toggle 收窄为只控制 DB 写入/查询；幂等永远在
- Redis 成为该服务的硬依赖（启动时 Ping 验证）

## Architecture

### 新增 `internal/idempotency/` 包

```go
// Package idempotency provides a Redis-backed idempotency check for send RPCs.
package idempotency

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
    //     (in-flight); caller returns ErrIdempotencyConflict.
    //
    // channel is "email" or "sms", used in the Redis key namespace.
    Reserve(ctx context.Context, channel, senderID, key string, ttl time.Duration) (acquired bool, payload []byte, err error)

    // Complete stores the payload and overwrites the PENDING marker set by
    // Reserve. Idempotent: safe to call after a successful Reserve.
    Complete(ctx context.Context, channel, senderID, key string, payload []byte, ttl time.Duration) error

    // Release deletes the reservation. Used when the send fails so the next
    // caller can retry the same key. No-op if the key does not exist.
    Release(ctx context.Context, channel, senderID, key string) error
}

// RedisChecker implements Checker with *redis.Client.
type RedisChecker struct {
    client *redis.Client
}
```

### Redis 数据模型

**Key 命名：** `msg:idem:{channel}:{senderID}:{key}`

- `channel` = `email` / `sms`，避免跨通道撞键
- `senderID` = 调用方提供的发送者标识（如 `user:42`）
- `key` = 调用方提供的幂等键（最长 64 字符，proto 限制）

**Value：** JSON 序列化的 payload。`Reserve` 时先写入 `"PENDING"` 字符串作为标记。

```json
// PENDING 标记
"PENDING"

// 完成后的 payload（成功）
{"status":"SENT","id":1234567890,"vendor":"EMAIL_VENDOR_CUSTOM_SMTP","account":"default"}

// 注意：失败不缓存。失败时 Release 删除 key，让调用方可以重试。
```

### 并发模型

`Reserve` 用 `SET NX EX`（Redis 原子操作）抢锁：

```
1. SET key "PENDING" NX EX ttl
   - OK   → acquired=true，调用方进入发送路径
   - nil  → key 已存在，进入 GET 分支
2. (key 已存在) GET key
   - "PENDING"         → acquired=false, payload=nil（in-flight）
   - JSON payload      → acquired=false, payload=<cached>
```

只有一个调用方能通过 Reserve 进入发送路径，杜绝 thundering herd。

`Complete` 用 `SET key payload EX ttl`（覆盖 PENDING）。
`Release` 用 `DEL key`。

### Service 层改造

`message.Service` 加两个字段：

```go
type Service struct {
    db            *gorm.DB
    idem          idempotency.Checker // never nil after New
    gid           thirdcall.GIDService
    emailRegistry *email.AccountRegistry
    smsRegistry   *sms.AccountRegistry
    smsRouter     *sms.Router

    persistEmailEnabled bool
    persistSMSEnabled   bool

    emailIdemTTL time.Duration
    smsIdemTTL   time.Duration
}
```

`New` 签名加 `idem idempotency.Checker` 参数和两个 TTL。

外层 `service.New` 启动时：
1. `redisx.New(cfg.Redis)` 创建 Redis 客户端（含 Ping 验证，连不上直接报错）
2. `idempotency.NewRedisChecker(client)` 创建 Checker
3. 解析 `cfg.Idempotency.EmailTTL` / `SMSTTL`（字符串 → time.Duration）
4. 把 Checker 和 TTL 传给 `message.New`

### SendEmail/SendSMS 流程

替换原来的 DB 幂等块（`if s.persistEmailEnabled { dal.GetEmailRecordByIdempotencyKey ... }`）。新流程：

```
1. If idempotency_key is empty → skip Redis, proceed with send (no dedup)
2. Reserve("email", sender_id, key, ttl):
   - err != nil            → return ErrInternal (Redis broken, fail closed)
   - acquired=true          → continue to step 3
   - acquired=false, payload!=nil → return cached SendResponse (cache hit)
   - acquired=false, payload==nil → return ErrIdempotencyConflict (in-flight)
3. Send to vendor (existing logic, unchanged)
4. Outcome:
   - success (result != nil):
       Complete("email", sender_id, key, json(response), ttl)  // overwrite PENDING
       (Complete error logged but doesn't fail the response — send already succeeded)
   - failure (vendor rejected, network error, etc.):
       Release("email", sender_id, key)  // delete reservation, allow retry
5. Return send response
```

**关键语义：**

- **失败不缓存**：vendor 拒绝/超时一律 Release，调用方同 key 可重试。和当前 DB 行为（缓存失败，返回相同错误）不同 —— 用户明确选择这个改动以便瞬时错误能重试。
- **Complete/Release 错误不阻塞返回**：发送已经发生（成功）或者即将返回失败，Redis 状态机的滞后写入只打日志。
- **`respondIdempotentEmail` / `respondIdempotentSMS` 函数删除**：缓存反序列化逻辑内联或抽到 `internal/idempotency/` 的 helper。

## DB Schema 改动

 destructive（memory 记录：no prod data yet，可破坏性迁移）：

- 删 `message_email_records.idempotency_key` 列（GORM model 移除字段）
- 删 `message_sms_records.idempotency_key` 列
- 删 `email_records_sender_id_idempotency_key_idx` partial unique index
- 删 `sms_records_sender_id_idempotency_key_idx` partial unique index
- `cmd/migrate/main.go` 移除创建索引的 raw SQL；AutoMigrate 自动处理列删除（drop column）
- `dal.GetEmailRecordByIdempotencyKey` / `dal.GetSMSRecordByIdempotencyKey` 删除
- `dal/email_record_test.go` / `sms_record_test.go` 中相关测试用例（`TestGetXxxRecordByIdempotencyKey_*`）删除

## Config

新增 `redis` 和 `idempotency` 段：

```yaml
redis:
  addr: localhost:6379
  password: ""
  db: 0
  # 其他 go-common/redisx 字段

idempotency:
  email_ttl: 5m   # email 通道幂等窗口
  sms_ttl: 5m     # sms 通道幂等窗口
```

```go
type Config struct {
    // ...existing fields...
    Redis       *redisx.Config
    Idempotency *IdempotencyConfig
}

// IdempotencyConfig 控制幂等窗口。TTL 用字符串（time.ParseDuration 格式），
// viper 不直接支持 "5m" → time.Duration 的 decode。
type IdempotencyConfig struct {
    EmailTTL string `default:"5m"`
    SMSTTL   string `default:"5m"`
}

// EmailTTLDuration parses EmailTTL. Nil-receiver-safe; falls back to 5m on
// empty/unparseable values (defensive — module-mode callers may skip Load).
func (c *IdempotencyConfig) EmailTTLDuration() time.Duration { ... }

// SMSTTLDuration mirrors EmailTTLDuration.
func (c *IdempotencyConfig) SMSTTLDuration() time.Duration { ... }
```

TTL 用字符串而非 `time.Duration` —— viper 默认不支持把 `"5m"` 直接 decode 成 Duration，字符串 + `time.ParseDuration` 更明确。

## Error Codes

新增 1 个：

```go
// ErrIdempotencyConflict indicates a send with the same idempotency_key is
// currently in flight (another caller holds the reservation). Caller can
// retry the same request after the in-flight call completes.
var ErrIdempotencyConflict = xerr.New(
    "IDEMPOTENCY_CONFLICT",
    xerr.CategoryConflict,
    409,
    "idempotency_key is in flight",
)
```

`ErrPersistenceDisabled`（已有）继续用于查询路径。

## Failure Modes

| 情况 | 行为 |
|------|------|
| 启动时 Redis 连不上 | `redisx.New` 返回错误，`service.New` 直接失败，服务起不来 |
| 运行期 Reserve/Complete/Release 报错 | SendEmail/SendSMS 返回 `ErrInternal`；绝不放过重复发送 |
| TTL 过期后同 key 重发 | 视为新请求，正常发送 |
| 进程在 Reserve 后崩溃 | key 卡在 PENDING 直到 TTL；期间同 key 返回 `ErrIdempotencyConflict`；TTL 后可重试 |

## Interaction with persistence toggle

`2026-06-26-persistence-toggle` 的范围收窄：

| 元素 | 旧 | 新 |
|------|-----|-----|
| `persistence.email/sms.enabled` 控制 DB 写入 | ✓ | ✓（保留） |
| `persistence.email/sms.enabled` 控制查询返回 `ErrPersistenceDisabled` | ✓ | ✓（保留） |
| `persistence.email/sms.enabled` gate SendEmail/SendSMS 幂等检查 | ✓ | **删除** |

代码改动：
- `email.go` / `sms.go` 中 `if s.persistEmailEnabled { 幂等块 }` 的 gate 拆除，幂等块替换为 Redis 版本（无条件执行）
- `if s.persistEmailEnabled { persistXxxRecord }` 的 gate 保留
- `TestSendEmail_PersistenceDisabled_IdempotencyNoOp` 改写为 `TestSendEmail_PersistenceDisabled_IdempotencyStillWorks` —— 关掉持久化时，同 key 重复发送只打 vendor 一次（Redis 仍然 dedup）

最终行为矩阵：

| persistence | Redis idempotency | 发送路径 | 查询路径 |
|-------------|-------------------|----------|----------|
| on（默认） | on | Redis dedup + DB 落库 | 正常 |
| off | on | Redis dedup，不写 DB | ErrPersistenceDisabled |

Redis 永远必装，persistence toggle 只控制 DB。

## Test Plan

新增 `internal/idempotency/`：

1. `TestRedisChecker_Reserve_AcquiresOnFirstCall` —— 首次 SETNX 成功，acquired=true
2. `TestRedisChecker_Reserve_ReturnsCachedOnCompleted` —— 第二次同 key 拿到 payload，acquired=false, payload 非空
3. `TestRedisChecker_Reserve_ReturnsConflictOnInFlight` —— PENDING 状态时 acquired=false, payload=nil
4. `TestRedisChecker_Complete_OverwritesPending` —— Complete 后 GET 拿到 payload 而非 PENDING
5. `TestRedisChecker_Release_DeletesKey` —— Release 后 GET 返回 nil
6. `TestRedisChecker_TTL_ExpiresAfterDuration` —— TTL 过期后 key 自动消失

用 miniredis（`redisx.NewTestClient(t)` 已封装），无外部 Redis 依赖。

修改 `internal/service/message/`：

7. 更新 `newTestEmailService` / `newTestSMSService` helper 注入 miniredis Checker
8. `TestSendEmail_Idempotent_SecondCallReturnsCached`（替换原 `TestSendEmail_Idempotent_*` DB 版本）—— 同 key 第二次返回缓存响应，provider 只被调一次
9. `TestSendEmail_IdempotencyConflict_OnConcurrentInFlight` —— 模拟 PENDING 状态，第二次返回 `ErrIdempotencyConflict`
10. `TestSendEmail_Failure_NotCached_Retryable` —— 失败后同 key 可重发
11. `TestSendEmail_PersistenceDisabled_IdempotencyStillWorks` —— 改写：persistence off 时 Redis 仍 dedup

`internal/store/dal/`：

12. 删除 `TestGetEmailRecordByIdempotencyKey_*` / `TestGetSMSRecordByIdempotencyKey_*`

## Out of Scope

- per-channel idempotency enable flag（要关就两个都关，YAGNI）
- async 队列、批量发送
- Redis 集群/分片配置（用 redisx 默认单机/集群透明处理）
- DB 失败兜底（不在 Redis 但在 DB unique index 上抢）—— 已选完全替换
- 幂等键长度/格式校验（沿用 proto 现有 `max_len=64`）

## 部署影响

服务新增 Redis 依赖。Docker 部署的 `docker-compose.yaml` 需加 Redis 服务（参考已有 postgres 服务定义）。`config.docker.yaml` 加 `redis.addr: ${MESSAGE_SERVICE_REDIS_ADDR}` 等环境变量。

## 关联

- 替换的设计：[[2026-06-26-persistence-toggle-design|persistence-toggle-design]]（其范围在本设计中收窄）
- 上一版幂等实现：DB partial unique index（已删除）
