# 拆分 Email/SMS 记录表 + 新增 Scene 场景枚举 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 `message_records` 单表拆为 `email_records` + `sms_records` 两张表，proto 拆为 `EmailRecord` / `SMSRecord` 两个 message（删除 `Channel` 和 `MessageRecord`），查询接口拆为 6 个 RPC（Get/List/Stats × Email/SMS），新增 `EmailScene` / `SmsScene` 必填枚举和 `sender_id` 必填字段，并扩展 `go-common/email.Message` 加可选 template 字段。

**Architecture:** 三层重构：① DB 层删旧表加新表（model 严格按 gorm skill §3 含 `DeletedAt`）；② proto 层彻底拆分，删除 oneof 和 Channel；③ service/dal 层按 channel 拆分文件，保持单一 `internal/service/message/` 子包。所有改动 TDD 风格，每个任务结束 commit。

**Tech Stack:** Go 1.25+ / GORM + gorm.io/cli / PostgreSQL / buf + grpc-gateway / go-common（local replace 到 `../go-common`）/ xerr + xcodes / testcontainers（via `dbx.SetupTestDB`）。

---

## 前置准备（一次性）

- [ ] **P1: 创建 worktree**

执行 brainstorming→writing-plans 流程要求在专用 worktree 中执行。本计划在主仓库 worktree 里完成。

```bash
git worktree add ../message-service-split-records -b feature/split-email-sms-records
cd ../message-service-split-records
```

---

## Phase 1: go-common 改动

### Task 1: 扩展 `go-common/email.Message` 加可选 template 字段

**Files:**
- Modify: `../go-common/message/email/sender.go`

**Why:** `EmailRecord` 要存 `template_id` / `template_params`，service 层把它们透传到 `emailcommon.Message`，go-common 必须 `Message` struct 提供对应字段。当前 vendor 实现（SMTP/Mailgun）不消费这两个字段，保持现状；未来 vendor 实现模板渲染时自行消费。

- [ ] **Step 1: 修改 `../go-common/message/email/sender.go`**

替换 `Message` struct 定义为：

```go
// Message represents an email to be sent.
type Message struct {
    To       string
    Cc       []string
    Bcc      []string
    Subject  string
    Body     string
    HTMLBody string
    ReplyTo  string

    // Template is an optional vendor-side template identifier. Vendors
    // that do not support templating (e.g. SMTP) ignore this field.
    Template string
    // TemplateParams supplies variable substitutions when Template is set.
    // Vendors that do not support templating ignore this field.
    TemplateParams map[string]string
}
```

- [ ] **Step 2: 验证 go-common 编译通过**

```bash
cd ../go-common
go build ./...
go test ./message/...
```

Expected: 编译通过，所有现有测试通过（因为字段是新增可选，不影响现有调用）。

- [ ] **Step 3: Commit go-common 改动**

```bash
cd ../go-common
git add message/email/sender.go
git commit -m "feat(email): add optional Template and TemplateParams fields to Message

Vendors that do not support templating ignore these fields. Prepares
for message-service email_records.template_id persistence."
```

- [ ] **Step 4: 回到 message-service worktree**

```bash
cd ../message-service-split-records
```

---

## Phase 2: proto 改动

### Task 2: 改写 `message.proto` 并重新生成代码

**Files:**
- Modify: `api/proto/message/v1/message.proto`
- Regenerate: `gen/message/v1/*.pb.go`, `gen/message/v1/*.pb.gw.go` (via `make proto`)

**Why:** 这是 wire 层的彻底重构。`Channel` / `MessageRecord` 删除；新增 `EmailScene` / `SmsScene` enum、`EmailRecord` / `SMSRecord` message、6 个新查询 RPC。发送请求加 `scene` / `sender_id` 必填字段（CEL）和 `template_id` / `template_params` 可选字段。

- [ ] **Step 1: 整体替换 `api/proto/message/v1/message.proto`**

文件完整内容如下（直接覆盖原文件）：

```proto
syntax = "proto3";

package message.v1;

import "buf/validate/validate.proto";
import "google/api/annotations.proto";

// MessageStatus represents the delivery status of a message.
enum MessageStatus {
  MESSAGE_STATUS_UNSPECIFIED = 0;
  MESSAGE_STATUS_PENDING = 1;
  MESSAGE_STATUS_SENT = 2;
  MESSAGE_STATUS_FAILED = 3;
}

// EmailVendor represents the email service brand.
enum EmailVendor {
  EMAIL_VENDOR_UNSPECIFIED = 0;
  EMAIL_VENDOR_CUSTOM_SMTP = 1;
  EMAIL_VENDOR_ALIYUN = 2;
  EMAIL_VENDOR_TENCENT = 3;
  EMAIL_VENDOR_NETEASE = 4;
}

// SmsVendor represents the SMS service brand.
enum SmsVendor {
  SMS_VENDOR_UNSPECIFIED = 0;
  SMS_VENDOR_ALIYUN = 1;
}

// EmailScene represents the business purpose of an email (login code,
// forgot-password, register verification, ...). Required on every send.
enum EmailScene {
  EMAIL_SCENE_UNSPECIFIED = 0;
  EMAIL_SCENE_LOGIN_CODE = 1;
  EMAIL_SCENE_FORGOT_PASSWORD = 2;
  EMAIL_SCENE_REGISTER = 3;
  EMAIL_SCENE_CHANGE_PASSWORD = 4;
  EMAIL_SCENE_BIND_ACCOUNT = 5;
  EMAIL_SCENE_NOTIFICATION = 6;
}

// SmsScene represents the business purpose of an SMS. Required on every send.
enum SmsScene {
  SMS_SCENE_UNSPECIFIED = 0;
  SMS_SCENE_LOGIN_CODE = 1;
  SMS_SCENE_FORGOT_PASSWORD = 2;
  SMS_SCENE_REGISTER = 3;
  SMS_SCENE_CHANGE_PASSWORD = 4;
  SMS_SCENE_BIND_ACCOUNT = 5;
}

// MessageService handles sending, recording, and querying messages.
service MessageService {
  // SendEmail sends an email via the configured vendor/account
  // (or the default fallback chain when both are unset).
  rpc SendEmail(SendEmailRequest) returns (SendResponse) {
    option (google.api.http) = {
      post: "/v1/messages:email"
      body: "*"
    };
  }

  // SendSMS sends an SMS via the configured vendor/account
  // (or routes by phone country code when both are unset).
  rpc SendSMS(SendSMSRequest) returns (SendResponse) {
    option (google.api.http) = {
      post: "/v1/messages:sms"
      body: "*"
    };
  }

  // GetEmail returns a single email record by ID.
  rpc GetEmail(GetEmailRequest) returns (EmailRecord) {
    option (google.api.http) = {
      get: "/v1/emails/{id}"
    };
  }

  // ListEmails returns a paginated list of email records matching the filter.
  rpc ListEmails(ListEmailsRequest) returns (ListEmailsResponse) {
    option (google.api.http) = {
      get: "/v1/emails"
    };
  }

  // GetEmailStats returns aggregated statistics for emails matching the filter.
  rpc GetEmailStats(GetEmailStatsRequest) returns (EmailStatsResponse) {
    option (google.api.http) = {
      get: "/v1/emails:stats"
    };
  }

  // GetSMS returns a single SMS record by ID.
  rpc GetSMS(GetSMSRequest) returns (SMSRecord) {
    option (google.api.http) = {
      get: "/v1/sms/{id}"
    };
  }

  // ListSMS returns a paginated list of SMS records matching the filter.
  rpc ListSMS(ListSMSRequest) returns (ListSMSResponse) {
    option (google.api.http) = {
      get: "/v1/sms"
    };
  }

  // GetSMSStats returns aggregated statistics for SMS messages matching the filter.
  rpc GetSMSStats(GetSMSStatsRequest) returns (SMSStatsResponse) {
    option (google.api.http) = {
      get: "/v1/sms:stats"
    };
  }
}

// SendEmailRequest is the request to send an email.
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
  // Template is optional: vendors that do not support templating ignore it.
  string template_id = 10;
  map<string, string> template_params = 11;
  // Scene is the business purpose of this email (required).
  EmailScene scene = 12;
  // SenderID identifies who/what triggered this send (user_id / admin_id /
  // system / module name). Required for audit traceability.
  string sender_id = 13;

  option (buf.validate.message).cel = {
    id: "vendor_account_pair",
    message: "vendor and account must both be set or both be empty",
    expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
  };
  option (buf.validate.message).cel = {
    id: "scene_required",
    message: "scene is required",
    expression: "this.scene != 0"
  };
  option (buf.validate.message).cel = {
    id: "sender_required",
    message: "sender_id is required",
    expression: "this.sender_id != ''"
  };
}

// SendSMSRequest is the request to send an SMS.
message SendSMSRequest {
  string to = 1 [(buf.validate.field).string.min_len = 1];
  string content = 2;
  string template_id = 3;
  map<string, string> template_params = 4;
  // Vendor + Account optionally select a specific configured account.
  // Both must be set together; if both zero/empty, sender routes by phone country.
  SmsVendor vendor = 5;
  string account = 6;
  // Scene is the business purpose of this SMS (required).
  SmsScene scene = 7;
  // SenderID identifies who/what triggered this send (required for audit).
  string sender_id = 8;

  option (buf.validate.message).cel = {
    id: "vendor_account_pair",
    message: "vendor and account must both be set or both be empty",
    expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
  };
  option (buf.validate.message).cel = {
    id: "scene_required",
    message: "scene is required",
    expression: "this.scene != 0"
  };
  option (buf.validate.message).cel = {
    id: "sender_required",
    message: "sender_id is required",
    expression: "this.sender_id != ''"
  };
}

// SendResponse is the response from SendEmail and SendSMS.
message SendResponse {
  int64 id = 1;
  MessageStatus status = 2;
  // vendor reports which vendor actually handled the send.
  oneof vendor {
    EmailVendor email_vendor = 3;
    SmsVendor sms_vendor = 4;
  }
}

// GetEmailRequest is the request to fetch a single email record.
message GetEmailRequest {
  int64 id = 1 [(buf.validate.field).int64.gt = 0];
}

// GetSMSRequest is the request to fetch a single SMS record.
message GetSMSRequest {
  int64 id = 1 [(buf.validate.field).int64.gt = 0];
}

// EmailRecord is the stored record of a sent email.
message EmailRecord {
  int64 id = 1;
  EmailVendor vendor = 2;
  string account = 3;
  EmailScene scene = 4;
  MessageStatus status = 5;
  string target = 6;
  string sender_id = 7;
  repeated string cc = 8;
  repeated string bcc = 9;
  string subject = 10;
  string content = 11;
  string html_body = 12;
  string reply_to = 13;
  string template_id = 14;
  map<string, string> template_params = 15;
  string error_message = 16;
  int32 attempts = 17;
  int64 sent_at = 18;
  int64 created_at = 19;
  int64 updated_at = 20;
}

// SMSRecord is the stored record of a sent SMS.
message SMSRecord {
  int64 id = 1;
  SmsVendor vendor = 2;
  string account = 3;
  SmsScene scene = 4;
  MessageStatus status = 5;
  string target = 6;
  string sender_id = 7;
  string content = 8;
  string template_id = 9;
  map<string, string> template_params = 10;
  string error_message = 11;
  int32 attempts = 12;
  int64 sent_at = 13;
  int64 created_at = 14;
  int64 updated_at = 15;
}

// ListEmailsRequest filters and paginates a ListEmails query.
message ListEmailsRequest {
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  MessageStatus status = 3;
  string target = 4;
  string sender_id = 5;
  int64 start_time = 6;
  int64 end_time = 7;
  int32 page = 8;
  int32 page_size = 9;
}

// ListEmailsResponse contains a page of email records plus total count.
message ListEmailsResponse {
  repeated EmailRecord records = 1;
  int32 total = 2;
}

// ListSMSRequest filters and paginates a ListSMS query.
message ListSMSRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string target = 4;
  string sender_id = 5;
  int64 start_time = 6;
  int64 end_time = 7;
  int32 page = 8;
  int32 page_size = 9;
}

// ListSMSResponse contains a page of SMS records plus total count.
message ListSMSResponse {
  repeated SMSRecord records = 1;
  int32 total = 2;
}

// GetEmailStatsRequest filters a GetEmailStats query.
message GetEmailStatsRequest {
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  int64 start_time = 3;
  int64 end_time = 4;
}

// GetSMSStatsRequest filters a GetSMSStats query.
message GetSMSStatsRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  int64 start_time = 3;
  int64 end_time = 4;
}

// EmailStatsResponse contains aggregated email statistics.
message EmailStatsResponse {
  int64 total = 1;
  int64 sent = 2;
  int64 failed = 3;
  double success_rate = 4;
  repeated EmailVendorStats vendors = 5;
}

// SMSStatsResponse contains aggregated SMS statistics.
message SMSStatsResponse {
  int64 total = 1;
  int64 sent = 2;
  int64 failed = 3;
  double success_rate = 4;
  repeated SmsVendorStats vendors = 5;
}

// EmailVendorStats holds per-vendor statistics for email messages.
message EmailVendorStats {
  EmailVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}

// SmsVendorStats holds per-vendor statistics for SMS messages.
message SmsVendorStats {
  SmsVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}
```

