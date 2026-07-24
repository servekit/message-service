# Vendor 字段 enum 化实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `SendEmailRequest.vendor` / `SendSMSRequest.vendor` 从 string 改为 proto enum（EmailVendor / SmsVendor），同时删除 Provider enum，让 vendor 成为唯一的厂商表示。

**Architecture:** 五阶段：① proto 改动 + 代码生成 ② store model + repository 改造 ③ email/sms registry 接受 enum 输入（内部仍用 string key） ④ service 层 enum↔string 转换 + send/query 改造 ⑤ config.yaml + 全量验证。所有 vendor 概念在 message-service 内部，go-common 不动。

**Tech Stack:** Go 1.22+ / GORM / PostgreSQL / gRPC / buf / gorm-gen / testify

**Spec:** `docs/superpowers/specs/2026-06-16-vendor-enum-design.md`

---

## File Structure

**Proto:**
- `api/proto/message/v1/message.proto` — 删 Provider enum，加 EmailVendor/SmsVendor，调整 SendResponse/MessageRecord 用 oneof vendor，ListMessagesRequest/GetMessageStatsRequest 拆 vendor filter，ProviderStats 拆为 EmailVendorStats/SmsVendorStats
- `gen/message/v1/message.pb.go` — buf 自动重生成

**Store:**
- `internal/store/models/message_record.go` — Provider 字段重命名为 Vendor
- `internal/store/generated/message_record.go` — `gorm gen` 重新生成
- `internal/store/repository/message_repository.go` — ListFilter/StatsFilter 拆 vendor，ProviderStats 改 VendorStat（带 channel）

**Internal message:**
- `internal/message/email/registry.go` — buildProvider switch 用 vendor string 分发，加 defaultSMTPHost 函数，加 vendor 名校验
- `internal/message/email/registry_test.go` — 测试新增 vendor 集合
- `internal/message/sms/registry.go` — 加 vendor 名校验
- `internal/message/sms/registry_test.go` — 测试

**Service:**
- `internal/service/send.go` — 删 emailProviderToProto / smsProviderToProto，加 emailVendorToString / smsVendorToString，SendResponse 构造 oneof vendor
- `internal/service/query.go` — list filter 拆 vendor，stats 拆 channel
- `internal/service/service.go` — toProtoRecord 改 oneof vendor 构造，messageRepo 接口签名调整
- `internal/service/service_test.go` — vendor 字段改 enum，断言改 vendor enum

**Config:**
- `config.yaml` — email vendor key 改 `custom_smtp`（旧 `smtp` 不再对应 enum 值）

---

## Phase 1: Proto 改动

### Task 1: Proto schema 改造 + buf 生成

**Files:**
- Modify: `api/proto/message/v1/message.proto`
- Regenerate: `gen/message/v1/message.pb.go`

- [ ] **Step 1: 替换 Provider enum 为 EmailVendor + SmsVendor**

把 `api/proto/message/v1/message.proto:24-30`（Provider enum 整段）替换为：

```proto
// EmailVendor represents the email service brand. SMTP is the protocol;
// the same protocol connects different brands. CUSTOM_SMTP is the escape
// hatch when the brand is not in the enum (host must be provided in config).
enum EmailVendor {
  EMAIL_VENDOR_UNSPECIFIED = 0;
  EMAIL_VENDOR_CUSTOM_SMTP = 1;
  EMAIL_VENDOR_ALIYUN = 2;
  EMAIL_VENDOR_TENCENT = 3;
  EMAIL_VENDOR_NETEASE = 4;
}

// SmsVendor represents the SMS service brand. Each brand has its own API,
// so each value requires a corresponding go-common subpackage.
enum SmsVendor {
  SMS_VENDOR_UNSPECIFIED = 0;
  SMS_VENDOR_ALIYUN = 1;
}
```

- [ ] **Step 2: 调整 SendEmailRequest / SendSMSRequest**

把 `SendEmailRequest.vendor` 字段（line 55）和 CEL 校验改为：

```proto
message SendEmailRequest {
  string to = 1 [(buf.validate.field).string.email = true];
  repeated string cc = 2 [(buf.validate.field).repeated.items.string.email = true];
  repeated string bcc = 3 [(buf.validate.field).repeated.items.string.email = true];
  string subject = 4 [(buf.validate.field).string.min_len = 1];
  string body = 5;
  string html_body = 6;
  string reply_to = 7 [(buf.validate.field).string.email = true];
  // Vendor + Account optionally select a specific configured account.
  // Both must be set together; if both zero/empty, sender uses default fallback.
  EmailVendor vendor = 8;
  string account = 9;

  option (buf.validate.message).cel = {
    id: "vendor_account_pair",
    message: "vendor and account must both be set or both be empty",
    expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
  };
}
```

把 `SendSMSRequest.vendor` 字段（line 72）和 CEL 校验改为：

```proto
message SendSMSRequest {
  string to = 1 [(buf.validate.field).string.min_len = 1];
  string content = 2;
  string template_id = 3;
  map<string, string> template_params = 4;
  // Vendor + Account optionally select a specific configured account.
  // Both must be set together; if both zero/empty, sender uses default fallback.
  SmsVendor vendor = 5;
  string account = 6;

  option (buf.validate.message).cel = {
    id: "vendor_account_pair",
    message: "vendor and account must both be set or both be empty",
    expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
  };
}
```

- [ ] **Step 3: 调整 SendResponse**

把 `SendResponse`（line 82-86）改为：

```proto
message SendResponse {
  int64 id = 1;
  MessageStatus status = 2;
  // vendor reports which vendor actually handled the send.
  oneof vendor {
    EmailVendor email_vendor = 3;
    SmsVendor sms_vendor = 4;
  }
}
```

