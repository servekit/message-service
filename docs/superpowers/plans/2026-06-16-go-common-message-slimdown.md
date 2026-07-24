# go-common/message 瘦身实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 把 go-common/message 从"完整发送栈"瘦身为"纯 Provider 接口 + 厂商实现"，Sender/SendResult/AccountRegistry/Router 下沉到 message-service，并接入 Router 到 sendSMS。

**Architecture:** 四阶段：① 搬家（代码迁到 `internal/message/`，行为不变）② 改造（引入 `AccountProvider` 包装 + `SendResult` 扩展 + DB schema）③ 接入 Router 到 sendSMS ④ 清理 go-common。

**Tech Stack:** Go 1.22+ / GORM / PostgreSQL / gRPC / pytest-style 测试（testify）

**Spec:** `docs/superpowers/specs/2026-06-16-go-common-message-slimdown-design.md`

---

## File Structure

**message-service 新增**：
- `internal/message/email/sender.go` — `Sender` + `SendResult`（删 Hook）+ `AccountProvider`
- `internal/message/email/registry.go` — `AccountRegistry` + `Config`
- `internal/message/sms/sender.go` — 同 email
- `internal/message/sms/registry.go` — 同 email
- `internal/message/sms/router.go` — `Router`（改造为 `[]AccountProvider`）
- 同名 `_test.go` 一一对应

**message-service 修改**：
- `pkg/config/config.go:14-21` — import 路径切换
- `internal/service/service.go:17-18,31-32,136-156` — import + 注入 Router
- `internal/service/send.go:14-15,20-93,103-179` — 调用方适配（SendResult 字段重命名 + 落 Account + 空时走 Router）
- `internal/service/service_test.go:17-22,121-147` — import + mock 构造适配
- `internal/store/models/message_record.go:12-32` — 加 `Account string` 列
- `cmd/migrate/main.go`（如有）— AutoMigrate 自动覆盖新列
- `config.yaml` — 加 `sms.routes` 示例

**go-common/message 删除**：
- `email/sender.go` 中的 `Sender`/`SendResult`/`Hook`/`HookFunc`/`WithHook`/`SenderOption`（保留 `Provider` + `Message`）
- `email/registry/`（整个子包）
- `sms/sender.go` 中同上
- `sms/registry/`（整个子包）
- `sms/router.go`
- `integration_test.go`

---

## Phase 1: 搬家（行为不变）

### Task 1: 搬 email Sender + SendResult 到 internal/message/email

**Files:**
- Create: `internal/message/email/sender.go`
- Create: `internal/message/email/sender_test.go`
- Source: `/Users/moss/code/base/go-common/message/email/sender.go`、`sender_test.go`

- [x] **Step 1: 创建 internal/message/email/sender.go（带 AccountProvider 类型）**

直接复制 go-common 的 `email/sender.go` 全部内容，包名改为 `email`（保持），做三处修改：

1. 删除 `Hook` interface、`HookFunc` type、`WithHook` function、`SenderOption` type、`fireHooks` method
2. `Sender` struct 删 `hooks []Hook` 字段
3. `NewSender` 签名改为 `func NewSender(providers []*AccountProvider) *Sender`（无 opts）
4. `Send` 内部 `s.fireHooks(...)` 调用全删
5. `SendResult` 字段重命名：`Provider string` → `Vendor string`；新增 `Account string`
6. 文件顶部新增 `AccountProvider` 类型：

```go
// AccountProvider wraps a vendor Provider with its (vendor, account) identity,
// so Sender/Router can return enough context for record persistence.
type AccountProvider struct {
    Vendor   string
    Account  string
    Provider Provider // 来自 go-common/message/email
}
```

注意：`Provider` 接口 + `Message` 类型仍 import 自 `github.com/servekit/go-common/message/email`。本文件不重新定义它们。

- [x] **Step 2: 创建 internal/message/email/sender_test.go（删 hook 测试 + 适配新签名）**

复制 go-common 的 `email/sender_test.go`，做以下修改：

1. 删除 `TestSender_Send_hookOnSuccess` / `TestSender_Send_hookOnFailure` / `TestSender_Send_hookWithFallback` / `TestSender_Send_multipleHooks` 四个测试
2. `testProvider` 嵌入 `AccountProvider` 调用方适配：构造 providers 时用 `&AccountProvider{Vendor: "smtp", Account: "default", Provider: &testProvider{...}}`
3. 断言 `result.Provider` 改为 `result.Vendor`
4. 新增一个测试验证 SendResult.Account：

```go
func TestSender_Send_recordsVendorAndAccount(t *testing.T) {
    p := &AccountProvider{Vendor: "smtp", Account: "primary", Provider: &testProvider{name: "smtp"}}
    s := NewSender([]*AccountProvider{p})

    result, err := s.Send(context.Background(), &Message{To: "user@test.com", Body: "Hello"})
    require.NoError(t, err)
    require.Equal(t, "smtp", result.Vendor)
    require.Equal(t, "primary", result.Account)
}
```

- [x] **Step 3: 跑测试验证 fail**

```bash
cd /Users/moss/code/base/message-service
go test ./internal/message/email/ -run TestSender -v
```

Expected: 编译失败（`NewSender` 签名变了，老调用方还没改）—— 这是预期的，因为 service.go 还没切换 import。

实际上由于本 task 不动 service.go，跑测试时 `internal/message/email/` 包是独立的，应该能编译。但 `internal/service/` 仍然 import go-common 的版本，所以全量 build 会冲突。

