# CLAUDE.md — message-service

## 项目定位

消息发送服务。负责短信、邮件等消息的发送、记录与供应商管理。
底层发送能力由 `go-common/message` 提供（已实现 SMTP、Mailgun 邮件和阿里云短信等供应商），本服务负责工程化封装：消息持久化、供应商配置、发送记录查询。
可独立部署为 gRPC 服务（`pkg/Server`），也可作为 Go 模块 in-process 使用（`pkg/Module`），或通过 gRPC 客户端远程调用（`pkg/Client`）。

## 架构设计

### pkg/ 三种使用方式（与 user-service 一致）

- **`Server`**（`pkg/server.go`）：独立微服务部署。封装 gRPC server，注册 interceptors，监听 `:19092`。纯 gRPC：不启 gateway，对外 HTTP 面由网关（testkit）提供
- **`Module`**（`pkg/module.go`）：in-process 使用。其他 Go 服务直接 import，无网络开销，注入已有的 DB/Redis 连接
- **`Client`**（`pkg/client.go`）：远程 gRPC 客户端。embed `pb.MessageServiceClient`，所有 RPC 方法直接可用

### 与 go-common/message 的关系

- `go-common/message` 提供底层发送能力（`email.Sender`、`sms.Sender`、`sms.Router`）
- message-service 在 service 层调用这些 Sender，不直接操作供应商 API
- 新增供应商或消息类型时，只需扩展 `go-common/message`，message-service 无需改动
- message-service 新增的是：发送记录持久化、供应商配置管理、发送历史查询等工程化能力

## 技术栈约定

### Redis

- 使用 `go-common/redisx` 统一初始化：`redisx.New(cfg)` 创建客户端（含 Ping 验证）
- 客户端通过构造函数注入（`func NewXxx(client *redis.Client)`），不持有全局连接
- key 命名：`<module>:<purpose>:<identifier>`，如 `msg:ratelimit:1001`、`msg:cache:config`
- 测试用 `redisx.NewTestClient(t)` 做内存 Redis（miniredis 封装）

### 数据库 / GORM

- 使用 `go-common/dbx` 统一初始化：`dbx.New(cfg)` 创建连接（含连接池、slog 日志、GORM 配置）
- 使用 `gorm.io/cli` 做代码生成（参考 gorm-cli-development skill）
- Model 定义在 `internal/store/models/`，生成代码输出到 `internal/store/generated/`
- 数据库使用 PostgreSQL
- 迁移工具：GORM AutoMigrate，入口为 `cmd/server` 的 `migrate` 子命令（`go run ./cmd/server migrate`），支持建表、新增字段和索引
- **不使用外键约束（REFERENCES）**，关系完整性由应用层保证。只保留 UNIQUE 约束和索引
- GORM 模型不定义 `foreignKey` 关联字段（如 `User User`），只用 ID 字段（如 `UserID int64`）
- GORM 日志通过 dbx 内置 slog logger 输出，与 slog 统一，禁止 GORM 默认的 fmt 日志

### 持久化开关（per-channel）与幂等（Redis）

- **幂等（Redis）**：永远走 Redis，与持久化无关。`internal/idempotency.Checker` 接口（`RedisChecker` 实现）三段式：`Reserve`（SETNX PENDING）/ `Complete`（SET payload）/ `Release`（DEL）。失败不缓存，可重试。TTL per-channel：`idempotency.email_ttl` / `sms_ttl`（默认 5m）。Key 格式：`msg:idem:{channel}:{senderID}:{key}`。in-flight 时返回 `xcodes.ErrIdempotencyConflict`（409）。
- **持久化**：`persistence.email/sms.enabled`（默认 true）只控制 DB 写入与查询。关闭后发送路径不写 DB（仍走 vendor），查询返回 `xcodes.ErrPersistenceDisabled`（503）。
- **Redis 是硬依赖**：启动时 `redisx.New` Ping 验证，连不上服务起不来。运行期 Redis 报错 → SendEmail/SendSMS 返回 ErrInternal，绝不放过重复发送。
- **DB schema**：`message_*_records` 不再有 `idempotency_key` 列（已删）。幂等键只在 Redis 里。
- `pkg/option.WithEmailPersistence` / `WithSMSPersistence` 用于 module 模式从代码覆盖 persistence（不影响 Redis 幂等）。
- `message.Service` 持有 `idem`、`persistEmailEnabled` / `persistSMSEnabled`、`emailIdemTTL` / `smsIdemTTL` 字段，由 `service.New` 在解析 yaml + option 后注入。

