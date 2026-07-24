# go-common/message 内联设计

**日期**:2026-06-25
**状态**:设计已确认,待写实施计划
**作者**:moss

## 背景

`go-common/message` 经过 2026-06-16 的"瘦身"重构后,只保留了三件东西:

- `Message` struct(`email.Message` / `sms.Message`)
- `Provider` interface(`Name() / Send(ctx, *Message) error`)
- 厂商实现子包(`email/smtp/`、`sms/aliyun/`)

合计 4 个文件 ~250 行代码 + 2 个测试。

实际使用情况:

- `go-common/message` 的外部使用者**只有 message-service**(grep 验证:`/Users/moss/code/base` 下除 message-service 外无 import)。
- message-service 内部 `internal/provider/{email,sms}/` 仍以 wrapper 模式持有 go-common 的 `Provider`,通过 `AccountProvider` struct 附加 vendor+account 身份。
- 加新 vendor 时需在 go-common 改代码 + 跨仓库协调,迭代成本高,收益(被复用)为零。

**判断**:剩余的 Provider interface + Message + vendor impl 应该一并搬入 message-service,并且去掉 wrapper 中间层(vendor impl 直接实现 `AccountProvider`)。这是 2026-06-16 瘦身方向的最后一步。

## 目标

1. **删除 go-common/message 整个目录** —— 零外部使用者,保留只会造成"两份代码"的混乱。
2. **合并 `Provider` 与 `AccountProvider`** —— vendor impl 直接实现 `AccountProvider` interface,消除 wrapper 中间层。
3. **`Vendor` 字段升级为 proto enum** —— 取代字符串,消除 service 层的 `pb.EmailVendor_value[result.Vendor]` 反向转换(4 处)。
4. **行为零变化** —— 不动 proto、DB schema、API 行为、YAML 配置形态。

## 非目标

- 不重命名 `internal/provider/` 为 `internal/message/`(命名沿用 CLAUDE.md 已定义的 vendor protocol layer 约定)。
- 不动 `internal/service/message/` 的业务逻辑(只改类型引用和反向转换消除)。
- 不引入新 vendor —— 虽然扩展门槛降低了,但新增 vendor 是独立工作。
- 不重构 SMTP / Aliyun vendor impl 内部发送逻辑 —— 只加方法,不动核心代码。
- 不改 proto / DB schema / YAML 配置格式。

## 目录结构(迁移后)

**扁平化设计**:vendor impl 文件直接放在 `email/` 和 `sms/` 包内,不再用子目录。原因:Go 不允许循环 import —— 子包 smtp/aliyun 实现 AccountProvider interface 时要 import 父包(用 Message 类型),父包 registry 又要 import 子包(调用 NewSMTPProvider/NewAliyunProvider),会形成环。把 vendor impl 放进同一 package,问题消失。

```
internal/provider/
├── email/
│   ├── message.go              [新] Message struct,从 go-common 平移
│   ├── sender.go               [改] AccountProvider struct→interface,合并原 Provider
│   ├── registry.go             [改] buildProvider 返回 AccountProvider,不再 wrap
│   ├── smtp.go                 [新] SMTP vendor impl,平移自 go-common/email/smtp
│   ├── smtp_test.go            [新] fakeSMTPServer 测试,平移自 go-common
│   ├── sender_test.go          [改] mock 改为实现 interface
│   └── registry_test.go        [改] 同上
└── sms/
    ├── message.go              [新]
    ├── sender.go               [改]
    ├── registry.go             [改]
    ├── router.go               [改] 字段访问 → 方法调用
    ├── router_builder.go       [改] 类型引用变化
    ├── aliyun.go               [新] Aliyun vendor impl,平移自 go-common/sms/aliyun
    ├── aliyun_test.go          [新] mock SDK client 测试,平移自 go-common
    ├── sender_test.go          [改]
    ├── registry_test.go        [改]
    ├── router_test.go          [改]
    └── router_builder_test.go  [改]
```