为隔离验证：`go test ./internal/message/email/` 应该独立编译并通过。如果失败，根据错误修正。

- [x] **Step 4: 跑通 email 包测试**

```bash
go test ./internal/message/email/ -v
```

Expected: PASS（所有非 hook 测试通过，包括新增的 `TestSender_Send_recordsVendorAndAccount`）。

- [x] **Step 5: Commit**

```bash
git add internal/message/email/sender.go internal/message/email/sender_test.go
git commit -m "refactor(message): move email Sender/SendResult to internal/message/email

From go-common. Drops Hook mechanism (unused). Sender now holds
[]*AccountProvider; SendResult fields renamed Provider→Vendor, adds Account."
```

---

### Task 2: 搬 email AccountRegistry + Config

**Files:**
- Create: `internal/message/email/registry.go`
- Create: `internal/message/email/registry_test.go`
- Source: `/Users/moss/code/base/go-common/message/email/registry/registry.go`、`registry_test.go`

- [x] **Step 1: 创建 registry.go**

复制 go-common 的 `email/registry/registry.go`，做以下修改：

1. 包名从 `registry` 改为 `email`（与 sender.go 同包）
2. import 改为：`smptprovider "github.com/servekit/go-common/message/email/smtp"`、`mailgunprovider "github.com/servekit/go-common/message/email/mailgun"`（接口仍来自本包的 `Provider`，但因为 sender.go 里我们没重新定义 `Provider`，需要 import go-common 的 email 包）

**重要：包名冲突处理**——本包名是 `email`，又需要 import `github.com/servekit/go-common/message/email`（因为 `Provider` 接口在那）。Go 不允许同包名 import。解决方案：

将 go-common 的 email 包用别名 import：

```go
import (
    emailcommon "github.com/servekit/go-common/message/email"
    smtpprovider "github.com/servekit/go-common/message/email/smtp"
    mailgunprovider "github.com/servekit/go-common/message/email/mailgun"
)
```

然后 sender.go 和 registry.go 中所有 `Provider` 改为 `emailcommon.Provider`、`Message` 改为 `emailcommon.Message`。

`AccountProvider.Provider` 字段类型改为 `emailcommon.Provider`。

`buildProvider` 返回类型改为 `emailcommon.Provider`。

`AccountRegistry.vendors` 类型改为 `map[string]map[string]*AccountProvider`（不再是 `emailcommon.Provider`，因为我们要带 vendor/account）。

`flattenProviders` 返回 `[]*AccountProvider`。

3. `AccountConfig` / `VendorConfig` / `Config` 类型原样保留（仅 YAML 字段）

- [x] **Step 2: 创建 registry_test.go**

复制 go-common 的 `email/registry/registry_test.go`，包名改为 `email`，断言适配：

- `vendors["smtp"]["A"]` 类型从 `email.Provider` 改为 `*AccountProvider`
- 测试用 `&AccountProvider{Vendor: ..., Account: ..., Provider: &testProvider{...}}` 构造

- [x] **Step 3: 跑测试验证 pass**

```bash
go test ./internal/message/email/ -v
```

Expected: PASS（包括 Task 1 的 sender 测试 + 本 task 的 registry 测试）。

- [x] **Step 4: Commit**

```bash
git add internal/message/email/registry.go internal/message/email/registry_test.go
git commit -m "refactor(message): move email AccountRegistry to internal/message/email

From go-common/message/email/registry. Holds []*AccountProvider instead of
raw Providers, so SenderFor returns context for record persistence."
```

---

### Task 3: 搬 sms Sender + SendResult 到 internal/message/sms

**Files:**
- Create: `internal/message/sms/sender.go`
- Create: `internal/message/sms/sender_test.go`

镜像 Task 1，把 sms 版本搬过来。

- [x] **Step 1: 创建 sender.go**

复制 go-common 的 `sms/sender.go`，与 Task 1 相同的修改（删 Hook、加 AccountProvider、SendResult 字段重命名）。

import：
```go
smscommon "github.com/servekit/go-common/message/sms"
```

`AccountProvider.Provider` 字段类型 `smscommon.Provider`。

- [x] **Step 2: 创建 sender_test.go**

复制 go-common 的 `sms/sender_test.go`，删除 hook 相关测试（如有），适配 `[]*AccountProvider` + `result.Vendor`。

- [x] **Step 3: 跑测试 pass**

```bash
go test ./internal/message/sms/ -run TestSender -v
```

Expected: PASS

- [x] **Step 4: Commit**

```bash
git add internal/message/sms/sender.go internal/message/sms/sender_test.go
git commit -m "refactor(message): move sms Sender/SendResult to internal/message/sms"
```

---

### Task 4: 搬 sms AccountRegistry + Config

**Files:**
- Create: `internal/message/sms/registry.go`
- Create: `internal/message/sms/registry_test.go`

镜像 Task 2，把 sms 版本搬过来。

- [x] **Step 1: 创建 registry.go**

复制 go-common 的 `sms/registry/registry.go`，包名改 `sms`，import 别名 `smscommon`，`AccountRegistry.vendors` 类型改 `map[string]map[string]*AccountProvider`。

`Config.DefaultCountry` 字段**保留**（Router 接入要用）。

- [x] **Step 2: 创建 registry_test.go**

复制 go-common 的 `sms/registry/registry_test.go`，包名改 `sms`，适配 `*AccountProvider`。

- [x] **Step 3: 跑测试 pass**

```bash
go test ./internal/message/sms/ -v
```

Expected: PASS

- [x] **Step 4: Commit**