### Redis

- 使用 `go-common/redisx` 统一初始化：`redisx.New(cfg)` 创建客户端（含 Ping 验证）
- 客户端通过构造函数注入到 `idempotency.NewRedisChecker`，再注入到 `message.Service`
- key 命名：`msg:idem:<channel>:<senderID>:<key>`（幂等键）
- 测试用 `redisx.NewTestClient(t)` 做 miniredis（已封装在 go-common）

### 数据库集成测试

- 使用 `dbx.SetupTestDB(t)` 启动 PostgreSQL testcontainer（已封装在 go-common）
- 每个测试用例前清理数据（truncate 或事务回滚），保证测试隔离
- Error path（连接失败、超时等）可用 `go-sqlmock` 做单元测试补充

### gRPC / Proto

- Proto 定义在 `api/proto/message/v1/message.proto`
- 生成代码来自 `github.com/servekit/api/gen/go`（`replace ../api/gen/go`）；改 proto 在 `../api` 仓库做并 `make gen`，本仓库 `go mod tidy` 后即可用
- gRPC server 监听 `:19092`；不监听 HTTP（对外 HTTP 面由网关 testkit-service 提供）
- **有限集合的字段必须使用 proto enum，不用 string**。当前已定义的枚举：
  - `MessageStatus`（UNSPECIFIED / PENDING / SENT / FAILED）
    - `SENT` 表示 vendor 同步接受请求（SMTP server OK / SMS vendor API 返回 OK），**不代表**用户最终收到。SMS 异步送达状态当前不跟踪。
    - `PENDING` 当前不写入 DB（同步流程下是瞬态），proto 保留枚举值供未来异步发送使用。
  - `EmailVendor`（UNSPECIFIED / ALIYUN / TENCENT / NETEASE）
  - `SmsVendor`（ALIYUN / TENCENT / VOLCENGINE / BYTEPLUS / HUAWEI）
  - `EmailScene` / `SmsScene`（业务用途，必填）
- **DB 层直接存 proto enum 的 int32 数字值**，GORM 原生支持，无需自定义 Scan/Value
- Model 字段用 `int32`，service 层用 `pb.MessageStatus(v)` / `int32(v)` 做双向转换（proto 原生能力，无额外函数）
- **不使用 `internal/enum` 包**（已删除）
- go-common/message 返回的 Provider 是字符串（如 `"aliyun"`）；email 的反向映射（string → enum）真相源是 `parseVendorName`，位于 `internal/provider/email/registry.go`。SMS 的反向映射仍在 `internal/service/setup.go` 的 `parseSMSVendorName`（fail-fast on unknown）。
- YAML 配置的 vendor key 是字符串（如 `aliyun`），email 在 `internal/provider/email/registry.go` 的 `parseVendorName` 转 enum；SMS 仍在 `internal/service/setup.go` 的 `parseSMSVendorName` 转 enum（fail-fast on unknown）
- **幂等键**：`SendEmailRequest.idempotency_key` / `SendSMSRequest.idempotency_key` 可选，proto `max_len=64`，DB 有 partial unique 索引 `(sender_id, idempotency_key) WHERE idempotency_key != ''`。service 层 `validateSendXxxRequest` 是 defense-in-depth（应对 module 模式不挂 protovalidate interceptor 的场景）

### 邮件附件（EmailAttachment）

- **设计原则**：附件统一作为 MIME part 附加到邮件；message-service **不修改 htmlBody**。caller 想在 body 中放附件链接/图片，自己在 htmlBody 里写 `<a href>` / `<img src>`。
- **字节来源二选一（XOR）**：
  - `url`：service 在发送时拉取（OSS pre-signed URL 等大对象场景）。SSRF 防护：只允许 http/https scheme，HTTP client 不跟随 redirect。
  - `content`：caller 直接传字节流（小附件，无需 OSS）。
  - service 层 `validateAttachments` 强制 XOR；两者都设或都不设报 `ErrInvalidAttachment`。
- **配置位置**：`email.attachment.*`（嵌套在 email 下，因为附件是 email 专属能力）。所有字段走 `default:` tag 由 configx 在 Load 时填默认值，service 代码直接读字段，**没有 `XxxOr()` 兜底方法**：
  - `fetch_timeout`（string，默认 `"30s"`，time.ParseDuration 格式）
  - `max_bytes`（默认 10MB，单个附件硬上限，同时管 url fetch 和 content 长度）
  - `max_inline_bytes`（默认 2MB，单个 `content` 字段上限）
  - `max_total_inline_bytes`（默认 5MB，单次请求所有 `content` 总和上限）
  - 超出返回 `ErrAttachmentTooLarge`，错误信息提示 caller 改用 url