**命名约定**:vendor impl 文件内,类型名加 vendor 前缀以避免与父包已有类型(Sender、AccountRegistry、Config、AccountConfig)冲突:
- `SMTPProvider` / `SMTPConfig` / `NewSMTPProvider`(email 包内)
- `AliyunProvider` / `AliyunConfig` / `NewAliyunProvider`(sms 包内)

`internal/service/message/email.go`、`sms.go` 改动:`emailcommon.Message` → 本地 `email.Message`;删除 `pb.EmailVendor_value[result.Vendor]` 反向转换;日志改用 `.String()`。

`go-common/message/` 整个目录**删除**。

## AccountProvider interface 形态

### Before(struct wrapper)

```go
// go-common/message/email/sender.go
type Provider interface {
    Name() string
    Send(ctx context.Context, msg *Message) error
}

// message-service/internal/provider/email/sender.go
type AccountProvider struct {
    Vendor   string
    Account  string
    Provider emailcommon.Provider // wraps go-common Provider
}

type SendResult struct {
    Vendor  string  // 字符串
    ...
}
```

### After(interface)

```go
// internal/provider/email/sender.go
type AccountProvider interface {
    Vendor() pb.EmailVendor       // 强类型,取代 Name() string
    Account() string
    Send(ctx context.Context, msg *Message) error
}

type SendResult struct {
    Vendor  pb.EmailVendor  // enum
    ...
}
```

```go
// internal/provider/email/smtp.go (package email,与 sender.go/registry.go 同包)
type SMTPConfig struct { ... }

type SMTPProvider struct {
	account string
	client  *mail.Client
	from    string
}

func NewSMTPProvider(account string, cfg *SMTPConfig) (*SMTPProvider, error) { ... }

func (p *SMTPProvider) Vendor() pb.EmailVendor {
	return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP
}
func (p *SMTPProvider) Account() string { return p.account }
func (p *SMTPProvider) Send(ctx context.Context, msg *Message) error { ... }

var _ AccountProvider = (*SMTPProvider)(nil)
```

vendor impl 与 sender/registry 在同一 package,无 import cycle。registry.buildProvider 直接调用 `NewSMTPProvider`(同包函数)。Aliyun 侧对称:`AliyunConfig` / `AliyunProvider` / `NewAliyunProvider`。

### 关键变化点(对比表)

| 点 | Before | After |
|---|---|---|
| `AccountProvider` | struct,持有 Provider | interface,vendor impl 直接实现 |
| `Sender.providers` | `[]*AccountProvider` | `[]AccountProvider` |
| `Registry.vendors` | `map[enum]map[string]*AccountProvider` | `map[enum]map[string]AccountProvider` |
| Vendor 标识 | `Name() string` ("smtp"/"aliyun") | `Vendor() pb.EmailVendor` |
| `SendResult.Vendor` | `string` | `pb.EmailVendor` |
| `buildProvider` 签名 | 返回 `Provider`,外部 wrap | 返回 `AccountProvider`,不 wrap |
| registry 构造 | `&AccountProvider{Vendor:..., Account:..., Provider: p}` | 直接传 vendor impl(内部已含 Vendor/Account) |
| vendor 构造函数 | `smtp.NewProvider(cfg)` | `NewSMTPProvider(account, cfg)`(同包,email 侧)/ `NewAliyunProvider(account, cfg)`(sms 侧) |

### 连带影响(service 层)

| 文件 / 行 | 改动 |
|---|---|
| `service/message/email.go:86` | `pb.EmailVendor(pb.EmailVendor_value[result.Vendor])` → `result.Vendor` |
| `service/message/email.go:291` | `pb.EmailVendor_value[result.Vendor]` → `int32(result.Vendor)` |
| `service/message/email.go:79` | 日志 `result.Vendor` → `result.Vendor.String()` |
| `service/message/sms.go:89, 290` | 同 email 侧 |
| `service/message/sms.go:82` | 日志改 `.String()` |
| `service/message/email.go:18, sms.go:18` | `emailcommon.Message` → `provider/email.Message`(本地包) |

