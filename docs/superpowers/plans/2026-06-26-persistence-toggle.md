# 持久化开关实施计划（persistence.email/sms.enabled）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 message-service 增加 per-channel 持久化开关（email / sms 各自一个），让调用方可选关闭 send-record 落库行为。关闭后发送路径跳过幂等检查与 DB 写，查询路径返回 `ErrPersistenceDisabled`。默认开启，完全向后兼容。

**Architecture:** yaml 加顶层 `persistence` 段（`*bool` 区分"未设置"与"false"，所有 nil 层默认 true）；option 包加 `WithEmailPersistence` / `WithSMSPersistence` 用于 module 模式覆盖；`message.Service` 加 `persistEmailEnabled` / `persistSMSEnabled` 两个 bool 字段，由 `service.New` 解析 yaml + option 后注入；发送方法在两处（幂等检查、DB 写）加 gate，查询方法在入口加 gate 返回新错误码 `ErrPersistenceDisabled`。

**Tech Stack:** Go 1.22+、PostgreSQL（`dbx.SetupTestDB` testcontainer）、go-common/xerr、viper/configx、GORM。

**Spec:** `docs/superpowers/specs/2026-06-26-persistence-toggle-design.md`

---

## File Structure

| 文件 | 角色 | 改动 |
|------|------|------|
| `pkg/xcodes/message.go` | 错误码 | 新增 `ErrPersistenceDisabled` |
| `pkg/config/config.go` | 配置类型 | `Config` 加 `Persistence` 字段；新增 `PersistenceConfig` / `ChannelToggle` + `EmailEnabled()` / `SMSEnabled()` nil-safe 方法 |
| `pkg/config/config_test.go` | 配置测试 | 加 nil-safe 方法测试 + yaml 省略段测试 |
| `pkg/option/option.go` | functional option | 加 `EmailPersistence` / `SMSPersistence` 字段 + `WithEmailPersistence` / `WithSMSPersistence` |
| `internal/service/message/service.go` | message subpackage 入口 | 加 `PersistenceConfig` struct + `Service` 两个 bool 字段 + `New` 签名加参数 |
| `internal/service/message/email.go` | Email service | `SendEmail` 加 enabled gate（幂等 + 写库两处）；查询方法加 disabled gate |
| `internal/service/message/email_test.go` | Email service 测试 | 加 helper `newTestEmailServiceNoPersist` + 5 个 persistence 测试 |
| `internal/service/message/sms.go` | SMS service | 与 Email 对称 |
| `internal/service/message/sms_test.go` | SMS service 测试 | 与 Email 对称 |
| `internal/service/service.go` | service root | `New` 解析 effective persistence 值（yaml + option override），传给 `message.New` |
| `config.yaml` | 部署配置 | 加 `persistence` 段（注释默认 true） |

---

## Phase 1: 基础类型（无破坏性变更）

### Task 1: 新增错误码 `ErrPersistenceDisabled`

**Files:**
- Modify: `pkg/xcodes/message.go`

- [ ] **Step 1: 在 `pkg/xcodes/message.go` 末尾追加新错误码**

打开 `pkg/xcodes/message.go`，在文件末尾（`ErrMessageSendFailed` 定义之后）追加：

```go
// ErrPersistenceDisabled indicates the caller invoked a query method on a
// channel whose persistence has been disabled in config. The send path still
// works (vendor call only); only Get/List/Stats and idempotency check are
// unavailable.
var ErrPersistenceDisabled = xerr.New(
	"PERSISTENCE_DISABLED",
	xerr.CategoryServiceUnavailable,
	503,
	"persistence is disabled for this channel",
)
```

- [ ] **Step 2: 编译验证**

Run: `go build ./pkg/xcodes/...`
Expected: 无输出（成功）。

- [ ] **Step 3: Commit**

```bash
git add pkg/xcodes/message.go
git commit -m "feat(xcodes): add ErrPersistenceDisabled"
```

---