注意：原 `Provider provider = 3` 字段编号复用为 oneof case（编号 3），新增 sms_vendor 编号 4。

- [ ] **Step 4: 调整 MessageRecord**

把 `MessageRecord`（line 92-111）改为：

```proto
message MessageRecord {
  int64 id = 1;
  Channel channel = 2;
  // vendor is oneof because email and sms use different enum types.
  oneof vendor {
    EmailVendor email_vendor = 19;
    SmsVendor sms_vendor = 20;
  }
  MessageStatus status = 4;
  string target = 5;
  string subject = 6;
  string content = 7;
  string template_id = 8;
  map<string, string> template_params = 9;
  string sender_id = 10;
  string error_message = 11;
  int32 attempts = 12;
  int64 sent_at = 13;
  int64 created_at = 14;
  repeated string cc = 15;
  repeated string bcc = 16;
  string html_body = 17;
  string reply_to = 18;
}
```

注意：原 `Provider provider = 3` 删除，oneof 用编号 19/20（避开已使用编号）。

- [ ] **Step 5: 调整 ListMessagesRequest**

把 `ListMessagesRequest`（line 113-122）改为：

```proto
message ListMessagesRequest {
  Channel channel = 1;
  MessageStatus status = 2;
  string target = 3;
  // Filter by vendor. Set the one matching channel; the other is ignored.
  EmailVendor email_vendor = 9;
  SmsVendor sms_vendor = 10;
  int64 start_time = 5;
  int64 end_time = 6;
  int32 page = 7;
  int32 page_size = 8;
}
```

注意：原 `Provider provider = 4` 删除；email_vendor / sms_vendor 用编号 9/10。

- [ ] **Step 6: 调整 GetMessageStatsRequest**

把 `GetMessageStatsRequest`（line 129-134）改为：

```proto
message GetMessageStatsRequest {
  Channel channel = 1;
  // Filter by vendor. Set the one matching channel; the other is ignored.
  EmailVendor email_vendor = 5;
  SmsVendor sms_vendor = 6;
  int64 start_time = 3;
  int64 end_time = 4;
}
```

注意：原 `Provider provider = 2` 删除；email_vendor / sms_vendor 用编号 5/6。

- [ ] **Step 7: 调整 MessageStatsResponse 和 ProviderStats**

把 `MessageStatsResponse`（line 136-142）和 `ProviderStats`（line 144-149）改为：

```proto
message MessageStatsResponse {
  int64 total = 1;
  int64 sent = 2;
  int64 failed = 3;
  double success_rate = 4;
  repeated EmailVendorStats email_stats = 5;
  repeated SmsVendorStats sms_stats = 6;
}

message EmailVendorStats {
  EmailVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}

message SmsVendorStats {
  SmsVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}
```

- [ ] **Step 8: buf generate**

```bash
cd /Users/moss/code/base/message-service
buf generate
```

Expected: 无输出（成功）。`gen/message/v1/message.pb.go` 重生成，包含 `EmailVendor`/`SmsVendor` 类型，不再有 `Provider`。

- [ ] **Step 9: 验证 proto 编译**

```bash
go build ./gen/...
```

Expected: PASS（gen 包独立编译）。整体 `go build ./...` 此时会失败，因为 service/repository 还在引用 `pb.Provider`——后续 task 会修复。

- [ ] **Step 10: Commit**

```bash
git add api/proto/message/v1/message.proto gen/message/v1/message.pb.go
git commit -m "$(cat <<'EOF'
refactor(proto)!: replace Provider enum with EmailVendor + SmsVendor

vendor field type changes from string to channel-specific enum. SMTP
is protocol not vendor: EmailVendor lists brands (CUSTOM_SMTP,
ALIYUN, TENCENT, NETEASE), all sharing the generic smtp client.
Provider enum deleted; SendResponse/MessageRecord use oneof vendor.

BREAKING CHANGE: PB clients must regenerate stubs. ListMessages and
GetMessageStats filter signature changed. MessageStatsResponse
provider_stats split into email_stats + sms_stats.
EOF
)"
```

---

## Phase 2: Store 改造

### Task 2: MessageRecord.Provider 重命名为 Vendor

**Files:**
- Modify: `internal/store/models/message_record.go:15`

- [ ] **Step 1: 重命名 Provider 字段为 Vendor**

把 `internal/store/models/message_record.go:13-17`：

```go
type MessageRecord struct {
	ID             int64           `gorm:"primaryKey"`
	Channel        int32           `gorm:"not null;index"`
	Provider       int32           `gorm:"not null;index"`
	Account        string          `gorm:"size:64;column:account"`
	Status         int32           `gorm:"not null;default:0;index"`
```

改为：

```go
type MessageRecord struct {
	ID             int64           `gorm:"primaryKey"`
	Channel        int32           `gorm:"not null;index"`
	Vendor         int32           `gorm:"not null;index"`
	Account        string          `gorm:"size:64;column:account"`
	Status         int32           `gorm:"not null;default:0;index"`
```

- [ ] **Step 2: 重新生成 gorm gen 代码**

```bash
cd /Users/moss/code/base/message-service
make generate
```

Expected: 无输出（成功）。`internal/store/generated/message_record.go` 中 `Provider field.Number[int32]` 变为 `Vendor field.Number[int32]`，column 标签从 `"provider"` 变为 `"vendor"`。

- [ ] **Step 3: 验证 store 包编译**

```bash
go build ./internal/store/...
```

Expected: 此时 repository 包编译会失败（还在用 `generated.MessageRecord.Provider` 和 `ListFilter.Provider`），后续 task 修复。models 和 generated 包应该编译通过。

- [ ] **Step 4: Commit（model + generated 一起）**

```bash
git add internal/store/models/message_record.go internal/store/generated/message_record.go
git commit -m "refactor(models): rename MessageRecord.Provider to Vendor"
```

