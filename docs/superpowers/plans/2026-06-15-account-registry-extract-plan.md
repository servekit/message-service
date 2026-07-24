# AccountRegistry 提取实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 message-service 的 vendor+account 选择逻辑提取到 go-common/message/{email,sms} 作为可复用的 `AccountRegistry`，message-service 删除约 190 行基础设施代码。

**Architecture:** go-common 新增 `AccountRegistry` 类型，从 YAML config 构造 provider map 并提供 `DefaultSender` / `SenderFor` 两个 API。message-service 的 `Config.Email`/`Config.SMS` 直接复用 go-common 的 config 类型，service 层调用 `registry.SenderFor(vendor, account)` 替换原有的 `selectEmailSender`/`selectSMSSender`。

**Tech Stack:** Go 1.x，go-common/message（email + sms），GORM，gRPC，testify/require + go-cmp/cmp（测试）

**Spec:** `docs/superpowers/specs/2026-06-15-account-registry-extract-design.md`

---

## 文件结构

### go-common 新增

| 文件 | 职责 |
|---|---|
| `message/email/registry.go` | `Config` / `VendorConfig` / `AccountConfig` / `AccountRegistry` / `NewAccountRegistry` / `NewAccountRegistryFromProviders` / `DefaultSender` / `SenderFor` |
| `message/email/registry_test.go` | registry 单元测试（用 fake provider） |
| `message/sms/registry.go` | email 的 SMS 对称版本 |
| `message/sms/registry_test.go` | SMS registry 单元测试 |

### message-service 修改

| 文件 | 改动 |
|---|---|
| `pkg/config/config.go` | 删除 6 个本地 config 类型，`Config.Email`/`Config.SMS` 改用 go-common 类型 |
| `internal/service/service.go` | 删除 `buildEmailAccounts` / `buildSMSAccounts` / `newEmailProvider` / `newSMSProvider` / `flattenEmail` / `flattenSMS`；`MessageService` 字段改为 `emailRegistry` / `smsRegistry` |
| `internal/service/send.go` | 删除 `selectEmailSender` / `selectSMSSender`；改为调用 `registry.SenderFor` |
| `internal/service/service_test.go` | `TestSendEmail_SelectAccount*` 系列改造为用 `email.Config` 构造 registry |

---

## Task 1: go-common email/registry.go 类型与基础构造

**Files:**
- Create: `/Users/moss/code/base/go-common/message/email/registry.go`
- Create: `/Users/moss/code/base/go-common/message/email/registry_test.go`

- [ ] **Step 1.1: 写测试 - 类型定义、空 config、NewAccountRegistryFromProviders**

将以下内容写入 `message/email/registry_test.go`：

```go
package email

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"
)

// fakeProvider 是测试用 Provider，跟踪自己是否被调用。
// 复用 sender_test.go 已有的 testProvider 思路，但增加 sentCount 字段
// 用于断言 SenderFor 返回的 Sender 只调用预期的 provider。
type fakeProvider struct {
	name      string
	err       error
	sentCount int
}

func (p *fakeProvider) Name() string { return p.name }
func (p *fakeProvider) Send(_ context.Context, _ *Message) error {
	p.sentCount++
	if p.err != nil {
		return p.err
	}
	return nil
}

// TestNewAccountRegistryFromProviders_empty 验证空 map 也能构造成功。
// DefaultSender 此时返回的 Sender 在 Send 时报 "no provider available"。
func TestNewAccountRegistryFromProviders_empty(t *testing.T) {
	r := NewAccountRegistryFromProviders(nil)

	require.NotNil(t, r.DefaultSender())
	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "u@x.com"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result)
}

// TestNewAccountRegistryFromProviders_singleVendorAccount 验证单 vendor 单 account 的基本结构。
func TestNewAccountRegistryFromProviders_singleVendorAccount(t *testing.T) {
	p := &fakeProvider{name: "smtp"}
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"smtp": {"primary": p},
	})

	def := r.DefaultSender()
	require.NotNil(t, def)
	result, err := def.Send(context.Background(), &Message{To: "u@x.com"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "smtp", result.Provider)
	require.Equal(t, 1, p.sentCount, "default sender should have called the only provider")
}

// TestNewAccountRegistryFromProviders_sortOrder 验证默认 sender 的 fallback 链
// 按 vendor 名升序、account 名升序排列。
//
// 构造顺序故意打乱：mailgun/zzz, smtp/aaa, smtp/bbb, mailgun/aaa
// 期望 fallback 链：mailgun/aaa → mailgun/zzz → smtp/aaa → smtp/bbb
func TestNewAccountRegistryFromProviders_sortOrder(t *testing.T) {
	mgA := &fakeProvider{name: "mailgun", err: errors.New("mgA down")}
	mgZ := &fakeProvider{name: "mailgun", err: errors.New("mgZ down")}
	smtpA := &fakeProvider{name: "smtp", err: errors.New("smtpA down")}
	smtpB := &fakeProvider{name: "smtp"} // 第一个会成功的

	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"mailgun": {"zzz": mgZ, "aaa": mgA},
		"smtp":    {"bbb": smtpB, "aaa": smtpA},
	})

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "u@x.com"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "smtp", result.Provider, "should fall through to the working smtp/bbb")
	require.Equal(t, 4, result.Attempts, "should have tried all 4 providers in order")

	// 验证尝试顺序：mgA → mgZ → smtpA → smtpB
	require.Equal(t, 1, mgA.sentCount)
	require.Equal(t, 1, mgZ.sentCount)
	require.Equal(t, 1, smtpA.sentCount)
	require.Equal(t, 1, smtpB.sentCount)
}

// 占位：编译需要 sort 包，后续 step 会用到。
var _ = sort.Strings
```

- [ ] **Step 1.2: 运行测试，确认失败（编译错误）**

```bash
cd /Users/moss/code/base/go-common && go test ./message/email/ -run TestNewAccountRegistryFromProviders -v
```

Expected: 编译失败 — `NewAccountRegistryFromProviders` 未定义。

- [ ] **Step 1.3: 实现 registry.go 的类型 + 基础构造**

将以下内容写入 `message/email/registry.go`：

