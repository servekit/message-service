# message-service 鲁棒性修复设计

- 日期：2026-06-22
- 范围：P0 数据丢失 + P1 健壮性 + 幂等键
- 状态：已对齐，待 plan

## 背景

从 `pkg/handler` 一路到 dal/provider 的全链路 review 发现若干严谨性问题，集中在「发送 → 持久化」的事务性、幂等性、错误返回语义、死代码四类。本设计文档定义修复范围与方案。

完整问题清单见最后一节「未解决问题」。

## 设计决策

| # | 决策点 | 选择 | 理由 |
|---|--------|------|------|
| 1 | 修复范围 | P0 + P1 + 幂等键 | 一次性把发送流程的事务性和幂等性补齐 |
| 2 | 幂等键 | proto 加 optional 字段 + DB partial unique 索引 | 解决网络重试导致重复发送 |
| 3 | 失败响应是否带 record_id | **不带**（撤回） | 调用方实际只需要失败原因，把 vendor 信息塞 error message 即可，避免 SendError 类型/interceptor/proto message 三层复杂度 |
| 4 | setupJobs 死代码 | 保留 `internal/jobs/` 包，停用 `setupJobs` 调用 + 删除注释块 | jobs 包基础结构已写好且有测试，未来按需启用 |
| 5 | SMS 异步送达状态 | 只做同步阶段 | go-common 当前 `Provider.Send()` 不返回 vendor message id，做异步状态需跨仓库改 go-common，超出本次范围 |
| 6 | PENDING 中间态 | 不做 | 当前同步流程下 PENDING 是瞬态、调用方看不到；等做异步发送时再加 |

## 架构

### 状态机（proto 不变）

```
新建请求 ──→ SENT     (vendor 同步接受)
          ──→ FAILED   (vendor 拒绝 / 网络错 / 超时)
```

- `MESSAGE_STATUS_PENDING` proto 枚举值**保留**，注释改为「预留：未来做异步送达时使用，当前不写入 DB」
- `MESSAGE_STATUS_SENT` 语义**明确化**：「vendor 同步接受，**不代表**用户最终收到」（SMS 异步送达状态本服务暂不跟踪）
- 不引入 `DELIVERED`，等异步状态方案时再加

### 发送流程（Email / SMS 对称）

```
SendEmail/SendSMS(ctx, req):
  1. 显式校验：vendor+account both-or-neither、scene、sender_id、idempotency_key 长度
  2. 幂等查重：若 req.idempotency_key != "",
       dal.GetByEmailRecordByIdempotencyKey(sender_id, key)
       命中 → 返回已有响应（SENT 直接成功返回；FAILED 返回 error）
  3. 选择 sender（含 SMS vendor+account 显式校验）
  4. ID = gid.NextID(ctx)
  5. sendResult, sendErr = sender.Send(ctx, msg)  // 用原 ctx，client cancel 传到 vendor
  6. persistCtx = context.WithTimeout(context.Background(), 3s)
     record = buildRecord(ID, req, sendResult)  // status 已是终态
     if err := dal.CreateEmailRecord(persistCtx, record); err != nil:
         slog.Error(...)  // 不影响响应
  7. 响应：
     sendErr == nil → SendResponse{Id, Status: SENT, Vendor}
     sendErr != nil → xcodes.ErrMessageSendFailed.Wrapf(sendErr,
                      "vendor=%s account=%s attempts=%d",
                      result.Vendor, result.Account, result.Attempts)
```

### 关键属性

- **步骤 6 用 `context.Background()`** — 解决请求 ctx 取消时 persist 必失败的数据丢失问题（review P0 #1）
- **步骤 5 用原 ctx** — client cancel 能传到 vendor API，避免客户端断了服务端还在打 vendor 配额
- **步骤 1 显式校验** — 不依赖 protovalidate interceptor，应对 module 模式直接调用的场景（review P1 #6）
- **步骤 2 幂等查重** — 解决网络重试导致重复发送（review P0 #5）

### 撤回的设计（不再做）