---

### Task 3: repository filter/stats 改造

**Files:**
- Modify: `internal/store/repository/message_repository.go`

- [ ] **Step 1: ListFilter 拆 vendor 字段**

把 `ListFilter`（line 18-28）改为：

```go
// ListFilter holds parameters for listing message records.
type ListFilter struct {
	Channel     pb.Channel
	Status      pb.MessageStatus
	Target      string
	EmailVendor pb.EmailVendor // applied only when Channel == EMAIL
	SmsVendor   pb.SmsVendor   // applied only when Channel == SMS
	StartTime   *time.Time
	EndTime     *time.Time
	Page        int32
	PageSize    int32
}
```

- [ ] **Step 2: StatsFilter 拆 vendor 字段**

把 `StatsFilter`（line 30-36）改为：

```go
// StatsFilter holds parameters for querying message statistics.
type StatsFilter struct {
	Channel     pb.Channel
	EmailVendor pb.EmailVendor
	SmsVendor   pb.SmsVendor
	StartTime   *time.Time
	EndTime     *time.Time
}
```

- [ ] **Step 3: ProviderStats 改为 VendorStat（带 channel）**

把 `ProviderStats`（line 46-52）改为：

```go
// VendorStat contains per-vendor message statistics. The Vendor int must be
// interpreted by the caller based on Channel (EmailVendor vs SmsVendor).
type VendorStat struct {
	Channel pb.Channel
	Vendor  int32
	Total   int64
	Sent    int64
	Failed  int64
}
```

- [ ] **Step 4: 改 applyListFilter**

把 `applyListFilter`（line 213-233）的 Provider 部分（line 223-225）：

```go
	if f.Provider != 0 {
		q = q.Where(generated.MessageRecord.Provider.Eq(int32(f.Provider)))
	}
```

替换为：

```go
	if f.EmailVendor != 0 {
		q = q.Where(generated.MessageRecord.Vendor.Eq(int32(f.EmailVendor)))
	}
	if f.SmsVendor != 0 {
		q = q.Where(generated.MessageRecord.Vendor.Eq(int32(f.SmsVendor)))
	}
```

- [ ] **Step 5: 改 applyStatsFilter**

把 `applyStatsFilter`（line 236-250）的 Provider 部分（line 240-242）：

```go
	if f.Provider != 0 {
		q = q.Where(generated.MessageRecord.Provider.Eq(int32(f.Provider)))
	}
```

替换为：

```go
	if f.EmailVendor != 0 {
		q = q.Where(generated.MessageRecord.Vendor.Eq(int32(f.EmailVendor)))
	}
	if f.SmsVendor != 0 {
		q = q.Where(generated.MessageRecord.Vendor.Eq(int32(f.SmsVendor)))
	}
```

- [ ] **Step 6: 改 ProviderStats 方法**

把 `ProviderStats` 方法（line 162-208）重命名为 `VendorStats`，签名和实现调整。完整新版：

```go
// VendorStats returns per-vendor message statistics matching the filter.
// Each row contains Channel + Vendor int (interpret by channel).
//
// Uses r.db.Model(...) directly (rather than the generic gorm.G[T] chain)
// because raw SELECT with GROUP BY loses the model binding in the generic
// chain, which causes an empty FROM clause.
func (r *MessageRepository) VendorStats(ctx context.Context, filter StatsFilter) ([]VendorStat, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	q := r.db.Model(&models.MessageRecord{}).Where("id > 0")
	if filter.Channel != 0 {
		q = q.Where("channel = ?", int32(filter.Channel))
	}
	if filter.EmailVendor != 0 {
		q = q.Where("vendor = ?", int32(filter.EmailVendor))
	}
	if filter.SmsVendor != 0 {
		q = q.Where("vendor = ?", int32(filter.SmsVendor))
	}
	if filter.StartTime != nil {
		q = q.Where("created_at >= ?", *filter.StartTime)
	}
	if filter.EndTime != nil {
		q = q.Where("created_at <= ?", *filter.EndTime)
	}

	rows, err := q.
		Select(fmt.Sprintf(
			"channel, vendor, COUNT(*) as total, COUNT(*) FILTER (WHERE status = %d) as sent, COUNT(*) FILTER (WHERE status = %d) as failed",
			sentStatus, failedStatus,
		)).
		Group("channel, vendor").
		Rows()
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	defer func() { _ = rows.Close() }()

	var stats []VendorStat
	for rows.Next() {
		var s VendorStat
		if err := rows.Scan(&s.Channel, &s.Vendor, &s.Total, &s.Sent, &s.Failed); err != nil {
			return nil, xcodes.ErrInternal.Wrap(err)
		}
		stats = append(stats, s)
	}
	if err := rows.Err(); err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	return stats, nil
}
```

注意 SQL `WHERE "vendor = ?"` 用列名 `vendor`（GORM 默认列名 = 字段名 lower）。

- [ ] **Step 7: 验证 repository 包编译**

```bash
go build ./internal/store/...
```

Expected: PASS。

- [ ] **Step 8: Commit**

```bash
git add internal/store/repository/message_repository.go
git commit -m "refactor(repository): split Provider filter into EmailVendor + SmsVendor"
```

---

## Phase 3: Registry 改造

### Task 4: email registry — vendor 名校验 + defaultSMTPHost

**Files:**
- Modify: `internal/message/email/registry.go`
- Modify: `internal/message/email/registry_test.go`

- [ ] **Step 1: 新增 defaultSMTPHost 函数**

在 `internal/message/email/registry.go` 的 `buildProvider` 函数之前（约 line 140），新增：