```bash
git add internal/message/sms/registry.go internal/message/sms/registry_test.go
git commit -m "refactor(message): move sms AccountRegistry to internal/message/sms"
```

---

### Task 5: 搬 sms Router（暂保持 []Provider 输入，不接入）

**Files:**
- Create: `internal/message/sms/router.go`
- Create: `internal/message/sms/router_test.go`
- Source: `/Users/moss/code/base/go-common/message/sms/router.go`、`router_test.go`

- [x] **Step 1: 创建 router.go（原样搬运）**

复制 go-common 的 `sms/router.go`，**暂时保持原样**（输入 `[]Provider`，输出 `*SendResult`）。包名 `sms`，import 别名 `smscommon`，所有 `sms.Provider` 改为 `smscommon.Provider`、`sms.Message` 改为 `smscommon.Message`。

本 task 暂不引入 `[]*AccountProvider` 输入——那是 Task 10 的改造内容。本 task 只是把代码搬过来，使其能在 message-service 内被引用。

`SendResult` 引用本包的 `SendResult`（Task 3 中定义），因此 `result.Provider` 需要改为 `result.Vendor`。但由于 router 不持有 account 信息，先留空 `Account`。

- [x] **Step 2: 创建 router_test.go**

复制 go-common 的 `sms/router_test.go`，包名改 `sms`。适配：

- `trackProvider` 仍实现 `smscommon.Provider` 接口（`Name() / Send()`）
- `NewRouter("CN", []smscommon.Provider{...}, Route{...})`
- 断言 `result.Provider` 改为 `result.Vendor`

- [x] **Step 3: 跑测试 pass**

```bash
go test ./internal/message/sms/ -v
```

Expected: PASS

- [x] **Step 4: Commit**

```bash
git add internal/message/sms/router.go internal/message/sms/router_test.go
git commit -m "refactor(message): move sms Router to internal/message/sms (verbatim, unwired)"
```

---

### Task 6: 切换 message-service 的 import 到 internal/message

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `internal/service/service.go`
- Modify: `internal/service/service_test.go`

- [x] **Step 1: 改 pkg/config/config.go 的 import**

把：
```go
emailregistry "github.com/servekit/go-common/message/email/registry"
smsregistry "github.com/servekit/go-common/message/sms/registry"
```
改为：
```go
"message-service/internal/message/email"
"message-service/internal/message/sms"
```

`Config.Email *emailregistry.Config` → `*email.Config`；`Config.SMS *smsregistry.Config` → `*sms.Config`。

- [x] **Step 2: 改 internal/service/service.go 的 import**

把：
```go
emailregistry "github.com/servekit/go-common/message/email/registry"
smsregistry "github.com/servekit/go-common/message/sms/registry"
```
改为：
```go
"message-service/internal/message/email"
"message-service/internal/message/sms"
```

`MessageService.emailRegistry *emailregistry.AccountRegistry` → `*email.AccountRegistry`；同 sms。

`newWithDeps` 中 `emailregistry.NewAccountRegistry(cfg.Email)` → `email.NewAccountRegistry(cfg.Email)`；同 sms。

- [x] **Step 3: 改 internal/service/send.go 的 import 和调用方适配**

把：
```go
email "github.com/servekit/go-common/message/email"
sms "github.com/servekit/go-common/message/sms"
```
改为：
```go
emailcommon "github.com/servekit/go-common/message/email"
smscommon "github.com/servekit/go-common/message/sms"
"message-service/internal/message/email"
"message-service/internal/message/sms"
```

适配：
- `email.Message` → `emailcommon.Message`（go-common 的类型，因为 message-service 的 email 包没重新定义）
- `sms.Message` → `smscommon.Message`
- `email.SendResult` → `email.SendResult`（本包的）
- `sms.SendResult` → `sms.SendResult`（本包的）
- `result.Provider` → `result.Vendor`（在 `emailProviderToProto`/`smsProviderToProto` 调用处）

注意 `persist*Record` 中也用了 `result.Provider`，要全部改为 `result.Vendor`。

- [x] **Step 4: 改 internal/service/service_test.go 的 import 和 mock 构造**

把：
```go
email "github.com/servekit/go-common/message/email"
emailregistry "github.com/servekit/go-common/message/email/registry"
sms "github.com/servekit/go-common/message/sms"
smsregistry "github.com/servekit/go-common/message/sms/registry"
```
改为：
```go
emailcommon "github.com/servekit/go-common/message/email"
"message-service/internal/message/email"
smscommon "github.com/servekit/go-common/message/sms"
"message-service/internal/message/sms"
```

适配：
- `mockEmailProvider.Send(_ context.Context, _ *email.Message)` → `_ *emailcommon.Message`
- `mockSMSProvider.Send(_ context.Context, _ *sms.Message)` → `_ *smscommon.Message`
- `newTestEmailService` 中 `email.Provider` → `*email.AccountProvider`，构造时包装：`&email.AccountProvider{Vendor: "mock", Account: fmt.Sprintf("p%d", i), Provider: p}`
- `emailregistry.NewAccountRegistryFromProviders(map[string]map[string]email.Provider{"mock": accounts})` → `email.NewAccountRegistryFromProviders(map[string]map[string]*email.AccountProvider{"mock": accounts})`
- 同 sms

由于 `result.Provider` 改名为 `result.Vendor`，但 mock provider name 仍是 `"smtp"`/`"mailgun"`/`"aliyun"`，断言 `resp.Provider`（这是 proto 字段，没变）依然 work。但 `rec.Provider` 也是 proto enum，没变。所以**测试断言不需要改**。