- ❌ `SendError` 自定义类型 + `SendErrorInterceptor` + errdetails.ErrorInfo
- ❌ 新增 `SendErrorDetail` proto message
- ❌ 把 record_id 暴露到 gRPC error detail

**理由**：调用方拿到失败 error 时只需要失败原因，不需要二次 RPC 查 DB 记录。把 vendor/account/attempts 信息塞进 error message 即可，调用方一行 `err.Error()` 拿到全部信息。

record_id 仍然存 DB（审计、运维按 ID 查），只是不暴露到 RPC。

## 组件改动

### DB schema

**`email_records` 和 `sms_records` 两张表都加：**

```go
// internal/store/models/email_record.go 和 sms_record.go
type EmailRecord struct {
    ...
    IdempotencyKey string `gorm:"size:64;column:idempotency_key;index"`
    ...
}
```

- `size:64`：UUID 是 36 字符，留余量给自定义 key
- 单列 `index`：诊断查询「这个 key 命中过哪些记录」
- 默认 ''（空字符串），向后兼容老调用方

**Partial unique index**（GORM AutoMigrate 不支持，用 raw SQL）：

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_email_records_sender_idempotency
  ON email_records (sender_id, idempotency_key)
  WHERE idempotency_key != '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_sms_records_sender_idempotency
  ON sms_records (sender_id, idempotency_key)
  WHERE idempotency_key != '';
```

放在 `cmd/migrate/main.go` AutoMigrate 之后执行，幂等。

**`error_message` 加长度限制：**

```go
ErrorMessage string `gorm:"size:1024;column:error_message"`  // 原: 无 size
```

service 层 persist 时 truncate：

```go
// internal/service/message/util.go (新建)
const maxErrorMessageLen = 1024

func truncateErrorMessage(s string) string {
    if len(s) <= maxErrorMessageLen {
        return s
    }
    return s[:maxErrorMessageLen]
}
```

**迁移**：项目当前无生产数据，AutoMigrate 直接重建表即可，**不需要迁移脚本**。

### Proto 协议

**`SendEmailRequest` / `SendSMSRequest` 加幂等键字段：**

```proto
message SendEmailRequest {
  // ... 现有字段 ...
  
  // IdempotencyKey is optional. When set, a second request with the same
  // (sender_id, idempotency_key) returns the existing record without
  // re-sending. Use a UUID per logical send intent. Max length 64.
  string idempotency_key = 14;
}

message SendSMSRequest {
  // ... 现有字段 ...
  
  // IdempotencyKey is optional. See SendEmailRequest.idempotency_key.
  string idempotency_key = 9;
}
```

字段编号：Email=14（现有最大 13）、SMS=9（现有最大 8），不冲突。

**`MessageStatus` 注释更新：**

```proto
enum MessageStatus {
  // UNSPECIFIED — never persisted.
  MESSAGE_STATUS_UNSPECIFIED = 0;
  
  // PENDING — send in progress. Reserved for future async-send flow;
  // the sync flow does NOT write PENDING (currently unused).
  MESSAGE_STATUS_PENDING = 1;
  
  // SENT — the vendor synchronously accepted the send request.
  // For SMS: vendor API returned OK; does NOT mean handset received
  //          (async delivery not tracked yet).
  // For email: SMTP server accepted the message.
  MESSAGE_STATUS_SENT = 2;
  
  // FAILED — the vendor rejected the request, network/transport failed,
  // or context was cancelled. error_message carries the last error.
  MESSAGE_STATUS_FAILED = 3;
}
```

不改枚举数值（已部署的客户端兼容），只改注释。

### DAL 新增查询

```go
// internal/store/dal/email_record.go
// GetEmailRecordByIdempotencyKey returns the record for a given
// (sender_id, idempotency_key), or (nil, nil) if not found.
// Caller must ensure idempotencyKey != "" before calling.
func GetEmailRecordByIdempotencyKey(
    ctx context.Context, tx *gorm.DB, senderID, idempotencyKey string,
) (*models.EmailRecord, error) {
    record, err := gorm.G[models.EmailRecord](tx).
        Where(generated.EmailRecord.SenderID.Eq(senderID)).
        Where(generated.EmailRecord.IdempotencyKey.Eq(idempotencyKey)).
        Take(ctx)
    if err != nil {
        if errors.Is(err, gorm.ErrRecordNotFound) {
            return nil, nil
        }
        return nil, xcodes.ErrInternal.Wrap(err)
    }
    return &record, nil
}