```go
// defaultSMTPHost returns the conventional SMTP host for a branded vendor.
// Returns empty string for vendors without a canonical host (e.g.,
// "custom_smtp"), in which case the config must provide Host explicitly.
func defaultSMTPHost(vendor string) string {
	switch vendor {
	case "aliyun":
		return "smtp.aliyun.com"
	case "tencent":
		return "smtp.exmail.qq.com"
	case "netease":
		return "smtp.qiye.163.com"
	default:
		return ""
	}
}
```

- [ ] **Step 2: 改 buildProvider 用 vendor 名分发 + 默认 host**

把 `buildProvider`（line 144-164）整体替换为：

```go
// buildProvider dispatches to the corresponding subpackage based on vendor name
// and returns the underlying go-common emailcommon.Provider. The caller wraps
// the result in *AccountProvider. Add a case here when adding a new vendor, and
// add corresponding fields to AccountConfig.
//
// All current vendors use SMTP protocol and share smtpprovider.NewProvider;
// the vendor name only affects the default host when config.Host is empty.
func buildProvider(vendor string, ac AccountConfig) (emailcommon.Provider, error) {
	switch vendor {
	case "custom_smtp", "aliyun", "tencent", "netease":
		host := ac.Host
		if host == "" {
			host = defaultSMTPHost(vendor)
			if host == "" {
				return nil, fmt.Errorf("email vendor %q requires explicit host", vendor)
			}
		}
		return smtpprovider.NewProvider(&smtpprovider.Config{
			Host:     host,
			Port:     ac.Port,
			Username: ac.Username,
			Password: ac.Password,
			From:     ac.From,
		})
	default:
		return nil, fmt.Errorf("unknown vendor %q", vendor)
	}
}
```

- [ ] **Step 3: 改 AccountConfig 注释（清理 mailgun 残留 + 加 vendor 名约定）**

把 `AccountConfig`（line 28-38）注释更新：

```go
// AccountConfig is a single named account. Carries fields for all supported
// vendors; only the subset matching the parent vendor is used.
//
// fat-struct design: adding a new vendor means adding fields here. This is a
// low-frequency operation, and adding a new vendor requires a new subpackage
// anyway.
//
// vendor name in YAML must match pb.EmailVendor's lowercase form:
// "custom_smtp", "aliyun", "tencent", "netease". Unknown names rejected at
// registry construction.
type AccountConfig struct {
	Name     string
	Host     string // SMTP host; auto-filled from defaultSMTPHost when empty for branded vendors
	Port     int    // SMTP submission port (587 STARTTLS, 465 implicit TLS)
	Username string // SMTP
	Password string // SMTP
	From     string // SMTP
}
```

- [ ] **Step 4: 改测试 — 替换 smtp case 用 custom_smtp + 加默认 host 测试**

打开 `internal/message/email/registry_test.go`，找到 `TestNewAccountRegistry_smtpSuccess`（如存在；可能名为 `_smtpConstructs`）。把测试中 vendor key `"smtp"` 改为 `"custom_smtp"`：

```go
func TestNewAccountRegistry_customSMTPSuccess(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"custom_smtp": {Accounts: []AccountConfig{
				{Name: "primary", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)
}
```

新增默认 host 测试：

```go
func TestNewAccountRegistry_aliyunDefaultHost(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"aliyun": {Accounts: []AccountConfig{
				{Name: "primary", Port: 465, Username: "u", Password: "p", From: "noreply@aliyun.com"},
				// Host intentionally empty; registry should fill smtp.aliyun.com.
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestNewAccountRegistry_customSMTPRequiresHost(t *testing.T) {
	cfg := &Config{
		Vendors: map[string]VendorConfig{
			"custom_smtp": {Accounts: []AccountConfig{
				{Name: "primary", Port: 587, From: "noreply@example.com"},
				// Host intentionally empty; custom_smtp has no default.
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires explicit host")
}
```

注意：原 mock vendor 名 `"mailgun"` 在 registry_test.go 里（line 75-120 范围内）保留——它们通过 `NewAccountRegistryFromProviders` 注入，不走 `buildProvider`，只是 string key。

- [ ] **Step 5: 跑 email 包测试**

```bash
go test ./internal/message/email/ -v
```

Expected: PASS（所有非 buildProvider 测试 + 新增三个测试通过）。

- [ ] **Step 6: Commit**

```bash
git add internal/message/email/registry.go internal/message/email/registry_test.go
git commit -m "$(cat <<'EOF'
refactor(email): buildProvider dispatches by vendor name + default SMTP host

All EmailVendor values share the generic smtp provider; vendor name
only selects the default host (smtp.aliyun.com etc.) when config.Host
is empty. custom_smtp requires explicit host. Vendor name in YAML
must match pb.EmailVendor lowercase form.
EOF
)"
```

---

### Task 5: sms registry — vendor 名校验

**Files:**
- Modify: `internal/message/sms/registry.go`

- [ ] **Step 1: 改 buildProvider 注释，明确 vendor 名集合**

把 `internal/message/sms/registry.go` 的 `buildProvider`（line 168-184）注释更新：

```go
// buildProvider dispatches to the corresponding subpackage based on vendor name
// and returns the underlying go-common smscommon.Provider. The caller wraps
// the result in *AccountProvider.
//
// vendor name must match pb.SmsVendor's lowercase form: currently only
// "aliyun". Unknown names rejected. Adding a vendor requires both a new enum
// value in proto and a corresponding subpackage in go-common.
func buildProvider(vendor string, ac AccountConfig) (smscommon.Provider, error) {
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
```

实现部分不变，只更新注释。

- [ ] **Step 2: 验证 sms 包编译 + 测试**

```bash
go build ./internal/message/sms/...
go test ./internal/message/sms/ -v
```

Expected: PASS。

- [ ] **Step 3: Commit**

