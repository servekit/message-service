# Message Service Design Spec

## 概述

消息发送微服务。负责短信、邮件等消息的发送、记录与查询统计。
底层发送能力由 `go-common/message` 提供（SMTP、Mailgun 邮件 + 阿里云短信），本服务负责工程化封装：消息持久化、供应商配置、发送历史查询与统计。

支持三种使用方式：
- **Server**：独立 gRPC 微服务部署
- **Module**：in-process Go 模块，无网络开销
- **Client**：远程 gRPC 客户端

## 设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 供应商配置 | config.yaml 静态配置 | 简单直接，供应商相对固定 |
| 消息记录 | 完整记录（内容+状态+供应商） | 支持历史查询、审计、统计 |
| 失败处理 | 自动回退，不重试 | go-common/message 已内置供应商回退 |
| API 风格 | 按渠道分接口（SendEmail / SendSMS） | 参数明确，调用简单 |
| 查询能力 | 多维筛选 + 统计 | 支持按渠道/供应商/状态/时间筛选 + 成功率统计 |
| Service 架构 | 按渠道拆 service | 职责分离，各自持有对应 Sender |
| 记录更新 | Hook 机制 | SendResult 包含完整信息（provider/attempts/success） |

## 架构

```
                        ┌──────────────┐
   gRPC Request ──────> │ MessageService│ (薄分发层，实现 proto 接口)
                        └──────┬───────┘
                   ┌───────────┼───────────┐
                   ▼           ▼           ▼
             ┌───────────┐ ┌─────────┐ ┌───────────┐
             │EmailService│ │SMSService│ │QueryService│
             └─────┬─────┘ └────┬────┘ └─────┬─────┘
                   │            │             │
                   ▼            ▼             ▼
           go-common/message  go-common/message  repository
           (email.Sender)     (sms.Sender/Router) (查询/统计)
```

### MessageService（薄分发层）

- 实现 `pb.MessageServiceServer`
- `SendEmail` → 委托 `EmailService.Send`
- `SendSMS` → 委托 `SMSService.Send`
- `GetMessage` / `ListMessages` / `GetMessageStats` → 委托 `QueryService`
- 自身不包含业务逻辑

### EmailService

- 持有 `*email.Sender`（来自 go-common/message）
- `Send(ctx, req)`:
  1. 构建 `email.Message`
  2. 写入 `message_records`（status=pending）
  3. 调用 `email.Sender.Send(ctx, msg)`
  4. 通过 Hook 的 `AfterSend` 回调更新记录（provider/attempts/success）

### SMSService

- 持有 `*sms.Sender` 或 `*sms.Router`
- `Send(ctx, req)`:
  1. 构建 `sms.Message`
  2. 写入 `message_records`（status=pending）
  3. 调用 `sms.Sender.Send` 或 `sms.Router.Send`
  4. 通过 Hook 更新记录

### QueryService

- 持有 `*repository.MessageRepository`
- `Get(ctx, id)` → 单条记录查询
- `List(ctx, filter)` → 多维筛选分页查询（channel/status/target/provider/时间范围）
- `Stats(ctx, filter)` → 统计（总数/成功/失败/成功率/按供应商分组）

## 数据库

### message_records 表

```sql
CREATE TABLE message_records (
    id              BIGINT PRIMARY KEY,
    channel         VARCHAR(16) NOT NULL,        -- email / sms / push / ...
    provider        VARCHAR(32) NOT NULL,        -- smtp / mailgun / aliyun / ...
    status          VARCHAR(16) NOT NULL,        -- pending / sent / failed
    target          VARCHAR(255) NOT NULL,       -- 收件人（邮箱/手机号）
    subject         TEXT,                        -- 邮件标题（仅邮件）
    content         TEXT,                        -- 消息正文
    template_id     VARCHAR(64),                 -- 模板 ID
    template_params JSONB,                       -- 模板参数
    sender_id       VARCHAR(64),                 -- 发送方标识（签名、From 地址）
    error_message   TEXT,                        -- 失败原因
    attempts        INT NOT NULL DEFAULT 1,
    sent_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_message_records_channel   ON message_records (channel);
CREATE INDEX idx_message_records_status    ON message_records (status);
CREATE INDEX idx_message_records_target    ON message_records (target);
CREATE INDEX idx_message_records_provider  ON message_records (provider);
CREATE INDEX idx_message_records_created   ON message_records (created_at);
```

- 不使用外键约束，关系完整性由应用层保证
- `channel` 字段统一区分渠道，方便查询和统计
- `provider` 记录实际使用的供应商（回退后的最终供应商）
- `template_id` + `template_params` 支持模板发送（短信模板等）
- `sender_id` 记录发送方标识（短信签名、邮件 From 地址）

## Proto API

```protobuf
service MessageService {
  // 发送
  rpc SendEmail(SendEmailRequest) returns (SendResponse);
  rpc SendSMS(SendSMSRequest) returns (SendResponse);

  // 查询
  rpc GetMessage(GetMessageRequest) returns (MessageRecord);
  rpc ListMessages(ListMessagesRequest) returns (ListMessagesResponse);
  rpc GetMessageStats(GetMessageStatsRequest) returns (MessageStatsResponse);
}
```

### SendEmailRequest

| 字段 | 类型 | 说明 |
|------|------|------|
| to | string | 收件人邮箱 |
| cc | repeated string | 抄送 |
| bcc | repeated string | 密送 |
| subject | string | 邮件标题 |
| body | string | 纯文本正文 |
| html_body | string | HTML 正文（可选） |
| reply_to | string | 回复地址 |