- [ ] **Step 2: 运行 buf generate 重新生成代码**

```bash
make proto
```

Expected: `gen/message/v1/*.pb.go` 和 `*.pb.gw.go` 被重写，无错误。生成代码包含 `EmailRecord` / `SMSRecord` / `EmailScene` / `SmsScene` 类型，`MessageServiceServer` 接口包含 8 个方法（SendEmail / SendSMS / GetEmail / ListEmails / GetEmailStats / GetSMS / ListSMS / GetSMSStats）。

- [ ] **Step 3: 验证 buf 格式**

```bash
buf lint
buf format -d
```

Expected: 无 lint 错误，`buf format -d` 输出为空（格式已对齐）。

- [ ] **Step 4: Commit proto + 生成代码**

```bash
git add api/proto/message/v1/message.proto gen/
git commit -m "refactor(proto): split message_records, add scene enums and sender_id

- Delete Channel enum and MessageRecord message
- Add EmailScene (6 values) and SmsScene (5 values) enums
- Add EmailRecord and SMSRecord messages
- Split GetMessage/ListMessages/GetMessageStats into 6 RPCs
  (Get/List/Stats x Email/SMS)
- SendEmail/SendSMS requests now require scene and sender_id (CEL)
- SendEmail/SendSMS requests add optional template_id/template_params
- HTTP routes: /v1/emails/* and /v1/sms/* for queries"
```

**注意：** 此时 `go build ./...` 会失败（因为 service / handler / dal 还在引用已删除的 `pb.MessageRecord` / `pb.Channel` 等）。这是预期的——下面 Phase 3+ 会修复。

---

## Phase 3: store/models 层

### Task 3: 抽取共享类型到 `models/types.go`

**Files:**
- Create: `internal/store/models/types.go`

**Why:** `EmailRecord` 和 `SMSRecord` 都需要 `MapStringString` / `StringSlice` 两个 JSONB 类型，从 `message_record.go` 抽出来到独立文件，便于后续 `message_record.go` 整体删除。

- [ ] **Step 1: 创建 `internal/store/models/types.go`**

```go
package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// MapStringString is a JSONB-compatible map for template parameters.
type MapStringString map[string]string

// Scan implements sql.Scanner for JSONB.
func (m *MapStringString) Scan(value any) error {
	if value == nil {
		*m = nil
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("failed to unmarshal JSONB value: %v", value)
	}
	return json.Unmarshal(bytes, m)
}

// Value implements driver.Valuer for JSONB.
func (m MapStringString) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// StringSlice is a JSONB-compatible string slice for list fields like Cc/Bcc.
type StringSlice []string

// Scan implements sql.Scanner for JSONB.
func (s *StringSlice) Scan(value any) error {
	if value == nil {
		*s = nil
		return nil
	}
	bytes, ok := value.([]byte)
	if !ok {
		return fmt.Errorf("failed to unmarshal JSONB value: %v", value)
	}
	return json.Unmarshal(bytes, s)
}

// Value implements driver.Valuer for JSONB.
func (s StringSlice) Value() (driver.Value, error) {
	if s == nil {
		return nil, nil
	}
	return json.Marshal(s)
}
```

- [ ] **Step 2: 暂不 commit**——下一步删除 `message_record.go` 时会顺手移除原定义，避免重复。

---

### Task 4: 新建 `models/email_record.go`

**Files:**
- Create: `internal/store/models/email_record.go`
- Create: `internal/store/models/email_record_query.go`

**Why:** 邮件侧的 GORM model + 用于 vendor stats 聚合的 raw SQL 模板接口。

- [ ] **Step 1: 创建 `internal/store/models/email_record.go`**

```go
package models

import (
	"database/sql"
	"time"

	"gorm.io/gorm"
)

// EmailRecord stores a complete record of every email sent through the service.
type EmailRecord struct {
	ID             int64           `gorm:"primaryKey"`
	Vendor         int32           `gorm:"not null;default:0;index"`
	Account        string          `gorm:"size:64;column:account"`
	Scene          int32           `gorm:"not null;default:0;index"`
	Status         int32           `gorm:"not null;default:0;index"`
	Target         string          `gorm:"size:255;not null;index"`
	SenderID       string          `gorm:"size:64;column:sender_id;index"`
	Cc             StringSlice     `gorm:"type:jsonb;column:cc"`
	Bcc            StringSlice     `gorm:"type:jsonb;column:bcc"`
	Subject        string          `gorm:"type:text"`
	Content        string          `gorm:"type:text"`
	HTMLBody       string          `gorm:"type:text;column:html_body"`
	ReplyTo        string          `gorm:"size:255;column:reply_to"`
	TemplateID     string          `gorm:"size:64;column:template_id"`
	TemplateParams MapStringString `gorm:"type:jsonb;column:template_params"`
	ErrorMessage   string          `gorm:"column:error_message"`
	Attempts       int             `gorm:"not null;default:1"`
	SentAt         sql.NullTime    `gorm:"column:sent_at"`
	CreatedAt      time.Time       `gorm:"not null;default:now();index"`
	UpdatedAt      time.Time       `gorm:"not null;default:now()"`
	DeletedAt      gorm.DeletedAt  `gorm:"index"`
}
```

- [ ] **Step 2: 创建 `internal/store/models/email_record_query.go`**

```go
package models

import "time"

// EmailStatsRow is the scan target for typed raw SQL aggregations over the
// email_records table. It is NOT a GORM model — only used as the row type
// for the EmailRecordStatsQuery interface so gorm gen can produce a typed
// VendorStats method returning []EmailStatsRow.
type EmailStatsRow struct {
	Vendor int32
	Total  int64
	Sent   int64
	Failed int64
}

// EmailRecordStatsQuery defines typed raw SQL for per-vendor email stats
// aggregation. gorm gen discovers this interface by name (prefix matches
// the EmailRecord model) and generates a typed method on generated.Query.
//
// Splitting per-channel removes the COALESCE/NULLIF workaround that the
// old MessageRecordStatsQuery needed to merge email_vendor and sms_vendor
// columns — each table now has a single vendor column, so GROUP BY vendor
// is direct.
//
// startTime/endTime are *time.Time pointers; the template uses `!= nil`
// checks to conditionally include the predicates.
type EmailRecordStatsQuery[T any] interface {
	// SELECT
	//   vendor,
	//   COUNT(*) AS total,
	//   COUNT(*) FILTER (WHERE status = @sentStatus) AS sent,
	//   COUNT(*) FILTER (WHERE status = @failedStatus) AS failed
	// FROM email_records
	// {{where}}
	//   {{if vendor > 0}} vendor = @vendor {{end}}
	//   {{if scene > 0}} AND scene = @scene {{end}}
	//   {{if startTime != nil}} AND created_at >= @startTime {{end}}
	//   {{if endTime != nil}} AND created_at <= @endTime {{end}}
	// {{end}}
	// GROUP BY vendor
	VendorStats(
		sentStatus int32,
		failedStatus int32,
		vendor int32,
		scene int32,
		startTime *time.Time,
		endTime *time.Time,
	) ([]T, error)
}
```

---

### Task 5: 新建 `models/sms_record.go`

**Files:**
- Create: `internal/store/models/sms_record.go`
- Create: `internal/store/models/sms_record_query.go`

- [ ] **Step 1: 创建 `internal/store/models/sms_record.go`**