```bash
git add internal/message/sms/registry.go
git commit -m "docs(sms): clarify vendor name must match pb.SmsVendor lowercase form"
```

---

## Phase 4: Service 改造

### Task 6: service 层 enum↔string 转换 + send.go 改造

**Files:**
- Modify: `internal/service/send.go`

- [ ] **Step 1: 删除 emailProviderToProto / smsProviderToProto**

把 `internal/service/send.go:183-199`（从 `// --- provider name → proto enum helpers ---` 到文件末尾）整体替换为：

```go
// --- vendor enum ↔ string helpers ---

// emailVendorToString maps pb.EmailVendor to the vendor name used as the
// AccountRegistry map key (and the YAML config key). Returns "" for
// EMAIL_VENDOR_UNSPECIFIED — caller should treat as "not specified".
func emailVendorToString(v pb.EmailVendor) string {
	switch v {
	case pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP:
		return "custom_smtp"
	case pb.EmailVendor_EMAIL_VENDOR_ALIYUN:
		return "aliyun"
	case pb.EmailVendor_EMAIL_VENDOR_TENCENT:
		return "tencent"
	case pb.EmailVendor_EMAIL_VENDOR_NETEASE:
		return "netease"
	default:
		return ""
	}
}

// smsVendorToString is the SMS counterpart of emailVendorToString.
func smsVendorToString(v pb.SmsVendor) string {
	switch v {
	case pb.SmsVendor_SMS_VENDOR_ALIYUN:
		return "aliyun"
	default:
		return ""
	}
}
```

- [ ] **Step 2: 改 sendEmail — vendor enum 转 string + SendResponse oneof**

把 `sendEmail`（line 22-60）整体替换为：

```go
func (s *MessageService) sendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	vendorStr := emailVendorToString(req.GetVendor())
	sender, err := s.emailRegistry.SenderFor(vendorStr, req.GetAccount())
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}

	id, err := s.gid.NextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	msg := &emailcommon.Message{
		To:       req.GetTo(),
		Cc:       req.GetCc(),
		Bcc:      req.GetBcc(),
		Subject:  req.GetSubject(),
		Body:     req.GetBody(),
		HTMLBody: req.GetHtmlBody(),
		ReplyTo:  req.GetReplyTo(),
	}

	result, err := sender.Send(ctx, msg)
	if err != nil {
		// result==nil means validation error (empty recipient / no provider);
		// nothing was attempted, no record to persist.
		if result != nil {
			s.persistEmailRecord(ctx, id, req, result)
		}
		return nil, xcodes.ErrMessageSendFailed.Wrap(err)
	}

	s.persistEmailRecord(ctx, id, req, result)

	return &pb.SendResponse{
		Id:       id,
		Status:   pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_EmailVendor{
			EmailVendor: req.GetVendor(),
		},
	}, nil
}
```

注意：`req.GetVendor()` 在 SenderFor 时转 string，在 SendResponse 时直接用 enum（保证返回的就是请求的 vendor，因为 EmailVendor 都是 SMTP 共用，发送实际用的 vendor 就是请求的 vendor）。

- [ ] **Step 3: 改 sendSMS — vendor enum 转 string + SendResponse oneof**

把 `sendSMS`（line 62-113）整体替换为：

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

	// Branch on vendor/account:
	//   - both set     → use the explicitly selected Sender (registry path).
	//   - both empty   → route by phone country code via smsRouter; nil router
	//                    means routes were not configured (caller must specify
	//                    vendor+account explicitly).
	// Proto CEL validation guarantees one of these two cases; partial spec is
	// rejected at the gRPC layer before reaching this code.
	var result *sms.SendResult
	var sendErr error
	if req.GetVendor() != 0 && req.GetAccount() != "" {
		vendorStr := smsVendorToString(req.GetVendor())
		sender, err := s.smsRegistry.SenderFor(vendorStr, req.GetAccount())
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
		// result==nil means validation error (empty recipient / no target /
		// unparseable phone); nothing was attempted, no record to persist.
		if result != nil {
			s.persistSMSRecord(ctx, id, req, result)
		}
		return nil, xcodes.ErrMessageSendFailed.Wrap(sendErr)
	}

	s.persistSMSRecord(ctx, id, req, result)

	return &pb.SendResponse{
		Id:       id,
		Status:   pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_SmsVendor{
			SmsVendor: req.GetVendor(),
		},
	}, nil
}
```

注意：CEL 校验现在用 `vendor != 0`（enum 0 值 = unspecified）替代 `vendor != ""`。

- [ ] **Step 4: 改 persistEmailRecord — 用 Vendor 列 + Account 不变**

把 `persistEmailRecord`（line 123-152）中 `Provider: int32(emailProviderToProto(result.Vendor))` 一行改为：

```go
		Vendor:         int32(req.GetVendor()),