// sms_record.go 同款 GetSMSRecordByIdempotencyKey
```

### Service 层流程改造

**`internal/service/message/email.go`：**

```go
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
    // 1. 显式校验（不依赖 protovalidate）
    if err := validateSendEmailRequest(req); err != nil {
        return nil, xcodes.ErrBadRequest.Wrap(err)
    }

    // 2. 幂等查重
    if key := req.GetIdempotencyKey(); key != "" {
        existing, err := dal.GetEmailRecordByIdempotencyKey(ctx, s.db, req.GetSenderId(), key)
        if err != nil {
            return nil, xcodes.ErrInternal.Wrap(err)
        }
        if existing != nil {
            return s.respondIdempotentEmail(existing)
        }
    }

    // 3. 选择 sender
    sender, err := s.emailRegistry.SenderFor(req.GetVendor(), req.GetAccount())
    if err != nil {
        return nil, xcodes.ErrBadRequest.Wrap(err)
    }

    // 4. 分配 ID
    id, err := s.gid.NextID(ctx)
    if err != nil {
        return nil, xcodes.ErrInternal.Wrap(err)
    }

    // 5. 调用 vendor（用原 ctx）
    msg := buildEmailMessage(req)  // build from req
    result, sendErr := sender.Send(ctx, msg)

    // 6. persist（用独立 ctx）— 仅在拿到 result 时落库
    //    result == nil 表示 vendor 调用前置失败（empty recipient / no provider），
    //    没有信息可 persist
    if result != nil {
        persistCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()

        record := buildEmailRecord(id, req, result)
        if err := dal.CreateEmailRecord(persistCtx, s.db, record); err != nil {
            slog.Error("persist email record", "id", id, "error", err)
        }
    }

    // 7. 响应
    if sendErr != nil {
        return nil, xcodes.ErrMessageSendFailed.Wrapf(sendErr,
            "vendor=%s account=%s attempts=%d",
            result.Vendor, result.Account, result.Attempts)
    }
    return &pb.SendResponse{
        Id:     id,
        Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
        Vendor: &pb.SendResponse_EmailVendor{
            EmailVendor: emailVendorFromString(result.Vendor),
        },
    }, nil
}

// validateSendEmailRequest 显式校验，不依赖 protovalidate interceptor
func validateSendEmailRequest(req *pb.SendEmailRequest) error {
    vendorSet := req.GetVendor() != pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
    accountSet := req.GetAccount() != ""
    if vendorSet != accountSet {
        return fmt.Errorf("vendor and account must be set together")
    }
    if req.GetScene() == pb.EmailScene_EMAIL_SCENE_UNSPECIFIED {
        return fmt.Errorf("scene is required")
    }
    if req.GetSenderId() == "" {
        return fmt.Errorf("sender_id is required")
    }
    if len(req.GetIdempotencyKey()) > 64 {
        return fmt.Errorf("idempotency_key too long (max 64)")
    }
    return nil
}