### Task 2: config 包加 `PersistenceConfig` + nil-safe 方法 + 测试

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [ ] **Step 1: 先写测试 — nil-safe 方法默认 true**

在 `pkg/config/config_test.go` 末尾追加：

```go
// TestPersistenceConfig_DefaultTrue verifies all nil layers resolve to the
// default true (backwards-compatible).
func TestPersistenceConfig_DefaultTrue(t *testing.T) {
	var nilPtr *PersistenceConfig
	require.True(t, nilPtr.EmailEnabled(), "nil *PersistenceConfig.EmailEnabled() must be true")
	require.True(t, nilPtr.SMSEnabled(), "nil *PersistenceConfig.SMSEnabled() must be true")

	var empty PersistenceConfig
	require.True(t, empty.EmailEnabled(), "zero-value PersistenceConfig.EmailEnabled() must be true")
	require.True(t, empty.SMSEnabled(), "zero-value PersistenceConfig.SMSEnabled() must be true")

	// Email non-nil but Enabled nil.
	p := &PersistenceConfig{Email: &ChannelToggle{}}
	require.True(t, p.EmailEnabled(), "nil Enabled must default to true")

	// Email false explicitly.
	falseVal := false
	p2 := &PersistenceConfig{Email: &ChannelToggle{Enabled: &falseVal}}
	require.False(t, p2.EmailEnabled(), "explicit false must be honored")

	// True explicit.
	trueVal := true
	p3 := &PersistenceConfig{SMS: &ChannelToggle{Enabled: &trueVal}}
	require.True(t, p3.SMSEnabled(), "explicit true must be honored")
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./pkg/config/ -run TestPersistenceConfig_DefaultTrue -v`
Expected: 编译失败 — `PersistenceConfig` / `ChannelToggle` 未定义。

- [ ] **Step 3: 实现 — 在 `pkg/config/config.go` 加新类型**

打开 `pkg/config/config.go`。

先在 `Config` struct 中追加字段（紧跟 `ThirdParty *ThirdPartyConfig` 之后）：

```go
type Config struct {
	Server     *ServerConfig
	Database   *dbx.Config
	Log        *logging.Config
	Email      *EmailConfig
	SMS        *SMSConfig
	Cron       *cronx.Config
	ThirdParty *ThirdPartyConfig
	Persistence *PersistenceConfig
}
```

然后在 `ThirdPartyConfig` 定义之前（或文件中其他 config 类型附近）追加：

```go
// PersistenceConfig controls whether send records are persisted per channel.
// All-nil → defaults to enabled (preserves existing behavior).
type PersistenceConfig struct {
	Email *ChannelToggle
	SMS   *ChannelToggle
}

// ChannelToggle wraps the *bool toggle. The pointer distinguishes "field
// omitted in yaml" from "explicitly false".
type ChannelToggle struct {
	Enabled *bool
}

// EmailEnabled resolves all nil layers (PersistenceConfig itself, Email,
// Enabled) to the default true. Safe on nil receiver.
func (p *PersistenceConfig) EmailEnabled() bool {
	if p == nil || p.Email == nil || p.Email.Enabled == nil {
		return true
	}
	return *p.Email.Enabled
}

// SMSEnabled mirrors EmailEnabled for the SMS channel.
func (p *PersistenceConfig) SMSEnabled() bool {
	if p == nil || p.SMS == nil || p.SMS.Enabled == nil {
		return true
	}
	return *p.SMS.Enabled
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./pkg/config/ -run TestPersistenceConfig_DefaultTrue -v`
Expected: PASS。

- [ ] **Step 5: 再写测试 — Load 时省略 persistence 段也默认 true**

在 `pkg/config/config_test.go` 末尾追加：