```go
package models

import (
	"database/sql"
	"time"

	"gorm.io/gorm"
)

// SMSRecord stores a complete record of every SMS sent through the service.
type SMSRecord struct {
	ID             int64           `gorm:"primaryKey"`
	Vendor         int32           `gorm:"not null;default:0;index"`
	Account        string          `gorm:"size:64;column:account"`
	Scene          int32           `gorm:"not null;default:0;index"`
	Status         int32           `gorm:"not null;default:0;index"`
	Target         string          `gorm:"size:255;not null;index"`
	SenderID       string          `gorm:"size:64;column:sender_id;index"`
	Content        string          `gorm:"type:text"`
	TemplateID     string          `gorm:"size:64;column:template_id"`
	TemplateParams MapStringString `gorm:"type:jsonb;column:template_params"`
	ErrorMessage   string          `gorm:"column:error_message"`
	Attempts       int             `gorm:"not null;default:1"`
	SentAt         sql.NullTime    `gorm:"column:sent_at"`
	CreatedAt      time.Time       `gorm:"not null;default:now();index"`
	UpdatedAt      time.Time       `gorm:"not null;default:now()"`
	DeletedAt      gorm.DeletedAt  `gorm:"index"`
}
```

- [ ] **Step 2: 创建 `internal/store/models/sms_record_query.go`**

```go
package models

import "time"

// SmsStatsRow is the scan target for typed raw SQL aggregations over the
// sms_records table. Not a GORM model.
type SmsStatsRow struct {
	Vendor int32
	Total  int64
	Sent   int64
	Failed int64
}

// SMSRecordStatsQuery defines typed raw SQL for per-vendor SMS stats
// aggregation. See EmailRecordStatsQuery doc for design notes.
type SMSRecordStatsQuery[T any] interface {
	// SELECT
	//   vendor,
	//   COUNT(*) AS total,
	//   COUNT(*) FILTER (WHERE status = @sentStatus) AS sent,
	//   COUNT(*) FILTER (WHERE status = @failedStatus) AS failed
	// FROM sms_records
	// {{where}}
	//   {{if vendor > 0}} vendor = @vendor {{end}}
	//   {{if scene > 0}} AND scene = @scene {{end}}
	//   {{if startTime != nil}} AND created_at >= @startTime {{end}}
	//   {{if endTime != nil}} AND created_at <= @endTime {{end}}
	// {{end}}
	// GROUP BY vendor
	VendorStats(
		sentStatus int32,
		failedStatus int32,
		vendor int32,
		scene int32,
		startTime *time.Time,
		endTime *time.Time,
	) ([]T, error)
}
```

---

### Task 6: 删除 `message_record.go`，更新 `genconfig.go`

**Files:**
- Delete: `internal/store/models/message_record.go`
- Modify: `internal/store/models/genconfig.go`

**Why:** 旧 model 已经被 EmailRecord + SMSRecord 取代，必须删除以避免 gorm gen 重复生成冲突。`genconfig.go` 的 `AllModels()` 改为返回新 model。

- [ ] **Step 1: 删除 `internal/store/models/message_record.go`**

```bash
rm internal/store/models/message_record.go
```

- [ ] **Step 2: 修改 `internal/store/models/genconfig.go` 的 `AllModels()`**

打开 `internal/store/models/genconfig.go`，把 `AllModels` 函数替换为：

```go
// AllModels returns all GORM models for AutoMigrate.
func AllModels() []any {
	return []any{
		&EmailRecord{},
		&SMSRecord{},
	}
}
```

文件顶部 genconfig.Config 那块（FieldTypeMap）保持不变。

- [ ] **Step 3: 运行 gorm gen 重新生成代码**

```bash
make generate
```

Expected: `internal/store/generated/` 下生成 `email_record.go`、`sms_record.go`、`email_record_query.go`、`sms_record_query.go`；旧的 `message_record.go` 在 generated 目录里**还残留**（gorm gen 不会自动清理）。

- [ ] **Step 4: 手动删除 generated 目录里的旧文件**

```bash
rm internal/store/generated/message_record.go
```

- [ ] **Step 5: 验证 models 包编译通过**

```bash
go build ./internal/store/models/...
```

Expected: 编译通过。

- [ ] **Step 6: Commit models 层改动**

```bash
git add internal/store/models/ internal/store/generated/
git commit -m "refactor(store): split message_record model into email_record and sms_record

- Create models/types.go with shared MapStringString and StringSlice
- Add EmailRecord and SMSRecord models (DeletedAt per gorm skill §3)
- Add EmailRecordStatsQuery and SMSRecordStatsQuery template interfaces
- Delete models/message_record.go and generated/message_record.go
- AllModels() now returns EmailRecord and SMSRecord"
```

**注意：** 此时 `go build ./...` 仍会失败，因为 dal / service 还在引用 `models.MessageRecord`。下面 Phase 4 修复。

---

## Phase 4: dal 层

### Task 7: 新建 `dal/email_record.go`（TDD）

**Files:**
- Create: `internal/store/dal/email_record.go`
- Create: `internal/store/dal/email_record_test.go`

**Why:** 邮件侧 DAL：Create / Get / List（含 scene 过滤）/ CountStats / ListVendorStats。所有函数 package-level，第一个参数为 `ctx`，第二个为 `tx *gorm.DB`（与现有约定一致）。

- [ ] **Step 1: 创建测试文件 `internal/store/dal/email_record_test.go`**

```go
package dal

import (
	"context"
	"testing"
	"time"

	"message-service/internal/store/models"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupEmailDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	err := db.AutoMigrate(&models.EmailRecord{})
	require.NoError(t, err, "auto-migrate should succeed")
	return db
}

func newTestEmailRecord(status int32, scene int32, target string, vendor int32) *models.EmailRecord {
	return &models.EmailRecord{
		ID:      time.Now().UnixNano(),
		Vendor:  vendor,
		Scene:   scene,
		Status:  status,
		Target:  target,
		Subject: "Test Subject",
		Content: "Test content body",
		SenderID: "user:42",
		Attempts: 1,
	}
}

func TestCreateEmailRecord(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	record := newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"user@example.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)
	require.NoError(t, CreateEmailRecord(ctx, db, record))

	found, err := GetEmailRecord(ctx, db, record.ID)
	require.NoError(t, err)
	assert.Equal(t, record.ID, found.ID)
	assert.Equal(t, int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP), found.Vendor)
	assert.Equal(t, int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE), found.Scene)
	assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), found.Status)
	assert.Equal(t, "user@example.com", found.Target)
	assert.Equal(t, "user:42", found.SenderID)
	assert.Equal(t, "Test Subject", found.Subject)
	assert.Equal(t, 1, found.Attempts)
	assert.False(t, found.CreatedAt.IsZero())
	assert.False(t, found.UpdatedAt.IsZero())
}

func TestGetEmailRecord_NotFound(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	_, err := GetEmailRecord(ctx, db, 99999999)
	assert.Error(t, err)
}

func TestListEmailRecords_ByScene(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_REGISTER),
		"c@d.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)))

	records, total, err := ListEmailRecords(ctx, db, EmailListFilter{
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		Page:     1,
		PageSize: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Len(t, records, 1)
	assert.Equal(t, "a@b.com", records[0].Target)
}

func TestListEmailRecords_ByVendor(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"c@d.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_ALIYUN),
	)))

	records, total, err := ListEmailRecords(ctx, db, EmailListFilter{
		Vendor:   pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP,
		Page:     1,
		PageSize: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Len(t, records, 1)
}

func TestListEmailRecords_PageSizeClamped(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
			int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
			int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
			"user@example.com",
			int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
		)))
	}

	records, _, err := ListEmailRecords(ctx, db, EmailListFilter{
		Page:     1,
		PageSize: 1000,
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(records), 100)
	assert.Len(t, records, 5)
}

func TestCountEmailStats(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"b@c.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_FAILED),
		int32(pb.EmailScene_EMAIL_SCENE_REGISTER),
		"d@e.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_ALIYUN),
	)))

	stats, err := CountEmailStats(ctx, db, EmailStatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(3), stats.Total)
	assert.Equal(t, int64(2), stats.Sent)
	assert.Equal(t, int64(1), stats.Failed)
	assert.InDelta(t, 66.67, stats.SuccessRate, 0.1)
}

func TestListEmailVendorStats(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE),
		"a@b.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP),
	)))
	require.NoError(t, CreateEmailRecord(ctx, db, newTestEmailRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_FAILED),
		int32(pb.EmailScene_EMAIL_SCENE_REGISTER),
		"c@d.com",
		int32(pb.EmailVendor_EMAIL_VENDOR_ALIYUN),
	)))

	stats, err := ListEmailVendorStats(ctx, db, EmailStatsFilter{})
	require.NoError(t, err)
	require.Len(t, stats, 2)
}
```

- [ ] **Step 2: 运行测试看失败**

```bash
go test ./internal/store/dal/ -run TestCreateEmailRecord -v
```

Expected: 编译失败——`CreateEmailRecord` / `GetEmailRecord` / `ListEmailRecords` / `EmailListFilter` / `CountEmailStats` / `EmailStatsFilter` / `ListEmailVendorStats` 都未定义。

- [ ] **Step 3: 创建 `internal/store/dal/email_record.go`**