```go
package email

import (
	"fmt"
	"sort"
)

// Config 是 email 账号的 YAML 配置，按 vendor 分组。
type Config struct {
	Vendors map[string]VendorConfig
}

// VendorConfig 持有一个 vendor（如 "smtp"、"mailgun"）下的所有账号。
type VendorConfig struct {
	Accounts []AccountConfig
}

// AccountConfig 是单个命名账号。装载所有已支持 vendor 的字段，只有
// 对应 vendor 的子集生效。fat-struct 设计：新增 vendor 时需在此添加字段。
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

// NewAccountRegistryFromProviders 从已构造的 provider map 构建 registry。
// 默认 fallback 链按 vendor 名升序、account 名升序排列。
//
// 这个构造函数主要用于测试和高级用例；生产代码应使用 NewAccountRegistry
// 从 Config 直接构造。
func NewAccountRegistryFromProviders(vendors map[string]map[string]Provider) *AccountRegistry {
	r := &AccountRegistry{
		vendors: vendors,
	}
	r.def = NewSender(flattenProviders(vendors))
	return r
}

// DefaultSender 返回包含所有 provider 的 fallback sender（按构造时确定的顺序）。
func (r *AccountRegistry) DefaultSender() *Sender {
	return r.def
}

// flattenProviders 把嵌套 map 按 vendor 名升序、account 名升序展开为扁平的 provider 列表。
// 排序确定性保证默认 fallback 顺序在多次启动间稳定。
func flattenProviders(vendors map[string]map[string]Provider) []Provider {
	vendorNames := make([]string, 0, len(vendors))
	for v := range vendors {
		vendorNames = append(vendorNames, v)
	}
	sort.Strings(vendorNames)

	var out []Provider
	for _, v := range vendorNames {
		accounts := vendors[v]
		acctNames := make([]string, 0, len(accounts))
		for a := range accounts {
			acctNames = append(acctNames, a)
		}
		sort.Strings(acctNames)
		for _, a := range acctNames {
			out = append(out, accounts[a])
		}
	}
	return out
}

// 占位避免 unused import；NewAccountRegistry 在 Task 2 中使用。
var _ = fmt.Errorf
```

- [ ] **Step 1.4: 运行测试，确认通过**

```bash
cd /Users/moss/code/base/go-common && go test ./message/email/ -run TestNewAccountRegistryFromProviders -v
```

Expected: PASS（3 个测试全部通过）。

- [ ] **Step 1.5: 提交**

```bash
cd /Users/moss/code/base/go-common && git add message/email/registry.go message/email/registry_test.go && git commit -m "feat(message/email): add AccountRegistry type with provider-map constructor"
```

---

## Task 2: 实现 SenderFor（email）

**Files:**
- Modify: `/Users/moss/code/base/go-common/message/email/registry.go`（追加 SenderFor 方法）
- Modify: `/Users/moss/code/base/go-common/message/email/registry_test.go`（追加测试）

- [ ] **Step 2.1: 写测试 - SenderFor 5 个分支**

在 `message/email/registry_test.go` 文件末尾追加：

```go
// TestSenderFor_bothEmpty 验证 vendor+account 都空时返回 DefaultSender。
func TestSenderFor_bothEmpty(t *testing.T) {
	p := &fakeProvider{name: "smtp"}
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"smtp": {"primary": p},
	})

	got, err := r.SenderFor("", "")
	require.NoError(t, err)
	require.Same(t, r.DefaultSender(), got, "both empty should return DefaultSender")
}

// TestSenderFor_bothSet 验证 vendor+account 都设置时返回只含该 provider 的 Sender。
// 通过让目标 provider 失败、其他 provider 成功，验证 Sender 只调用了目标 provider。
func TestSenderFor_bothSet(t *testing.T) {
	target := &fakeProvider{name: "smtp", err: errors.New("target down")}
	other := &fakeProvider{name: "mailgun"} // 不应被调用

	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"smtp":    {"primary": target},
		"mailgun": {"primary": other},
	})

	got, err := r.SenderFor("smtp", "primary")
	require.NoError(t, err)
	require.NotSame(t, r.DefaultSender(), got, "specific selection should NOT be the default fallback sender")

	result, err := got.Send(context.Background(), &Message{To: "u@x.com"})
	require.Error(t, err)
	require.False(t, result.Success)
	require.Equal(t, "smtp", result.Provider)
	require.Equal(t, 1, result.Attempts, "no fallback — should only try the selected provider")
	require.Equal(t, 1, target.sentCount)
	require.Equal(t, 0, other.sentCount, "other provider should not have been tried")
}

// TestSenderFor_partialVendorOnly 验证只设置 vendor 时报错。
func TestSenderFor_partialVendorOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"smtp": {"primary": &fakeProvider{name: "smtp"}},
	})

	_, err := r.SenderFor("smtp", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

// TestSenderFor_partialAccountOnly 验证只设置 account 时报错。
func TestSenderFor_partialAccountOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"smtp": {"primary": &fakeProvider{name: "smtp"}},
	})

	_, err := r.SenderFor("", "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

// TestSenderFor_unknownVendor 验证未知 vendor 时报错。
func TestSenderFor_unknownVendor(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"smtp": {"primary": &fakeProvider{name: "smtp"}},
	})

	_, err := r.SenderFor("tencent", "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email vendor")
}

// TestSenderFor_unknownAccount 验证已知 vendor 但未知 account 时报错。
func TestSenderFor_unknownAccount(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"smtp": {"primary": &fakeProvider{name: "smtp"}},
	})

	_, err := r.SenderFor("smtp", "secondary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email account")
}
```

- [ ] **Step 2.2: 运行测试，确认失败（SenderFor 未定义）**

```bash
cd /Users/moss/code/base/go-common && go test ./message/email/ -run TestSenderFor -v
```

Expected: 编译失败 — `r.SenderFor` 未定义。

- [ ] **Step 2.3: 实现 SenderFor**

在 `message/email/registry.go` 文件末尾（删除占位的 `var _ = fmt.Errorf` 那行）追加：

```go
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
func (r *AccountRegistry) SenderFor(vendor, account string) (*Sender, error) {
	if vendor == "" && account == "" {
		return r.def, nil
	}
	if vendor == "" || account == "" {
		return nil, fmt.Errorf("email: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("email: unknown vendor %q", vendor)
	}
	p, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("email: unknown account %q under vendor %q", account, vendor)
	}
	return NewSender([]Provider{p}), nil
}
```

- [ ] **Step 2.4: 运行测试，确认通过**

```bash
cd /Users/moss/code/base/go-common && go test ./message/email/ -v
```

Expected: 所有测试 PASS（包括 Task 1 的 3 个 + 本任务的 6 个 = 9 个新增测试，加上原有 sender_test.go）。

- [ ] **Step 2.5: 提交**

```bash
cd /Users/moss/code/base/go-common && git add message/email/registry.go message/email/registry_test.go && git commit -m "feat(message/email): add AccountRegistry.SenderFor with no-fallback semantics"
```

---

## Task 3: 实现 NewAccountRegistry（从 Config 构造）