```

（req.GetVendor() 是 EmailVendor，转 int32 存 DB）

但 `result.Vendor` 在 sender SendResult 里仍是 string（"custom_smtp" 等），与 req.GetVendor() 应一致。直接用 req 的 enum 更可靠。

新版 persistEmailRecord 完整：

```go
func (s *MessageService) persistEmailRecord(ctx context.Context, id int64, req *pb.SendEmailRequest, result *email.SendResult) {
	record := &models.MessageRecord{
		ID:       id,
		Channel:  int32(pb.Channel_CHANNEL_EMAIL),
		Target:   req.GetTo(),
		Cc:       models.StringSlice(req.GetCc()),
		Bcc:      models.StringSlice(req.GetBcc()),
		Subject:  req.GetSubject(),
		Content:  req.GetBody(),
		HTMLBody: req.GetHtmlBody(),
		ReplyTo:  req.GetReplyTo(),
		Attempts: result.Attempts,
		Vendor:   int32(req.GetVendor()),
		Account:  result.Account,
	}

	if result.Success {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		if result.Error != nil {
			record.ErrorMessage = result.Error.Error()
		}
	}

	if err := s.repo.Create(ctx, record); err != nil {
		slog.Error("persist email record", "record_id", id, "error", err)
	}
}
```

- [ ] **Step 5: 改 persistSMSRecord — 同上**

`persistSMSRecord`（line 155-181）的 `Provider:` 行改为 `Vendor: int32(req.GetVendor()),`。

新版完整：

```go
func (s *MessageService) persistSMSRecord(ctx context.Context, id int64, req *pb.SendSMSRequest, result *sms.SendResult) {
	record := &models.MessageRecord{
		ID:             id,
		Channel:        int32(pb.Channel_CHANNEL_SMS),
		Target:         req.GetTo(),
		Content:        req.GetContent(),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
		Vendor:         int32(req.GetVendor()),
		Account:        result.Account,
	}

	if result.Success {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
		record.SentAt = sql.NullTime{Time: time.Now(), Valid: true}
	} else {
		record.Status = int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)
		if result.Error != nil {
			record.ErrorMessage = result.Error.Error()
		}
	}

	if err := s.repo.Create(ctx, record); err != nil {
		slog.Error("persist sms record", "record_id", id, "error", err)
	}
}
```

- [ ] **Step 6: 验证 send.go 编译**

```bash
go build ./internal/service/...
```

Expected: FAIL（query.go 和 service.go 还在用 `pb.Provider`）。这是预期的；下个 task 修复。

- [ ] **Step 7: Commit**

```bash
git add internal/service/send.go
git commit -m "$(cat <<'EOF'
refactor(service): send path uses vendor enum, SendResponse oneof