### SendSMSRequest

| 字段 | 类型 | 说明 |
|------|------|------|
| to | string | 手机号 |
| content | string | 直接内容（与 template 二选一） |
| template_id | string | 模板 ID |
| template_params | map<string, string> | 模板参数 |

### SendResponse

| 字段 | 类型 | 说明 |
|------|------|------|
| id | int64 | message_records ID |
| status | string | sent / failed |
| provider | string | 实际使用的供应商 |

### ListMessagesRequest

| 字段 | 类型 | 说明 |
|------|------|------|
| channel | string | 筛选：渠道 |
| status | string | 筛选：状态 |
| target | string | 筛选：收件人 |
| provider | string | 筛选：供应商 |
| start_time | int64 | 时间范围起始 |
| end_time | int64 | 时间范围结束 |
| page | int32 | 页码 |
| page_size | int32 | 每页数量 |

### MessageStatsResponse

| 字段 | 类型 | 说明 |
|------|------|------|
| total | int64 | 总数 |
| sent | int64 | 成功数 |
| failed | int64 | 失败数 |
| success_rate | double | 成功率 |
| provider_stats | repeated ProviderStats | 按供应商分组统计 |

## 配置

```yaml
server:
  grpc_port: 9000
  http_port: 8080

database:
  host: localhost
  port: 5432
  user: postgres
  password: postgres
  dbname: message_service
  sslmode: disable

redis:
  addr: localhost:6379

email:
  providers:
    - type: smtp
      host: smtp.example.com
      port: 587
      username: xxx
      password: xxx
      from: noreply@example.com
    - type: mailgun
      domain: example.com
      api_key: xxx
      from: noreply@example.com

sms:
  default_country: CN
  providers:
    - type: aliyun
      access_key_id: xxx
      access_key_secret: xxx
      sign_name: xxx
      region_id: cn-hangzhou
  routes:
    - country: US
      providers:
        - type: twilio
          # ...
```

## Hook 机制

利用 go-common/message 的 `Hook` / `HookFunc` 机制更新发送记录：

1. 创建 `email.Sender` 时传入 `email.WithHook(email.HookFunc(func(ctx context.Context, result *email.SendResult) { ... }))`
2. Hook 回调中从 `result` 获取：`Success`、`Provider`、`Attempts`、`Error`
3. 通过 `context` 传递 `recordID`（在写入 pending 记录后存入 ctx），Hook 中根据 ID 更新记录

同样的模式适用于 `sms.Sender`。

## 依赖注入流程

`service.New(cfg, opts...)` 中：
1. 从配置构造 `email.Sender`（含 Hook）和 `sms.Sender`/`sms.Router`（含 Hook）
2. 初始化 `repository.MessageRepository`（持有 `*gorm.DB`）
3. 创建 `EmailService`（持有 Sender + Repository）
4. 创建 `SMSService`（持有 Sender/Router + Repository）
5. 创建 `QueryService`（持有 Repository）
6. 创建 `MessageService`（持有以上三者）
7. 支持通过 `WithDB`/`WithRedis` 注入已有连接（Module 模式）

## 目录结构

```
message-service/
├── api/proto/message/
│   └── message.proto
├── cmd/server/
│   └── main.go
├── gen/
├── internal/
│   ├── config/
│   │   └── config.go
│   ├── xcodes/
│   │   └── message.go
│   ├── store/
│   │   ├── models/
│   │   │   └── message_record.go
│   │   ├── generated/
│   │   └── repository/
│   │       └── message_repository.go
│   ├── email/
│   │   └── email_service.go
│   ├── sms/
│   │   └── sms_service.go
│   ├── query/
│   │   └── query_service.go
│   ├── service/
│   │   └── message_service.go
│   └── middleware/
│       └── interceptors.go
├── pkg/
│   ├── server.go
│   ├── module.go
│   ├── client.go
│   └── ptr/
│       └── ptr.go
├── migrations/
├── docs/
├── Makefile
├── config.yaml
├── go.mod
└── go.sum
```

## 错误码

| 错误码 | 含义 |
|--------|------|
| MESSAGE_NOT_FOUND | 消息记录不存在 |
| MESSAGE_SEND_FAILED | 消息发送失败（所有供应商都失败） |
| INVALID_PARAMETER | 参数校验失败 |
| CHANNEL_NOT_SUPPORTED | 不支持的消息渠道 |

## 测试策略

- **单元测试**：mock `email.Sender`/`sms.Sender`，测试 EmailService/SMSService 的发送流程和 Hook 更新逻辑
- **集成测试**：用 `dbx.SetupTestDB(t)` 测试 Repository 的查询和统计 SQL
- **端到端测试**：启动 Server + Client，测试完整 gRPC 调用链路

## 与 user-service 的对应关系

| user-service | message-service |
|-------------|----------------|
| `internal/service/user_service.go` | `internal/service/message_service.go` |
| `internal/identity/` | `internal/email/` + `internal/sms/` |
| `internal/session/` | 无（不需要会话） |
| `internal/rbac/` | 无（不需要权限） |
| `internal/store/` | `internal/store/`（同构） |
| `pkg/server.go` | `pkg/server.go`（同构） |
| `pkg/module.go` | `pkg/module.go`（同构） |
| `pkg/client.go` | `pkg/client.go`（同构） |
