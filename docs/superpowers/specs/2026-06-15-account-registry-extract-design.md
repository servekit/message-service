# AccountRegistry 提取设计

**日期：** 2026-06-15
**状态：** 已通过，待实施

## 背景

message-service 当前在 `internal/service/` 中维护着一套"按 vendor+account 选择 sender"的逻辑：

- `pkg/config/config.go` 定义 `EmailConfig` / `EmailVendorConfig` / `EmailAccountConfig`（SMS 同构）
- `internal/service/service.go` 的 `buildEmailAccounts` / `buildSMSAccounts` / `newEmailProvider` / `newSMSProvider` / `flattenEmail` / `flattenSMS` 负责：
  - 从 YAML config 构造 vendor → account → Provider 的嵌套 map
  - 按 vendor 名升序、account 名升序展开成扁平的 provider 列表
  - switch 分发到各 vendor 子包构造 Provider
- `internal/service/send.go` 的 `selectEmailSender` / `selectSMSSender` 在每次 RPC 调用时：
  - 如果 vendor+account 都为空，返回默认 Sender（包含所有 provider）
  - 如果 vendor+account 都设置，**新建一个只含 1 个 provider 的 Sender**（绕过 fallback）

这套逻辑总共约 150 行，本质上是 go-common/message 该提供的能力。其他服务（如 user-service、pay-service）若需要发消息，要么复制这套代码，要么只能用裸 `email.NewSender` 自己管 provider 列表。

## 目标

- 把 vendor+account → Provider 的索引、查找、构造逻辑提取到 `go-common/message/{email,sms}`
- message-service 只保留业务逻辑，删除约 150 行基础设施代码
- 其他服务能直接复用 `email.NewAccountRegistry(cfg)` 一行接入

## 非目标

- 接入 `sms.Router`（按手机号国家码自动路由）— 当前 message-service 实际场景只有国内短信，不需要。Router 在 go-common 中保留不删，留作未来钩子
- 拆分 `SendDomesticSMS` / `SendInternationalSMS` RPC — 行业实践（阿里云、腾讯云、Twilio、AWS）都是统一 API + 手机号国家码自动路由，没有人在 API 层拆分。当前用不上，避免过度设计
- 修改 `email.Sender` 或 `sms.Sender` 的 fallback 行为

## 行业研究结论

调研了主流 SMS provider 的 API 设计：

| Provider | 国内/国际区分方式 |
|---|---|
| 阿里云 | 统一 API，根据 PhoneNumber 国际区号自动区分；签名和模板分别申请 |
| 腾讯云 | 统一 SendSms API，通过签名、模板和号码国家码区分 |
| Twilio | 统一 API，根据 To 号码国家自动路由 |
| AWS SNS | 统一 API，按国家配置 configuration set |

**关键结论：** 所有主流厂商都用「统一 API + 手机号国家码自动路由」模式。go-common 现有 `sms.Router` 的设计是行业对齐的，但其复杂度（libphonenumber 解析）在当前 message-service 单国场景下不必要。