```go
package dal

import (
	"context"
	"errors"
	"time"

	pb "message-service/gen/message/v1"
	"message-service/internal/store/generated"
	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"gorm.io/gorm"
)

// EmailListFilter holds parameters for listing email records.
type EmailListFilter struct {
	Vendor    pb.EmailVendor
	Scene     pb.EmailScene
	Status    pb.MessageStatus
	Target    string
	SenderID  string
	StartTime *time.Time
	EndTime   *time.Time
	Page      int32
	PageSize  int32
}

// EmailStatsFilter holds parameters for querying email statistics.
type EmailStatsFilter struct {
	Vendor    pb.EmailVendor
	Scene     pb.EmailScene
	StartTime *time.Time
	EndTime   *time.Time
}

// EmailVendorStat contains per-vendor email statistics.
type EmailVendorStat struct {
	Vendor pb.EmailVendor
	Total  int64
	Sent   int64
	Failed int64
}

// CreateEmailRecord inserts a new email record. record.ID is backfilled
// on success.
func CreateEmailRecord(ctx context.Context, tx *gorm.DB, record *models.EmailRecord) error {
	if err := gorm.G[models.EmailRecord](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetEmailRecord returns the email record with the given ID, or
// xcodes.ErrEmailNotFound when no such record exists.
func GetEmailRecord(ctx context.Context, tx *gorm.DB, id int64) (*models.EmailRecord, error) {
	record, err := gorm.G[models.EmailRecord](tx).
		Where(generated.EmailRecord.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrEmailNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListEmailRecords returns a page of email records matching filter, along
// with the total count. page_size is clamped to dbx.MaxPageSize.
func ListEmailRecords(ctx context.Context, tx *gorm.DB, filter EmailListFilter) ([]*models.EmailRecord, int64, error) {
	pageSize := dbx.ClampPageSize(int(filter.PageSize))
	if filter.Page < 1 {
		filter.Page = 1
	}

	q := applyEmailListFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter)
	total, err := q.Count(ctx, generated.EmailRecord.ID.Column().Name)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	q = applyEmailListFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter)
	offset := int((filter.Page - 1) * int32(pageSize))
	results, err := q.
		Order(generated.EmailRecord.CreatedAt.Desc()).
		Offset(offset).
		Limit(pageSize).
		Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.EmailRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}
	return records, total, nil
}

// CountEmailStats returns aggregated email statistics matching filter.
func CountEmailStats(ctx context.Context, tx *gorm.DB, filter EmailStatsFilter) (*Stats, error) {
	total, err := applyEmailStatsFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter).
		Count(ctx, generated.EmailRecord.ID.Column().Name)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	sent, err := applyEmailStatsFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter).
		Where(generated.EmailRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_SENT))).
		Count(ctx, generated.EmailRecord.ID.Column().Name)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	failed, err := applyEmailStatsFilter(gorm.G[models.EmailRecord](tx).Where(generated.EmailRecord.ID.Gt(0)), filter).
		Where(generated.EmailRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_FAILED))).
		Count(ctx, generated.EmailRecord.ID.Column().Name)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	var successRate float64
	if total > 0 {
		successRate = float64(sent) / float64(total) * 100
	}

	return &Stats{
		Total:       total,
		Sent:        sent,
		Failed:      failed,
		SuccessRate: successRate,
	}, nil
}

// ListEmailVendorStats returns per-vendor email statistics matching filter.
func ListEmailVendorStats(ctx context.Context, tx *gorm.DB, filter EmailStatsFilter) ([]EmailVendorStat, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	rows, err := generated.EmailRecordStatsQuery[models.EmailStatsRow](tx).VendorStats(
		ctx,
		sentStatus, failedStatus,
		int32(filter.Vendor), int32(filter.Scene),
		filter.StartTime, filter.EndTime,
	)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	stats := make([]EmailVendorStat, len(rows))
	for i, r := range rows {
		stats[i] = EmailVendorStat{
			Vendor: pb.EmailVendor(r.Vendor),
			Total:  r.Total,
			Sent:   r.Sent,
			Failed: r.Failed,
		}
	}
	return stats, nil
}

// --- internal helpers ---

func applyEmailListFilter(q gorm.ChainInterface[models.EmailRecord], f EmailListFilter) gorm.ChainInterface[models.EmailRecord] {
	if f.Vendor != 0 {
		q = q.Where(generated.EmailRecord.Vendor.Eq(int32(f.Vendor)))
	}
	if f.Scene != 0 {
		q = q.Where(generated.EmailRecord.Scene.Eq(int32(f.Scene)))
	}
	if f.Status != 0 {
		q = q.Where(generated.EmailRecord.Status.Eq(int32(f.Status)))
	}
	if f.Target != "" {
		q = q.Where(generated.EmailRecord.Target.Eq(f.Target))
	}
	if f.SenderID != "" {
		q = q.Where(generated.EmailRecord.SenderID.Eq(f.SenderID))
	}
	if f.StartTime != nil {
		q = q.Where(generated.EmailRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.EmailRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}

func applyEmailStatsFilter(q gorm.ChainInterface[models.EmailRecord], f EmailStatsFilter) gorm.ChainInterface[models.EmailRecord] {
	if f.Vendor != 0 {
		q = q.Where(generated.EmailRecord.Vendor.Eq(int32(f.Vendor)))
	}
	if f.Scene != 0 {
		q = q.Where(generated.EmailRecord.Scene.Eq(int32(f.Scene)))
	}
	if f.StartTime != nil {
		q = q.Where(generated.EmailRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.EmailRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}
```

**注意：** 这里引用了 `xcodes.ErrEmailNotFound`，该错误码在 Phase 7（Task 15）才创建。为避免编译错误，**此 Task 顺序需要调整**——先做 Task 15（xcodes）再做 Task 7。见下方重排序。

- [ ] **Step 4: 跳过运行测试**——xcodes 还未拆分，编译会失败。先去 Task 15（xcodes），完成后再回来跑测试。

---

### Task 8: 新建 `dal/sms_record.go`（TDD）

**Files:**
- Create: `internal/store/dal/sms_record.go`
- Create: `internal/store/dal/sms_record_test.go`

- [ ] **Step 1: 创建测试文件 `internal/store/dal/sms_record_test.go`**

```go
package dal

import (
	"context"
	"testing"
	"time"

	"message-service/internal/store/models"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupSMSDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	err := db.AutoMigrate(&models.SMSRecord{})
	require.NoError(t, err, "auto-migrate should succeed")
	return db
}

func newTestSMSRecord(status int32, scene int32, target string, vendor int32) *models.SMSRecord {
	return &models.SMSRecord{
		ID:       time.Now().UnixNano(),
		Vendor:   vendor,
		Scene:    scene,
		Status:   status,
		Target:   target,
		Content:  "Your code: 1234",
		SenderID: "user:42",
		Attempts: 1,
	}
}

func TestCreateSMSRecord(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	record := newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"+8613800001111",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)
	require.NoError(t, CreateSMSRecord(ctx, db, record))

	found, err := GetSMSRecord(ctx, db, record.ID)
	require.NoError(t, err)
	assert.Equal(t, record.ID, found.ID)
	assert.Equal(t, int32(pb.SmsVendor_SMS_VENDOR_ALIYUN), found.Vendor)
	assert.Equal(t, int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), found.Scene)
	assert.Equal(t, int32(pb.MessageStatus_MESSAGE_STATUS_SENT), found.Status)
	assert.Equal(t, "+8613800001111", found.Target)
	assert.Equal(t, "user:42", found.SenderID)
	assert.Equal(t, "Your code: 1234", found.Content)
	assert.Equal(t, 1, found.Attempts)
}

func TestGetSMSRecord_NotFound(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	_, err := GetSMSRecord(ctx, db, 99999999)
	assert.Error(t, err)
}

func TestListSMSRecords_ByScene(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"+8613800001111",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_REGISTER),
		"+8613800002222",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)))

	records, total, err := ListSMSRecords(ctx, db, SmsListFilter{
		Scene:    pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		Page:     1,
		PageSize: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	assert.Len(t, records, 1)
	assert.Equal(t, "+8613800001111", records[0].Target)
}

func TestCountSMSStats(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"+8613800001111",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_FAILED),
		int32(pb.SmsScene_SMS_SCENE_REGISTER),
		"+8613800002222",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)))

	stats, err := CountSMSStats(ctx, db, SmsStatsFilter{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), stats.Total)
	assert.Equal(t, int64(1), stats.Sent)
	assert.Equal(t, int64(1), stats.Failed)
	assert.InDelta(t, 50.0, stats.SuccessRate, 0.1)
}

func TestListSMSVendorStats(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"+8613800001111",
		int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
	)))

	stats, err := ListSMSVendorStats(ctx, db, SmsStatsFilter{})
	require.NoError(t, err)
	require.Len(t, stats, 1)
	assert.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, stats[0].Vendor)
}
```

- [ ] **Step 2: 跳过运行测试**——`dal/sms_record.go` 还没实现，先实现再跑。

- [ ] **Step 3: 创建 `internal/store/dal/sms_record.go`**