Replaces *ProviderToProto helpers with emailVendorToString /
smsVendorToString (proto enum → registry string key). SendResponse
now constructs the oneof vendor case matching the channel. Persist
records use the request's vendor enum (authoritative).
EOF
)"
```

---

### Task 7: query.go + service.go 改造

**Files:**
- Modify: `internal/service/query.go`
- Modify: `internal/service/service.go`

- [ ] **Step 1: 改 listMessages — filter 拆 vendor**

把 `internal/service/query.go:20-52` 整体替换为：

```go
func (s *MessageService) listMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	f := repository.ListFilter{
		Channel:     req.GetChannel(),
		Status:      req.GetStatus(),
		Target:      req.GetTarget(),
		EmailVendor: req.GetEmailVendor(),
		SmsVendor:   req.GetSmsVendor(),
		Page:        req.GetPage(),
		PageSize:    req.GetPageSize(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	records, total, err := s.repo.List(ctx, f)
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.MessageRecord, len(records))
	for i, r := range records {
		protoRecords[i] = toProtoRecord(r)
	}

	return &pb.ListMessagesResponse{
		Records: protoRecords,
		Total:   int32(total),
	}, nil
}
```

- [ ] **Step 2: 改 getMessageStats — filter + response 拆 channel**

把 `getMessageStats`（line 54-95）整体替换为：

```go
func (s *MessageService) getMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
	f := repository.StatsFilter{
		Channel:     req.GetChannel(),
		EmailVendor: req.GetEmailVendor(),
		SmsVendor:   req.GetSmsVendor(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	stats, err := s.repo.Stats(ctx, f)
	if err != nil {
		return nil, err
	}

	vendorStats, err := s.repo.VendorStats(ctx, f)
	if err != nil {
		return nil, err
	}

	var emailStats []*pb.EmailVendorStats
	var smsStats []*pb.SmsVendorStats
	for _, vs := range vendorStats {
		switch vs.Channel {
		case pb.Channel_CHANNEL_EMAIL:
			emailStats = append(emailStats, &pb.EmailVendorStats{
				Vendor: pb.EmailVendor(vs.Vendor),
				Total:  vs.Total,
				Sent:   vs.Sent,
				Failed: vs.Failed,
			})
		case pb.Channel_CHANNEL_SMS:
			smsStats = append(smsStats, &pb.SmsVendorStats{
				Vendor: pb.SmsVendor(vs.Vendor),
				Total:  vs.Total,
				Sent:   vs.Sent,
				Failed: vs.Failed,
			})
		}
	}

	return &pb.MessageStatsResponse{
		Total:       stats.Total,
		Sent:        stats.Sent,
		Failed:      stats.Failed,
		SuccessRate: stats.SuccessRate,
		EmailStats:  emailStats,
		SmsStats:    smsStats,
	}, nil
}
```

- [ ] **Step 3: 改 messageRepo 接口签名**

`internal/service/service.go:38-44` 的 `messageRepo` interface：

```go
type messageRepo interface {
	Create(ctx context.Context, record *models.MessageRecord) error
	FindByID(ctx context.Context, id int64) (*models.MessageRecord, error)
	List(ctx context.Context, filter repository.ListFilter) ([]*models.MessageRecord, int, error)
	Stats(ctx context.Context, filter repository.StatsFilter) (*repository.Stats, error)
	ProviderStats(ctx context.Context, filter repository.StatsFilter) ([]repository.ProviderStats, error)
}
```

改为：

```go
type messageRepo interface {
	Create(ctx context.Context, record *models.MessageRecord) error
	FindByID(ctx context.Context, id int64) (*models.MessageRecord, error)
	List(ctx context.Context, filter repository.ListFilter) ([]*models.MessageRecord, int, error)
	Stats(ctx context.Context, filter repository.StatsFilter) (*repository.Stats, error)
	VendorStats(ctx context.Context, filter repository.StatsFilter) ([]repository.VendorStat, error)
}
```

- [ ] **Step 4: 改 toProtoRecord — Vendor oneof**

把 `service.go:176-200` 的 `toProtoRecord` 改为：

```go
func toProtoRecord(r *models.MessageRecord) *pb.MessageRecord {
	rec := &pb.MessageRecord{
		Id:             r.ID,
		Channel:        pb.Channel(r.Channel),
		Status:         pb.MessageStatus(r.Status),
		Target:         r.Target,
		Cc:             []string(r.Cc),
		Bcc:            []string(r.Bcc),
		Subject:        r.Subject,
		Content:        r.Content,
		HtmlBody:       r.HTMLBody,
		ReplyTo:        r.ReplyTo,
		TemplateId:     r.TemplateID,
		TemplateParams: map[string]string(r.TemplateParams),
		SenderId:       r.SenderID,
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		CreatedAt:      r.CreatedAt.Unix(),
	}
	switch pb.Channel(r.Channel) {
	case pb.Channel_CHANNEL_EMAIL:
		rec.Vendor = &pb.MessageRecord_EmailVendor{EmailVendor: pb.EmailVendor(r.Vendor)}
	case pb.Channel_CHANNEL_SMS:
		rec.Vendor = &pb.MessageRecord_SmsVendor{SmsVendor: pb.SmsVendor(r.Vendor)}
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	return rec
}
```

- [ ] **Step 5: 验证 service 包编译**

```bash
go build ./internal/service/...
```

Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/service/query.go internal/service/service.go
git commit -m "$(cat <<'EOF'
refactor(service): query path splits vendor filter, response oneof

ListMessages/GetMessageStats pass EmailVendor + SmsVendor filters
separately. getMessageStats buckets VendorStat rows by channel into
EmailVendorStats + SmsVendorStats. toProtoRecord constructs the
MessageRecord oneof vendor case from Channel + Vendor int.
EOF
)"
```

---

### Task 8: service_test.go 改造

**Files:**
- Modify: `internal/service/service_test.go`

- [ ] **Step 1: 改 mockEmailProvider / mockSMSProvider 的 name 属性（保留 string）**

测试中 `&mockEmailProvider{name: "smtp"}` 和 `name: "mailgun"` 等保留——这些是 mock 内部 name，与 enum 解耦。无需修改。

但 `&mockEmailProvider{name: "mailgun"}` 这种使用要全部改为 enum 已知名（因为 mock name 在 sender SendResult 里作为 Vendor string 返回，最终经过 service 层不再有转换；但 persist 时直接用 req.GetVendor()，所以 mock name 实际不影响持久化）。

**结论：mock provider 的 name 字符串无需修改。**

- [ ] **Step 2: 改 SendEmailRequest.vendor 字面量**

把 `service_test.go` 中所有 `Vendor: "..."` 改为对应 enum。例如：

```go
// Before
resp, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
	To:      "user@example.com",
	Subject: "Test",
	Body:    "Body",
	Vendor:  "smtp",
	Account: "A",
})

// After
resp, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
	To:      "user@example.com",
	Subject: "Test",
	Body:    "Body",
	Vendor:  pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP,
	Account: "A",
})
```

用 sed 批量替换：

```bash
cd /Users/moss/code/base/message-service/internal/service
sed -i '' 's/Vendor:  "smtp"/Vendor:  pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP/g' service_test.go
sed -i '' 's/Vendor:  "mailgun"/Vendor:  pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP/g' service_test.go  # mock name 改不影响
sed -i '' 's/Vendor:  "aliyun"/Vendor:  pb.SmsVendor_SMS_VENDOR_ALIYUN/g' service_test.go
```

注意：以上 sed 是参考；实际改动需逐处确认（mock 字段而非 proto 字段不要改）。

- [ ] **Step 3: 改 Provider 断言为 vendor enum**

把所有 `pb.Provider_PROVIDER_*` 断言改为对应 vendor enum：

```go
// Before
assert.Equal(t, pb.Provider_PROVIDER_SMTP, resp.Provider)
assert.Equal(t, int32(pb.Provider_PROVIDER_SMTP), rec.Provider)

// After
// SendResponse 现在用 oneof vendor
assert.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, resp.GetEmailVendor())
// MessageRecord 也用 oneof
assert.Equal(t, pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, rec.GetEmailVendor())
```

注意 `resp.Provider` 字段已不存在；改用 `resp.GetEmailVendor()` / `resp.GetSmsVendor()`（proto oneof 生成的 getter）。

`rec.Provider` 同理改为 `rec.GetEmailVendor()` / `rec.GetSmsVendor()`。

- [ ] **Step 4: 改 fallback / select account 测试中的 vendor mock name**

`TestSendEmail_SelectAccount` 等用 `email.NewAccountRegistryFromProviders` 直接构造 registry 的测试，vendor 字段是 `*AccountProvider.Vendor`（仍 string），保留 `"smtp"` / `"mailgun"` 等字符串。但请求字段 `Vendor: "..."` 必须 enum。

具体：`service_test.go:359-360` 这种 `"mailgun": {"g": &email.AccountProvider{Vendor: "mailgun", ...}}` 保留（registry map key 是 string）。但 `service_test.go:384` `Vendor: "mailgun"` 改为 `Vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP`（虽然 mock name 是 mailgun 但请求侧用合法 enum）。

但这种 mismatch 让测试不真实——更干净的做法是把 mock name 也对齐到 enum 已知名。具体到这个测试，把所有 mockEmailProvider name "mailgun" 改成 "custom_smtp"，AccountProvider.Vendor 也改成 "custom_smtp"，让 fallback 链是 smtp→custom_smtp。

- [ ] **Step 5: 跑 service 测试**

```bash
go test ./internal/service/ -v
```

Expected: PASS。如有失败，按错误调整（多见于 oneof getter 名错误或字段类型 mismatch）。

- [ ] **Step 6: Commit**

```bash
git add internal/service/service_test.go
git commit -m "test(service): adapt to vendor enum + oneof response shape"
```

---

## Phase 5: 收尾

### Task 9: config.yaml 调整

**Files:**
- Modify: `config.yaml`

- [ ] **Step 1: 把 email.vendors.smtp 改为 custom_smtp**

打开 `config.yaml`，找到：

```yaml
email:
  vendors:
    smtp:
      accounts:
        - name: default
          host: smtp.example.com
          port: 587
          username: ""
          password: ""
          from: noreply@example.com
```

改为：

```yaml
email:
  vendors:
    custom_smtp:
      accounts:
        - name: default
          host: smtp.example.com
          port: 587
          username: ""
          password: ""
          from: noreply@example.com
    # aliyun:  # uncomment to use Aliyun mail (host auto-filled as smtp.aliyun.com)
    #   accounts:
    #     - name: default
    #       username: ""
    #       password: ""
    #       from: noreply@aliyun.com
```

SMS 部分 `aliyun:` key 保持不变（与 SmsVendor 小写形式一致）。

- [ ] **Step 2: Commit**

```bash
git add config.yaml
git commit -m "chore(config): rename email vendor smtp → custom_smtp to match enum"
```

---

### Task 10: 全量验证

- [ ] **Step 1: 全量 build**

```bash
cd /Users/moss/code/base/message-service
go build ./...
```

Expected: PASS。

- [ ] **Step 2: 全量 test**

```bash
go test ./...
```

Expected: PASS（所有包，包括 testcontainer 集成测试）。

- [ ] **Step 3: 全量 vet**

```bash
go vet ./...
```

Expected: PASS。

- [ ] **Step 4: 跑 migrate（验证 DB schema 升级）**

```bash
go run ./cmd/migrate/
```

Expected: 自动 migrate 把 `provider` 列重命名为 `vendor`（或新建 vendor 列；GORM AutoMigrate 对字段重命名的处理需观察）。如果 DB 已有旧数据，需手动 DROP TABLE 再跑（项目未上线）。

- [ ] **Step 5: Commit（如有 migrate 相关变更）**

如无变更，跳过。如有 schema 调整，单独 commit。

---

## Self-Review

**1. Spec coverage**：
- ✅ 删除 Provider enum → Task 1 Step 1
- ✅ EmailVendor + SmsVendor 分开 → Task 1 Step 1
- ✅ SendEmailRequest/SendSMSRequest vendor 字段 enum → Task 1 Step 2
- ✅ SendResponse oneof vendor → Task 1 Step 3
- ✅ MessageRecord oneof vendor → Task 1 Step 4
- ✅ ListMessagesRequest 拆 vendor filter → Task 1 Step 5
- ✅ GetMessageStatsRequest 拆 vendor filter → Task 1 Step 6
- ✅ MessageStatsResponse 拆 email_stats + sms_stats → Task 1 Step 7
- ✅ ProviderStats 拆 EmailVendorStats + SmsVendorStats → Task 1 Step 7
- ✅ CEL 校验 vendor == 0 → Task 1 Step 2
- ✅ MessageRecord.Provider → Vendor → Task 2
- ✅ gorm gen 重新生成 → Task 2 Step 2
- ✅ repository filter 拆 vendor → Task 3
- ✅ email registry buildProvider + defaultSMTPHost → Task 4
- ✅ sms registry 注释明确 vendor 名集合 → Task 5
- ✅ service 层 enum↔string 转换 → Task 6 Step 1
- ✅ send.go 删 *ProviderToProto + SendResponse oneof → Task 6
- ✅ query.go 改造 → Task 7
- ✅ service.go toProtoRecord oneof → Task 7 Step 4
- ✅ messageRepo 接口签名 → Task 7 Step 3
- ✅ 测试改造 → Task 8
- ✅ config.yaml vendor 名 → Task 9
- ✅ DB 重置（项目未上线） → Task 10 Step 4

**2. Placeholder scan**：
- Task 8 Step 2 的 sed 命令有"参考；实际改动需逐处确认"——这是显式声明该步骤需要人工判断（mock 字段 vs proto 字段区分）。这不是 placeholder，是该 task 的真实复杂度。
- 无 TBD/TODO/implement-later。

**3. Type consistency**：
- `EmailVendor` / `SmsVendor` proto 类型，service 用 `pb.EmailVendor` / `pb.SmsVendor`，repository 用 `int32` 转 — 一致
- `emailVendorToString` / `smsVendorToString` 返回 string，作为 registry map key — 一致
- `defaultSMTPHost(vendor string)` 输入 string，与 buildProvider 内部一致
- `VendorStat.Channel` 类型 `pb.Channel`，与 `VendorStat.Vendor` (int32) 配对解释 — 一致
- `MessageRecord_EmailVendor` / `MessageRecord_SmsVendor` 是 proto oneof case 类型名（buf 生成规则：`<MessageName>_<OneofCase>`），与 service.go toProtoRecord 中使用一致
- `SendResponse_EmailVendor` / `SendResponse_SmsVendor` 同上

**4. 跨 task 的字段名一致**：
- Task 2 重命名为 `Vendor` (model)，Task 3 SQL 用 `vendor` 列名（lower）— 一致
- Task 6 SendResponse 用 `Vendor: &pb.SendResponse_EmailVendor{...}`，与 Task 1 proto 定义一致

**5. Spec 中描述的"保留 mock vendor name"在 plan 中如何处理**：
- Task 8 Step 1 明确：mock provider 的 `name` 字符串保留（不影响 enum 化）
- Task 8 Step 4 明确：`AccountProvider.Vendor` 字段（仍 string）的值，倾向对齐 enum 已知名

Plan 完整，可以执行。