```go
// TestLoad_PersistenceOmitted_DefaultsTrue verifies that a yaml without the
// persistence section loads with both channels enabled.
func TestLoad_PersistenceOmitted_DefaultsTrue(t *testing.T) {
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
	require.True(t, cfg.Persistence.EmailEnabled(), "omitted persistence.email must default to true")
	require.True(t, cfg.Persistence.SMSEnabled(), "omitted persistence.sms must default to true")
}

// TestLoad_PersistenceExplicitFalse verifies that explicit false is loaded.
func TestLoad_PersistenceExplicitFalse(t *testing.T) {
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
third_party:
  gid:
    mode: module
    snowflake:
      machine_id: 1
      start_time: "2026-06-01T00:00:00Z"
persistence:
  email:
    enabled: false
  sms:
    enabled: true
`)
	chdir(t, dir)

	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Persistence.EmailEnabled(), "explicit email.enabled=false must be honored")
	require.True(t, cfg.Persistence.SMSEnabled(), "explicit sms.enabled=true must be honored")
}
```

- [ ] **Step 6: 运行新测试验证通过**

Run: `go test ./pkg/config/ -run 'TestLoad_Persistence' -v`
Expected: 两个用例都 PASS。

- [ ] **Step 7: 跑整个 config 包测试做回归**

Run: `go test ./pkg/config/ -v`
Expected: 所有现有用例 + 3 个新用例全部 PASS。

- [ ] **Step 8: Commit**

```bash
git add pkg/config/config.go pkg/config/config_test.go
git commit -m "feat(config): add PersistenceConfig with nil-safe defaults"
```

---

### Task 3: option 包加 `WithEmailPersistence` / `WithSMSPersistence`

**Files:**
- Modify: `pkg/option/option.go`

- [ ] **Step 1: 在 `pkg/option/option.go` 的 `Options` struct 加两个字段**

修改 `Options` struct 为：

```go
// Options holds resolved option values.
type Options struct {
	DB         *gorm.DB
	GIDService thirdcall.GIDService

	EmailPersistence *bool // nil = use yaml/default
	SMSPersistence   *bool
}
```

- [ ] **Step 2: 在 `WithGIDService` 之后追加两个 option 函数**

在文件末尾的 `Apply` 函数之前追加：

```go
// WithEmailPersistence overrides the email persistence toggle. When set,
// takes precedence over yaml config. Use to disable persistence from code:
//
//	messageservice.NewModule(cfg, option.WithEmailPersistence(false))
func WithEmailPersistence(enabled bool) Option {
	return func(o *Options) { o.EmailPersistence = &enabled }
}

// WithSMSPersistence mirrors WithEmailPersistence for the SMS channel.
func WithSMSPersistence(enabled bool) Option {
	return func(o *Options) { o.SMSPersistence = &enabled }
}
```

- [ ] **Step 3: 编译验证**

Run: `go build ./pkg/option/...`
Expected: 无输出（成功）。

- [ ] **Step 4: Commit**

```bash
git add pkg/option/option.go
git commit -m "feat(option): add WithEmailPersistence / WithSMSPersistence"
```

---

## Phase 2: message.Service 接入开关字段（一次性改完所有 caller，build 不断）

### Task 4: message.Service 加 PersistenceConfig + 字段，同步更新所有 caller

**Files:**
- Modify: `internal/service/message/service.go`
- Modify: `internal/service/service.go`（仅 `New` 内对 `message.New` 的调用，先传固定 true，下一 Task 再做 effective 解析）
- Modify: `internal/service/message/email_test.go`（helper）
- Modify: `internal/service/message/sms_test.go`（helper）

- [ ] **Step 1: 修改 `internal/service/message/service.go`**

替换 `Service` struct 和 `New` 函数：

```go
// PersistenceConfig is the resolved (yaml + option) form consumed by the
// message subpackage. Plain bools — nil-handling is done by the caller
// (config.PersistenceConfig.EmailEnabled / option.Options resolution).
type PersistenceConfig struct {
	Email bool
	SMS   bool
}

// Service is the message domain service. Resources are injected at
// construction; the subpackage does not manage their lifecycle.
type Service struct {
	db            *gorm.DB
	gid           thirdcall.GIDService
	emailRegistry *email.AccountRegistry
	smsRegistry   *sms.AccountRegistry
	smsRouter     *sms.Router // nil when no routes configured

	persistEmailEnabled bool
	persistSMSEnabled   bool
}