```go
package dal

import (
	"context"
	"errors"
	"time"

	pb "message-service/gen/message/v1"
	"message-service/internal/store/generated"
	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	"github.com/servekit/go-common/dbx"

	"gorm.io/gorm"
)

// SmsListFilter holds parameters for listing SMS records.
type SmsListFilter struct {
	Vendor    pb.SmsVendor
	Scene     pb.SmsScene
	Status    pb.MessageStatus
	Target    string
	SenderID  string
	StartTime *time.Time
	EndTime   *time.Time
	Page      int32
	PageSize  int32
}

// SmsStatsFilter holds parameters for querying SMS statistics.
type SmsStatsFilter struct {
	Vendor    pb.SmsVendor
	Scene     pb.SmsScene
	StartTime *time.Time
	EndTime   *time.Time
}

// SmsVendorStat contains per-vendor SMS statistics.
type SmsVendorStat struct {
	Vendor pb.SmsVendor
	Total  int64
	Sent   int64
	Failed int64
}

// CreateSMSRecord inserts a new SMS record. record.ID is backfilled on success.
func CreateSMSRecord(ctx context.Context, tx *gorm.DB, record *models.SMSRecord) error {
	if err := gorm.G[models.SMSRecord](tx).Create(ctx, record); err != nil {
		return xcodes.ErrInternal.Wrap(err)
	}
	return nil
}

// GetSMSRecord returns the SMS record with the given ID, or
// xcodes.ErrSMSNotFound when no such record exists.
func GetSMSRecord(ctx context.Context, tx *gorm.DB, id int64) (*models.SMSRecord, error) {
	record, err := gorm.G[models.SMSRecord](tx).
		Where(generated.SMSRecord.ID.Eq(id)).
		Take(ctx)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, xcodes.ErrSMSNotFound.New()
		}
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return &record, nil
}

// ListSMSRecords returns a page of SMS records matching filter, along
// with the total count. page_size is clamped to dbx.MaxPageSize.
func ListSMSRecords(ctx context.Context, tx *gorm.DB, filter SmsListFilter) ([]*models.SMSRecord, int64, error) {
	pageSize := dbx.ClampPageSize(int(filter.PageSize))
	if filter.Page < 1 {
		filter.Page = 1
	}

	q := applySMSListFilter(gorm.G[models.SMSRecord](tx).Where(generated.SMSRecord.ID.Gt(0)), filter)
	total, err := q.Count(ctx, generated.SMSRecord.ID.Column().Name)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	q = applySMSListFilter(gorm.G[models.SMSRecord](tx).Where(generated.SMSRecord.ID.Gt(0)), filter)
	offset := int((filter.Page - 1) * int32(pageSize))
	results, err := q.
		Order(generated.SMSRecord.CreatedAt.Desc()).
		Offset(offset).
		Limit(pageSize).
		Find(ctx)
	if err != nil {
		return nil, 0, xcodes.ErrInternal.Wrap(err)
	}

	records := make([]*models.SMSRecord, len(results))
	for i := range results {
		records[i] = &results[i]
	}
	return records, total, nil
}

// CountSMSStats returns aggregated SMS statistics matching filter.
func CountSMSStats(ctx context.Context, tx *gorm.DB, filter SmsStatsFilter) (*Stats, error) {
	total, err := applySMSStatsFilter(gorm.G[models.SMSRecord](tx).Where(generated.SMSRecord.ID.Gt(0)), filter).
		Count(ctx, generated.SMSRecord.ID.Column().Name)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	sent, err := applySMSStatsFilter(gorm.G[models.SMSRecord](tx).Where(generated.SMSRecord.ID.Gt(0)), filter).
		Where(generated.SMSRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_SENT))).
		Count(ctx, generated.SMSRecord.ID.Column().Name)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	failed, err := applySMSStatsFilter(gorm.G[models.SMSRecord](tx).Where(generated.SMSRecord.ID.Gt(0)), filter).
		Where(generated.SMSRecord.Status.Eq(int32(pb.MessageStatus_MESSAGE_STATUS_FAILED))).
		Count(ctx, generated.SMSRecord.ID.Column().Name)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	var successRate float64
	if total > 0 {
		successRate = float64(sent) / float64(total) * 100
	}

	return &Stats{
		Total:       total,
		Sent:        sent,
		Failed:      failed,
		SuccessRate: successRate,
	}, nil
}

// ListSMSVendorStats returns per-vendor SMS statistics matching filter.
func ListSMSVendorStats(ctx context.Context, tx *gorm.DB, filter SmsStatsFilter) ([]SmsVendorStat, error) {
	sentStatus := int32(pb.MessageStatus_MESSAGE_STATUS_SENT)
	failedStatus := int32(pb.MessageStatus_MESSAGE_STATUS_FAILED)

	rows, err := generated.SMSRecordStatsQuery[models.SmsStatsRow](tx).VendorStats(
		ctx,
		sentStatus, failedStatus,
		int32(filter.Vendor), int32(filter.Scene),
		filter.StartTime, filter.EndTime,
	)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}

	stats := make([]SmsVendorStat, len(rows))
	for i, r := range rows {
		stats[i] = SmsVendorStat{
			Vendor: pb.SmsVendor(r.Vendor),
			Total:  r.Total,
			Sent:   r.Sent,
			Failed: r.Failed,
		}
	}
	return stats, nil
}

// --- internal helpers ---

func applySMSListFilter(q gorm.ChainInterface[models.SMSRecord], f SmsListFilter) gorm.ChainInterface[models.SMSRecord] {
	if f.Vendor != 0 {
		q = q.Where(generated.SMSRecord.Vendor.Eq(int32(f.Vendor)))
	}
	if f.Scene != 0 {
		q = q.Where(generated.SMSRecord.Scene.Eq(int32(f.Scene)))
	}
	if f.Status != 0 {
		q = q.Where(generated.SMSRecord.Status.Eq(int32(f.Status)))
	}
	if f.Target != "" {
		q = q.Where(generated.SMSRecord.Target.Eq(f.Target))
	}
	if f.SenderID != "" {
		q = q.Where(generated.SMSRecord.SenderID.Eq(f.SenderID))
	}
	if f.StartTime != nil {
		q = q.Where(generated.SMSRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.SMSRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}

func applySMSStatsFilter(q gorm.ChainInterface[models.SMSRecord], f SmsStatsFilter) gorm.ChainInterface[models.SMSRecord] {
	if f.Vendor != 0 {
		q = q.Where(generated.SMSRecord.Vendor.Eq(int32(f.Vendor)))
	}
	if f.Scene != 0 {
		q = q.Where(generated.SMSRecord.Scene.Eq(int32(f.Scene)))
	}
	if f.StartTime != nil {
		q = q.Where(generated.SMSRecord.CreatedAt.Gte(*f.StartTime))
	}
	if f.EndTime != nil {
		q = q.Where(generated.SMSRecord.CreatedAt.Lte(*f.EndTime))
	}
	return q
}
```

---

### Task 9: 添加共享 `Stats` 类型到 dal 包

**Files:**
- Create: `internal/store/dal/stats.go`

**Why:** `CountEmailStats` 和 `CountSMSStats` 都返回 `*Stats`，需要在 dal 包内定义此类型。原 `dal/message_record.go` 里有此类型，删除文件后会丢失。

- [ ] **Step 1: 创建 `internal/store/dal/stats.go`**

```go
package dal

// Stats contains aggregated message statistics.
// Shared by CountEmailStats and CountSMSStats.
type Stats struct {
	Total       int64
	Sent        int64
	Failed      int64
	SuccessRate float64
}
```

---

### Task 10: 删除 `dal/message_record.go` 和测试

**Files:**
- Delete: `internal/store/dal/message_record.go`
- Delete: `internal/store/dal/message_record_test.go`

- [ ] **Step 1: 删除两个文件**

```bash
rm internal/store/dal/message_record.go internal/store/dal/message_record_test.go
```

- [ ] **Step 2: 验证 dal 包编译通过**

```bash
go build ./internal/store/dal/...
```

Expected: 编译通过。如有 `ListFilter` / `StatsFilter` / `VendorStat` 等类型未定义错误，确认 dal 目录里只有 `email_record.go` / `sms_record.go` / `stats.go` 三个源文件。

- [ ] **Step 3: 跑 dal 测试**

```bash
go test ./internal/store/dal/... -v
```

Expected: 所有 email/sms 测试通过。

- [ ] **Step 4: Commit dal 层改动**

```bash
git add internal/store/dal/
git commit -m "refactor(dal): split message_record dal into email_record and sms_record

- Add email_record.go with EmailListFilter / EmailStatsFilter / EmailVendorStat
  and CreateEmailRecord / GetEmailRecord / ListEmailRecords / CountEmailStats /
  ListEmailVendorStats
- Add sms_record.go (symmetric to email)
- Add stats.go with shared Stats type
- Delete message_record.go and message_record_test.go
- All filters support scene and sender_id filtering"
```

---

## Phase 5: xcodes 层

### Task 11: 拆分 `ErrMessageNotFound` 为 `ErrEmailNotFound` + `ErrSMSNotFound`

**Files:**
- Modify: `pkg/xcodes/message.go`

**Why:** dal 层 `GetEmailRecord` / `GetSMSRecord` 引用了拆分后的错误码，必须先于 dal 编译通过完成。

- [ ] **Step 1: 修改 `pkg/xcodes/message.go`**

把：

```go
// ErrMessageNotFound indicates no message record matches the requested ID.
var ErrMessageNotFound = xerr.New("MESSAGE_NOT_FOUND", xerr.CategoryNotFound, 404, "message record not found")
```

替换为：

```go
// ErrEmailNotFound indicates no email record matches the requested ID.
var ErrEmailNotFound = xerr.New("EMAIL_NOT_FOUND", xerr.CategoryNotFound, 404, "email record not found")

// ErrSMSNotFound indicates no SMS record matches the requested ID.
var ErrSMSNotFound = xerr.New("SMS_NOT_FOUND", xerr.CategoryNotFound, 404, "sms record not found")
```

其余 `ErrBadRequest` / `ErrInternal` / `ErrMessageSendFailed` 保持不变。

- [ ] **Step 2: 验证 xcodes 包编译通过**

```bash
go build ./pkg/xcodes/...
```

Expected: 编译通过。

- [ ] **Step 3: Commit**

```bash
git add pkg/xcodes/message.go
git commit -m "refactor(xcodes): split ErrMessageNotFound into ErrEmailNotFound and ErrSMSNotFound"
```

---

## Phase 6: service 层

### Task 12: 拆分 `service/message/message.go` 为 `service.go` + `email.go`

**Files:**
- Create: `internal/service/message/service.go`
- Create: `internal/service/message/email.go`
- Modify: `internal/service/message/message.go` (临时保留 sms 部分，下一步删除)

**Why:** 把庞大的 `message.go`（397 行）拆为三个职责清晰的文件：`service.go`（Service struct + New）、`email.go`（邮件相关方法）、`sms.go`（短信相关方法）。当前任务先拆出 email 部分。

- [ ] **Step 1: 创建 `internal/service/message/service.go`**

```go
// Package message contains the message domain business logic.
//
// Layering contract (see golang-service-development skill §2):
//   - This is a SUBPACKAGE under internal/service/. Message methods are
//     NOT on the outer *service.Service; the outer service exposes them via
//     one-line facade methods (see ../service.go).
//   - Methods take proto types DIRECTLY and return proto types — no
//     intermediate Go structs. Conversion happens at the store boundary.
//   - Resources (db, gid, vendor registries) are injected via New; the
//     subpackage does NOT manage their lifecycle.
package message

import (
	"message-service/internal/provider/email"
	"message-service/internal/provider/sms"
	"message-service/pkg/thirdcall"

	"gorm.io/gorm"
)

// Service is the message domain service. Resources are injected at
// construction; the subpackage does not manage their lifecycle.
type Service struct {
	db            *gorm.DB
	gid           thirdcall.GIDService
	emailRegistry *email.AccountRegistry
	smsRegistry   *sms.AccountRegistry
	smsRouter     *sms.Router // nil when no routes configured
}

// New constructs a message domain service with injected resources.
func New(
	db *gorm.DB,
	gid thirdcall.GIDService,
	emailRegistry *email.AccountRegistry,
	smsRegistry *sms.AccountRegistry,
	smsRouter *sms.Router,
) *Service {
	return &Service{
		db:            db,
		gid:           gid,
		emailRegistry: emailRegistry,
		smsRegistry:   smsRegistry,
		smsRouter:     smsRouter,
	}
}
```

- [ ] **Step 2: 创建 `internal/service/message/email.go`**