**Files:**
- Modify: `/Users/moss/code/base/go-common/message/email/registry.go`（追加 `NewAccountRegistry` + vendor switch）
- Modify: `/Users/moss/code/base/go-common/message/email/registry_test.go`（追加测试）

- [ ] **Step 3.1: 写测试 - 从 Config 构造**

在 `message/email/registry_test.go` 文件末尾追加：

```go
// TestNewAccountRegistry_emptyConfig 验证 nil/空 config 能构造成功（默认 sender 无 provider）。
func TestNewAccountRegistry_emptyConfig(t *testing.T) {
	r, err := NewAccountRegistry(nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

// TestNewAccountRegistry_smtpSuccess 验证 SMTP vendor 配置能成功构造。
// 使用 fake host，因为 NewProvider 只创建 mail.Client，不会真连。
func TestNewAccountRegistry_smtpSuccess(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"smtp": {Accounts: []AccountConfig{
				{Name: "primary", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	// 通过 SenderFor 拿到单 provider sender，发空消息看是否调用成功（不会真连，因为 Send 需要 msg.To）
	sender, err := r.SenderFor("smtp", "primary")
	require.NoError(t, err)
	require.NotNil(t, sender)
}

// TestNewAccountRegistry_smtpInvalidPort 验证 SMTP port=0 时构造失败。
// smtp.NewProvider 调用 mail.WithPort(0) 会返回 error。
func TestNewAccountRegistry_smtpInvalidPort(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"smtp": {Accounts: []AccountConfig{
				{Name: "primary", Host: "smtp.example.com", Port: 0, From: "noreply@example.com"},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp")
}

// TestNewAccountRegistry_mailgunSuccess 验证 Mailgun vendor 配置能成功构造。
func TestNewAccountRegistry_mailgunSuccess(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"mailgun": {Accounts: []AccountConfig{
				{Name: "primary", Domain: "example.com", APIKey: "key-xxx", From: "noreply@example.com"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)
}

// TestNewAccountRegistry_unknownVendor 验证未知 vendor 名时报错。
func TestNewAccountRegistry_unknownVendor(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"sendgrid": {Accounts: []AccountConfig{{Name: "primary"}}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown vendor")
}

// TestNewAccountRegistry_duplicateAccountName 验证同一 vendor 下重复 account 名时报错。
func TestNewAccountRegistry_duplicateAccountName(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"smtp": {Accounts: []AccountConfig{
				{Name: "primary", Host: "a.example.com", Port: 587, From: "noreply@x.com"},
				{Name: "primary", Host: "b.example.com", Port: 587, From: "noreply@y.com"},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}
```

- [ ] **Step 3.2: 运行测试，确认失败**

```bash
cd /Users/moss/code/base/go-common && go test ./message/email/ -run TestNewAccountRegistry -v
```

Expected: 编译失败 — `NewAccountRegistry` 未定义。

- [ ] **Step 3.3: 实现 NewAccountRegistry**

在 `message/email/registry.go` 顶部 import 块加入 vendor 子包，并在文件末尾追加构造函数：

修改 import 块（替换现有 import）：

```go
import (
	"fmt"
	"sort"

	mailgunprovider "github.com/servekit/go-common/message/email/mailgun"
	smtpprovider "github.com/servekit/go-common/message/email/smtp"
)
```

在文件末尾追加：

```go
// NewAccountRegistry 从 YAML config 构造 registry。
//
// 行为：
//   - 遍历每个 vendor，根据 vendor 名调用对应子包构造 Provider
//   - 同一 vendor 下 account 名不允许重复
//   - 未知 vendor 名、provider 构造失败均返回 error
//
// 默认 fallback 链按 vendor 名升序、account 名升序排列（由
// NewAccountRegistryFromProviders 保证）。
func NewAccountRegistry(cfg *Config) (*AccountRegistry, error) {
	vendors := make(map[string]map[string]Provider)
	if cfg == nil {
		return NewAccountRegistryFromProviders(vendors), nil
	}

	for vendorName, vc := range cfg.Vendors {
		accounts := make(map[string]Provider)
		for _, ac := range vc.Accounts {
			if _, dup := accounts[ac.Name]; dup {
				return nil, fmt.Errorf("email: duplicate account name %q under vendor %q", ac.Name, vendorName)
			}
			p, err := buildProvider(vendorName, ac)
			if err != nil {
				return nil, fmt.Errorf("email: account %s/%s: %w", vendorName, ac.Name, err)
			}
			accounts[ac.Name] = p
		}
		vendors[vendorName] = accounts
	}

	return NewAccountRegistryFromProviders(vendors), nil
}

// buildProvider 根据 vendor 名分发到对应子包构造 Provider。
// 新增 vendor 时在此添加 case，并在 AccountConfig 添加对应字段。
func buildProvider(vendor string, ac AccountConfig) (Provider, error) {
	switch vendor {
	case "smtp":
		return smtpprovider.NewProvider(&smtpprovider.Config{
			Host:     ac.Host,
			Port:     ac.Port,
			Username: ac.Username,
			Password: ac.Password,
			From:     ac.From,
		})
	case "mailgun":
		return mailgunprovider.NewProvider(&mailgunprovider.Config{
			Domain:   ac.Domain,
			APIKey:   ac.APIKey,
			From:     ac.From,
			Endpoint: ac.Endpoint,
		}), nil
	default:
		return nil, fmt.Errorf("unknown vendor %q", vendor)
	}
}
```

同时删除文件末尾的占位行 `var _ = fmt.Errorf`（fmt 现在已被 NewAccountRegistry 实际使用）。

- [ ] **Step 3.4: 运行测试，确认通过**

```bash
cd /Users/moss/code/base/go-common && go test ./message/email/ -v
```

Expected: 所有测试 PASS（Task 1 + Task 2 + Task 3 = 14 个 registry 测试 + 原有 sender_test.go）。

- [ ] **Step 3.5: 提交**

```bash
cd /Users/moss/code/base/go-common && git add message/email/registry.go message/email/registry_test.go && git commit -m "feat(message/email): add NewAccountRegistry constructor with vendor switch"
```

---

## Task 4: go-common sms/registry.go（镜像 email）

**Files:**
- Create: `/Users/moss/code/base/go-common/message/sms/registry.go`
- Create: `/Users/moss/code/base/go-common/message/sms/registry_test.go`

- [ ] **Step 4.1: 写 sms/registry_test.go（完整覆盖）**

将以下内容写入 `message/sms/registry_test.go`：