// respondIdempotentEmail 根据 existing 记录构造响应
func (s *Service) respondIdempotentEmail(existing *models.EmailRecord) (*pb.SendResponse, error) {
    switch pb.MessageStatus(existing.Status) {
    case pb.MessageStatus_MESSAGE_STATUS_SENT:
        return &pb.SendResponse{
            Id:     existing.ID,
            Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
            Vendor: &pb.SendResponse_EmailVendor{
                EmailVendor: pb.EmailVendor(existing.Vendor),
            },
        }, nil
    case pb.MessageStatus_MESSAGE_STATUS_FAILED:
        return nil, xcodes.ErrMessageSendFailed.Newf(
            "previous attempt with same idempotency_key failed: %s", existing.ErrorMessage)
    default:
        return nil, xcodes.ErrInternal.Newf(
            "idempotent record in unexpected status %d", existing.Status)
    }
}
```

**`internal/service/message/sms.go`：** 结构对称，差异点：
- 校验函数 `validateSendSMSRequest`
- 幂等查重调 `dal.GetSMSRecordByIdempotencyKey`
- sender 选择保留 router 分支（vendor == UNSPECIFIED 时走 router）

### 其他 P1 修复（打包一起做）

**vendor unknown 告警：**

```go
// internal/service/message/email.go (emailVendorFromString)
func emailVendorFromString(s string) pb.EmailVendor {
    switch s {
    case "custom_smtp":  return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP
    case "aliyun":       return pb.EmailVendor_EMAIL_VENDOR_ALIYUN
    case "tencent":      return pb.EmailVendor_EMAIL_VENDOR_TENCENT
    case "netease":      return pb.EmailVendor_EMAIL_VENDOR_NETEASE
    default:
        slog.Warn("unknown email vendor name from go-common provider",
            "vendor", s,
            "hint", "add case to emailVendorFromString or upgrade message-service")
        return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
    }
}
// smsVendorFromString 同样模式
```

**`CountEmailStats` / `CountSMSStats` 合并 SQL（3 次查询 → 1 次）：**

现有实现分别跑 total/sent/failed 三次 COUNT。改为一次 raw SQL 用 `COUNT(*) FILTER (WHERE ...)`，跟 `ListEmailVendorStats` / `VendorStats` 风格一致。具体实现复用现有 `EmailRecordStatsQuery[T]` typed raw SQL 模式，新增一个不带 GROUP BY 的 `TotalStats` 方法签名（同样在 `internal/store/models/email_record.go` 的接口注释里定义 SQL 模板，由 gorm gen 生成）。

**`SuccessRate` total=0 时返回 -1：**

```go
var successRate float64
if total > 0 {
    successRate = float64(sent) / float64(total) * 100
} else {
    successRate = -1  // 明确「无数据」语义
}
```

**`gorm.G[T]().Where(ID.Gt(0))` 加注释说明意图：**

```go
// ID.Gt(0) 是 gorm gen typed chain 的锚点（snowflake ID 恒正，本身无过滤效果）。
// 直接 .Where(filter) 也能跑，但保留这个锚点方便后续扩展基础 WHERE 条件。
q := applyEmailListFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter)
```

### setupJobs 死代码清理

**保留**：
- `internal/jobs/jobs.go` 整个包（已写好且有测试）
- `pkg/config/config.go` 的 `Cron *cronx.Config` 字段（未来按需启用）

**删除**：
- `internal/service/service.go:114-119` 的注释块（注释掉的 `svc.setupJobs()` 调用）
- `internal/service/service.go:130-146` 的 `setupJobs` 方法本身
- 相关 import 如果不再使用

## 数据流

### 幂等查重命中流程

```
client → SendEmail(req={...,idempotency_key="abc-123"}) 
       → service.SendEmail
       → dal.GetEmailRecordByIdempotencyKey(sender_id, "abc-123")
       → 命中 SENT 记录
       → 返回 SendResponse{Id=existing.ID, Status=SENT}
       → 不调 gid、不调 senderRegistry、不调 vendor
```

### 正常发送流程

```
client → SendEmail(req={...,idempotency_key="abc-123"})
       → service.SendEmail
       → validateSendEmailRequest
       → dal.GetEmailRecordByIdempotencyKey → (nil, nil)
       → emailRegistry.SenderFor
       → gid.NextID
       → sender.Send(ctx, msg) → vendor API → result{Success=true}
       → persistCtx := background+3s
       → dal.CreateEmailRecord(persistCtx, record{status=SENT, idempotency_key="abc-123"})
       → 返回 SendResponse{Id, Status=SENT}