```go
package message

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	pb "message-service/gen/message/v1"
	"message-service/internal/provider/email"
	"message-service/internal/store/dal"
	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	emailcommon "github.com/servekit/go-common/message/email"
)

// SendEmail sends an email via the configured vendor/account, or the default
// fallback chain when both are unset.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
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
		To:             req.GetTo(),
		Cc:             req.GetCc(),
		Bcc:            req.GetBcc(),
		Subject:        req.GetSubject(),
		Body:           req.GetBody(),
		HTMLBody:       req.GetHtmlBody(),
		ReplyTo:        req.GetReplyTo(),
		Template:       req.GetTemplateId(),
		TemplateParams: req.GetTemplateParams(),
	}

	result, err := sender.Send(ctx, msg)
	if err != nil {
		if result != nil {
			s.persistEmailRecord(ctx, id, req, result)
		}
		return nil, xcodes.ErrMessageSendFailed.Wrap(err)
	}

	s.persistEmailRecord(ctx, id, req, result)

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_EmailVendor{
			EmailVendor: emailVendorFromString(result.Vendor),
		},
	}, nil
}

// GetEmail returns a single email record by ID.
func (s *Service) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	record, err := dal.GetEmailRecord(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	return toProtoEmailRecord(record), nil
}

// ListEmails returns a paginated list of email records matching the filter.
func (s *Service) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	f := dal.EmailListFilter{
		Vendor:   req.GetVendor(),
		Scene:    req.GetScene(),
		Status:   req.GetStatus(),
		Target:   req.GetTarget(),
		SenderID: req.GetSenderId(),
		Page:     req.GetPage(),
		PageSize: req.GetPageSize(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	records, total, err := dal.ListEmailRecords(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.EmailRecord, len(records))
	for i, r := range records {
		protoRecords[i] = toProtoEmailRecord(r)
	}

	return &pb.ListEmailsResponse{
		Records: protoRecords,
		Total:   int32(total),
	}, nil
}

// GetEmailStats returns aggregated statistics for emails matching the filter.
func (s *Service) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	f := dal.EmailStatsFilter{
		Vendor: req.GetVendor(),
		Scene:  req.GetScene(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	stats, err := dal.CountEmailStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendorStats, err := dal.ListEmailVendorStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendors := make([]*pb.EmailVendorStats, len(vendorStats))
	for i, vs := range vendorStats {
		vendors[i] = &pb.EmailVendorStats{
			Vendor: vs.Vendor,
			Total:  vs.Total,
			Sent:   vs.Sent,
			Failed: vs.Failed,
		}
	}

	return &pb.EmailStatsResponse{
		Total:       stats.Total,
		Sent:        stats.Sent,
		Failed:      stats.Failed,
		SuccessRate: stats.SuccessRate,
		Vendors:     vendors,
	}, nil
}

// --- record persistence (synchronous, error-logged) ---

func (s *Service) persistEmailRecord(ctx context.Context, id int64, req *pb.SendEmailRequest, result *email.SendResult) {
	record := &models.EmailRecord{
		ID:             id,
		Vendor:         int32(emailVendorFromString(result.Vendor)),
		Account:        result.Account,
		Scene:          int32(req.GetScene()),
		Target:         req.GetTo(),
		SenderID:       req.GetSenderId(),
		Cc:             models.StringSlice(req.GetCc()),
		Bcc:            models.StringSlice(req.GetBcc()),
		Subject:        req.GetSubject(),
		Content:        req.GetBody(),
		HTMLBody:       req.GetHtmlBody(),
		ReplyTo:        req.GetReplyTo(),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
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

	if err := dal.CreateEmailRecord(ctx, s.db, record); err != nil {
		slog.Error("persist email record", "record_id", id, "error", err)
	}
}

// --- proto ↔ model conversion ---

func toProtoEmailRecord(r *models.EmailRecord) *pb.EmailRecord {
	rec := &pb.EmailRecord{
		Id:             r.ID,
		Vendor:         pb.EmailVendor(r.Vendor),
		Account:        r.Account,
		Scene:          pb.EmailScene(r.Scene),
		Status:         pb.MessageStatus(r.Status),
		Target:         r.Target,
		SenderId:       r.SenderID,
		Cc:             []string(r.Cc),
		Bcc:            []string(r.Bcc),
		Subject:        r.Subject,
		Content:        r.Content,
		HtmlBody:       r.HTMLBody,
		ReplyTo:        r.ReplyTo,
		TemplateId:     r.TemplateID,
		TemplateParams: map[string]string(r.TemplateParams),
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		CreatedAt:      r.CreatedAt.Unix(),
		UpdatedAt:      r.UpdatedAt.Unix(),
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	return rec
}

// --- vendor enum ↔ string helpers ---

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

func emailVendorFromString(s string) pb.EmailVendor {
	switch s {
	case "custom_smtp":
		return pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP
	case "aliyun":
		return pb.EmailVendor_EMAIL_VENDOR_ALIYUN
	case "tencent":
		return pb.EmailVendor_EMAIL_VENDOR_TENCENT
	case "netease":
		return pb.EmailVendor_EMAIL_VENDOR_NETEASE
	default:
		return pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED
	}
}
```

- [ ] **Step 3: 暂不运行测试**——`message.go` 还有 SMS 部分，但里面引用了旧 `pb.MessageRecord`，会导致整包编译失败。下一步创建 `sms.go` 后删除 `message.go`。

---

### Task 13: 创建 `sms.go`，删除 `message.go`

**Files:**
- Create: `internal/service/message/sms.go`
- Delete: `internal/service/message/message.go`

- [ ] **Step 1: 创建 `internal/service/message/sms.go`**

```go
package message

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	pb "message-service/gen/message/v1"
	"message-service/internal/provider/sms"
	"message-service/internal/store/dal"
	"message-service/internal/store/models"
	"message-service/pkg/xcodes"

	smscommon "github.com/servekit/go-common/message/sms"
)

// SendSMS sends an SMS via the configured vendor/account, or routes by phone
// country code when both are unset.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
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
		if result != nil {
			s.persistSMSRecord(ctx, id, req, result)
		}
		return nil, xcodes.ErrMessageSendFailed.Wrap(sendErr)
	}

	s.persistSMSRecord(ctx, id, req, result)

	return &pb.SendResponse{
		Id:     id,
		Status: pb.MessageStatus_MESSAGE_STATUS_SENT,
		Vendor: &pb.SendResponse_SmsVendor{
			SmsVendor: smsVendorFromString(result.Vendor),
		},
	}, nil
}

// GetSMS returns a single SMS record by ID.
func (s *Service) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	record, err := dal.GetSMSRecord(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	return toProtoSMSRecord(record), nil
}

// ListSMS returns a paginated list of SMS records matching the filter.
func (s *Service) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	f := dal.SmsListFilter{
		Vendor:   req.GetVendor(),
		Scene:    req.GetScene(),
		Status:   req.GetStatus(),
		Target:   req.GetTarget(),
		SenderID: req.GetSenderId(),
		Page:     req.GetPage(),
		PageSize: req.GetPageSize(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	records, total, err := dal.ListSMSRecords(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	protoRecords := make([]*pb.SMSRecord, len(records))
	for i, r := range records {
		protoRecords[i] = toProtoSMSRecord(r)
	}

	return &pb.ListSMSResponse{
		Records: protoRecords,
		Total:   int32(total),
	}, nil
}

// GetSMSStats returns aggregated statistics for SMS messages matching the filter.
func (s *Service) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	f := dal.SmsStatsFilter{
		Vendor: req.GetVendor(),
		Scene:  req.GetScene(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	stats, err := dal.CountSMSStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendorStats, err := dal.ListSMSVendorStats(ctx, s.db, f)
	if err != nil {
		return nil, err
	}

	vendors := make([]*pb.SmsVendorStats, len(vendorStats))
	for i, vs := range vendorStats {
		vendors[i] = &pb.SmsVendorStats{
			Vendor: vs.Vendor,
			Total:  vs.Total,
			Sent:   vs.Sent,
			Failed: vs.Failed,
		}
	}

	return &pb.SMSStatsResponse{
		Total:       stats.Total,
		Sent:        stats.Sent,
		Failed:      stats.Failed,
		SuccessRate: stats.SuccessRate,
		Vendors:     vendors,
	}, nil
}

// --- record persistence (synchronous, error-logged) ---

func (s *Service) persistSMSRecord(ctx context.Context, id int64, req *pb.SendSMSRequest, result *sms.SendResult) {
	record := &models.SMSRecord{
		ID:             id,
		Vendor:         int32(smsVendorFromString(result.Vendor)),
		Account:        result.Account,
		Scene:          int32(req.GetScene()),
		Target:         req.GetTo(),
		SenderID:       req.GetSenderId(),
		Content:        req.GetContent(),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
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

	if err := dal.CreateSMSRecord(ctx, s.db, record); err != nil {
		slog.Error("persist sms record", "record_id", id, "error", err)
	}
}

// --- proto ↔ model conversion ---

func toProtoSMSRecord(r *models.SMSRecord) *pb.SMSRecord {
	rec := &pb.SMSRecord{
		Id:             r.ID,
		Vendor:         pb.SmsVendor(r.Vendor),
		Account:        r.Account,
		Scene:          pb.SmsScene(r.Scene),
		Status:         pb.MessageStatus(r.Status),
		Target:         r.Target,
		SenderId:       r.SenderID,
		Content:        r.Content,
		TemplateId:     r.TemplateID,
		TemplateParams: map[string]string(r.TemplateParams),
		ErrorMessage:   r.ErrorMessage,
		Attempts:       int32(r.Attempts),
		CreatedAt:      r.CreatedAt.Unix(),
		UpdatedAt:      r.UpdatedAt.Unix(),
	}
	if r.SentAt.Valid {
		rec.SentAt = r.SentAt.Time.Unix()
	}
	return rec
}

// --- vendor enum ↔ string helpers ---

func smsVendorToString(v pb.SmsVendor) string {
	switch v {
	case pb.SmsVendor_SMS_VENDOR_ALIYUN:
		return "aliyun"
	default:
		return ""
	}
}

func smsVendorFromString(s string) pb.SmsVendor {
	switch s {
	case "aliyun":
		return pb.SmsVendor_SMS_VENDOR_ALIYUN
	default:
		return pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
	}
}
```

- [ ] **Step 2: 删除 `internal/service/message/message.go`**

```bash
rm internal/service/message/message.go
```

- [ ] **Step 3: 验证 service 包编译通过**

```bash
go build ./internal/service/message/...
```

Expected: 编译通过。如果有错误，检查：
- `email.go` / `sms.go` 里的 `email.SendResult` / `sms.SendResult` 类型是否正确
- `dal.EmailListFilter` / `dal.SmsListFilter` 字段名匹配

- [ ] **Step 4: Commit service 子包改动**

```bash
git add internal/service/message/
git commit -m "refactor(service/message): split message.go into service.go + email.go + sms.go

- service.go: Service struct + New (shared dependencies)
- email.go: SendEmail/GetEmail/ListEmails/GetEmailStats/persist/toProto
- sms.go: SendSMS/GetSMS/ListSMS/GetSMSStats/persist/toProto (symmetric)
- Both persist functions now write scene, sender_id, and email template fields"
```