// New constructs a message domain service with injected resources.
func New(
	db *gorm.DB,
	gid thirdcall.GIDService,
	emailRegistry *email.AccountRegistry,
	smsRegistry *sms.AccountRegistry,
	smsRouter *sms.Router,
	persistence PersistenceConfig,
) *Service {
	return &Service{
		db:                  db,
		gid:                 gid,
		emailRegistry:       emailRegistry,
		smsRegistry:         smsRegistry,
		smsRouter:           smsRouter,
		persistEmailEnabled: persistence.Email,
		persistSMSEnabled:   persistence.SMS,
	}
}
```

- [ ] **Step 2: 更新 `internal/service/service.go` 中 `message.New` 的调用**

定位到 `service.New` 函数里的 `svc := &Service{ ... }` 之前的 `message.New(...)` 调用，改为：

```go
svc := &Service{
	cfg:     cfg,
	mgr:     mgr,
	db:      db,
	gid:     gid,
	message: message.New(db, gid, emailRegistry, smsRegistry, smsRouter, message.PersistenceConfig{
		Email: true, // Task 5 will replace with cfg+option resolved values
		SMS:   true,
	}),
}
```

- [ ] **Step 3: 更新 `internal/service/message/email_test.go` 中的 helper**

定位到 `newTestEmailService` 函数（约第 67-81 行），把 `New(...)` 调用改为追加 `PersistenceConfig` 参数：

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
		getTestGID(t),
		email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: accounts}),
		nil, // smsRegistry
		nil, // smsRouter
		PersistenceConfig{Email: true, SMS: true},
	)
}
```

- [ ] **Step 4: 更新 `internal/service/message/sms_test.go` 中的 helper**

打开 `internal/service/message/sms_test.go`。定位到 `newTestSMSServiceWithRouter` 函数（约第 44-67 行），把末尾的 `New(...)` 调用追加 `PersistenceConfig` 参数：

```go
return New(db, getTestGID(t), nil, registry, router, PersistenceConfig{Email: true, SMS: true})
```

- [ ] **Step 5: 编译验证整个项目**

Run: `go build ./...`
Expected: 无输出（成功）。如果有遗漏的 caller，编译错误会指出来，按相同方式追加参数即可。

- [ ] **Step 6: 跑现有测试做回归（确保行为未变）**

Run: `go test ./internal/service/message/...`
Expected: 所有现有用例 PASS（暂时跳过 testcontainer 相关用例如果本地没启 docker，用 `-short` 也行）。

Run: `go test ./...`
Expected: PASS（这一步只是确认现有行为未受影响，新行为还未加入）。

- [ ] **Step 7: Commit**

```bash
git add internal/service/message/service.go internal/service/service.go \
        internal/service/message/email_test.go internal/service/message/sms_test.go
git commit -m "refactor(message): add PersistenceConfig to Service + update callers"
```

---

## Phase 3: 解析 effective 值 + 业务行为开关

### Task 5: service.New 解析 effective persistence 值（yaml + option override）

**Files:**
- Modify: `internal/service/service.go`

- [ ] **Step 1: 修改 `service.New` 中传给 message.New 的 PersistenceConfig**

定位到 Task 4 Step 2 修改的位置，把硬编码的 `Email: true, SMS: true` 替换为解析逻辑：

```go
emailEnabled := cfg.Persistence.EmailEnabled()
if o.EmailPersistence != nil {
	emailEnabled = *o.EmailPersistence
}
smsEnabled := cfg.Persistence.SMSEnabled()
if o.SMSPersistence != nil {
	smsEnabled = *o.SMSPersistence
}

svc := &Service{
	cfg:     cfg,
	mgr:     mgr,
	db:      db,
	gid:     gid,
	message: message.New(db, gid, emailRegistry, smsRegistry, smsRouter, message.PersistenceConfig{
		Email: emailEnabled,
		SMS:   smsEnabled,
	}),
}
```