- [x] **Step 5: 跑全量测试**

```bash
go build ./...
go test ./...
```

Expected: PASS（包括 internal/message/ 和 internal/service/ 的所有测试）。

如果 `service_test.go` 中 mock 包装出错，根据错误修正（主要是 `*AccountProvider` 包装）。

- [x] **Step 6: Commit**

```bash
git add pkg/config/config.go internal/service/service.go internal/service/send.go internal/service/service_test.go
git commit -m "refactor(service): switch imports to internal/message

Email/SMS registry now served from message-service internals.
go-common still provides Provider interface + vendor subpackages."
```

---

## Phase 2: 改造（落 Account 到 MessageRecord）

### Task 7: MessageRecord 加 Account 列

**Files:**
- Modify: `internal/store/models/message_record.go:12-32`
- Modify: `internal/service/service_test.go:158-172`（newTestRecord helper 可选加 account）

- [x] **Step 1: 加 Account 字段**

在 `MessageRecord` struct 中，`Provider` 字段下方加：

```go
Provider       int32           `gorm:"not null;index"`
Account        string          `gorm:"size:64;column:account"`  // 新增
Status         int32           `gorm:"not null;default:0;index"`
```

不加索引（查询频率低）。

- [x] **Step 2: 跑迁移测试验证 DB schema 升级**

```bash
go test ./internal/service/ -run TestQuery -v
```

`setupQueryTest` 调用 `db.AutoMigrate(&models.MessageRecord{})`，AutoMigrate 会自动加 `account` 列。

Expected: PASS（PostgreSQL testcontainer 启动，AutoMigrate 成功）。

- [x] **Step 3: Commit**

```bash
git add internal/store/models/message_record.go
git commit -m "feat(models): add Account column to MessageRecord"
```

---

### Task 8: persist*Record 填 Account 字段

**Files:**
- Modify: `internal/service/send.go:103-159`
- Modify: `internal/service/service_test.go`（断言加 Account）

- [x] **Step 1: 改 persistEmailRecord 填 Account**

```go
record := &models.MessageRecord{
    ID:       id,
    Channel:  int32(pb.Channel_CHANNEL_EMAIL),
    Target:   req.GetTo(),
    ...
    Attempts: result.Attempts,
    Provider: int32(emailProviderToProto(result.Vendor)),
    Account:  result.Account,  // 新增
}
```

- [x] **Step 2: 改 persistSMSRecord 同样填 Account**

```go
record := &models.MessageRecord{
    ...
    Attempts: result.Attempts,
    Provider: int32(smsProviderToProto(result.Vendor)),
    Account:  result.Account,  // 新增
}
```

- [x] **Step 3: 加测试断言 Account 落库**

在 `TestSendEmail_SelectAccount` 中加：

```go
rec := repo.getRecord(resp.Id)
require.NotNil(t, rec)
assert.Equal(t, "A", rec.Account)  // 选中 smtp/A，account 应为 "A"
```

同样在 `TestSendSMS_Success` 中：

```go
rec := repo.getRecord(resp.Id)
require.NotNil(t, rec)
assert.Equal(t, "p0", rec.Account)  // mock 构造时 account 名为 "p0"
```

- [x] **Step 4: 跑测试 pass**

```bash
go test ./internal/service/ -v
```

Expected: PASS（新断言通过）。

- [x] **Step 5: Commit**

```bash
git add internal/service/send.go internal/service/service_test.go
git commit -m "feat(service): persist Account in MessageRecord"
```

---

## Phase 3: 接入 Router 到 sendSMS

### Task 9: Router 改造为输入 []*AccountProvider

**Files:**
- Modify: `internal/message/sms/router.go`
- Modify: `internal/message/sms/router_test.go`

- [x] **Step 1: 改 router.go 的 Route 和 Router 类型**

```go
type Route struct {
    Country   string              // ISO 3166-1 alpha-2
    Targets   []*AccountProvider  // 替代 Providers []smscommon.Provider
}

type Router struct {
    defaultCountry string
    defaultTargets []*AccountProvider  // 替代 defaultProviders
    routes         map[string][]*AccountProvider
}

func NewRouter(defaultCountry string, defaultTargets []*AccountProvider, routes ...Route) *Router {
    m := make(map[string][]*AccountProvider, len(routes))
    for _, r := range routes {
        m[r.Country] = r.Targets
    }
    return &Router{
        defaultCountry: defaultCountry,
        defaultTargets: defaultTargets,
        routes:         m,
    }
}
```

- [x] **Step 2: 改 Send 方法适配 []*AccountProvider**

把循环 `for _, p := range providers` 改为：

```go
for _, ap := range targets {
    if ctx.Err() != nil {
        return &SendResult{
            Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
            Success: false, Error: ctx.Err(),
            Duration: time.Since(start), Attempts: attempts,
        }, ctx.Err()
    }
    attempts++
    lastVendor = ap.Vendor
    lastAccount = ap.Account
    if err := ap.Provider.Send(ctx, msg); err != nil {
        lastErr = err
        continue
    }
    return &SendResult{
        Message: msg, Vendor: ap.Vendor, Account: ap.Account, Target: msg.To,
        Success:  true,
        Duration: time.Since(start), Attempts: attempts,
    }, nil
}
return &SendResult{
    Message: msg, Vendor: lastVendor, Account: lastAccount, Target: msg.To,
    Success: false, Error: lastErr,
    Duration: time.Since(start), Attempts: attempts,
}, fmt.Errorf("sms: all targets failed for %s, last error: %w", msg.To, lastErr)
```