```go
package sms

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeProvider 跟踪自己是否被调用。与 sender_test.go 的 testProvider 思路一致。
type registryFakeProvider struct {
	name      string
	err       error
	sentCount int
}

func (p *registryFakeProvider) Name() string { return p.name }
func (p *registryFakeProvider) Send(_ context.Context, _ *Message) error {
	p.sentCount++
	if p.err != nil {
		return p.err
	}
	return nil
}

func TestNewAccountRegistryFromProviders_empty(t *testing.T) {
	r := NewAccountRegistryFromProviders(nil)
	require.NotNil(t, r.DefaultSender())

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "13800138000"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result)
}

func TestNewAccountRegistryFromProviders_sortOrder(t *testing.T) {
	// 故意打乱顺序：aliyun/zzz, aliyun/aaa, twilio/aaa
	// 期望 fallback 链：aliyun/aaa → aliyun/zzz → twilio/aaa
	a := &registryFakeProvider{name: "aliyun", err: errors.New("a down")}
	z := &registryFakeProvider{name: "aliyun", err: errors.New("z down")}
	tw := &registryFakeProvider{name: "twilio"}

	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"aliyun": {"zzz": z, "aaa": a},
		"twilio": {"aaa": tw},
	})

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "13800138000"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "twilio", result.Provider)
	require.Equal(t, 3, result.Attempts)
}

func TestSenderFor_bothEmpty(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"aliyun": {"primary": &registryFakeProvider{name: "aliyun"}},
	})

	got, err := r.SenderFor("", "")
	require.NoError(t, err)
	require.Same(t, r.DefaultSender(), got)
}

func TestSenderFor_bothSet(t *testing.T) {
	target := &registryFakeProvider{name: "aliyun", err: errors.New("down")}
	other := &registryFakeProvider{name: "twilio"}

	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"aliyun": {"primary": target},
		"twilio": {"primary": other},
	})

	got, err := r.SenderFor("aliyun", "primary")
	require.NoError(t, err)

	result, err := got.Send(context.Background(), &Message{To: "13800138000"})
	require.Error(t, err)
	require.False(t, result.Success)
	require.Equal(t, 1, result.Attempts, "no fallback for specific selection")
	require.Equal(t, 1, target.sentCount)
	require.Equal(t, 0, other.sentCount)
}

func TestSenderFor_partialVendorOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"aliyun": {"primary": &registryFakeProvider{name: "aliyun"}},
	})
	_, err := r.SenderFor("aliyun", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_unknownVendor(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"aliyun": {"primary": &registryFakeProvider{name: "aliyun"}},
	})
	_, err := r.SenderFor("tencent", "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms vendor")
}

func TestSenderFor_unknownAccount(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[string]map[string]Provider{
		"aliyun": {"primary": &registryFakeProvider{name: "aliyun"}},
	})
	_, err := r.SenderFor("aliyun", "secondary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms account")
}

func TestNewAccountRegistry_emptyConfig(t *testing.T) {
	r, err := NewAccountRegistry(nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestNewAccountRegistry_aliyunSuccess(t *testing.T) {
	cfg := &Config{
		DefaultCountry: "CN",
		Vendors: map[string]VendorConfig{
			"aliyun": {Accounts: []AccountConfig{
				{Name: "primary", AccessKeyID: "xxx", AccessKeySecret: "yyy", SignName: "sign", RegionID: "cn-hangzhou"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	sender, err := r.SenderFor("aliyun", "primary")
	require.NoError(t, err)
	require.NotNil(t, sender)
}

func TestNewAccountRegistry_unknownVendor(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"tencent": {Accounts: []AccountConfig{{Name: "primary"}}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown vendor")
}

func TestNewAccountRegistry_duplicateAccountName(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"aliyun": {Accounts: []AccountConfig{
				{Name: "primary", AccessKeyID: "a", AccessKeySecret: "b", SignName: "s", RegionID: "cn-hangzhou"},
				{Name: "primary", AccessKeyID: "c", AccessKeySecret: "d", SignName: "s2", RegionID: "cn-hangzhou"},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

// TestNewAccountRegistry_defaultCountryPreserved 验证 Config.DefaultCountry 字段被保留。
// Registry 内部不读取它，但保留使 YAML 兼容。
func TestNewAccountRegistry_defaultCountryPreserved(t *testing.T) {
	cfg := &Config{
		DefaultCountry: "US",
		Vendors:        map[string]VendorConfig{},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.Equal(t, "US", cfg.DefaultCountry, "field should be preserved on the input config")
	_ = r // 不使用 registry，仅验证字段保留
}
```

- [ ] **Step 4.2: 写 sms/registry.go（完整实现）**

将以下内容写入 `message/sms/registry.go`：