### 保留不变

- `SendResult` 其他字段(Message, Account, Target, Success, Error, Duration, Attempts)
- `Sender.Send()` fallback 逻辑
- `Registry.SenderFor()` 选择逻辑
- SMTP / Aliyun vendor impl 核心发送逻辑

## 数据流(以 email 为例)

```
YAML config (vendor 字符串 "custom_smtp")
    ↓ config.Load 转换为 enum(fail-fast on unknown)
provider/email/Config{Vendors: map[pb.EmailVendor]*VendorConfig}
    ↓ NewAccountRegistry
registry.go::buildProvider(vendor, account, ac)
    ↓ NewSMTPProvider(account, &SMTPConfig{...})  → *SMTPProvider  (同包函数,email 侧)
provider/email/registry 持有: map[pb.EmailVendor]map[string]AccountProvider
    ↓ NewSender(providers)
Sender.Send(ctx, *Message)
    ↓ 遍历 providers,调用 p.Send() —— p 自带 Vendor() 和 Account()
SendResult{Vendor: pb.EmailVendor, Account: "primary", Success: true}
    ↓
service/message/email.go → 持久化(record.Vendor = result.Vendor,无反向转换)
```

vendor impl 在构造时就知道自己的 Vendor/Account,不再依赖 registry 注入。

## 错误处理

| 场景 | 处理 |
|---|---|
| vendor impl 构造失败 | `buildProvider` 返回 error,registry 包装为 `email: account %s/%s: %w` |
| Send 校验失败(空收件人) | vendor impl 返回 `fmt.Errorf("smtp: recipient is empty")` |
| Send 调用失败 | Sender 尝试下一个 provider,全失败返回 `email: all providers failed, last error: %w` |
| vendor impl 内部 error | 保留 `fmt.Errorf` 风格(vendor 层是协议层,不用 xerr —— 与 CLAUDE.md 一致) |

## 测试策略

### 测试文件归属

| 测试文件 | 来源 | 状态 |
|---|---|---|
| `provider/email/smtp/smtp_test.go` | go-common 平移 | **原样保留** |
| `provider/sms/aliyun/aliyun_test.go` | go-common 平移 | **原样保留** |
| `provider/email/sender_test.go` | message-service 已有 | **改动**:mock 实现接口 |
| `provider/email/registry_test.go` | message-service 已有 | **改动** |
| `provider/sms/*_test.go` | message-service 已有 | **改动** |

### mock provider 形态

```go
// Before — 构造 wrapper
&email.AccountProvider{
    Vendor:   "smtp",
    Account:  "primary",
    Provider: &mockProvider{...},
}

// After — 直接实现 interface
type fakeProvider struct {
    vendor  pb.EmailVendor
    account string
    err     error
}
func (f *fakeProvider) Vendor() pb.EmailVendor { return f.vendor }
func (f *fakeProvider) Account() string        { return f.account }
func (f *fakeProvider) Send(ctx context.Context, _ *email.Message) error { return f.err }
```

每个测试包各自维护 fakeProvider,因为返回的 Vendor enum 类型不同(`pb.EmailVendor` vs `pb.SmsVendor`)。

### 验证清单

```bash
gofmt -l .
goimports -l .
golangci-lint run ./...
go build ./...
go test -race -coverprofile=coverage.out ./...
```

重点关注:
- `provider/email/sender_test.go` —— Sender fallback 逻辑
- `provider/email/registry_test.go` —— vendor+account 解析
- `provider/sms/router_test.go` —— 按国家路由(最复杂)
- `service/message/email_test.go` + `sms_test.go` —— 业务层端到端

## 兼容性与影响面

### import 影响范围