---

### Task 14: 更新父 `internal/service/service.go` 的 facade 方法

**Files:**
- Modify: `internal/service/service.go:134-159`

**Why:** 父 `Service` 之前有 `GetMessage` / `ListMessages` / `GetMessageStats` 三个 facade，现在替换为 6 个新 facade（Get/List/Stats × Email/SMS）。`SendEmail` / `SendSMS` 保留。

- [ ] **Step 1: 修改 `internal/service/service.go`**

打开文件，找到 "facade methods" 注释下的代码块：

```go
// --- facade methods (one per RPC, delegate to subpackage) ---

// SendEmail delegates to the message subpackage.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return s.message.SendEmail(ctx, req)
}

// SendSMS delegates to the message subpackage.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return s.message.SendSMS(ctx, req)
}

// GetMessage delegates to the message subpackage.
func (s *Service) GetMessage(ctx context.Context, req *pb.GetMessageRequest) (*pb.MessageRecord, error) {
	return s.message.GetMessage(ctx, req)
}

// ListMessages delegates to the message subpackage.
func (s *Service) ListMessages(ctx context.Context, req *pb.ListMessagesRequest) (*pb.ListMessagesResponse, error) {
	return s.message.ListMessages(ctx, req)
}

// GetMessageStats delegates to the message subpackage.
func (s *Service) GetMessageStats(ctx context.Context, req *pb.GetMessageStatsRequest) (*pb.MessageStatsResponse, error) {
	return s.message.GetMessageStats(ctx, req)
}
```

替换为：

```go
// --- facade methods (one per RPC, delegate to subpackage) ---

// SendEmail delegates to the message subpackage.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return s.message.SendEmail(ctx, req)
}

// SendSMS delegates to the message subpackage.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return s.message.SendSMS(ctx, req)
}

// GetEmail delegates to the message subpackage.
func (s *Service) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	return s.message.GetEmail(ctx, req)
}

// ListEmails delegates to the message subpackage.
func (s *Service) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	return s.message.ListEmails(ctx, req)
}

// GetEmailStats delegates to the message subpackage.
func (s *Service) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	return s.message.GetEmailStats(ctx, req)
}

// GetSMS delegates to the message subpackage.
func (s *Service) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	return s.message.GetSMS(ctx, req)
}

// ListSMS delegates to the message subpackage.
func (s *Service) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	return s.message.ListSMS(ctx, req)
}

// GetSMSStats delegates to the message subpackage.
func (s *Service) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	return s.message.GetSMSStats(ctx, req)
}
```

- [ ] **Step 2: 验证 service 包编译通过**

```bash
go build ./internal/service/...
```

Expected: 编译通过。

- [ ] **Step 3: Commit**

```bash
git add internal/service/service.go
git commit -m "refactor(service): replace GetMessage/List/Stats facades with per-channel facades

SendEmail/SendSMS facades kept. Add GetEmail/ListEmails/GetEmailStats and
GetSMS/ListSMS/GetSMSStats delegating to message subpackage."
```

---

## Phase 7: handler 层

### Task 15: 更新 `handler/message.go` 的 RPC 方法集

**Files:**
- Modify: `pkg/handler/message.go`

**Why:** handler 实现 `pb.MessageServiceServer` 接口，proto 改了接口就必须改 handler。

- [ ] **Step 1: 修改 `pkg/handler/message.go`**

打开文件，把 RPC 方法实现部分（从 `SendEmail` 方法开始到文件结尾）替换为：

```go
// SendEmail delegates to service.SendEmail.
func (h *Handler) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return h.svc.SendEmail(ctx, req)
}

// SendSMS delegates to service.SendSMS.
func (h *Handler) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return h.svc.SendSMS(ctx, req)
}

// GetEmail delegates to service.GetEmail.
func (h *Handler) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	return h.svc.GetEmail(ctx, req)
}

// ListEmails delegates to service.ListEmails.
func (h *Handler) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	return h.svc.ListEmails(ctx, req)
}

// GetEmailStats delegates to service.GetEmailStats.
func (h *Handler) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	return h.svc.GetEmailStats(ctx, req)
}

// GetSMS delegates to service.GetSMS.
func (h *Handler) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	return h.svc.GetSMS(ctx, req)
}

// ListSMS delegates to service.ListSMS.
func (h *Handler) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	return h.svc.ListSMS(ctx, req)
}

// GetSMSStats delegates to service.GetSMSStats.
func (h *Handler) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	return h.svc.GetSMSStats(ctx, req)
}
```

文件顶部（包注释、Handler struct、New、Compile-time assertion、Start/Stop）保持不变。

- [ ] **Step 2: 验证整个仓库编译通过**

```bash
go build ./...
```

Expected: 编译通过。如有错误：
- 检查 handler 里有没有遗漏的方法（应该是 8 个）
- 检查 service 父级和子包接口是否对齐

- [ ] **Step 3: Commit**

```bash
git add pkg/handler/message.go
git commit -m "refactor(handler): update RPC stubs to per-channel query methods"
```

---

## Phase 8: 测试更新

### Task 16: 拆分 `service/message/message_test.go` 为 `email_test.go` + `sms_test.go`

**Files:**
- Delete: `internal/service/message/message_test.go`
- Create: `internal/service/message/email_test.go`
- Create: `internal/service/message/sms_test.go`

**Why:** 现有 `message_test.go` 大量使用旧 `pb.MessageRecord` / `pb.Channel`，必须按新接口重写。邮件测试和短信测试各自独立到对应文件。

- [ ] **Step 1: 删除旧测试文件**

```bash
rm internal/service/message/message_test.go
```

- [ ] **Step 2: 创建 `internal/service/message/email_test.go`**

```go
package message

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"message-service/internal/provider/email"
	"message-service/internal/store/models"
	"message-service/pkg/config"
	"message-service/pkg/thirdcall"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	emailcommon "github.com/servekit/go-common/message/email"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// --- mocks (providers only; persistence goes through the real dal) ---

type mockEmailProvider struct {
	name string
	err  error
}

func (m *mockEmailProvider) Name() string { return m.name }
func (m *mockEmailProvider) Send(_ context.Context, _ *emailcommon.Message) error {
	return m.err
}

// --- helpers ---

var testGIDOnce sync.Once
var testGID thirdcall.GIDService

func getTestGID(t *testing.T) thirdcall.GIDService {
	t.Helper()
	testGIDOnce.Do(func() {
		var err error
		testGID, err = thirdcall.NewGIDService(&config.GIDConfig{
			Mode: "module",
			Snowflake: &config.SnowflakeConfig{
				MachineID: 1,
				StartTime: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			},
		})
		require.NoError(t, err)
	})
	return testGID
}

func setupEmailTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.EmailRecord{}), "auto-migrate should succeed")
	return db
}

func newTestEmailService(t *testing.T, providers []emailcommon.Provider) *Service {
	t.Helper()
	db := setupEmailTestDB(t)
	accounts := make(map[string]*email.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = &email.AccountProvider{
			Vendor: p.Name(), Account: fmt.Sprintf("p%d", i), Provider: p,
		}
	}
	return New(
		db,
		getTestGID(t),
		email.NewAccountRegistryFromProviders(map[string]map[string]*email.AccountProvider{"mock": accounts}),
		nil, // smsRegistry
		nil, // smsRouter
	)
}

// --- tests ---

func TestSendEmail_Success(t *testing.T) {
	svc := newTestEmailService(t, []emailcommon.Provider{
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
	assert.Greater(t, resp.Id, int64(0))

	// Verify persistence: scene and sender_id recorded.
	record, err := svc.GetEmail(context.Background(), &pb.GetEmailRequest{Id: resp.Id})
	require.NoError(t, err)
	assert.Equal(t, pb.EmailScene_EMAIL_SCENE_LOGIN_CODE, record.Scene)
	assert.Equal(t, "user:42", record.SenderId)
	assert.Equal(t, "user@example.com", record.Target)
}

func TestSendEmail_ProviderError_PersistsFailedRecord(t *testing.T) {
	svc := newTestEmailService(t, []emailcommon.Provider{
		&mockEmailProvider{name: "mock", err: fmt.Errorf("smtp timeout")},
	})

	_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       "user@example.com",
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_REGISTER,
		SenderId: "user:42",
	})
	require.Error(t, err)

	// Verify a FAILED record was persisted. List by sender_id to find it.
	resp, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		SenderId: "user:42",
	})
	require.NoError(t, err)
	require.Len(t, resp.Records, 1)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, resp.Records[0].Status)
	assert.Equal(t, pb.EmailScene_EMAIL_SCENE_REGISTER, resp.Records[0].Scene)
}

func TestListEmails_ByScene(t *testing.T) {
	svc := newTestEmailService(t, []emailcommon.Provider{
		&mockEmailProvider{name: "mock"},
	})

	// Send two emails with different scenes.
	for _, scene := range []pb.EmailScene{
		pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		pb.EmailScene_EMAIL_SCENE_REGISTER,
	} {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       "user@example.com",
			Subject:  "Test",
			Body:     "Hello",
			Scene:    scene,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListEmails(context.Background(), &pb.ListEmailsRequest{
		Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.Total)
	assert.Len(t, resp.Records, 1)
}

func TestGetEmailStats(t *testing.T) {
	svc := newTestEmailService(t, []emailcommon.Provider{
		&mockEmailProvider{name: "mock"},
	})

	// Send 2 successful + 1 failed.
	for i := 0; i < 2; i++ {
		_, err := svc.SendEmail(context.Background(), &pb.SendEmailRequest{
			To:       "user@example.com",
			Subject:  "Test",
			Body:     "Hello",
			Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}
	// Failed one
	_, _ = svc.SendEmail(context.Background(), &pb.SendEmailRequest{
		To:       "fail@example.com",
		Subject:  "Test",
		Body:     "Hello",
		Scene:    pb.EmailScene_EMAIL_SCENE_LOGIN_CODE,
		SenderId: "user:42",
		Vendor:   pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP, // route to non-existent provider
		Account:  "nonexistent",
	})

	resp, err := svc.GetEmailStats(context.Background(), &pb.GetEmailStatsRequest{})
	require.NoError(t, err)
	// At minimum, the 2 successful sends are counted.
	assert.GreaterOrEqual(t, resp.Total, int64(2))
	assert.GreaterOrEqual(t, resp.Sent, int64(2))
}
```

- [ ] **Step 3: 创建 `internal/service/message/sms_test.go`**