- [ ] **Step 2: 编译验证**

Run: `go build ./...`
Expected: 无输出（成功）。

- [ ] **Step 3: Commit**

```bash
git add internal/service/service.go
git commit -m "feat(service): resolve effective persistence from yaml+option"
```

---

### Task 6: SendEmail 加 enabled gate（幂等检查 + DB 写两处）

**Files:**
- Modify: `internal/service/message/email.go`
- Modify: `internal/service/message/email_test.go`

- [ ] **Step 1: 先写测试 — 关闭持久化时跳过 DB 写**

在 `internal/service/message/email_test.go` 中，先在文件顶部 helper 区追加一个 no-persist helper：

```go
func newTestEmailServiceNoPersist(t *testing.T, providers []email.AccountProvider) *Service {
	t.Helper()
	db := setupEmailTestDB(t)
	accounts := make(map[string]email.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	return New(
		db,
		getTestGID(t),
		email.NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]email.AccountProvider{pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP: accounts}),
		nil, nil,
		PersistenceConfig{Email: false, SMS: false},
	)
}
```

然后在文件末尾追加测试：

```go
func TestSendEmail_PersistenceDisabled_SkipsDB(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})

	resp, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       "user@example.com",
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)

	// DB must be empty: persistence disabled.
	_, err = svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: resp.Id})
	require.Error(t, err, "GetEmail must fail (persistence disabled returns ErrPersistenceDisabled)")
}

func TestSendEmail_PersistenceDisabled_IdempotencyNoOp(t *testing.T) {
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

	// Without DB-backed idempotency, both calls hit the provider.
	assert.Equal(t, 2, provider.calls, "provider must be called twice when persistence disabled")
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/service/message/ -run 'TestSendEmail_PersistenceDisabled' -v`
Expected: FAIL — `provider.calls` == 1（当前代码仍走幂等检查并去重）。

- [ ] **Step 3: 实现 — SendEmail 加 enabled gate**

打开 `internal/service/message/email.go`。定位到 `SendEmail` 方法。

将原有幂等检查块：

```go
// Idempotency check.
if key := req.GetIdempotencyKey(); key != "" {
	existing, err := dal.GetEmailRecordByIdempotencyKey(ctx, s.db, req.GetSenderId(), key)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	if existing != nil {
		return respondIdempotentEmail(existing)
	}
}
```

改为：

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

将原有持久化块：

```go
// result is guaranteed non-nil from here. Persist with an independent
// context so request cancellation does not lose the record.
persistCtx, cancel := context.WithTimeout(context.Background(), persistTimeout)
defer cancel()
s.persistEmailRecord(persistCtx, id, req, result)
```

改为：

```go
// result is guaranteed non-nil from here. Persist with an independent
// context so request cancellation does not lose the record. Skipped when
// persistence disabled — caller opted out of DB writes.
if s.persistEmailEnabled {
	persistCtx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	s.persistEmailRecord(persistCtx, id, req, result)
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./internal/service/message/ -run 'TestSendEmail_PersistenceDisabled' -v`
Expected: 两个用例 PASS。

- [ ] **Step 5: 跑所有现有 email 测试做回归（开启持久化时行为不变）**

Run: `go test ./internal/service/message/ -run TestSendEmail -v`
Expected: 全部 PASS（既有 + 新增）。

- [ ] **Step 6: Commit**

```bash
git add internal/service/message/email.go internal/service/message/email_test.go
git commit -m "feat(message/email): gate SendEmail idempotency+persist by toggle"
```

---

### Task 7: SendSMS 加 enabled gate

**Files:**
- Modify: `internal/service/message/sms.go`
- Modify: `internal/service/message/sms_test.go`

- [ ] **Step 1: 在 `internal/service/message/sms_test.go` 追加 no-persist helper 与测试**

先在 helper 区（`newTestSMSServiceWithRouter` 之后）追加 no-persist 变体：