```go
package sms

import (
	"fmt"
	"sort"

	aliyunprovider "github.com/servekit/go-common/message/sms/aliyun"
)

// Config 是 SMS 账号的 YAML 配置，按 vendor 分组。
//
// DefaultCountry 字段当前不被 Registry 使用，保留是为了：
//  1. 兼容现有 message-service YAML（已写 default_country: CN）
//  2. 为未来接入 Router（按手机号国家码路由）留接口
type Config struct {
	DefaultCountry string `default:"CN"` // ISO 3166-1 alpha-2，未使用
	Vendors        map[string]VendorConfig
}

// VendorConfig 持有一个 vendor（如 "aliyun"）下的所有账号。
type VendorConfig struct {
	Accounts []AccountConfig
}

// AccountConfig 是单个命名 SMS 账号。fat-struct 设计：新增 vendor 时需在此添加字段。
type AccountConfig struct {
	Name            string
	AccessKeyID     string // aliyun
	AccessKeySecret string // aliyun
	SignName        string // aliyun
	RegionID        string // aliyun
}

// AccountRegistry 按 (vendor, account) 索引 Provider，提供默认 fallback
// sender 和 per-account sender。语义与 email.AccountRegistry 对称。
type AccountRegistry struct {
	vendors map[string]map[string]Provider
	def     *Sender
}

// NewAccountRegistryFromProviders 从已构造的 provider map 构建 registry。
// 默认 fallback 链按 vendor 名升序、account 名升序排列。
func NewAccountRegistryFromProviders(vendors map[string]map[string]Provider) *AccountRegistry {
	r := &AccountRegistry{vendors: vendors}
	r.def = NewSender(flattenProviders(vendors))
	return r
}

// NewAccountRegistry 从 YAML config 构造 registry。
//
// 行为：
//   - 遍历每个 vendor，根据 vendor 名调用对应子包构造 Provider
//   - 同一 vendor 下 account 名不允许重复
//   - 未知 vendor 名、provider 构造失败均返回 error
//   - DefaultCountry 字段被保留但不读取（见 Config 注释）
func NewAccountRegistry(cfg *Config) (*AccountRegistry, error) {
	vendors := make(map[string]map[string]Provider)
	if cfg == nil {
		return NewAccountRegistryFromProviders(vendors), nil
	}

	for vendorName, vc := range cfg.Vendors {
		accounts := make(map[string]Provider)
		for _, ac := range vc.Accounts {
			if _, dup := accounts[ac.Name]; dup {
				return nil, fmt.Errorf("sms: duplicate account name %q under vendor %q", ac.Name, vendorName)
			}
			p, err := buildProvider(vendorName, ac)
			if err != nil {
				return nil, fmt.Errorf("sms: account %s/%s: %w", vendorName, ac.Name, err)
			}
			accounts[ac.Name] = p
		}
		vendors[vendorName] = accounts
	}

	return NewAccountRegistryFromProviders(vendors), nil
}

// buildProvider 根据 vendor 名分发到对应子包构造 Provider。
func buildProvider(vendor string, ac AccountConfig) (Provider, error) {
	switch vendor {
	case "aliyun":
		return aliyunprovider.NewProvider(&aliyunprovider.Config{
			AccessKeyID:     ac.AccessKeyID,
			AccessKeySecret: ac.AccessKeySecret,
			SignName:        ac.SignName,
			RegionID:        ac.RegionID,
		})
	default:
		return nil, fmt.Errorf("unknown vendor %q", vendor)
	}
}

// DefaultSender 返回包含所有 provider 的 fallback sender。
func (r *AccountRegistry) DefaultSender() *Sender {
	return r.def
}

// SenderFor 根据 vendor+account 选择 sender。语义与 email.AccountRegistry.SenderFor 对称：
//   - 都空 → DefaultSender
//   - 都设置 → 仅含该 provider 的 sender（无 fallback）
//   - 只设置一个 / 未知 → error
func (r *AccountRegistry) SenderFor(vendor, account string) (*Sender, error) {
	if vendor == "" && account == "" {
		return r.def, nil
	}
	if vendor == "" || account == "" {
		return nil, fmt.Errorf("sms: vendor and account must be set together")
	}
	accounts, ok := r.vendors[vendor]
	if !ok {
		return nil, fmt.Errorf("sms: unknown vendor %q", vendor)
	}
	p, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("sms: unknown account %q under vendor %q", account, vendor)
	}
	return NewSender([]Provider{p}), nil
}

// flattenProviders 按 vendor 名升序、account 名升序展开嵌套 map。
func flattenProviders(vendors map[string]map[string]Provider) []Provider {
	vendorNames := make([]string, 0, len(vendors))
	for v := range vendors {
		vendorNames = append(vendorNames, v)
	}
	sort.Strings(vendorNames)

	var out []Provider
	for _, v := range vendorNames {
		accounts := vendors[v]
		acctNames := make([]string, 0, len(accounts))
		for a := range accounts {
			acctNames = append(acctNames, a)
		}
		sort.Strings(acctNames)
		for _, a := range acctNames {
			out = append(out, accounts[a])
		}
	}
	return out
}
```

- [ ] **Step 4.3: 运行测试，确认通过**

```bash
cd /Users/moss/code/base/go-common && go test ./message/sms/ -v
```

Expected: 所有测试 PASS（11 个新增 registry 测试 + 原有 sender/router 测试）。

- [ ] **Step 4.4: 提交**

```bash
cd /Users/moss/code/base/go-common && git add message/sms/registry.go message/sms/registry_test.go && git commit -m "feat(message/sms): add AccountRegistry mirroring email package"
```

---

## Task 5: go-common 全量测试验证

**Files:** 无文件改动，仅验证

- [ ] **Step 5.1: 运行 go-common 全量测试 + race 检测**

```bash
cd /Users/moss/code/base/go-common && go test -race ./...
```

Expected: 所有 package 测试 PASS，无 race 警告。重点关注 `message/email/` 和 `message/sms/` 输出。

- [ ] **Step 5.2: 运行 gofmt + goimports 检查**

```bash
cd /Users/moss/code/base/go-common && gofmt -l message/email/registry.go message/email/registry_test.go message/sms/registry.go message/sms/registry_test.go
goimports -l message/email/registry.go message/email/registry_test.go message/sms/registry.go message/sms/registry_test.go
```

Expected: 两个命令均无输出（所有文件已格式化）。

- [ ] **Step 5.3: 如果 go-common 项目使用 golangci-lint，运行检查**

```bash
cd /Users/moss/code/base/go-common && golangci-lint run ./message/... 2>&1 | head -20
```

Expected: 无 lint 错误。如果有，修复后重新运行。

---

## Task 6: message-service config.go 改造

**Files:**
- Modify: `/Users/moss/code/base/message-service/pkg/config/config.go`

- [ ] **Step 6.1: 修改 config.go**

打开 `pkg/config/config.go`，做以下改动：

**1. 修改 import 块：**

原 import 块：
```go
import (
	"fmt"
	"time"

	"github.com/servekit/go-common/configx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"
)
```

改为：
```go
import (
	"fmt"
	"time"

	"github.com/servekit/go-common/configx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/logging"
	"github.com/servekit/go-common/message/email"
	"github.com/servekit/go-common/message/sms"
)
```

**2. 修改 Config struct：**

原：
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

改为：
```go
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Log        *logging.Config
	Email      *email.Config
	SMS        *sms.Config
	ThirdParty *ThirdPartyConfig
}
```

**3. 删除本地 config 类型定义：**

删除以下 6 个类型定义（从 `type EmailConfig struct` 开始到 `type SMSAccountConfig struct` 结束的整段，包括所有注释）：

```go
type EmailConfig struct { ... }
type EmailVendorConfig struct { ... }
type EmailAccountConfig struct { ... }
type SMSConfig struct { ... }
type SMSVendorConfig struct { ... }
type SMSAccountConfig struct { ... }
```

**保留** `ServerConfig` / `ThirdPartyConfig` / `GIDConfig` / `SnowflakeConfig`。

- [ ] **Step 6.2: 验证编译**

```bash
cd /Users/moss/code/base/message-service && go build ./pkg/config/
```

Expected: 编译成功。如果失败，可能其他文件还在引用旧类型 — 这是预期的，下一个 Task 会处理。

如果 `pkg/config/` 本身编译失败，先检查 import 是否正确，类型是否完整删除。

- [ ] **Step 6.3: 提交**

```bash
cd /Users/moss/code/base/message-service && git add pkg/config/config.go && git commit -m "refactor(config): use go-common message config types directly"
```

---

## Task 7: message-service service.go 改造

**Files:**
- Modify: `/Users/moss/code/base/message-service/internal/service/service.go`

- [ ] **Step 7.1: 修改 service.go 的 import 块**

删除不再需要的 import：