参考：
- [阿里云短信 API 概览](https://help.aliyun.com/zh/sms/developer-reference/api-dysmsapi-2017-05-25-overview)
- [腾讯云 SendSms](https://cloud.tencent.com/document/product/382/55981)
- [Twilio International vs Domestic](https://help.twilio.com/articles/25724588501147-International-versus-Domestic-Traffic)
- [AWS SNS SMS](https://docs.aws.amazon.com/sns/latest/dg/sns-mobile-phone-number-as-subscriber.html)

## 设计

### go-common/message/email/registry.go（新增）

```go
package email

// Config 是 email 账号的 YAML 配置。
type Config struct {
    Vendors map[string]VendorConfig
}

// VendorConfig 持有一个 vendor（如 "smtp"、"mailgun"）下的所有账号。
type VendorConfig struct {
    Accounts []AccountConfig
}

// AccountConfig 是单个命名账号。装载所有已支持 vendor 的字段，只有
// 对应 vendor 的子集生效。
//
// fat-struct 设计：新增 vendor 时需在此添加字段。这是低频操作，且新增
// vendor 本来就要改 go-common 加 subpackage，故可接受。
type AccountConfig struct {
    Name     string
    Host     string // SMTP
    Port     int    // SMTP submission port (587 STARTTLS, 465 implicit TLS)
    Username string // SMTP
    Password string // SMTP
    From     string // SMTP & Mailgun
    Domain   string // Mailgun
    APIKey   string // Mailgun
    Endpoint string // Mailgun API endpoint
}

// AccountRegistry 按 (vendor, account) 索引 Provider，提供默认 fallback
// sender 和 per-account sender。
type AccountRegistry struct {
    vendors map[string]map[string]Provider
    def     *Sender
}

// NewAccountRegistry 从 config 构造 registry。
//
// 行为：
//   - vendor 按名升序、每个 vendor 内 account 按名升序展开成默认 fallback 链
//   - 调用 vendor 子包（smtp.NewProvider、mailgun.NewProvider 等）构造 Provider
//   - 未知 vendor 名、重复 account 名、provider 构造失败均返回 error
//
// 排序确定性是为了默认 fallback 顺序在多次启动间稳定，便于排查和复现。
func NewAccountRegistry(cfg *Config) (*AccountRegistry, error)

// DefaultSender 返回包含所有 provider 的 fallback sender（按构造时确定的顺序）。
func (r *AccountRegistry) DefaultSender() *Sender

// SenderFor 根据 vendor+account 选择 sender。
//
// 行为：
//   - vendor 和 account 都为空 → 返回 DefaultSender（fallback 链）
//   - vendor 和 account 都设置 → 返回仅含该 provider 的 sender（无 fallback）
//   - 只设置一个 → error（vendor 和 account 必须同时设置）
//   - vendor 未知 → error
//   - account 未知 → error
//
// 设计取舍：指定 vendor+account 时不做 fallback。caller 显式选了某个账号，
// 语义上就是"用这个账号发，失败就失败"，便于排查和审计。
func (r *AccountRegistry) SenderFor(vendor, account string) (*Sender, error)
```

**实现要点：**
- `NewAccountRegistry` 内部把现有 message-service 的 `buildEmailAccounts` + `newEmailProvider` + `flattenEmail` 合并实现
- vendor 名到 provider 构造函数的 switch（smtp / mailgun / ...）放在 `registry.go` 内部，对调用方透明
- per-account 的 `SenderFor` 返回的 Sender 不需要缓存（每次 RPC 调用都构造一个新的，但只含 1 个 provider，开销很小）。如果未来 benchmark 显示有压力，再加 cache

### go-common/message/sms/registry.go（新增）

结构与 email 对称，差异：

```go
type Config struct {
    DefaultCountry string `default:"CN"` // 暂未使用，留给未来 Router 接入
    Vendors        map[string]VendorConfig
}

type AccountConfig struct {
    Name            string
    AccessKeyID     string
    AccessKeySecret string
    SignName        string
    RegionID        string
}
```

`AccountRegistry`、`NewAccountRegistry`、`DefaultSender`、`SenderFor` 接口签名与 email 完全对称。

**DefaultCountry 字段保留理由：** 现有 message-service 的 `SMSConfig.DefaultCountry` 在 YAML 中已有使用（`config.yaml` 写了 `default_country: CN`）。本次重构把 `SMSConfig` 整体迁到 `sms.Config`，`DefaultCountry` 字段一并迁移，YAML 字段名不变。Registry 内部不读取该字段，但保留它使 YAML 兼容且为未来接入 Router 留钩子。

### message-service/pkg/config/config.go

```go
import (
    "github.com/servekit/go-common/message/email"
    "github.com/servekit/go-common/message/sms"
)

type Config struct {
    Server     *ServerConfig
    Database   *dbx.Config
    Log        *logging.Config
    Email      *email.Config   // 直接用 go-common 的
    SMS        *sms.Config     // 直接用 go-common 的
    ThirdParty *ThirdPartyConfig
}
```

**删除：** `EmailConfig` / `EmailVendorConfig` / `EmailAccountConfig` / `SMSConfig` / `SMSVendorConfig` / `SMSAccountConfig` 共 6 个本地类型。

### message-service/internal/service/service.go

```go
type MessageService struct {
    pb.UnimplementedMessageServiceServer

    db            *gorm.DB
    ownDB         bool
    repo          messageRepo
    gid           thirdcall.GIDService
    emailRegistry *email.AccountRegistry  // 替换 emailSender + emailAccts
    smsRegistry   *sms.AccountRegistry    // 替换 smsSender + smsAccts
    manager       *lifecycle.Manager
}
```

`newWithDeps` 改为：

```go
emailRegistry, err := email.NewAccountRegistry(cfg.Email)
if err != nil {
    return nil, fmt.Errorf("email registry: %w", err)
}
smsRegistry, err := sms.NewAccountRegistry(cfg.SMS)
if err != nil {
    return nil, fmt.Errorf("sms registry: %w", err)
}

svc := &MessageService{
    db:            db,
    repo:          msgRepo,
    gid:           gid,
    emailRegistry: emailRegistry,
    smsRegistry:   smsRegistry,
}
```

**删除函数（约 150 行）：**
- `buildEmailAccounts`
- `buildSMSAccounts`
- `newEmailProvider`
- `newSMSProvider`
- `flattenEmail`
- `flattenSMS`

### message-service/internal/service/send.go

`sendEmail` 和 `sendSMS` 改为：

```go
func (s *MessageService) sendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
    sender, err := s.emailRegistry.SenderFor(req.GetVendor(), req.GetAccount())
    if err != nil {
        return nil, xcodes.ErrBadRequest.Wrap(err)
    }
    // ... 其余逻辑不变
}
```

**删除：** `selectEmailSender` / `selectSMSSender` 共约 40 行。

## 错误码

`SenderFor` 返回的 error 类型由 go-common 内部决定（裸 `fmt.Errorf`）。message-service 在 service 层统一包装为 `xcodes.ErrBadRequest`：

- 旧实现：`xcodes.ErrBadRequest.New(fmt.Sprintf("unknown email vendor: %s", vendor))`
- 新实现：`xcodes.ErrBadRequest.Wrap(err)`

调用方看到的错误码不变（仍是 `ErrBadRequest`），错误消息可读性略降（go-common 提供的原始 error 文本），但可接受。

## 测试

### go-common 新增测试

**`email/registry_test.go`** 和 **`sms/registry_test.go`** 各覆盖：

| 场景 | 期望 |
|---|---|
| 空 config | registry 构造成功，DefaultSender 返回的 Sender 在 Send 时返回 "no provider available" |
| 单 vendor 单 account | 默认链含 1 个 provider，SenderFor 默认和指定一致 |
| 多 vendor 多 account | 默认链顺序：vendor 名升序，account 名升序 |
| SenderFor 双空 | 返回 DefaultSender |
| SenderFor 双设置 | 返回只含该 provider 的 Sender（验证：只尝试 1 个 provider） |
| SenderFor 只设置 vendor | error |
| SenderFor 只设置 account | error |
| SenderFor 未知 vendor | error |
| SenderFor 未知 account | error |
| 重复 account 名 | NewAccountRegistry 返回 error |
| 未知 vendor 名 | NewAccountRegistry 返回 error |
| provider 构造失败（如 SMTP port=0） | NewAccountRegistry 返回 error |

测试用 fake provider（实现 `Name()` 返回 vendor 名，`Send()` 返回 nil 或 error），不依赖真实 SMTP/Mailgun。

### message-service 测试更新

**`internal/service/service_test.go`** 的 `TestSendEmail_SelectAccount*` 系列改造：

```go
// 旧
svc.emailAccts = map[string]map[string]email.Provider{
    "smtp": {"A": primary},
}
svc.emailSender = email.NewSender(all)

// 新
cfg := &email.Config{
    Vendors: map[string]email.VendorConfig{
        "smtp": {Accounts: []email.AccountConfig{{Name: "A", Host: "...", Port: 587, ...}}},
    },
}
registry, err := email.NewAccountRegistry(cfg)
svc.emailRegistry = registry
```

## 实施顺序

1. **go-common 新增 `email/registry.go` 和 `email/registry_test.go`**，先写测试再写实现（TDD）
2. **go-common 新增 `sms/registry.go` 和 `sms/registry_test.go`**，同上
3. **go-common 全量测试通过**（`go test ./...`）
4. **message-service 改 `pkg/config/config.go`**，删除本地 config 类型
5. **message-service 改 `internal/service/service.go`**，删除 build/flatten/new 函数，引入 registry
6. **message-service 改 `internal/service/send.go`**，删除 select 函数
7. **message-service 改 `internal/service/service_test.go`**，更新测试构造方式
8. **message-service 全量测试通过 + `golangci-lint run` + `go test -race`**

**不更新 CLAUDE.md：** 现有"新增供应商或消息类型时，只需扩展 go-common/message"段落在重构后依然准确（甚至更准确），无需改动。

## 兼容性

**对 message-service API 消费者：** proto 接口不变，YAML 配置结构不变。仅内部实现重构。

**对 go-common 其他消费者：** 当前仅 message-service 使用 `email.Sender` / `sms.Sender`，新增 `AccountRegistry` 不影响现有 API。

**YAML 配置：** 字段名和结构完全不变，`config.yaml` 无需修改。

## 风险

| 风险 | 缓解 |
|---|---|
| go-common 单测覆盖不足，NewAccountRegistry 的排序逻辑出错 | 用 fake provider 在测试中断言 SenderFor 返回的 Sender 内部 provider 顺序 |
| message-service 测试构造方式大改，引入隐藏 bug | 测试覆盖保留所有现有场景（SelectAccount / UnknownVendor / PartialSpec），新增 Registry 直接验证 |
| 未来真要接 Router 时，Registry 接口需要变 | DefaultCountry 字段已留，AccountRegistry 内部加 `router *Router` 字段不破坏外部 API |

## 关联

**实现计划：** 待生成（将链接到 `docs/superpowers/plans/2026-06-15-account-registry-extract-plan.md`）