```go
// newTestSMSServiceNoPersist mirrors newTestSMSServiceWithRouter but with
// persistence disabled for both channels.
func newTestSMSServiceNoPersist(t *testing.T, providers []sms.AccountProvider) *Service {
	t.Helper()
	db := setupSMSTestDB(t)
	accounts := make(map[string]sms.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = p
	}
	registry := sms.NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]sms.AccountProvider{pb.SmsVendor_SMS_VENDOR_ALIYUN: accounts})

	router, err := sms.BuildRouter(&sms.Config{
		DefaultCountry: "CN",
		Routes: []*sms.RouteConfig{{
			Country: "*",
			Targets: []*sms.RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "p0"}},
		}},
	}, registry)
	require.NoError(t, err)
	require.NotNil(t, router)

	return New(db, getTestGID(t), nil, registry, router, PersistenceConfig{Email: false, SMS: false})
}
```

> `mockSMSProvider`（现有）`Vendor()` 硬编码返回 `SMS_VENDOR_ALIYUN`，`Account()` 返回 `m.name`，没有可配置字段。下面测试中 `Account` 用 mock 实际暴露的 name `"mock"`（路由 target 用 `"p0"`，所以发送走 router 路径，account 字段在请求里可省略）。

在文件末尾追加测试：

```go
func TestSendSMS_PersistenceDisabled_SkipsDB(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})

	resp, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		To:       "+8613800001111",
		Content:  "code",
		Scene:    pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		// No vendor/account — router picks aliyun/p0.
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)

	_, err = svc.GetSMS(context.Background(), &pb.GetSMSRequest{Id: resp.Id})
	require.Error(t, err, "GetSMS must fail when persistence disabled")
}

func TestSendSMS_PersistenceDisabled_IdempotencyNoOp(t *testing.T) {
	provider := &mockSMSProvider{name: "mock"}
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{provider})

	req := &pb.SendSMSRequest{
		To:             "+8613800001111",
		Content:        "code",
		Scene:          pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:       "user:42",
		IdempotencyKey: "sms-1",
	}

	_, err := svc.SendSMS(context.Background(), req)
	require.NoError(t, err)
	_, err = svc.SendSMS(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 2, provider.calls, "provider must be called twice when persistence disabled")
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/service/message/ -run 'TestSendSMS_PersistenceDisabled' -v`
Expected: FAIL（当前 SMS 仍走幂等检查）。

- [ ] **Step 3: 实现 — SendSMS 加 enabled gate**

打开 `internal/service/message/sms.go`。定位到 `SendSMS` 方法。

将原有幂等检查块改为：