```

### 发送失败流程

```
client → SendEmail(req)
       → validateSendEmailRequest
       → 幂等查重 → 不命中
       → emailRegistry.SenderFor
       → gid.NextID
       → sender.Send(ctx, msg) → vendor API 失败 → result{Success=false}, err
       → persistCtx := background+3s
       → dal.CreateEmailRecord(persistCtx, record{status=FAILED, error_message=...})
       → 返回 xcodes.ErrMessageSendFailed.Wrapf(err, "vendor=... account=... attempts=...")
       → client 拿到 err，err.Error() 含完整失败原因
```

### 请求取消流程（review P0 #1 核心场景）

```
client → SendEmail(req) → 5s 后客户端断开（ctx cancelled）
       → validateSendEmailRequest
       → 幂等查重 → 不命中
       → emailRegistry.SenderFor
       → gid.NextID
       → sender.Send(ctx, msg) → vendor API 正常返回成功，但 sender.Send 内部
                                检查 ctx.Err() != nil → 返回 result{Success=false, Error=ctx.Err()}
       → persistCtx := background+3s（独立 ctx，不受 client cancel 影响）
       → dal.CreateEmailRecord(persistCtx, record{status=FAILED}) → 成功
       → 返回 error（client 已经断开收不到）
       → DB 里有 FAILED 记录，运维/审计可见 ✓
```

## 错误处理

### 错误返回语义

| 场景 | 返回 | 调用方看到 |
|------|------|----------|
| 校验失败（vendor+account 错配、scene 缺失等） | `ErrBadRequest.Wrap(err)` | grpc InvalidArgument |
| 幂等命中 SENT 记录 | `SendResponse{Status=SENT}` | 成功响应 |
| 幂等命中 FAILED 记录 | `ErrMessageSendFailed.Newf("previous attempt ... failed: %s", errMsg)` | grpc Internal，含上次失败原因 |
| SenderFor 失败（unknown vendor/account） | `ErrBadRequest.Wrap(err)` | grpc InvalidArgument |
| gid 失败 | `ErrInternal.Wrap(err)` | grpc Internal |
| vendor 同步失败 | `ErrMessageSendFailed.Wrapf(err, "vendor=... account=... attempts=...")` | grpc Internal，message 含完整失败信息 |
| persist 失败（DB 抖动） | slog.Error，**不影响响应** | 响应正常返回（但 record 未落库，by-id 查询拿 NOT_FOUND） |

### persist 失败的容忍策略

**已知风险**（接受）：发送成功但 persist 失败时，vendor 实际发出但 DB 无记录。

**理由**：
- 概率小（DB 抖动）
- 原代码就有这个问题，本次不引入新风险
- 等做异步状态时加 PENDING 中间态 + cron 补偿自然解决
- 如果改成「persist 失败就返回错误」，会让「vendor 已成功」变成「client 看到失败」，可能导致调用方误重试

## 测试策略

### 测试金字塔

```
单元测试（mock provider + 真实 DB testcontainer）
├─ service/message/{email,sms}_test.go        — 发送流程、幂等、显式校验
├─ store/dal/{email,sms}_record_test.go        — 新增 GetXxxRecordByIdempotencyKey
└─ store/dal/{email,sms}_record_test.go        — stats SQL 合并后行为不变