原 import：
```go
import (
	"context"
	"fmt"
	"sort"

	"message-service/internal/store/models"
	"message-service/internal/store/repository"
	"message-service/pkg/config"
	"message-service/pkg/option"
	"message-service/pkg/thirdcall"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	email "github.com/servekit/go-common/message/email"
	mailgunprovider "github.com/servekit/go-common/message/email/mailgun"
	smtpprovider "github.com/servekit/go-common/message/email/smtp"
	sms "github.com/servekit/go-common/message/sms"
	aliyunprovider "github.com/servekit/go-common/message/sms/aliyun"

	"gorm.io/gorm"
)
```

改为：
```go
import (
	"context"
	"fmt"

	"message-service/internal/store/models"
	"message-service/internal/store/repository"
	"message-service/pkg/config"
	"message-service/pkg/option"
	"message-service/pkg/thirdcall"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/message/email"
	"github.com/servekit/go-common/message/sms"

	"gorm.io/gorm"
)
```

（删除 `sort` 和 4 个 vendor 子包 import）

- [ ] **Step 7.2: 修改 MessageService struct 字段**

原：
```go
type MessageService struct {
	pb.UnimplementedMessageServiceServer

	db          *gorm.DB
	ownDB       bool
	repo        messageRepo
	gid         thirdcall.GIDService
	emailSender *email.Sender                        // default fallback sender (all accounts)
	smsSender   *sms.Sender                          // default fallback sender (all accounts)
	emailAccts  map[string]map[string]email.Provider // vendor → account → provider
	smsAccts    map[string]map[string]sms.Provider   // vendor → account → provider
	manager     *lifecycle.Manager
}
```

改为：
```go
type MessageService struct {
	pb.UnimplementedMessageServiceServer

	db            *gorm.DB
	ownDB         bool
	repo          messageRepo
	gid           thirdcall.GIDService
	emailRegistry *email.AccountRegistry
	smsRegistry   *sms.AccountRegistry
	manager       *lifecycle.Manager
}
```

- [ ] **Step 7.3: 修改 newWithDeps 函数**

原：
```go
func newWithDeps(cfg *config.Config, db *gorm.DB, gid thirdcall.GIDService) (*MessageService, error) {
	msgRepo := newRepository(db)

	emailAccts, err := buildEmailAccounts(cfg)
	if err != nil {
		return nil, fmt.Errorf("email accounts: %w", err)
	}

	smsAccts, err := buildSMSAccounts(cfg)
	if err != nil {
		return nil, fmt.Errorf("sms accounts: %w", err)
	}

	svc := &MessageService{
		db:         db,
		repo:       msgRepo,
		gid:        gid,
		emailAccts: emailAccts,
		smsAccts:   smsAccts,
	}

	svc.emailSender = email.NewSender(flattenEmail(emailAccts))
	svc.smsSender = sms.NewSender(flattenSMS(smsAccts))

	return svc, nil
}
```

改为：
```go
func newWithDeps(cfg *config.Config, db *gorm.DB, gid thirdcall.GIDService) (*MessageService, error) {
	msgRepo := newRepository(db)

	emailRegistry, err := email.NewAccountRegistry(cfg.Email)
	if err != nil {
		return nil, fmt.Errorf("email registry: %w", err)
	}

	smsRegistry, err := sms.NewAccountRegistry(cfg.SMS)
	if err != nil {
		return nil, fmt.Errorf("sms registry: %w", err)
	}

	return &MessageService{
		db:            db,
		repo:          msgRepo,
		gid:           gid,
		emailRegistry: emailRegistry,
		smsRegistry:   smsRegistry,
	}, nil
}
```

- [ ] **Step 7.4: 删除所有 build/flatten/new 函数**

删除以下 6 个函数的完整定义（包括注释）：

- `buildEmailAccounts`
- `buildSMSAccounts`
- `newEmailProvider`
- `newSMSProvider`
- `flattenEmail`
- `flattenSMS`

这些函数原本在 `newRepository` 函数定义之后、`toProtoRecord` 函数定义之前。

- [ ] **Step 7.5: 验证编译（service 包可能仍报错，但 import 应正确）**

```bash
cd /Users/moss/code/base/message-service && go build ./internal/service/ 2>&1 | head -30
```

Expected: 可能仍有编译错误（来自 send.go 还没改），但不应有 "undefined: buildEmailAccounts" 之类已删除函数的引用。如果还有，说明删除不彻底。

- [ ] **Step 7.6: 提交**

```bash
cd /Users/moss/code/base/message-service && git add internal/service/service.go && git commit -m "refactor(service): use email/sms AccountRegistry from go-common"
```

---

## Task 8: message-service send.go 改造

**Files:**
- Modify: `/Users/moss/code/base/message-service/internal/service/send.go`

- [ ] **Step 8.1: 修改 send.go 的 import 块**

原 import：
```go
import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	pb "message-service/gen/message/v1"

	email "github.com/servekit/go-common/message/email"
	sms "github.com/servekit/go-common/message/sms"
)
```

改为（删除 `fmt` 和 vendor 包 import，因为不再需要构造 sender）：
```go
import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	pb "message-service/gen/message/v1"
)
```

注意：删除 `email` 和 `sms` 的 import 是因为 `sendEmail` / `sendSMS` 函数本身不再直接引用这些包（types 通过 `*pb.SendEmailRequest` 等暴露）。如果 `email.Message` / `sms.Message` 还在使用（用于构造发送消息），则保留这些 import。**先尝试删除，编译失败再加回。**

- [ ] **Step 8.2: 修改 sendEmail 函数的开头**

原：
```go
func (s *MessageService) sendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	sender, err := s.selectEmailSender(req.GetVendor(), req.GetAccount())
	if err != nil {
		return nil, err
	}
```

改为：
```go
func (s *MessageService) sendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	sender, err := s.emailRegistry.SenderFor(req.GetVendor(), req.GetAccount())
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
```

- [ ] **Step 8.3: 修改 sendSMS 函数的开头**

原：
```go
func (s *MessageService) sendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	sender, err := s.selectSMSSender(req.GetVendor(), req.GetAccount())
	if err != nil {
		return nil, err
	}
```

改为：
```go
func (s *MessageService) sendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	sender, err := s.smsRegistry.SenderFor(req.GetVendor(), req.GetAccount())
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
```

- [ ] **Step 8.4: 删除 selectEmailSender 和 selectSMSSender 函数**

删除以下两个函数的完整定义（包括注释）：

```go
// selectEmailSender picks an email.Sender for the request. ...
func (s *MessageService) selectEmailSender(vendor, account string) (*email.Sender, error) { ... }

// selectSMSSender is the SMS counterpart of selectEmailSender.
func (s *MessageService) selectSMSSender(vendor, account string) (*sms.Sender, error) { ... }
```