```go
// Idempotency check (skipped when persistence disabled).
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

将原有持久化块改为：

```go
if s.persistSMSEnabled {
	persistCtx, cancel := context.WithTimeout(context.Background(), persistTimeout)
	defer cancel()
	s.persistSMSRecord(persistCtx, id, req, result)
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./internal/service/message/ -run 'TestSendSMS_PersistenceDisabled' -v`
Expected: 两个用例 PASS。

- [ ] **Step 5: 跑所有 SMS 测试做回归**

Run: `go test ./internal/service/message/ -run TestSendSMS -v`
Expected: 全部 PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/service/message/sms.go internal/service/message/sms_test.go
git commit -m "feat(message/sms): gate SendSMS idempotency+persist by toggle"
```

---

### Task 8: Email 查询方法加 disabled gate

**Files:**
- Modify: `internal/service/message/email.go`
- Modify: `internal/service/message/email_test.go`

- [ ] **Step 1: 先写测试 — 4 个查询方法在关闭时返回 ErrPersistenceDisabled**

在 `internal/service/message/email_test.go` 末尾追加：

```go
import "errors" // 如果已有则跳过；用 grep 确认

func TestGetEmail_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: 1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled),
		"err must wrap ErrPersistenceDisabled, got: %v", err)
}

func TestListEmails_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled))
}

func TestListEmailsByCursor_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.ListEmailsByCursor(context.Background(), &pb.ListEmailsByCursorRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled))
}

func TestGetEmailStats_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestEmailServiceNoPersist(t, []email.AccountProvider{
		&mockEmailProvider{name: "mock"},
	})
	_, err := svc.GetEmailStats(context.Background(), &pb.GetEmailStatsRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled))
}
```

> `errors` 包用 `errors.Is`，需要 import。`xcodes` 包需要 import。先 `grep -n "^import" internal/service/message/email_test.go` 看现有 import 块，缺什么补什么。

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/service/message/ -run 'PersistenceDisabled_ReturnsError' -v -count=1`
Expected: FAIL — 当前查询方法不返回 `ErrPersistenceDisabled`。

- [ ] **Step 3: 实现 — 4 个查询方法各自在入口加 gate**

打开 `internal/service/message/email.go`。

在 `GetEmail` 方法体开头（`record, err := dal.GetEmailRecord(...)` 之前）插入：

```go
if !s.persistEmailEnabled {
	return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("email persistence is disabled"))
}
```

在 `ListEmails` 方法体开头（`f := dal.EmailListFilter{...}` 之前）插入同样的 gate。

在 `ListEmailsByCursor` 方法体开头插入同样的 gate。

在 `GetEmailStats` 方法体开头插入同样的 gate。

> `fmt` 包已经在文件中 import（被 `respondIdempotentEmail` 使用），无需新增 import。

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./internal/service/message/ -run 'PersistenceDisabled_ReturnsError' -v -count=1`
Expected: 4 个用例 PASS。

- [ ] **Step 5: 跑 email 所有测试做回归**

Run: `go test ./internal/service/message/ -v -count=1`
Expected: 全部 PASS（email + sms 都跑）。

- [ ] **Step 6: Commit**

```bash
git add internal/service/message/email.go internal/service/message/email_test.go
git commit -m "feat(message/email): return ErrPersistenceDisabled from query RPCs"
```

---

### Task 9: SMS 查询方法加 disabled gate

**Files:**
- Modify: `internal/service/message/sms.go`
- Modify: `internal/service/message/sms_test.go`

- [ ] **Step 1: 先写测试 — 4 个 SMS 查询方法**

在 `internal/service/message/sms_test.go` 末尾追加（确保 import `errors` 与 `xcodes`，参照 email_test.go Task 8 Step 1 的 import 处理）：

```go
func TestGetSMS_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.GetSMS(context.Background(), &pb.GetSMSRequest{Id: 1})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled))
}

func TestListSMS_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.ListSMS(context.Background(), &pb.ListSMSRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled))
}

func TestListSMSByCursor_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.ListSMSByCursor(context.Background(), &pb.ListSMSByCursorRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled))
}

func TestGetSMSStats_PersistenceDisabled_ReturnsError(t *testing.T) {
	svc := newTestSMSServiceNoPersist(t, []sms.AccountProvider{
		&mockSMSProvider{name: "mock"},
	})
	_, err := svc.GetSMSStats(context.Background(), &pb.GetSMSStatsRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, xcodes.ErrPersistenceDisabled))
}
```

- [ ] **Step 2: 运行测试验证失败**

Run: `go test ./internal/service/message/ -run 'SMS.*PersistenceDisabled_ReturnsError' -v -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现 — 4 个 SMS 查询方法加 gate**

打开 `internal/service/message/sms.go`。

在 `GetSMS`、`ListSMS`、`ListSMSByCursor`、`GetSMSStats` 各自方法体开头插入：

```go
if !s.persistSMSEnabled {
	return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
}
```

> `fmt` 已 import（被 `respondIdempotentSMS` 用）。

- [ ] **Step 4: 运行测试验证通过**

Run: `go test ./internal/service/message/ -v -count=1`
Expected: 全部 PASS（email + sms）。

- [ ] **Step 5: Commit**

```bash
git add internal/service/message/sms.go internal/service/message/sms_test.go
git commit -m "feat(message/sms): return ErrPersistenceDisabled from query RPCs"
```

---

## Phase 4: 配置文档 + 最终验证

### Task 10: `config.yaml` 加 persistence 段（默认注释打开）

**Files:**
- Modify: `config.yaml`

- [ ] **Step 1: 在 `config.yaml` 中 `cron:` 段之前插入 persistence 段**

打开 `config.yaml`。在 `cron:` 行之前插入：

```yaml
persistence:
  email:
    enabled: true   # set false to skip DB writes for emails (sends only)
  sms:
    enabled: true   # set false to skip DB writes for SMS (sends only)