- **gRPC `MaxRecvMsgSize`**：`server.max_recv_msg_size_bytes` 默认 10MB（gRPC 默认 4MB 太小，撑不起 inline content），同样走 `default:` tag。注入路径：`pkg/server.go` 通过 `grpcx.ServerConfig.ServerOptions` 传给 `grpc.NewServer`（go-common 的 grpcx 已扩展支持自定义 ServerOption）。
- **DB schema**：`message_email_record_attachments` 表只有元数据（filename / url / mime_type / size_bytes），不存字节。`url` 列允许为空（content 来源的附件没有 url）。
- **proto `kind` 字段已删除**：原来有 `LINK` / `MIME` 两态，简化后只剩 MIME 一种来源，整个 `AttachmentKind` enum 移除（proto 用 `reserved 1; reserved "kind";` 占位）。

### 错误处理

- 返回的错误统一使用 `go-common/xerr`，不用裸 `fmt.Errorf` 或 `errors.New`
- 预定义业务错误码：`xerr.New(reason, category, httpCode, message)`
- **错误码集中在 `internal/xcodes/` 中按领域分文件定义**，避免散落在各模块造成重复
- 通用错误码直接用 `xcodes` 包：`xcodes.ErrNotFound`、`xcodes.ErrInternal` 等
- 创建错误：`xcodes.ErrXxx.New()` 或 `xcodes.ErrXxx.New("override message")`
- 包装底层错误：`xcodes.ErrXxx.Wrap(err)` 或 `xcodes.ErrXxx.Wrapf(err, "context: %d", id)`
- 禁止 panic，所有错误通过返回值传递
- 使用 `errors.Is()` / `errors.As()` 比较，xerr 已实现 `Unwrap()` 和 `Is()`

### 日志

- 使用标准库 `log/slog`，禁止 `fmt.Println`、`log.Println` 等非结构化输出
- 库代码（`internal/` 中的业务逻辑）不直接打日志，通过返回 error 交给调用方
- 只有 `cmd/server/` 入口层和 middleware 可以打日志
- slog 使用结构化参数：`slog.Error("msg", "key", value)`，不用 `slog.Sprintf`

### 依赖

- `go-common` — 消息发送（`message`）、错误码（`xerr`）、Redis（`redisx`）、数据库（`dbx`）

### 通用

- ID 生成：雪花算法
- 配置：YAML，用 `github.com/spf13/viper`
- 遵循 golang-development skill 的编码规范

### 文件内函数排列

- 导出的类型、构造函数、方法放在文件上部
- 未导出的辅助函数（`lowercase`）放在文件底部，用 `// --- internal helpers ---` 分隔
- 目的：打开文件即可看到公开 API，快速了解模块能力

### 错误处理

- 禁止擅自添加 `//nolint` 注释，必须显式处理每个 error
- 即使是辅助操作（审计日志、缓存失效等），也要显式处理 error，不允许用 `_ =` 忽略
- 唯一例外：资源清理（`Close()` 等）可以用 `_ =`

## 代码质量

```bash
gofmt -w <file.go>
goimports -w <file.go>
golangci-lint run ./...
go test -race -coverprofile=coverage.out ./...
```

## 目录结构

```
message-service/
├── cmd/server/              # 启动入口：serve（默认）+ migrate 子命令（单二进制）
├── gen/                     # protoc 生成代码
├── internal/
│   ├── config/              # 配置结构体与加载
│   ├── xcodes/              # 集中错误码定义（按领域分文件）
│   ├── store/               # 数据库相关
│   │   ├── models/          # GORM Model 定义
│   │   ├── generated/       # gorm.io/cli 生成代码
│   │   └── repository/      # 数据库操作（使用 generated 代码）
│   ├── service/             # gRPC service 实现（调用 go-common/message）
│   └── middleware/          # gRPC 拦截器
├── pkg/                     # 可被外部 import（Server / Module / Client）
│   ├── server.go            # NewServer — 独立微服务部署
│   ├── module.go            # NewModule — in-process 使用
│   ├── client.go            # NewClient — 远程 gRPC 客户端
│   └── ptr/                 # 指针工具函数
├── CLAUDE.md
├── Makefile
├── config.yaml
├── go.mod
└── go.sum
```