注意：`email.NewSender([]email.Provider{p})` 的调用随这两个函数一起删除，所以 `email` import 可能可以删除。但 `email.Message` 在 sendEmail 中仍被使用（构造 msg），所以保留 import。

- [ ] **Step 8.5: 验证编译**

```bash
cd /Users/moss/code/base/message-service && go build ./...
```

Expected: 编译成功（除了测试文件，下一步处理）。

如果失败：
- `undefined: email.Sender` 之类 → import 没保留
- `cannot use s.emailRegistry (type *email.AccountRegistry) as type ...` → 类型不匹配，检查 Task 7 改动

- [ ] **Step 8.6: 提交**

```bash
cd /Users/moss/code/base/message-service && git add internal/service/send.go && git commit -m "refactor(service): use registry.SenderFor replacing local select functions"
```

---

## Task 9: message-service service_test.go 改造

**Files:**
- Modify: `/Users/moss/code/base/message-service/internal/service/service_test.go`

**背景：** 现有测试通过 `newTestEmailService(t, repo, providers)` 接受一个 `[]email.Provider`，内部把 `providers[0]` 放进单 vendor 单 account 的 map，把全部 providers 喂给 `email.NewSender` 当默认 fallback。重构后，registry 的默认 fallback 链由 map 决定，所以这个 helper 必须改造。

策略：把 helper 的签名改为直接接收 registry，调用方负责构造。这样最直接、最少歧义。

- [ ] **Step 9.1: 改造 newTestEmailService helper**

原（约第 121-130 行）：
```go
func newTestEmailService(t *testing.T, repo *mockRepo, providers []email.Provider) *MessageService {
	t.Helper()
	svc := &MessageService{
		repo:       repo,
		gid:        getTestGID(t),
		manager:    lifecycle.NewManager(),
		emailAccts: map[string]map[string]email.Provider{"mock": {"default": providers[0]}},
	}
	all := append([]email.Provider(nil), providers...)
	svc.emailSender = email.NewSender(all)
	return svc
}
```

改为：
```go
// newTestEmailService 构造一个使用 fake registry 的 MessageService。
// providers 是按 fallback 顺序的 provider 列表，会被包装成单 vendor "mock"
// 下名为 "p0", "p1", ... 的多个 account。
func newTestEmailService(t *testing.T, repo *mockRepo, providers []email.Provider) *MessageService {
	t.Helper()
	accounts := make(map[string]email.Provider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	return &MessageService{
		repo:          repo,
		gid:           getTestGID(t),
		manager:       lifecycle.NewManager(),
		emailRegistry: email.NewAccountRegistryFromProviders(map[string]map[string]email.Provider{
			"mock": accounts,
		}),
	}
}
```

注意：如果 `fmt` 还没在 service_test.go 中 import，添加到 import 块。

- [ ] **Step 9.2: 改造 newTestSMSService helper（对称改）**

原（约第 132-141 行）：
```go
func newTestSMSService(t *testing.T, repo *mockRepo, providers []sms.Provider) *MessageService {
	t.Helper()
	svc := &MessageService{
		repo:     repo,
		gid:      getTestGID(t),
		manager:  lifecycle.NewManager(),
		smsAccts: map[string]map[string]sms.Provider{"mock": {"default": providers[0]}},
	}
	all := append([]sms.Provider(nil), providers...)
	svc.smsSender = sms.NewSender(all)
	return svc
}
```

改为：
```go
func newTestSMSService(t *testing.T, repo *mockRepo, providers []sms.Provider) *MessageService {
	t.Helper()
	accounts := make(map[string]sms.Provider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	return &MessageService{
		repo:        repo,
		gid:         getTestGID(t),
		manager:     lifecycle.NewManager(),
		smsRegistry: sms.NewAccountRegistryFromProviders(map[string]map[string]sms.Provider{
			"mock": accounts,
		}),
	}
}
```

- [ ] **Step 9.3: 改造 TestSendEmail_SelectAccount 测试**

原（约第 274-310 行）：
```go
func TestSendEmail_SelectAccount(t *testing.T) {
	repo := newMockRepo()
	primary := &mockEmailProvider{name: "smtp"}
	secondary := &mockEmailProvider{name: "mailgun"}
	svc := &MessageService{
		repo: repo,
		gid:  getTestGID(t),
		emailAccts: map[string]map[string]email.Provider{
			"smtp":    {"A": primary},
			"mailgun": {"g": secondary},
		},
	}
	svc.emailSender = email.NewSender([]email.Provider{primary, secondary})

	// Select smtp/A — should report PROVIDER_SMTP.
	resp, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:      "user@example.com",
		Subject: "Test",
		Body:    "Body",
		Vendor:  "smtp",
		Account: "A",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.Provider_PROVIDER_SMTP, resp.Provider)

	// Select mailgun/g — should report PROVIDER_MAILGUN.
	resp, err = svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:      "user@example.com",
		Subject: "Test",
		Body:    "Body",
		Vendor:  "mailgun",
		Account: "g",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.Provider_PROVIDER_MAILGUN, resp.Provider)
}
```

改为：
```go
func TestSendEmail_SelectAccount(t *testing.T) {
	repo := newMockRepo()
	primary := &mockEmailProvider{name: "smtp"}
	secondary := &mockEmailProvider{name: "mailgun"}
	svc := &MessageService{
		repo: repo,
		gid:  getTestGID(t),
		emailRegistry: email.NewAccountRegistryFromProviders(map[string]map[string]email.Provider{
			"smtp":    {"A": primary},
			"mailgun": {"g": secondary},
		}),
	}

	// Select smtp/A — should report PROVIDER_SMTP.
	resp, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:      "user@example.com",
		Subject: "Test",
		Body:    "Body",
		Vendor:  "smtp",
		Account: "A",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.Provider_PROVIDER_SMTP, resp.Provider)

	// Select mailgun/g — should report PROVIDER_MAILGUN.
	resp, err = svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:      "user@example.com",
		Subject: "Test",
		Body:    "Body",
		Vendor:  "mailgun",
		Account: "g",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.Provider_PROVIDER_MAILGUN, resp.Provider)
}
```

- [ ] **Step 9.4: 验证 SelectAccount_UnknownVendor 和 PartialSpec 测试**

这两个测试当前使用 `newTestEmailService(t, repo, []email.Provider{...})`，传入单 provider。改 helper 后这些测试不需要修改调用方式（因为 helper 内部处理了 providers→registry 的转换），但是错误断言可能需要更新。