非测试文件 import go-common/message 的有 7 个(全部在 `internal/provider/` 和 `internal/service/message/`):

```
internal/provider/email/sender.go
internal/provider/email/registry.go
internal/provider/sms/sender.go
internal/provider/sms/registry.go
internal/provider/sms/router.go
internal/service/message/email.go
internal/service/message/sms.go
```

`pkg/`、`cmd/`、`internal/config/`、`internal/middleware/` **不直接 import** go-common/message,Module/Client/Server 三种使用方式零影响。

### go.mod 影响

- message-service 的 `go.mod`:**不变**(仍依赖 go-common 整体,只是不再用 message 子包)。
- aliyun SMS SDK 和 go-mail SDK 的依赖**不删** —— import 路径从 `go-common/message/sms/aliyun` 改到 `message-service/internal/provider/sms/aliyun`,SDK 本身还在用。
- go-common 的 `go.mod`:**可选清理** —— 移除只为 message 引入的依赖(`alibabacloud-go/dysmsapi`、`wneessen/go-mail`)。本次 spec 不强制要求,作为后续 cleanup。

### DB / proto / YAML

- proto:**不变**。
- DB schema:**不变**。
- YAML 配置:**格式不变**。

## 实施顺序(高层)

按依赖顺序自底向上:

1. 创建 `message.go`:从 `go-common/message/{email,sms}/sender.go` 拆出 `Message` struct 平移。
2. 创建 vendor impl 文件 `smtp.go` / `aliyun.go`(同包,加 `account` 字段、`Vendor()`、`Account()` 方法,vendor 前缀命名)。
3. 改造 `sender.go`:`AccountProvider` struct→interface,`SendResult.Vendor` 改 enum。
4. 改造 `registry.go`:`buildProvider` 返回 interface,移除 wrapper,直接调用 `NewSMTPProvider` / `NewAliyunProvider`。
5. 改造 `router.go`(SMS):字段访问改方法调用。
6. 改造 `service/message/{email,sms}.go`:删除 `pb.XxxVendor_value[result.Vendor]` 反向转换,日志用 `.String()`,import 改本地包。
7. 改造测试:`&AccountProvider{...}` → `&fakeProvider{...}`。
8. 删除 `go-common/message/` 整个目录。
9. 验证:gofmt / goimports / lint / build / test 全过。

详细步骤由后续实施计划给出。

## 风险

- **跨仓库改动**:涉及 `go-common` 和 `message-service` 两个仓库。但 go-common 是本地 replace 依赖,改完 message-service 后,go-common 的删除是独立操作,不影响 message-service 构建。
- **`Vendor` 类型变更的连锁**:`SendResult.Vendor` 从 string→enum 影响所有读这个字段的代码。grep 已穷举:5 处(service 层 4 处反向转换 + 日志 1 处),全部已纳入 spec。
- **vendor impl 测试平移**:fakeSMTPServer 测试绑定 `127.0.0.1:0` 临时端口,平移到 message-service 后行为应一致;Aliyun mock client 测试纯单元,无环境依赖。
- **golangci-lint**:`Name() string` 方法原本带 `//nolint:revive`,删除后无需 nolint,反而更干净。
- **扁平化后的 package 文件数**:email 包 7 个 .go 文件、sms 包 10 个。仍在可读范围内,加新 vendor 时只需加一个 `<vendor>.go` 文件。

## 关联

**实现计划**:待写(writing-plans 阶段)

**历史**:
- 2026-06-16 [`go-common-message-slimdown-design`](./2026-06-16-go-common-message-slimdown-design.md):把 Sender/SendResult/Registry/Router 从 go-common 搬到 message-service,删除 Hook。保留了 Provider interface + Message + vendor impl 在 go-common。
- 本次(2026-06-25):完成最后一步,把残留的 Provider interface + Message + vendor impl 也搬过来,并合并 `Provider`/`AccountProvider`,Vendor 字段升级为 enum。