需要新增局部变量 `lastVendor`、`lastAccount`。

错误信息 "no provider available" 改为 "no route target for country X"。

- [x] **Step 3: 改 router_test.go 适配新签名**

每个测试构造 providers 改为 `[]*AccountProvider{...}`，例如：

```go
cn := &AccountProvider{Vendor: "aliyun", Account: "cn", Provider: &trackProvider{name: "cn"}}
def := &AccountProvider{Vendor: "aliyun", Account: "default", Provider: &trackProvider{name: "default"}}

router := NewRouter("CN", []*AccountProvider{def},
    Route{Country: "CN", Targets: []*AccountProvider{cn}},
)
```

断言 `result.Provider` 改为 `result.Vendor`。

新增一个测试验证 SendResult.Account：

```go
func TestRouter_Send_recordsAccount(t *testing.T) {
    cn := &AccountProvider{Vendor: "aliyun", Account: "cn", Provider: &trackProvider{name: "cn"}}
    router := NewRouter("CN", nil, Route{Country: "CN", Targets: []*AccountProvider{cn}})

    result, err := router.Send(context.Background(), &smscommon.Message{To: "+8613800138000", Content: "test"})
    require.NoError(t, err)
    require.Equal(t, "cn", result.Account)
    require.Equal(t, "aliyun", result.Vendor)
}
```

- [x] **Step 4: 跑测试 pass**

```bash
go test ./internal/message/sms/ -v
```

Expected: PASS

- [x] **Step 5: Commit**

```bash
git add internal/message/sms/router.go internal/message/sms/router_test.go
git commit -m "refactor(sms): Router takes []*AccountProvider, records Vendor+Account"
```

---

### Task 10: 加 routes 配置 + Router 启动构造

**Files:**
- Modify: `internal/message/sms/registry.go`（Config 加 Routes 字段）
- Create: `internal/message/sms/router_builder.go`（从 Config 构造 Router）
- Create: `internal/message/sms/router_builder_test.go`
- Modify: `config.yaml`

- [x] **Step 1: 扩展 Config 加 Routes 字段**

在 `registry.go` 中：

```go
type Config struct {
    DefaultCountry string                 `default:"CN"`
    Vendors        map[string]VendorConfig
    Routes         []RouteConfig          // 新增
}

type RouteConfig struct {
    Country string
    Targets []RouteTarget
}

type RouteTarget struct {
    Vendor  string
    Account string
}
```

- [x] **Step 2: 创建 router_builder.go**

```go
package sms

import (
    "fmt"
)

// BuildRouter constructs a Router from Config + AccountRegistry. Each RouteTarget
// must reference a (vendor, account) already defined in Config.Vendors; otherwise
// construction fails (fail-fast at startup).
//
// Returns nil Router and nil error if cfg.Routes is empty — caller decides
// whether that's an error (sendSMS path treats empty routes as misconfiguration).
func BuildRouter(cfg *Config, reg *AccountRegistry) (*Router, error) {
    if cfg == nil || len(cfg.Routes) == 0 {
        return nil, nil
    }

    defaultCountry := cfg.DefaultCountry
    if defaultCountry == "" {
        defaultCountry = "CN"
    }

    var defaultTargets []*AccountProvider
    routes := make([]Route, 0, len(cfg.Routes))

    for _, rc := range cfg.Routes {
        targets := make([]*AccountProvider, 0, len(rc.Targets))
        for _, t := range rc.Targets {
            ap, err := reg.lookup(t.Vendor, t.Account)
            if err != nil {
                return nil, fmt.Errorf("sms: route %s: %w", rc.Country, err)
            }
            targets = append(targets, ap)
        }
        if rc.Country == "*" {
            defaultTargets = targets
            continue
        }
        routes = append(routes, Route{Country: rc.Country, Targets: targets})
    }

    return NewRouter(defaultCountry, defaultTargets, routes...), nil
}
```

需要在 `AccountRegistry` 上新增 `lookup(vendor, account string) (*AccountProvider, error)` 方法（返回包装后的 AccountProvider）：

```go
// lookup returns the AccountProvider for (vendor, account). Untyped version of
// SenderFor — used by Router construction to obtain provider with identity.
func (r *AccountRegistry) lookup(vendor, account string) (*AccountProvider, error) {
    if vendor == "" || account == "" {
        return nil, fmt.Errorf("vendor and account must be set together")
    }
    accounts, ok := r.vendors[vendor]
    if !ok {
        return nil, fmt.Errorf("unknown vendor %q", vendor)
    }
    ap, ok := accounts[account]
    if !ok {
        return nil, fmt.Errorf("unknown account %q under vendor %q", account, vendor)
    }
    return ap, nil
}
```

注意：因为 `vendors map[string]map[string]*AccountProvider` 已经存的是 `*AccountProvider`，`lookup` 直接返回即可。

- [x] **Step 3: 创建 router_builder_test.go**