`TestSendEmail_SelectAccount_UnknownVendor` 当前断言 `require.Error(t, err)` 即可。在新实现下，`registry.SenderFor("tencent", "A")` 会返回 `fmt.Errorf("email: unknown vendor %q", "tencent")`，service 包装为 `xcodes.ErrBadRequest`。错误仍然是 error，断言通过。

`TestSendEmail_SelectAccount_PartialSpec` 当前测试只填 vendor 不填 account。proto 层的 CEL 验证（`vendor_account_pair`）应该先拦截，返回 validation error。如果通过 proto 验证进到 service，`SenderFor("smtp", "")` 会返回 "must be set together" 错误。两种都是 error，断言通过。

确认这两个测试无需改动，直接运行即可。

- [ ] **Step 9.5: 运行测试**

```bash
cd /Users/moss/code/base/message-service && go test ./internal/service/ -v -run "TestSendEmail|TestSendSMS|TestQuery" 2>&1 | tail -80
```

Expected: 所有测试 PASS。

常见问题排查：
- `undefined: svc.emailSender` → 漏改某处构造（grep `emailSender` 找到所有引用）
- `cannot use primary (type *mockEmailProvider) as type email.Provider` → mockEmailProvider 接口实现问题，但既然原来能编译，重构后应该也行；检查是否引入了新的接口要求
- `SenderFor returned unexpected error` → vendor/account 名字大小写或映射问题
- `provider mismatch` → registry 默认链顺序与原 `email.NewSender(all)` 不一致；检查 helper 中 providers 是否按 fallback 顺序注册

- [ ] **Step 9.6: 提交**

```bash
cd /Users/moss/code/base/message-service && git add internal/service/service_test.go && git commit -m "test(service): adapt to AccountRegistry-based sender construction"
```

---

## Task 10: 最终验证

**Files:** 无文件改动

- [ ] **Step 10.1: 运行 message-service 全量测试 + race**

```bash
cd /Users/moss/code/base/message-service && go test -race -coverprofile=coverage.out ./...
```

Expected: 所有测试 PASS，无 race 警告。检查覆盖率没有显著下降（与重构前对比）。

- [ ] **Step 10.2: 运行 gofmt + goimports**

```bash
cd /Users/moss/code/base/message-service && gofmt -l pkg/config/config.go internal/service/service.go internal/service/send.go internal/service/service_test.go
goimports -l pkg/config/config.go internal/service/service.go internal/service/send.go internal/service/service_test.go
```

Expected: 两个命令均无输出。

- [ ] **Step 10.3: 运行 golangci-lint**

```bash
cd /Users/moss/code/base/message-service && golangci-lint run ./...
```

Expected: 无 lint 错误。

- [ ] **Step 10.4: 用 config.yaml 启动 server，做 smoke test**

```bash
cd /Users/moss/code/base/message-service && go run ./cmd/server/ &
SERVER_PID=$!
sleep 2

# 用 grpcurl 测试 SendEmail 走默认 sender（vendor+account 都空）
grpcurl -plaintext -d '{
  "to": "test@example.com",
  "subject": "smoke test",
  "body": "hello"
}' localhost:9000 message.v1.MessageService/SendEmail

# 测试未知 vendor 报错
grpcurl -plaintext -d '{
  "to": "test@example.com",
  "subject": "should fail",
  "body": "hello",
  "vendor": "unknown",
  "account": "x"
}' localhost:9000 message.v1.MessageService/SendEmail

kill $SERVER_PID
```

Expected:
- 第一次调用：要么成功（如果 SMTP 真的能发），要么返回 `ErrMessageSendFailed`（SMTP 连接失败，但 registry 构造本身成功）
- 第二次调用：返回 `ErrBadRequest`，错误消息含 "unknown email vendor"

如果 server 启动失败（registry 构造失败），检查 `config.yaml` 是否符合新结构（应该完全不变）。

- [ ] **Step 10.5: 验证 git diff，确认改动范围**

```bash
cd /Users/moss/code/base/message-service && git diff main --stat
```

Expected: 改动文件限于：
- `pkg/config/config.go`（删除 6 个类型 + 改 2 个字段类型）
- `internal/service/service.go`（删 build/flatten/new + 改 struct 字段）
- `internal/service/send.go`（删 select + 改 sendEmail/sendSMS 开头）
- `internal/service/service_test.go`（改测试构造方式）

go-common 仓库（独立 git）：
- `message/email/registry.go`（新增）
- `message/email/registry_test.go`（新增）
- `message/sms/registry.go`（新增）
- `message/sms/registry_test.go`（新增）

- [ ] **Step 10.6: 同步设计文档到 Obsidian（如已更新 spec 或新增 plan）**

如果 spec 文档有变化（本计划没改 spec，所以通常不需要），运行：

```bash
# 将 plan 同步到 Obsidian
cp /Users/moss/code/base/message-service/docs/superpowers/plans/2026-06-15-account-registry-extract-plan.md \
   "$HOME/Library/Mobile Documents/iCloud~md~obsidian/Documents/only/services/message-service/plan/v1/1-account-registry-extract.md"
```

并在 Obsidian 中更新 `services/message-service/index.md` 和 `changes.md`。

---

## Spec Coverage 自检

| Spec 要求 | 对应 Task |
|---|---|
| `email.Config` / `VendorConfig` / `AccountConfig` 类型 | Task 1 |
| `email.AccountRegistry` struct | Task 1 |
| `email.NewAccountRegistry` 构造函数 | Task 3 |
| `email.NewAccountRegistryFromProviders`（测试用） | Task 1 |
| `email.DefaultSender` 方法 | Task 1 |
| `email.SenderFor` 5 个分支 | Task 2 |
| vendor switch（smtp / mailgun）| Task 3 |
| 重复 account 名报错 | Task 3 测试覆盖 |
| 未知 vendor 报错 | Task 3 测试覆盖 |
| provider 构造失败报错 | Task 3 测试覆盖（smtp port=0）|
| `sms.Config` 多 `DefaultCountry` 字段 | Task 4 |
| SMS 全部对称 | Task 4 |
| message-service 删除本地 config 类型 | Task 6 |
| message-service 删除 build/flatten/new | Task 7 |
| message-service 删除 select | Task 8 |
| message-service 测试改造 | Task 9 |
| go-common 全量测试通过 | Task 5 |
| message-service 全量测试 + race + lint | Task 10 |
| YAML config 兼容（不改 config.yaml）| Task 10 smoke test 验证 |
| proto 不变 | Task 10 验证（不修改 proto） |
| 错误码不变（仍是 ErrBadRequest）| Task 8 测试覆盖 |

所有 spec 要求均有 task 对应。