```go
package message

import (
	"context"
	"fmt"
	"testing"

	"message-service/internal/provider/sms"
	"message-service/internal/store/models"
	"message-service/pkg/config"

	pb "message-service/gen/message/v1"

	"github.com/servekit/go-common/dbx"
	smscommon "github.com/servekit/go-common/message/sms"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// --- mocks ---

type mockSMSProvider struct {
	name string
	err  error
}

func (m *mockSMSProvider) Name() string { return m.name }
func (m *mockSMSProvider) Send(_ context.Context, _ *smscommon.Message) error {
	return m.err
}

// --- helpers ---

func setupSMSTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbx.SetupTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.SMSRecord{}), "auto-migrate should succeed")
	return db
}

func newTestSMSServiceWithRouter(t *testing.T, providers []smscommon.Provider) *Service {
	t.Helper()
	db := setupSMSTestDB(t)
	accounts := make(map[string]*sms.AccountProvider, len(providers))
	for i, p := range providers {
		accounts[fmt.Sprintf("p%d", i)] = &sms.AccountProvider{
			Vendor: p.Name(), Account: fmt.Sprintf("p%d", i), Provider: p,
		}
	}
	registry := sms.NewAccountRegistryFromProviders(map[string]map[string]*sms.AccountProvider{"mock": accounts})
	router, err := sms.BuildRouter(&config.SMSConfig{}, registry)
	require.NoError(t, err)
	return New(db, getTestGID(t), nil, registry, router)
}

// --- tests ---

func TestSendSMS_Success(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []smscommon.Provider{
		&mockSMSProvider{name: "mock"},
	})

	resp, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		To:       "+8613800001111",
		Content:  "Your code: 1234",
		Scene:    pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId: "user:42",
	})
	require.NoError(t, err)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_SENT, resp.Status)
	assert.Greater(t, resp.Id, int64(0))

	record, err := svc.GetSMS(context.Background(), &pb.GetSMSRequest{Id: resp.Id})
	require.NoError(t, err)
	assert.Equal(t, pb.SmsScene_SMS_SCENE_LOGIN_CODE, record.Scene)
	assert.Equal(t, "user:42", record.SenderId)
	assert.Equal(t, "+8613800001111", record.Target)
}

func TestSendSMS_ProviderError_PersistsFailedRecord(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []smscommon.Provider{
		&mockSMSProvider{name: "mock", err: fmt.Errorf("aliyun timeout")},
	})

	_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
		To:       "+8613800001111",
		Content:  "Your code: 1234",
		Scene:    pb.SmsScene_SMS_SCENE_REGISTER,
		SenderId: "user:42",
	})
	require.Error(t, err)

	resp, err := svc.ListSMS(context.Background(), &pb.ListSMSRequest{
		SenderId: "user:42",
	})
	require.NoError(t, err)
	require.Len(t, resp.Records, 1)
	assert.Equal(t, pb.MessageStatus_MESSAGE_STATUS_FAILED, resp.Records[0].Status)
	assert.Equal(t, pb.SmsScene_SMS_SCENE_REGISTER, resp.Records[0].Scene)
}

func TestListSMS_ByScene(t *testing.T) {
	svc := newTestSMSServiceWithRouter(t, []smscommon.Provider{
		&mockSMSProvider{name: "mock"},
	})

	for _, scene := range []pb.SmsScene{
		pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		pb.SmsScene_SMS_SCENE_REGISTER,
	} {
		_, err := svc.SendSMS(context.Background(), &pb.SendSMSRequest{
			To:       "+8613800001111",
			Content:  "Your code: 1234",
			Scene:    scene,
			SenderId: "user:42",
		})
		require.NoError(t, err)
	}

	resp, err := svc.ListSMS(context.Background(), &pb.ListSMSRequest{
		Scene: pb.SmsScene_SMS_SCENE_LOGIN_CODE,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.Total)
	assert.Len(t, resp.Records, 1)
}
```

- [ ] **Step 4: 跑测试**

```bash
go test ./internal/service/message/... -v
```

Expected: 所有测试通过。如果有失败：
- 检查 `email.NewAccountRegistryFromProviders` / `sms.NewAccountRegistryFromProviders` / `sms.BuildRouter` 函数签名（这些应该已存在，沿用旧测试代码）
- 检查 mock provider 是否被 registry 正确识别

- [ ] **Step 5: Commit**

```bash
git add internal/service/message/
git commit -m "test(service/message): split message_test.go into email_test.go and sms_test.go

- email_test.go: SendEmail success/failure, ListEmails by scene, GetEmailStats
- sms_test.go: SendSMS success/failure, ListSMS by scene
- All sends now pass scene and sender_id (required fields)"
```

---

## Phase 9: 最终验证

### Task 17: 跑完整质量检查

- [ ] **Step 1: 格式化**

```bash
make fmt
```

Expected: 无输出（格式已对齐）。

- [ ] **Step 2: 静态检查**

```bash
make vet
```

Expected: 无问题。

- [ ] **Step 3: lint**

```bash
make lint
```

Expected: golangci-lint 无报错。常见问题：
- 未使用的 import
- 命名规范
- error 处理（禁止 `_ =`）

- [ ] **Step 4: 完整测试**

```bash
make test
```

Expected: 所有包测试通过，race detector 无问题。

- [ ] **Step 5: 验证生成代码与 models 同步**

```bash
make generate
git diff --exit-code internal/store/generated/
```

Expected: 无 diff（generated 与 models 一致）。

- [ ] **Step 6: 验证 proto 生成代码同步**

```bash
make proto
git diff --exit-code gen/
```

Expected: 无 diff。

- [ ] **Step 7: 验证本地迁移可用**

```bash
# 需要 config.yaml 配好数据库连接，可以用 testcontainers 或本地 PostgreSQL
make migrate
```

Expected: AutoMigrate 成功创建 `email_records` 和 `sms_records` 两张表，原 `message_records` 表不会自动删除（AutoMigrate 不会 drop）。

如果需要手动清理旧表（开发环境推荐）：

```sql
DROP TABLE IF EXISTS message_records;
```

- [ ] **Step 8: 如有 lint/test 失败，修复后 amend 或追加 commit**

修复后必须创建新 commit（不要 `--amend`）：

```bash
git add <fixed files>
git commit -m "fix: address lint/test issues from split-records refactor"
```

---

### Task 18: 更新 Obsidian 文档（同步状态）

**Files (Obsidian vault):**
- Modify: `services/message-service/changes.md`（标记 design 状态为 done）

- [ ] **Step 1: 追加实施完成记录**

```bash
obsidian vault=only append file="services/message-service/changes" content="
- 2026-06-21: 完成 services/message-service/design/v2/split-email-sms-records-and-add-scene.md 实施 — 拆表 + scene 枚举 + sender_id 必填 + 6 个查询 RPC 全部上线"
```

- [ ] **Step 2: 更新 design frontmatter**

```bash
obsidian vault=only property:set file="services/message-service/design/v2/split-email-sms-records-and-add-scene" name="status" value="implemented"
```

---

## Self-Review Notes

**Spec coverage check:**

| Spec 要求 | 任务覆盖 |
|----------|---------|
| 删 `message_records` 表 | Task 6 + Task 10 + Task 17 Step 7 |
| 新增 `email_records` / `sms_records` | Task 4 + Task 5 |
| 删 `Channel` / `MessageRecord` | Task 2 |
| 新增 `EmailScene` / `SmsScene` enum | Task 2 |
| 新增 `EmailRecord` / `SMSRecord` message | Task 2 |
| 6 个新 RPC（Get/List/Stats × Email/SMS） | Task 2（proto）+ Task 12/13（service）+ Task 14（父 facade）+ Task 15（handler） |
| 删 `Get/List/Stats × Messages` RPC | Task 2 + Task 14 + Task 15 |
| SendEmail/SendSMS 加 scene + sender_id（必填）+ template（可选） | Task 2（proto CEL）+ Task 12/13（service 持久化） |
| go-common/email.Message 加 template 字段 | Task 1 |
| Model 按 gorm skill §3（含 DeletedAt） | Task 4 + Task 5 |
| EmailRecord 不加 SenderID 之外的多余字段 | Task 4（确认无 SenderID 之外多余字段） |
| 错误码拆分（ErrEmailNotFound + ErrSMSNotFound） | Task 11 |
| Service 保持单一子包 + 文件拆分 | Task 12 + Task 13 |
| 测试用 dbx.SetupTestDB（testcontainer 真实 PG） | Task 7 + Task 8 + Task 16 |
| 测试按 channel 拆分 | Task 7（dal email）+ Task 8（dal sms）+ Task 16（service email/sms） |
| List/Stats 支持 scene 过滤 | Task 7（dal EmailListFilter）+ Task 8（dal SmsListFilter） |

**所有 spec 要求已映射到具体任务。**

**Placeholder scan:** 无 TBD / TODO / 占位符。

**Type consistency check:**
- `dal.EmailListFilter` / `dal.EmailStatsFilter` / `dal.EmailVendorStat` — Task 7 定义，Task 12 使用 ✓
- `dal.SmsListFilter` / `dal.SmsStatsFilter` / `dal.SmsVendorStat` — Task 8 定义，Task 13 使用 ✓
- `dal.Stats` — Task 9 定义，Task 7/8 使用 ✓
- `models.EmailRecord` / `models.SMSRecord` — Task 4/5 定义，Task 7/8/12/13 使用 ✓
- `xcodes.ErrEmailNotFound` / `xcodes.ErrSMSNotFound` — Task 11 定义，Task 7/8 使用 ✓
- Service struct 字段（db/gid/emailRegistry/smsRegistry/smsRouter）— Task 12 定义，Task 13 使用 ✓

---

## 执行顺序总览

执行时严格按 Phase 1 → 9 顺序，但 **Task 11（xcodes 拆分）必须在 Task 7（dal email）之前完成**，因为 dal 引用了拆分后的错误码。

推荐顺序：

```
Task 1 (go-common) → Task 2 (proto) → Task 3 (types) → Task 4 (email model)
→ Task 5 (sms model) → Task 6 (delete message model) → Task 11 (xcodes)
→ Task 7 (dal email) → Task 8 (dal sms) → Task 9 (stats shared) → Task 10 (delete dal message)
→ Task 12 (service email) → Task 13 (service sms + delete message.go) → Task 14 (父 service)
→ Task 15 (handler) → Task 16 (service tests) → Task 17 (verify) → Task 18 (obsidian)
```