```go
package sms

import (
    "context"
    "testing"

    "github.com/stretchr/testify/require"
)

func TestBuildRouter_emptyConfig(t *testing.T) {
    reg := NewAccountRegistryFromProviders(nil)
    r, err := BuildRouter(nil, reg)
    require.NoError(t, err)
    require.Nil(t, r, "empty routes → nil router (caller decides if that's error)")
}

func TestBuildRouter_validRoutes(t *testing.T) {
    cn := &AccountProvider{Vendor: "aliyun", Account: "default", Provider: &trackProvider{name: "cn"}}
    reg := NewAccountRegistryFromProviders(map[string]map[string]*AccountProvider{
        "aliyun": {"default": cn},
    })
    cfg := &Config{
        DefaultCountry: "CN",
        Routes: []RouteConfig{
            {Country: "CN", Targets: []RouteTarget{{Vendor: "aliyun", Account: "default"}}},
            {Country: "*", Targets: []RouteTarget{{Vendor: "aliyun", Account: "default"}}},
        },
    }

    r, err := BuildRouter(cfg, reg)
    require.NoError(t, err)
    require.NotNil(t, r)

    result, err := r.Send(context.Background(), &smscommon.Message{To: "+8613800138000", Content: "hi"})
    require.NoError(t, err)
    require.True(t, result.Success)
    require.Equal(t, "default", result.Account)
}

func TestBuildRouter_unknownVendor(t *testing.T) {
    reg := NewAccountRegistryFromProviders(map[string]map[string]*AccountProvider{
        "aliyun": {"default": &AccountProvider{Vendor: "aliyun", Account: "default", Provider: &trackProvider{name: "x"}}},
    })
    cfg := &Config{
        Routes: []RouteConfig{
            {Country: "CN", Targets: []RouteTarget{{Vendor: "twilio", Account: "default"}}},
        },
    }

    _, err := BuildRouter(cfg, reg)
    require.Error(t, err)
    require.Contains(t, err.Error(), "unknown vendor")
}
```

`smscommon` 是 go-common/message/sms 的别名 import。

- [x] **Step 4: 跑测试 pass**

```bash
go test ./internal/message/sms/ -run TestBuildRouter -v
```

Expected: PASS

- [x] **Step 5: 加 config.yaml 示例**

在 `config.yaml` 的 `sms:` 段下加：

```yaml
sms:
  default_country: CN
  vendors:
    aliyun:
      accounts:
        - name: default
          ...
  # routes:  # 取消注释并按需配置
  #   - country: CN
  #     targets:
  #       - { vendor: aliyun, account: default }
  #   - country: "*"
  #     targets:
  #       - { vendor: aliyun, account: default }
```

注释掉让默认配置不带 routes（保持向后兼容）。

- [x] **Step 6: Commit**

```bash
git add internal/message/sms/registry.go internal/message/sms/router_builder.go internal/message/sms/router_builder_test.go config.yaml
git commit -m "feat(sms): add Routes config + BuildRouter

Router now constructed from (Config, AccountRegistry) at startup. Route
targets reference (vendor, account) pairs already defined in Vendors;
unknown refs fail-fast at construction."
```

---

### Task 11: 接入 sendSMS — vendor/account 空时走 Router

**Files:**
- Modify: `internal/service/service.go:31-32,136-156`
- Modify: `internal/service/send.go:60-93`
- Modify: `internal/service/service_test.go:135-147`

- [x] **Step 1: 在 MessageService 加 smsRouter 字段**

`service.go` 中：

```go
type MessageService struct {
    pb.UnimplementedMessageServiceServer

    db            *gorm.DB
    ownDB         bool
    repo          messageRepo
    gid           thirdcall.GIDService
    emailRegistry *email.AccountRegistry
    smsRegistry   *sms.AccountRegistry
    smsRouter     *sms.Router  // 新增；nil 时 vendor/account 空的请求报错
    manager       *lifecycle.Manager
}
```

- [x] **Step 2: 在 newWithDeps 构造 Router**

```go
smsRegistry, err := sms.NewAccountRegistry(cfg.SMS)
if err != nil {
    return nil, fmt.Errorf("sms registry: %w", err)
}

smsRouter, err := sms.BuildRouter(cfg.SMS, smsRegistry)
if err != nil {
    return nil, fmt.Errorf("sms router: %w", err)
}

return &MessageService{
    db:            db,
    repo:          msgRepo,
    gid:           gid,
    emailRegistry: emailRegistry,
    smsRegistry:   smsRegistry,
    smsRouter:     smsRouter,
}, nil
```

- [x] **Step 3: 改 sendSMS 实现"空时走 Router"**

```go
func (s *MessageService) sendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
    var result *sms.SendResult
    var sendErr error

    if req.GetVendor() != "" && req.GetAccount() != "" {
        sender, err := s.smsRegistry.SenderFor(req.GetVendor(), req.GetAccount())
        if err != nil {
            return nil, xcodes.ErrBadRequest.Wrap(err)
        }
        result, sendErr = sender.Send(ctx, &smscommon.Message{
            To:       req.GetTo(),
            Content:  req.GetContent(),
            Template: req.GetTemplateId(),
            Params:   models.MapStringString(req.GetTemplateParams()),
        })
    } else {
        if s.smsRouter == nil {
            return nil, xcodes.ErrBadRequest.New("sms routes not configured; specify vendor and account explicitly")
        }
        result, sendErr = s.smsRouter.Send(ctx, &smscommon.Message{
            To:       req.GetTo(),
            Content:  req.GetContent(),
            Template: req.GetTemplateId(),
            Params:   models.MapStringString(req.GetTemplateParams()),
        })
    }

    id, err := s.gid.NextID(ctx)
    if err != nil {
        return nil, xcodes.ErrInternal.Wrap(err)
    }

    if sendErr != nil {
        if result != nil {
            s.persistSMSRecord(ctx, id, req, result)
        }
        return nil, xcodes.ErrMessageSendFailed.Wrap(sendErr)
    }

    s.persistSMSRecord(ctx, id, req, result)

    return &pb.SendResponse{
        Id:       id,
        Status:   pb.MessageStatus_MESSAGE_STATUS_SENT,
        Provider: smsProviderToProto(result.Vendor),
    }, nil
}
```