集成测试（端到端 RPC 调用）
└─ pkg/server_test.go (可选)                   — 现有路径回归
```

### 必须覆盖的新场景

**A. 幂等性**（service 层）
- 同一个 idempotency_key 两次请求：
  - 第 1 次：mock provider 成功 → SENT 记录入库
  - 第 2 次：mock provider 不应被调用（用 call count 验证）→ 返回第 1 次的记录
- 不传 idempotency_key：两次请求都应调用 provider，生成两条独立记录
- 不同 idempotency_key：两次请求都应调用 provider
- 同 idempotency_key 命中 FAILED 记录：返回 error，`errors.Is(err, ErrMessageSendFailed)` 为 true，error message 含原失败原因
- 同 idempotency_key 不同 sender_id：应视为不同请求（验证 unique 索引是 `(sender_id, idempotency_key)` 而非只有 `idempotency_key`）

**B. 显式校验**（service 层，绕过 protovalidate）
- 缺 scene → ErrBadRequest，不调 gid、不调 provider
- vendor != 0 && account == "" → ErrBadRequest
- vendor == 0 && account != "" → ErrBadRequest
- sender_id 为空 → ErrBadRequest
- idempotency_key 长度 > 64 → ErrBadRequest

**C. 独立 ctx 持久化**（service 层，关键场景）
- 构造已取消的 ctx，调 svc.SendEmail
- mock provider 返回 result{Success=false, Error=ctx.Err()}（与现有 sender.go 行为一致）
- 验证：dal.GetEmailRecord 拿到 FAILED 记录（证明 persist 用了独立 ctx 成功落库）

**D. vendor unknown 告警**（service 层）
- mock provider 返回 Name() = "unknown_vendor"
- 用 slog handler 捕获日志，断言含 hint 信息
- 验证：返回的 SendResponse.Vendor 为 UNSPECIFIED

**E. ErrorMessage truncate**（service 层）
- mock provider 返回 error with 2000 字符 message
- 验证：persisted record.ErrorMessage 长度 == 1024

**F. 失败响应信息完整性**（service 层）
- mock provider 返回 error
- 验证：返回的 err.Error() 含 vendor、account、attempts 信息
- 验证：`errors.Is(err, xcodes.ErrMessageSendFailed)` 为 true

**G. DAL 新查询**（dal 层）
- GetEmailRecordByIdempotencyKey：插入带 key 的记录，查询命中
- GetEmailRecordByIdempotencyKey：查询不存在的 key，返回 (nil, nil)
- GetEmailRecordByIdempotencyKey：sender_id + key 组合，验证不串

**H. stats SQL 合并**（dal 层）
- 现有 TestCountEmailStats / TestCountSMSStats 跑通（行为不变）
- 可选：用 EXPLAIN 验证从 3 次查询变 1 次

**I. SuccessRate 无数据语义**（dal 层）
- 空表查询 stats，验证 SuccessRate == -1

### 测试基础设施

- 沿用 `dbx.SetupTestDB(t)` 启 PostgreSQL testcontainer
- 现有 `mockEmailProvider` / `mockSMSProvider` 加 `calls int` 字段计数
- 沿用 `getTestGID(t)`

### 不测的事

- vendor 真实发送（aliyun / smtp）：mock 掉
- grpc-gateway HTTP 路径：go-common 测过
- xerr 行为：go-common 测过
- protovalidate 行为：buf 测过，只验证 service 层兜底校验

## 回滚策略

项目当前无生产数据，**不需要灰度/回滚策略**。如果改动引入问题：
1. revert 代码 commit
2. `cmd/migrate/` 重建表（AutoMigrate 会自动 drop 旧列、加新列）

## 实施顺序（粗略，详细 plan 由 writing-plans skill 生成）

1. proto 加字段 + 注释更新 + 重新生成 pb 代码
2. model 加 IdempotencyKey 列、ErrorMessage size
3. cmd/migrate 加 partial unique index SQL
4. dal 加 GetXxxRecordByIdempotencyKey + stats SQL 合并
5. service/message/{email,sms}.go 流程改造 + util.go 新建
6. setupJobs 死代码清理
7. 测试补齐
8. gofmt + lint + 全套测试通过
9. CLAUDE.md 更新（如果有约定变化）
10. 同步到 Obsidian

## 未解决问题（接受现状，本次不修）

- **异步送达状态**：SMS vendor 接受 ≠ 用户手机收到。本次只做同步阶段，SENT 语义明确化。
- **进程崩溃恢复**：send 后、persist 前崩溃会丢记录。本次接受，未来加 PENDING 中间态解决。
- **发送成功但 persist 失败**：vendor 已发，DB 无记录。概率小，原代码就有，本次不引入新风险。
- **真异步发送 / 队列**：本次范围外，需要单独立项。
- **List 接口 cursor 分页**：当前 offset 分页对大表慢，cursor 分页更稳。YAGNI，本次不做。
- **vendor_message_id 列预留**：等做异步状态时再加，本次不加。

## 关联

**实现计划：** 待 writing-plans skill 生成，路径 [[services/message-service/plan/v1/robustness-fix|robustness-fix]]