```

- [ ] **Step 2: 验证 yaml 仍可加载**

Run: `go run ./cmd/migrate/ --help 2>&1 | head -5` （或任意会触发 Load() 的入口；只要没报 yaml 错就 OK）

如果 migrate 命令需要 DB 连接验证，改用：

Run: `go test ./pkg/config/ -v -count=1`
Expected: 全部 PASS（包括 Task 2 加的 3 个 persistence 测试）。

- [ ] **Step 3: Commit**

```bash
git add config.yaml
git commit -m "docs(config): add persistence section with default true"
```

---

### Task 11: 最终验证（全量测试 + lint + 手动 smoke）

**Files:** 无修改

- [ ] **Step 1: 跑全量单元测试**

Run: `go test -race -count=1 ./...`
Expected: 全部 PASS。如果有 testcontainer 用例因本地无 docker 跳过，记录为 `--- SKIP`，不算失败。

- [ ] **Step 2: 跑 lint**

Run: `golangci-lint run ./...`
Expected: 无报错。

- [ ] **Step 3: 跑格式化检查**

Run: `gofmt -l .` 与 `goimports -l .`
Expected: 无输出（所有文件已格式化）。如果有输出，对列出的文件跑 `gofmt -w` / `goimports -w` 后回到 Step 1 重测。

- [ ] **Step 4: 手动 smoke test（可选但推荐）**

启 docker postgres + 修改 `config.yaml`：

```yaml
persistence:
  email:
    enabled: false
```

启动服务，用 `cmd/testclient` 发一封邮件：

Run: `go run ./cmd/server/ &` 然后用 `go run ./cmd/testclient send-email ...`

预期：
1. send-email 返回 SENT（发送成功）。
2. 查 DB `SELECT * FROM message_email_records WHERE id = <resp.id>`：无记录。
3. testclient 调用 `get-email <id>`：返回 503，错误码 `PERSISTENCE_DISABLED`。

恢复 `config.yaml`（`enabled: true`）后再次 send，确认 DB 有记录。

- [ ] **Step 5: 更新 CLAUDE.md 与 Obsidian 笔记（标记实施完成）**

在 `services/message-service/changes.md`（Obsidian）追加：

```
- 2026-06-26: 完成 services/message-service/design/v5/2026-06-26-persistence-toggle.md 实施 — per-channel persistence 开关；新增 ErrPersistenceDisabled；TDD 9 个 task 完成
```

CLAUDE.md 中如有需要补充"持久化可关闭"相关说明，追加到对应段落（"数据库 / GORM"或单独段落）。

- [ ] **Step 6: 最终 commit**

```bash
git add CLAUDE.md  # 如果改了
git commit -m "docs: mark persistence toggle implementation complete"
```

（如果 CLAUDE.md 没改，跳过 commit，直接结束。）

---

## 完成标准

- 所有现有测试 PASS（向后兼容）。
- 9 个新测试 PASS（5 email × persistence + 4 sms × persistence，加上 config 包 3 个）。
- `go build ./...` 无错。
- `golangci-lint run ./...` 无错。
- yaml 省略 persistence 段时行为 = 当前线上行为（默认开启）。
- yaml 显式 `enabled: false` 时：发送路径不写库、不去重；查询路径返回 `PERSISTENCE_DISABLED/503`。

## 关联

- 设计文档：[[services/message-service/design/v5/2026-06-26-persistence-toggle]]
- 项目内 spec：`docs/superpowers/specs/2026-06-26-persistence-toggle-design.md`