注意 `id` 生成的位置变了——以前先 `gid.NextID` 再发送；现在先发送再 `gid.NextID`。这样发送失败时不会浪费 ID（虽然雪花算法不太在意）。也可以保持原顺序（先生成 ID 再发送），但发送失败时不落库。这里改为发送后再生成 ID——更符合"发送成功才需要持久化"的语义。

实际上，原代码是先生成 ID 再发送，发送失败也落库（带 result）。新代码应保持这个语义。让我修正：

实际上原代码：
```go
id, err := s.gid.NextID(ctx)  // 先生成 ID
...
result, err := sender.Send(ctx, msg)  // 发送
if err != nil {
    if result != nil {
        s.persistSMSRecord(ctx, id, req, result)  // 失败也落库
    }
    return nil, ...
}
s.persistSMSRecord(ctx, id, req, result)  // 成功落库
```

OK 保持原顺序——先生成 ID，再发送，失败也用同一 ID 落库。让我修正上面的代码：

```go
func (s *MessageService) sendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
    id, err := s.gid.NextID(ctx)
    if err != nil {
        return nil, xcodes.ErrInternal.Wrap(err)
    }

    msg := &smscommon.Message{
        To:       req.GetTo(),
        Content:  req.GetContent(),
        Template: req.GetTemplateId(),
        Params:   models.MapStringString(req.GetTemplateParams()),
    }

    var result *sms.SendResult
    var sendErr error
    if req.GetVendor() != "" && req.GetAccount() != "" {
        sender, err := s.smsRegistry.SenderFor(req.GetVendor(), req.GetAccount())
        if err != nil {
            return nil, xcodes.ErrBadRequest.Wrap(err)
        }
        result, sendErr = sender.Send(ctx, msg)
    } else {
        if s.smsRouter == nil {
            return nil, xcodes.ErrBadRequest.New("sms routes not configured; specify vendor and account explicitly")
        }
        result, sendErr = s.smsRouter.Send(ctx, msg)
    }

    if sendErr != nil {
        if result != nil {
            s.persistSMSRecord(ctx, id, req, result)
        }
        return nil, xcodes.ErrMessageSendFailed.Wrap(sendErr)
    }

    s.persistSMSRecord(ctx, id, req, result)

    return &pb.SendResponse{
        Id:       id,
        Status:   pb.MessageStatus_MESSAGE_STATUS_SENT,
        Provider: smsProviderToProto(result.Vendor),
    }, nil
}
```

- [x] **Step 4: 改 newTestSMSService 支持注入 Router**

```go
func newTestSMSService(t *testing.T, repo *mockRepo, providers []smscommon.Provider) *MessageService {
    t.Helper()
    accounts := make(map[string]*sms.AccountProvider, len(providers))
    for i, p := range providers {
        accounts[fmt.Sprintf("p%d", i)] = &sms.AccountProvider{
            Vendor:   "mock",
            Account:  fmt.Sprintf("p%d", i),
            Provider: p,
        }
    }
    reg := sms.NewAccountRegistryFromProviders(map[string]map[string]*sms.AccountProvider{"mock": accounts})
    return &MessageService{
        repo:        repo,
        gid:         getTestGID(t),
        manager:     lifecycle.NewManager(),
        smsRegistry: reg,
        // smsRouter 留 nil，测试路由用专门的 helper
    }
}

// newTestSMSServiceWithRouter 构造带 Router 的 service（vendor/account 空时走 Router）。
func newTestSMSServiceWithRouter(t *testing.T, repo *mockRepo, router *sms.Router) *MessageService {
    t.Helper()
    return &MessageService{
        repo:      repo,
        gid:       getTestGID(t),
        manager:   lifecycle.NewManager(),
        smsRouter: router,
    }
}
```

注意 `mockSMSProvider` 仍实现 `smscommon.Provider`（来自 go-common），所以参数类型 `[]smscommon.Provider`。

- [x] **Step 5: 新增路由测试**

```go
func TestSendSMS_RouteByPhone(t *testing.T) {
    repo := newMockRepo()
    cn := &sms.AccountProvider{Vendor: "aliyun", Account: "cn-default", Provider: &mockSMSProvider{name: "aliyun"}}
    def := &sms.AccountProvider{Vendor: "aliyun", Account: "fallback", Provider: &mockSMSProvider{name: "aliyun"}}
    router := sms.NewRouter("CN", []*sms.AccountProvider{def},
        sms.Route{Country: "CN", Targets: []*sms.AccountProvider{cn}},
    )
    svc := newTestSMSServiceWithRouter(t, repo, router)

    resp, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
        To:      "+861380001111",
        Content: "Your code is 1234",
    })
    require.NoError(t, err)
    assert.Equal(t, pb.Provider_PROVIDER_ALIYUN, resp.Provider)

    rec := repo.getRecord(resp.Id)
    require.NotNil(t, rec)
    assert.Equal(t, "cn-default", rec.Account)
}

func TestSendSMS_NoRoutesConfigured(t *testing.T) {
    repo := newMockRepo()
    svc := newTestSMSServiceWithRouter(t, repo, nil)  // smsRouter = nil

    _, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
        To:      "+861380001111",
        Content: "hi",
    })
    require.Error(t, err)
    // xcodes.ErrBadRequest 类型
}
```

- [x] **Step 6: 跑全量测试 pass**

```bash
go test ./...
```

Expected: PASS

- [x] **Step 7: Commit**

```bash
git add internal/service/service.go internal/service/send.go internal/service/service_test.go
git commit -m "feat(service): wire Router into sendSMS

vendor/account both empty → route by phone country code via smsRouter.
smsRouter nil → BadRequest with explicit message (routes unconfigured)."
```

---

## Phase 4: 清理 go-common

### Task 12: 删除 go-common/message 中已下沉的代码

**Files:**
- Modify: `/Users/moss/code/base/go-common/message/email/sender.go`
- Delete: `/Users/moss/code/base/go-common/message/email/registry/`（整个目录）
- Modify: `/Users/moss/code/base/go-common/message/sms/sender.go`
- Delete: `/Users/moss/code/base/go-common/message/sms/registry/`（整个目录）
- Delete: `/Users/moss/code/base/go-common/message/sms/router.go`
- Delete: `/Users/moss/code/base/go-common/message/integration_test.go`

- [x] **Step 1: 修改 email/sender.go 只保留 Provider + Message**

把 `/Users/moss/code/base/go-common/message/email/sender.go` 简化为：

```go
// Package email provides the email Provider interface and Message type.
// Vendor implementations live in subpackages (smtp, mailgun, ...).
// Composition (Sender, fallback), account registry, and routing are now
// the caller's responsibility — see message-service/internal/message/email.
package email

import "context"

// Message represents an email to be sent.
type Message struct {
    To       string
    Cc       []string
    Bcc      []string
    Subject  string
    Body     string
    HTMLBody string
    ReplyTo  string
}

// Provider is the interface for email sending providers.
type Provider interface {
    Name() string
    Send(ctx context.Context, msg *Message) error
}
```

删除 `Sender`/`SendResult`/`Hook`/`HookFunc`/`WithHook`/`SenderOption`。

- [x] **Step 2: 修改 sms/sender.go 同样精简**

```go
// Package sms provides the SMS Provider interface and Message type.
// Vendor implementations live in subpackages (aliyun, ...).
// Composition, registry, and routing are now the caller's responsibility
// — see message-service/internal/message/sms.
package sms

import "context"

type Message struct {
    To       string
    Content  string
    Template string
    Params   map[string]string
}

type Provider interface {
    Name() string
    Send(ctx context.Context, msg *Message) error
}
```

- [x] **Step 3: 删除子包和文件**

```bash
cd /Users/moss/code/base/go-common
rm -rf message/email/registry
rm -rf message/sms/registry
rm message/sms/router.go
rm message/sms/router_test.go
rm message/integration_test.go
```

- [x] **Step 4: 跑 go-common 全量编译 + 测试**

```bash
cd /Users/moss/code/base/go-common
go build ./...
go test ./...
```

Expected: PASS。`smtp`/`mailgun`/`aliyun` 子包仍正常（它们只依赖父包的 Provider/Message，仍在）。

如果其他 go-common 包依赖被删的代码（grep 确认），处理之。

- [x] **Step 5: 跑 message-service 全量编译 + 测试**

```bash
cd /Users/moss/code/base/message-service
go build ./...
go test ./...
```

Expected: PASS

- [x] **Step 6: Commit（go-common repo）**

```bash
cd /Users/moss/code/base/go-common
git add message/
git commit -m "refactor(message)!: slim down to Provider interface + vendor impls

BREAKING CHANGE: Sender, SendResult, Hook, AccountRegistry, Router have
been moved to message-service/internal/message/. go-common/message now
exposes only Provider interface and Message type per channel, plus vendor
subpackages (smtp/mailgun/aliyun).

Only message-service imports go-common/message; impact is contained.
See message-service/docs/superpowers/specs/2026-06-16-go-common-message-slimdown-design.md."
```

注意 go-common 和 message-service 是两个 git repo，分两次 commit。

---

## Self-Review

**1. Spec coverage**：
- ✅ 切分边界（go-common 留 Provider+Message+厂商子包）→ Task 12
- ✅ Sender/SendResult/Registry 下沉 → Task 1-5
- ✅ Hook 删除 → Task 1（email）+ Task 3（sms）
- ✅ 目录结构 internal/message/ → Task 1-5
- ✅ import 切换 → Task 6
- ✅ Router 触发方式 A（空时走 Router）→ Task 11
- ✅ 配置 routes YAML → Task 10
- ✅ AccountProvider 包装 → Task 1 + Task 2
- ✅ SendResult 字段重命名 → Task 1 + Task 3
- ✅ MessageRecord 加 Account 列 → Task 7
- ✅ persist*Record 填 Account → Task 8
- ✅ sendSMS 流程 → Task 11
- ✅ 路由失败处理 → Task 9（router 错误）+ Task 11（router nil 报错）
- ✅ email 不接 Router → 不动 sendEmail，仅下沉代码
- ✅ 清理 go-common → Task 12

**2. Placeholder scan**：无 TBD/TODO。

**3. Type consistency**：
- `*AccountProvider`（不是 `AccountProvider` 值类型）—— 一致
- `[]*AccountProvider` 作为 Sender/Router 输入 —— 一致
- `result.Vendor` / `result.Account` —— 一致
- `smscommon.Provider` / `emailcommon.Provider` 别名 import —— 一致
- `BuildRouter` 返回 `(*Router, error)` —— 一致
- `lookup` 方法签名 `(vendor, account string) (*AccountProvider, error)` —— 一致
