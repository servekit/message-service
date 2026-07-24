# SenderID 语义收紧 + SMS region_code/phone 拆分 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 SenderID 语义注释收紧为"调用方业务服务名"；把 SMS 收件人从单一 `to` 字段拆为 `region_code`（ISO alpha-2）+ `phone`；新增 ListSMSRegions / ListSMSSenders / ListEmailSenders 三个聚合 RPC 供前端筛选下拉用。

**Architecture:** proto → GORM model → dal → service 全链路拆分。服务层用 `phonenumbers` 库把 (region_code, phone) 解析校验后归一化为 E.164 传给 router/providers，router/providers 零改动。聚合 RPC 走 `SELECT DISTINCT` 加可选 AND filter。

**Tech Stack:** Go 1.x, gRPC + grpc-gateway + buf + protovalidate, GORM + PostgreSQL, `github.com/nyaruka/phonenumbers`，`github.com/servekit/go-common` (dbx/redisx/jsonx/xerr)，Conventional Commits。

**前置阅读：** `docs/superpowers/specs/2026-07-01-sender-id-and-sms-region-split-design.md`

**全局约定：**
- 项目"无生产数据"，proto 字段重排可接受（破坏性变更不必保留旧字段号）
- 每个任务结束都 commit，提交信息用 Conventional Commits + scope
- TDD：先写测试 → 看失败 → 实现 → 看通过 → commit
- 注释一律英文，回复/commit message 用中/英文按 CLAUDE.md 约定（commit 用英文）
- 不要写"// removed" 或 "// was X" 类向后兼容注释，直接删

---

## 文件结构

| 路径 | 责任 | 本次改动 |
|------|------|---------|
| `api/proto/message/v1/message.proto` | gRPC API 契约 | 改 SendSMSRequest/SMSRecord/ListSMS*、加 3 个聚合 RPC、收紧 sender_id 注释 |
| `gen/message/v1/*.go` | buf 生成代码 | `make proto` 重生 |
| `internal/store/models/sms_record.go` | SMS GORM model | 删 Target、加 RegionCode + Phone、加 SenderID 注释 |
| `internal/store/models/email_record.go` | Email GORM model | 加 SenderID 注释 |
| `internal/store/generated/sms_record.go` | gorm gen 生成 | `make generate` 重生 |
| `cmd/migrate/main.go` | AutoMigrate 入口 | 加 `ALTER TABLE ... DROP COLUMN target` for SMS |
| `internal/store/dal/sms_record.go` | SMS 数据访问 | SmsListFilter 改字段、加 ListSMSRegions + ListSMSSenderIDs |
| `internal/store/dal/email_record.go` | Email 数据访问 | 加 ListEmailSenderIDs |
| `internal/store/dal/sms_record_test.go` | SMS dal 单测 | 改测试 helper、加聚合函数测试 |
| `internal/store/dal/email_record_test.go` | Email dal 单测 | 加聚合函数测试 |
| `internal/service/message/util.go` | 校验逻辑 | validateSendSMSRequest 新增 5 条规则 |
| `internal/service/message/util_test.go` | 校验单测 | 加新规则用例 |
| `internal/service/message/sms.go` | SMS service | SendSMS 加 parse/format、persist/toProto 字段调整、加 ListSMSRegions + ListSMSSenders |
| `internal/service/message/email.go` | Email service | 加 ListEmailSenders |
| `internal/service/service.go` | service root facade | 注册 3 个新 RPC facade 方法 |
| `cmd/testclient/commands.go` | testclient 子命令 | send-sms 拆 --region-code/--phone、加 list-sms-regions/list-sms-senders/list-email-senders |
| `cmd/testclient/main.go` | testclient 入口 | dispatch 注册新子命令、smoke-test 用新字段 |

---

## Task 1: proto 定义全部改完（一次性 codegen）

**Files:**
- Modify: `api/proto/message/v1/message.proto`

整个 proto 改动一次性做完——多次跑 `make proto` 浪费时间。改完一次 regenerate。

- [ ] **Step 1: 修改 message.proto——SMS 请求/响应/列表 message**

把 `SendSMSRequest` 字段重排为：

```proto
// SendSMSRequest is the request to send an SMS.
message SendSMSRequest {
  // region_code is the ISO 3166-1 alpha-2 region code used to parse phone
  // (e.g. "CN", "US", "HK"). Required. Acts as defaultRegion for
  // phonenumber parsing — NOT a dialing code like "86".
  string region_code = 1 [(buf.validate.field).string.pattern = "^[A-Z]{2}$"];

  // phone is the local phone number WITHOUT the international prefix
  // (e.g. "13800138000", "5551234567"). Must NOT start with "+".
  string phone = 2 [(buf.validate.field).string.min_len = 1];

  string content = 3;
  string template_id = 4;
  map<string, string> template_params = 5;

  // Vendor + Account optionally select a specific configured account.
  // Both must be set together; if both zero/empty, sender routes by phone country.
  SmsVendor vendor = 6;
  string account = 7;

  // Scene is the business purpose of this SMS (required).
  SmsScene scene = 8;

  // SenderID identifies the calling business service (e.g. "user-service",
  // "pay-service"). Required. NOT the end-user/admin id — the caller must
  // record that in its own audit trail.
  string sender_id = 9;

  // IdempotencyKey is optional. See SendEmailRequest.idempotency_key.
  string idempotency_key = 10 [(buf.validate.field).string.max_len = 64];

  option (buf.validate.message).cel = {
    id: "vendor_account_pair"
    message: "vendor and account must both be set or both be empty"
    expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
  };
  option (buf.validate.message).cel = {
    id: "scene_required"
    message: "scene is required"
    expression: "this.scene != 0"
  };
  option (buf.validate.message).cel = {
    id: "sender_required"
    message: "sender_id is required"
    expression: "this.sender_id != ''"
  };
  option (buf.validate.message).cel = {
    id: "phone_no_plus"
    message: "phone must not start with '+' — provide local number only"
    expression: "size(this.phone) == 0 || !startsWith(this.phone, '+')"
  };
}
```

把 `SMSRecord` 字段重排（target 删，加 region_code + phone）：

```proto
// SMSRecord is the stored record of a sent SMS.
message SMSRecord {
  int64 id = 1;
  SmsVendor vendor = 2;
  string account = 3;
  SmsScene scene = 4;
  MessageStatus status = 5;
  string region_code = 6;
  string phone = 7;
  // SenderID identifies the calling business service. See SendSMSRequest.sender_id.
  string sender_id = 8;
  string content = 9;
  string template_id = 10;
  map<string, string> template_params = 11;
  string error_message = 12;
  int32 attempts = 13;
  int64 sent_at = 14;
  int64 created_at = 15;
  int64 updated_at = 16;
}
```

把 `ListSMSRequest` 和 `ListSMSByCursorRequest` 中的 `string target = 4;` 删除，加：

```proto
  string region_code = 4;
  string phone = 5;  // 注意：原 sender_id 等后续字段编号要顺延
```

具体地，`ListSMSRequest` 重排为：

```proto
message ListSMSRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string region_code = 4;
  string phone = 5;
  string sender_id = 6;
  int64 start_time = 7;
  int64 end_time = 8;
  int32 page = 9;
  int32 page_size = 10;
  SortField sort_field = 11;
  SortDirection sort_direction = 12;
}
```

`ListSMSByCursorRequest` 同样把 `target` 改为 `region_code` + `phone`（占 4、5 号），后续字段顺延。

- [ ] **Step 2: 修改 message.proto——所有 8 处 sender_id 字段的注释收紧**

对以下 8 个 message 里的 `sender_id` 字段，统一把注释改成（如果原本只有一行，替换为下面这版；如果是多行，调整成下面这版）：

```proto
  // SenderID identifies the calling business service (e.g. "user-service",
  // "pay-service"). Required. NOT the end-user/admin id — the caller must
  // record that in its own audit trail.
  string sender_id = <保留原字段号>;
```

需要改的 8 处（List 类的 sender_id 是可选 filter，注释里 "Required." 那句改成 "Optional filter;")：

1. `SendEmailRequest.sender_id`（保留 "Required."）
2. `EmailRecord.sender_id`（去掉 "Required."，只是回显字段）
3. `ListEmailsRequest.sender_id`（改成 "Optional filter;"）
4. `ListEmailsByCursorRequest.sender_id`（改成 "Optional filter;"）
5. `SendSMSRequest.sender_id`（保留 "Required."；本字段在 Step 1 已经在重建的 SendSMSRequest 里改过，跳过）
6. `SMSRecord.sender_id`（去掉 "Required."）
7. `ListSMSRequest.sender_id`（改成 "Optional filter;"）
8. `ListSMSByCursorRequest.sender_id`（改成 "Optional filter;"）

- [ ] **Step 3: 修改 message.proto——新增 3 个聚合 RPC + 对应 message**

在 `service MessageService` 块里追加（紧跟 `GetSMSStats` 之后）：

```proto
  // ListSMSRegions returns distinct region_code values matching the filter,
  // for populating frontend SMS list filter dropdowns.
  rpc ListSMSRegions(ListSMSRegionsRequest) returns (ListSMSRegionsResponse) {
    option (google.api.http) = {get: "/v1/sms:regions"};
  }

  // ListSMSSenders returns distinct sender_id values matching the filter,
  // for populating frontend SMS list filter dropdowns.
  rpc ListSMSSenders(ListSMSSendersRequest) returns (ListSMSSendersResponse) {
    option (google.api.http) = {get: "/v1/sms:senders"};
  }

  // ListEmailSenders returns distinct sender_id values matching the filter,
  // for populating frontend email list filter dropdowns.
  rpc ListEmailSenders(ListEmailSendersRequest) returns (ListEmailSendersResponse) {
    option (google.api.http) = {get: "/v1/emails:senders"};
  }
```

在文件末尾追加 6 个新 message：

```proto
// ListSMSRegionsRequest filters a ListSMSRegions query. All fields optional;
// region_code itself is the aggregated field and thus NOT in the filter.
message ListSMSRegionsRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string sender_id = 4;
  int64 start_time = 5;
  int64 end_time = 6;
}

message ListSMSRegionsResponse {
  repeated string region_codes = 1;
}

// ListSMSSendersRequest filters a ListSMSSenders query. All fields optional;
// sender_id itself is the aggregated field and thus NOT in the filter.
// phone is excluded — too high-cardinality to scope a dropdown usefully.
message ListSMSSendersRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string region_code = 4;
  int64 start_time = 5;
  int64 end_time = 6;
}

message ListSMSSendersResponse {
  repeated string sender_ids = 1;
}

// ListEmailSendersRequest filters a ListEmailSenders query. All fields optional;
// sender_id itself is the aggregated field and thus NOT in the filter.
// target is excluded — too high-cardinality to scope a dropdown usefully.
message ListEmailSendersRequest {
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  MessageStatus status = 3;
  int64 start_time = 4;
  int64 end_time = 5;
}

message ListEmailSendersResponse {
  repeated string sender_ids = 1;
}
```

- [ ] **Step 4: 重新生成 proto 代码**

Run: `make proto`
Expected: 无错误；`gen/message/v1/*.go` 重新生成。

- [ ] **Step 5: 验证 build 通过（编译错误暴露后续要改的引用点）**

Run: `go build ./...`
Expected: 编译失败，错误集中在 `internal/service/message/sms.go`、`internal/service/message/email.go`、`internal/store/dal/sms_record.go`、`cmd/testclient/` 引用了已删除的 `Target` / `To` 字段或缺失的 service 方法。这些会在后续 task 修复。**不要在本 task 里修**——本 task 只动 proto 与生成代码。

如果出现 proto本身的语法错误（不是后续引用错误），回到 Step 1-3 修正。

- [ ] **Step 6: Commit**

```bash
git add api/proto/message/v1/message.proto gen/
git commit -m "feat(proto): split SMS recipient into region_code+phone, add aggregation RPCs, narrow sender_id semantics"
```

---

## Task 2: GORM model 改 SMS 字段 + SenderID 注释

**Files:**
- Modify: `internal/store/models/sms_record.go:12-29`
- Modify: `internal/store/models/email_record.go:14-36`

- [ ] **Step 1: 修改 MessageSMSRecord——删 Target，加 RegionCode + Phone，加 SenderID 注释**

把 `internal/store/models/sms_record.go` 里的 `MessageSMSRecord` 结构体改为：

```go
// MessageSMSRecord stores a complete record of every SMS sent through the service.
// See MessageEmailRecord for the rationale on the "Message" prefix.
type MessageSMSRecord struct {
	ID             int64           `gorm:"primaryKey"`
	Vendor         int32           `gorm:"not null;default:0;index"`
	Account        string          `gorm:"size:64;column:account"`
	Scene          int32           `gorm:"not null;default:0;index"`
	Status         int32           `gorm:"not null;default:0;index"`
	RegionCode     string          `gorm:"size:2;column:region_code;not null;index"`
	Phone          string          `gorm:"size:32;column:phone;not null;index"`
	// SenderID identifies the calling business service (e.g. "user-service",
	// "pay-service"). NOT the end-user/admin id — the caller is responsible
	// for recording that in its own audit trail.
	SenderID       string          `gorm:"size:64;column:sender_id;index"`
	Content        string          `gorm:"type:text"`
	TemplateID     string          `gorm:"size:64;column:template_id"`
	TemplateParams MapStringString `gorm:"type:jsonb;column:template_params"`
	ErrorMessage   string          `gorm:"size:1024;column:error_message"`
	Attempts       int             `gorm:"not null;default:1"`
	SentAt         sql.NullTime    `gorm:"column:sent_at"`
	CreatedAt      time.Time       `gorm:"not null;default:now();index"`
	UpdatedAt      time.Time       `gorm:"not null;default:now()"`
	DeletedAt      gorm.DeletedAt  `gorm:"index"`
}
```

- [ ] **Step 2: 修改 MessageEmailRecord——只加 SenderID 注释**

在 `internal/store/models/email_record.go` 的 `MessageEmailRecord.SenderID` 字段上方加注释：

```go
	// SenderID identifies the calling business service (e.g. "user-service",
	// "pay-service"). NOT the end-user/admin id — the caller is responsible
	// for recording that in its own audit trail.
	SenderID       string          `gorm:"size:64;column:sender_id;index"`
```

- [ ] **Step 3: 重新生成 gorm gen 代码**

Run: `make generate`
Expected: 无错误；`internal/store/generated/sms_record.go` 的 `MessageSMSRecord` struct 内 `Target field.String` 消失，新增 `RegionCode field.String` 和 `Phone field.String`。

- [ ] **Step 4: 验证生成结果**

Run: `grep -E "Target|RegionCode|Phone" internal/store/generated/sms_record.go`
Expected: 看到 `RegionCode field.String` 与 `Phone field.String`，**不再有** `Target field.String`。

- [ ] **Step 5: Commit**

```bash
git add internal/store/models/sms_record.go internal/store/models/email_record.go internal/store/generated/
git commit -m "refactor(models): split SMS target into region_code+phone, document sender_id semantics"
```

---

## Task 3: 迁移脚本显式 DROP target 列

**Files:**
- Modify: `cmd/migrate/main.go:50-62`

GORM AutoMigrate 不会删旧列。SMS 表的 `target` 列必须显式 DROP，否则旧列残留在新环境数据库里。

- [ ] **Step 1: 在 cmd/migrate/main.go 的 drops 列表追加 SMS target 列删除**

把 `runMigration` 函数里的 `drops` 改为：

```go
	drops := []string{
		`DROP INDEX IF EXISTS idx_email_records_sender_idempotency`,
		`DROP INDEX IF EXISTS idx_sms_records_sender_idempotency`,
		`ALTER TABLE message_email_records DROP COLUMN IF EXISTS idempotency_key`,
		`ALTER TABLE message_sms_records DROP COLUMN IF EXISTS idempotency_key`,
		// message_sms_records.target was replaced by (region_code, phone).
		// AutoMigrate does not drop columns, so explicit DDL is required.
		`ALTER TABLE message_sms_records DROP COLUMN IF EXISTS target`,
	}
```

- [ ] **Step 2: 跑 migrate 验证 DDL 不报错（testcontainer）**

Run: `go test ./cmd/migrate/...`
Expected: PASS。如果 migrate 测试套件里没有 DB 集成测试，至少 `go build ./cmd/migrate/` 应通过。

- [ ] **Step 3: Commit**

```bash
git add cmd/migrate/main.go
git commit -m "fix(migrate): drop legacy target column on message_sms_records"
```

---

## Task 4: TDD validateSendSMSRequest 新规则

**Files:**
- Modify: `internal/service/message/util.go:47-64`
- Test: `internal/service/message/util_test.go`

加 5 条新规则：region_code 格式、phone 不以 + 开头、phonenumbers.Parse 成功、IsValidNumber、GetRegionCodeForNumber 一致性。

- [ ] **Step 1: 在 util_test.go 末尾追加新规则的失败测试**

把 `internal/service/message/util_test.go` 末尾追加（注意原 `TestValidateSendSMSRequest_VendorWithoutAccount` 用了 `To: "+8613800001111"`，本步骤也要把它改成新字段）：

先改原测试：
```go
func TestValidateSendSMSRequest_VendorWithoutAccount(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "13800000111",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user:42",
		Vendor:     pb.SmsVendor_SMS_VENDOR_ALIYUN,
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "vendor and account")
}
```

再追加新用例：

```go
func TestValidateSendSMSRequest_RegionCodeInvalidFormat(t *testing.T) {
	cases := []string{"", "cn", "CHN", "C", "ABC", "12"}
	for _, rc := range cases {
		t.Run(rc, func(t *testing.T) {
			req := &pb.SendSMSRequest{
				RegionCode: rc,
				Phone:      "13800000111",
				Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
				SenderId:   "user-service",
			}
			err := validateSendSMSRequest(req)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "region_code")
		})
	}
}

func TestValidateSendSMSRequest_PhoneStartsWithPlus(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "+8613800001111",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "phone")
}

func TestValidateSendSMSRequest_PhoneUnparsable(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "not-a-phone!!!",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestValidateSendSMSRequest_RegionMismatch(t *testing.T) {
	// Region CN but phone parses as US number
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "5551234567",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	err := validateSendSMSRequest(req)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "region")
}

func TestValidateSendSMSRequest_ValidChineseNumber(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "13800000111",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	assert.NoError(t, validateSendSMSRequest(req))
}

func TestValidateSendSMSRequest_ValidUSNumber(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "US",
		Phone:      "5551234567",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	assert.NoError(t, validateSendSMSRequest(req))
}

func TestValidateSendSMSRequest_ValidHKNumber(t *testing.T) {
	req := &pb.SendSMSRequest{
		RegionCode: "HK",
		Phone:      "91234567",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   "user-service",
	}
	assert.NoError(t, validateSendSMSRequest(req))
}
```

- [ ] **Step 2: 跑测试确认它们 fail**

Run: `go test ./internal/service/message/ -run TestValidateSendSMSRequest`
Expected: 新加的测试 fail（编译通过但断言失败），因为旧 validateSendSMSRequest 不检查这些。

- [ ] **Step 3: 在 util.go 加 phonenumbers import 并实现新规则**

把 `internal/service/message/util.go` 顶部 import 改为：

```go
import (
	"fmt"
	"regexp"
	"time"

	pb "message-service/gen/message/v1"

	"github.com/nyaruka/phonenumbers"
)
```

把 `validateSendSMSRequest` 改为：

```go
// regionCodePattern matches ISO 3166-1 alpha-2 codes: exactly two uppercase
// ASCII letters. phonenumbers.Parse accepts this as the defaultRegion arg.
var regionCodePattern = regexp.MustCompile(`^[A-Z]{2}$`)

// validateSendSMSRequest mirrors validateSendEmailRequest for SMS, and adds
// phone parsing + region consistency checks that protovalidate cannot express.
func validateSendSMSRequest(req *pb.SendSMSRequest) error {
	vendorSet := req.GetVendor() != pb.SmsVendor_SMS_VENDOR_UNSPECIFIED
	accountSet := req.GetAccount() != ""
	if vendorSet != accountSet {
		return fmt.Errorf("vendor and account must be set together")
	}
	if req.GetScene() == pb.SmsScene_SMS_SCENE_UNSPECIFIED {
		return fmt.Errorf("scene is required")
	}
	if req.GetSenderId() == "" {
		return fmt.Errorf("sender_id is required")
	}
	if len(req.GetIdempotencyKey()) > maxIdempotencyKeyLen {
		return fmt.Errorf("idempotency_key too long (max %d)", maxIdempotencyKeyLen)
	}

	rc := req.GetRegionCode()
	if !regionCodePattern.MatchString(rc) {
		return fmt.Errorf("region_code must be 2 uppercase letters (ISO 3166-1 alpha-2), got %q", rc)
	}
	phone := req.GetPhone()
	if phone == "" {
		return fmt.Errorf("phone is required")
	}
	if phone[0] == '+' {
		return fmt.Errorf("phone must not start with '+' — provide local number only; region_code disambiguates the country")
	}
	num, err := phonenumbers.Parse(phone, rc)
	if err != nil {
		return fmt.Errorf("parse phone %q with region %q: %w", phone, rc, err)
	}
	if !phonenumbers.IsValidNumber(num) {
		return fmt.Errorf("phone %q is not a valid number in region %q", phone, rc)
	}
	if got := phonenumbers.GetRegionCodeForNumber(num); got != rc {
		return fmt.Errorf("phone %q parses as region %q, not %q", phone, got, rc)
	}
	return nil
}
```

- [ ] **Step 4: 跑测试确认它们 pass**

Run: `go test ./internal/service/message/ -run TestValidateSendSMSRequest -v`
Expected: 所有 SMS 校验测试 PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/service/message/util.go internal/service/message/util_test.go
git commit -m "feat(message): validate SMS region_code+phone via phonenumbers parse+region check"
```

---

## Task 5: SMS service 层 send/persist/toProto 切换字段

**Files:**
- Modify: `internal/service/message/sms.go`

把 SendSMS 内 `req.GetTo()` 改为 parse+format 流程；persistSMSRecord 改写 RegionCode + Phone；toProtoSMSRecord 改写 RegionCode + Phone。

- [ ] **Step 1: 改 SendSMS 中的 msg 构造**

定位 `internal/service/message/sms.go` 中 `msg := &sms.Message{...}` 这一段（约 64-69 行），把它替换为：

```go
	// Parse + format to E.164 so router/providers receive an unambiguous
	// input. validateSendSMSRequest already verified parse succeeds and
	// region matches; the error here is theoretical defense-in-depth.
	num, err := phonenumbers.Parse(req.GetPhone(), req.GetRegionCode())
	if err != nil {
		if idemKey != "" {
			releaseErr := s.idem.Release(context.Background(), "sms", req.GetSenderId(), idemKey)
			if releaseErr != nil {
				slog.Error("idempotency release after phone parse failure", "key", idemKey, "error", releaseErr)
			}
		}
		return nil, xcodes.ErrBadRequest.Wrapf(err, "parse phone %q", req.GetPhone())
	}
	e164 := phonenumbers.Format(num, phonenumbers.E164)

	msg := &sms.Message{
		To:       e164,
		Content:  req.GetContent(),
		Template: req.GetTemplateId(),
		Params:   models.MapStringString(req.GetTemplateParams()),
	}
```

在 import 块里加 `"github.com/nyaruka/phonenumbers"`。

- [ ] **Step 2: 改 persistSMSRecord 的 record 构造**

定位 `persistSMSRecord`（约 364-376 行），把 record 字段中的 `Target: req.GetTo(),` 删除并加：

```go
		RegionCode:     req.GetRegionCode(),
		Phone:          req.GetPhone(),
```

完整字段块：

```go
	record := &models.MessageSMSRecord{
		ID:             id,
		Vendor:         int32(result.Vendor),
		Account:        result.Account,
		Scene:          int32(req.GetScene()),
		RegionCode:     req.GetRegionCode(),
		Phone:          req.GetPhone(),
		SenderID:       req.GetSenderId(),
		Content:        req.GetContent(),
		TemplateID:     req.GetTemplateId(),
		TemplateParams: models.MapStringString(req.GetTemplateParams()),
		Attempts:       result.Attempts,
	}
```

- [ ] **Step 3: 改 toProtoSMSRecord**

定位 `toProtoSMSRecord`（约 395-416 行），把 `Target: r.Target,` 替换为：

```go
		RegionCode:     r.RegionCode,
		Phone:          r.Phone,
```

- [ ] **Step 4: 改 ListSMS / ListSMSByCursor 的 filter 构造**

`ListSMS` 中（约 183-191 行）和 `ListSMSByCursor` 中（约 230-238 行），把 `Target: req.GetTarget(),` 改为：

```go
		RegionCode:     req.GetRegionCode(),
		Phone:          req.GetPhone(),
```

（这两个 filter 字段在 Task 6 中加进 SmsListFilter；现在引用还没存在，编译会失败，但下一个 task 会修。）

- [ ] **Step 5: 跑 build 暴露 SmsListFilter 缺失字段错误（这是下一个 task 要修的）**

Run: `go build ./internal/service/message/`
Expected: 编译错误指向 `dal.SmsListFilter` 没有 `RegionCode` / `Phone` 字段。**这是预期的**，下一个 task 修。

- [ ] **Step 6: 暂不 commit，先做 Task 6 再合并 commit**

进 Task 6。

---

## Task 6: SMS dal List/Filter 切换字段

**Files:**
- Modify: `internal/store/dal/sms_record.go`
- Test: `internal/store/dal/sms_record_test.go`

`SmsListFilter` 删 Target，加 RegionCode + Phone。`applySMSListFilter` 同步改。test helper `newTestSMSRecord` 也得改。

- [ ] **Step 1: 改 SmsListFilter 结构体**

定位 `internal/store/dal/sms_record.go:18-30`，把整个 struct 改为：

```go
// SmsListFilter mirrors EmailListFilter for SMS records. See its doc comment
// for the rationale on splitting pagination fields out of the filter.
type SmsListFilter struct {
	Vendor        pb.SmsVendor
	Scene         pb.SmsScene
	Status        pb.MessageStatus
	RegionCode    string
	Phone         string
	SenderID      string
	StartTime     *time.Time
	EndTime       *time.Time
	SortField     pb.SortField
	SortDirection pb.SortDirection
}
```

- [ ] **Step 2: 改 applySMSListFilter**

定位 `applySMSListFilter`（约 237-260 行），把其中：

```go
	if f.Target != "" {
		q = q.Where(generated.MessageSMSRecord.Target.Eq(f.Target))
	}
```

替换为：

```go
	if f.RegionCode != "" {
		q = q.Where(generated.MessageSMSRecord.RegionCode.Eq(f.RegionCode))
	}
	if f.Phone != "" {
		q = q.Where(generated.MessageSMSRecord.Phone.Eq(f.Phone))
	}
```

- [ ] **Step 3: 改 sms_record_test.go 的 helper 与断言**

定位 `internal/store/dal/sms_record_test.go`：

`newTestSMSRecord` 改为：

```go
func newTestSMSRecord(status int32, scene int32, regionCode, phone string) *models.MessageSMSRecord {
	return &models.MessageSMSRecord{
		ID:         time.Now().UnixNano(),
		Vendor:     int32(pb.SmsVendor_SMS_VENDOR_ALIYUN),
		Scene:      scene,
		Status:     status,
		RegionCode: regionCode,
		Phone:      phone,
		Content:    "Your code: 1234",
		SenderID:   "user:42",
		Attempts:   1,
	}
}
```

`newTestSMSRecordAt` 改为：

```go
func newTestSMSRecordAt(id int64, createdAt time.Time, phone string) *models.MessageSMSRecord {
	r := newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN",
		phone,
	)
	r.ID = id
	r.CreatedAt = createdAt
	r.UpdatedAt = createdAt
	return r
}
```

所有 `newTestSMSRecord` 调用处的 `"+8613800001111"` 等参数改为 `"CN", "13800000111"` 等本地号码（删去 `+86` 前缀，分两参传）。具体修改点：

- `TestCreateSMSRecord`：`newTestSMSRecord(...SENT, ...LOGIN_CODE, "+8613800001111")` → `newTestSMSRecord(...SENT, ...LOGIN_CODE, "CN", "13800000111")`，断言 `"+8613800001111" == found.Target` 改为 `"CN" == found.RegionCode && "13800000111" == found.Phone`
- `TestListSMSRecords_ByScene`：两条记录同样改造，断言 `"+8613800001111" == result.List[0].Target` 改为 `"13800000111" == result.List[0].Phone`
- `TestCountSMSStats`：两条记录改造
- `TestListSMSVendorStats`：一条记录改造
- `TestListSMSRecords_Tiebreaker_StablePagination` / `TestListSMSRecords_ASC_Ordering` / `TestListSMSByCursor_FullSweep` / `TestListSMSByCursor_ASC`：把 `fmt.Sprintf("u%d@x.com", i)` 改为 `"CN", fmt.Sprintf("1380000%04d", i)`（任何稳定生成本地号的写法都可）

- [ ] **Step 4: 跑 SMS dal 测试**

Run: `go test ./internal/store/dal/ -run SMS -v`
Expected: PASS。

- [ ] **Step 5: 跑 SMS service 测试**

Run: `go test ./internal/service/message/ -run SMS -v`
Expected: 编译通过，所有 SMS 相关测试 PASS。如果有 `sms_test.go` 中 SendSMS 集成测试用了 `To` 字段，改为 `RegionCode: "CN", Phone: "13800000111"`。

如果 sms_test.go 引用了 `req.GetTo()` 或 `pb.SendSMSRequest{To: ...}`，把所有这种引用都改为 region_code + phone。

- [ ] **Step 6: 跑全量 build 验证**

Run: `go build ./...`
Expected: 编译通过（testclient 还没改，可能仍有错——下一步 Task 11 修）。

如果只有 `cmd/testclient/` 报错，可以进 Task 7。如果 service 或 dal 报错，回到上面对应 Step 修。

- [ ] **Step 7: Commit Task 5 + Task 6 的改动合并**

```bash
git add internal/service/message/sms.go internal/store/dal/sms_record.go internal/store/dal/sms_record_test.go internal/service/message/sms_test.go
git commit -m "refactor(sms): switch service+dal from target to region_code+phone"
```

---

## Task 7: TDD ListSMSRegions（dal + service + facade）

**Files:**
- Modify: `internal/store/dal/sms_record.go`
- Modify: `internal/store/dal/sms_record_test.go`
- Modify: `internal/service/message/sms.go`
- Modify: `internal/service/service.go`

- [ ] **Step 1: 写 dal 测试**

在 `internal/store/dal/sms_record_test.go` 末尾追加：

```go
func TestListSMSRegions_Distinct(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN", "13800000111",
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"CN", "13800000222",  // same region, different phone
	)))
	require.NoError(t, CreateSMSRecord(ctx, db, newTestSMSRecord(
		int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE),
		"HK", "91234567",
	)))

	regions, err := ListSMSRegions(ctx, db, SmsAggregationFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"CN", "HK"}, regions)
}

func TestListSMSRegions_FilterBySender(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	r1 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "CN", "13800000111")
	r1.SenderID = "user-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r1))

	r2 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "HK", "91234567")
	r2.SenderID = "pay-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r2))

	regions, err := ListSMSRegions(ctx, db, SmsAggregationFilter{SenderID: "user-service"})
	require.NoError(t, err)
	assert.Equal(t, []string{"CN"}, regions)
}

func TestListSMSRegions_Empty(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	regions, err := ListSMSRegions(ctx, db, SmsAggregationFilter{})
	require.NoError(t, err)
	assert.Empty(t, regions)
}
```

- [ ] **Step 2: 跑测试确认 fail**

Run: `go test ./internal/store/dal/ -run TestListSMSRegions`
Expected: 编译错误（`ListSMSRegions` 和 `SmsAggregationFilter` 还未定义）。

- [ ] **Step 3: 在 sms_record.go 加 SmsAggregationFilter 和 ListSMSRegions**

在 `internal/store/dal/sms_record.go` 中，紧跟 `SmsVendorStat` struct 之后追加：

```go
// SmsAggregationFilter holds parameters for SMS aggregation queries
// (ListSMSRegions / ListSMSSenderIDs). All fields optional — empty means
// no filter on that dimension. The aggregated field itself is NOT in the
// filter (e.g. ListSMSRegions does not filter by region_code).
type SmsAggregationFilter struct {
	Vendor     pb.SmsVendor
	Scene      pb.SmsScene
	Status     pb.MessageStatus
	SenderID   string  // used by ListSMSRegions; ignored by ListSMSSenderIDs
	RegionCode string  // used by ListSMSSenderIDs; ignored by ListSMSRegions
	StartTime  *time.Time
	EndTime    *time.Time
}
```

在文件末尾（`applySmsCursor` 之后）追加：

```go
// applySmsAggregationFilter attaches optional WHERE clauses for aggregation
// queries. Mirrors applySmsStatsWhere but on the untyped *gorm.DB chain used
// by SELECT DISTINCT queries.
func applySmsAggregationFilter(q *gorm.DB, f SmsAggregationFilter) *gorm.DB {
	if f.Vendor != 0 {
		q = q.Where("vendor = ?", int32(f.Vendor))
	}
	if f.Scene != 0 {
		q = q.Where("scene = ?", int32(f.Scene))
	}
	if f.Status != 0 {
		q = q.Where("status = ?", int32(f.Status))
	}
	if f.SenderID != "" {
		q = q.Where("sender_id = ?", f.SenderID)
	}
	if f.RegionCode != "" {
		q = q.Where("region_code = ?", f.RegionCode)
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at <= ?", *f.EndTime)
	}
	return q
}

// ListSMSRegions returns distinct region_code values matching the filter,
// ordered ascending. Used by the frontend to populate SMS list filter
// dropdowns.
func ListSMSRegions(ctx context.Context, tx *gorm.DB, f SmsAggregationFilter) ([]string, error) {
	q := tx.WithContext(ctx).Model(&models.MessageSMSRecord{}).
		Distinct("region_code").
		Where("region_code != ''").
		Order("region_code ASC")
	q = applySmsAggregationFilter(q, f)

	var regions []string
	if err := q.Scan(&regions).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return regions, nil
}
```

- [ ] **Step 4: 跑 dal 测试确认 pass**

Run: `go test ./internal/store/dal/ -run TestListSMSRegions -v`
Expected: 三个测试 PASS。

- [ ] **Step 5: 在 service/message/sms.go 加 ListSMSRegions 方法**

在 `GetSMSStats` 之后追加：

```go
// ListSMSRegions returns distinct region_code values matching the filter,
// for frontend SMS list filter dropdowns.
func (s *Service) ListSMSRegions(ctx context.Context, req *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	if !s.persistSMSEnabled {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsAggregationFilter{
		Vendor:   req.GetVendor(),
		Scene:    req.GetScene(),
		Status:   req.GetStatus(),
		SenderID: req.GetSenderId(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	regions, err := dal.ListSMSRegions(ctx, s.db, f)
	if err != nil {
		return nil, err
	}
	return &pb.ListSMSRegionsResponse{RegionCodes: regions}, nil
}
```

- [ ] **Step 6: 在 service/service.go 加 facade 方法**

在 `internal/service/service.go` 中 `GetSMSStats` facade 之后追加：

```go
// ListSMSRegions delegates to the message subpackage.
func (s *Service) ListSMSRegions(ctx context.Context, req *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	return s.message.ListSMSRegions(ctx, req)
}
```

- [ ] **Step 7: 跑全量 build**

Run: `go build ./...`
Expected: 编译通过（除 testclient 外）。

- [ ] **Step 8: Commit**

```bash
git add internal/store/dal/sms_record.go internal/store/dal/sms_record_test.go internal/service/message/sms.go internal/service/service.go
git commit -m "feat(message): add ListSMSRegions RPC for SMS list filter dropdown"
```

---

## Task 8: TDD ListSMSSenders（dal + service + facade）

**Files:**
- Modify: `internal/store/dal/sms_record.go`
- Modify: `internal/store/dal/sms_record_test.go`
- Modify: `internal/service/message/sms.go`
- Modify: `internal/service/service.go`

与 Task 7 同结构，但聚合 sender_id 字段。

- [ ] **Step 1: 写 dal 测试**

在 `internal/store/dal/sms_record_test.go` 末尾追加：

```go
func TestListSMSSenderIDs_Distinct(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	r1 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "CN", "13800000111")
	r1.SenderID = "user-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r1))

	r2 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "CN", "13800000222")
	r2.SenderID = "user-service"  // same sender
	require.NoError(t, CreateSMSRecord(ctx, db, r2))

	r3 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "HK", "91234567")
	r3.SenderID = "pay-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r3))

	senders, err := ListSMSSenderIDs(ctx, db, SmsAggregationFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"pay-service", "user-service"}, senders)
}

func TestListSMSSenderIDs_FilterByRegion(t *testing.T) {
	db := setupSMSDB(t)
	ctx := context.Background()

	r1 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "CN", "13800000111")
	r1.SenderID = "user-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r1))

	r2 := newTestSMSRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.SmsScene_SMS_SCENE_LOGIN_CODE), "HK", "91234567")
	r2.SenderID = "pay-service"
	require.NoError(t, CreateSMSRecord(ctx, db, r2))

	senders, err := ListSMSSenderIDs(ctx, db, SmsAggregationFilter{RegionCode: "HK"})
	require.NoError(t, err)
	assert.Equal(t, []string{"pay-service"}, senders)
}
```

- [ ] **Step 2: 跑测试确认 fail**

Run: `go test ./internal/store/dal/ -run TestListSMSSenderIDs`
Expected: 编译错误（`ListSMSSenderIDs` 未定义）。

- [ ] **Step 3: 实现 ListSMSSenderIDs**

在 `internal/store/dal/sms_record.go` 中 `ListSMSRegions` 之后追加：

```go
// ListSMSSenderIDs returns distinct sender_id values matching the filter,
// ordered ascending. Used by the frontend to populate SMS list filter
// dropdowns.
func ListSMSSenderIDs(ctx context.Context, tx *gorm.DB, f SmsAggregationFilter) ([]string, error) {
	q := tx.WithContext(ctx).Model(&models.MessageSMSRecord{}).
		Distinct("sender_id").
		Where("sender_id != ''").
		Order("sender_id ASC")
	q = applySmsAggregationFilter(q, f)

	var senders []string
	if err := q.Scan(&senders).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return senders, nil
}
```

- [ ] **Step 4: 跑 dal 测试确认 pass**

Run: `go test ./internal/store/dal/ -run TestListSMSSenderIDs -v`
Expected: 两个测试 PASS。

- [ ] **Step 5: 在 service/message/sms.go 加 ListSMSSenders 方法**

在 `ListSMSRegions` 之后追加：

```go
// ListSMSSenders returns distinct sender_id values matching the filter,
// for frontend SMS list filter dropdowns.
func (s *Service) ListSMSSenders(ctx context.Context, req *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
	if !s.persistSMSEnabled {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("sms persistence is disabled"))
	}
	f := dal.SmsAggregationFilter{
		Vendor:     req.GetVendor(),
		Scene:      req.GetScene(),
		Status:     req.GetStatus(),
		RegionCode: req.GetRegionCode(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	senders, err := dal.ListSMSSenderIDs(ctx, s.db, f)
	if err != nil {
		return nil, err
	}
	return &pb.ListSMSSendersResponse{SenderIds: senders}, nil
}
```

- [ ] **Step 6: 在 service/service.go 加 facade 方法**

在 `ListSMSRegions` facade 之后追加：

```go
// ListSMSSenders delegates to the message subpackage.
func (s *Service) ListSMSSenders(ctx context.Context, req *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
	return s.message.ListSMSSenders(ctx, req)
}
```

- [ ] **Step 7: 跑 build**

Run: `go build ./...`
Expected: 编译通过（除 testclient 外）。

- [ ] **Step 8: Commit**

```bash
git add internal/store/dal/sms_record.go internal/store/dal/sms_record_test.go internal/service/message/sms.go internal/service/service.go
git commit -m "feat(message): add ListSMSSenders RPC for SMS list filter dropdown"
```

---

## Task 9: TDD ListEmailSenders（dal + service + facade）

**Files:**
- Modify: `internal/store/dal/email_record.go`
- Modify: `internal/store/dal/email_record_test.go`
- Modify: `internal/service/message/email.go`
- Modify: `internal/service/service.go`

与 Task 8 平行，但作用于 email 表。

- [ ] **Step 1: 写 dal 测试**

在 `internal/store/dal/email_record_test.go` 末尾追加（先确认该文件的 helper 函数名，参考已有测试）：

```go
func TestListEmailSenderIDs_Distinct(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	r1 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_NOTIFICATION), "user@example.com")
	r1.SenderID = "user-service"
	require.NoError(t, CreateEmailRecord(ctx, db, r1))

	r2 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_NOTIFICATION), "admin@example.com")
	r2.SenderID = "user-service"
	require.NoError(t, CreateEmailRecord(ctx, db, r2))

	r3 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_NOTIFICATION), "biz@example.com")
	r3.SenderID = "pay-service"
	require.NoError(t, CreateEmailRecord(ctx, db, r3))

	senders, err := ListEmailSenderIDs(ctx, db, EmailAggregationFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"pay-service", "user-service"}, senders)
}

func TestListEmailSenderIDs_FilterByScene(t *testing.T) {
	db := setupEmailDB(t)
	ctx := context.Background()

	r1 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_LOGIN_CODE), "user@example.com")
	r1.SenderID = "user-service"
	require.NoError(t, CreateEmailRecord(ctx, db, r1))

	r2 := newTestEmailRecord(int32(pb.MessageStatus_MESSAGE_STATUS_SENT),
		int32(pb.EmailScene_EMAIL_SCENE_NOTIFICATION), "biz@example.com")
	r2.SenderID = "pay-service"
	require.NoError(t, CreateEmailRecord(ctx, db, r2))

	senders, err := ListEmailSenderIDs(ctx, db, EmailAggregationFilter{Scene: pb.EmailScene_EMAIL_SCENE_LOGIN_CODE})
	require.NoError(t, err)
	assert.Equal(t, []string{"user-service"}, senders)
}
```

> **注意**：`setupEmailDB` / `newTestEmailRecord` 的具体签名以现有 `email_record_test.go` 里的 helper 为准。如果参数列表与上面不匹配，调整调用方参数即可。

- [ ] **Step 2: 跑测试确认 fail**

Run: `go test ./internal/store/dal/ -run TestListEmailSenderIDs`
Expected: 编译错误（`ListEmailSenderIDs` / `EmailAggregationFilter` 未定义）。

- [ ] **Step 3: 在 email_record.go 加 EmailAggregationFilter 和 ListEmailSenderIDs**

在 `internal/store/dal/email_record.go` 中 `EmailVendorStat` 之后追加：

```go
// EmailAggregationFilter holds parameters for email aggregation queries
// (currently only ListEmailSenderIDs). All fields optional — empty means
// no filter on that dimension. The aggregated field itself is NOT in the
// filter (sender_id for ListEmailSenders).
type EmailAggregationFilter struct {
	Vendor    pb.EmailVendor
	Scene     pb.EmailScene
	Status    pb.MessageStatus
	StartTime *time.Time
	EndTime   *time.Time
}
```

在文件末尾（`applyEmailCursor` 之后）追加：

```go
// applyEmailAggregationFilter attaches optional WHERE clauses for email
// aggregation queries. Mirrors applyEmailStatsWhere but adds status.
func applyEmailAggregationFilter(q *gorm.DB, f EmailAggregationFilter) *gorm.DB {
	if f.Vendor != 0 {
		q = q.Where("vendor = ?", int32(f.Vendor))
	}
	if f.Scene != 0 {
		q = q.Where("scene = ?", int32(f.Scene))
	}
	if f.Status != 0 {
		q = q.Where("status = ?", int32(f.Status))
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at <= ?", *f.EndTime)
	}
	return q
}

// ListEmailSenderIDs returns distinct sender_id values matching the filter,
// ordered ascending. Used by the frontend to populate email list filter
// dropdowns.
func ListEmailSenderIDs(ctx context.Context, tx *gorm.DB, f EmailAggregationFilter) ([]string, error) {
	q := tx.WithContext(ctx).Model(&models.MessageEmailRecord{}).
		Distinct("sender_id").
		Where("sender_id != ''").
		Order("sender_id ASC")
	q = applyEmailAggregationFilter(q, f)

	var senders []string
	if err := q.Scan(&senders).Error; err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	return senders, nil
}
```

- [ ] **Step 4: 跑 dal 测试确认 pass**

Run: `go test ./internal/store/dal/ -run TestListEmailSenderIDs -v`
Expected: 两个测试 PASS。

- [ ] **Step 5: 在 service/message/email.go 加 ListEmailSenders 方法**

在 `GetEmailStats` 之后追加：

```go
// ListEmailSenders returns distinct sender_id values matching the filter,
// for frontend email list filter dropdowns.
func (s *Service) ListEmailSenders(ctx context.Context, req *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error) {
	if !s.persistEmailEnabled {
		return nil, xcodes.ErrPersistenceDisabled.Wrap(fmt.Errorf("email persistence is disabled"))
	}
	f := dal.EmailAggregationFilter{
		Vendor: req.GetVendor(),
		Scene:  req.GetScene(),
		Status: req.GetStatus(),
	}
	if startTime := req.GetStartTime(); startTime != 0 {
		t := time.Unix(startTime, 0)
		f.StartTime = &t
	}
	if endTime := req.GetEndTime(); endTime != 0 {
		t := time.Unix(endTime, 0)
		f.EndTime = &t
	}

	senders, err := dal.ListEmailSenderIDs(ctx, s.db, f)
	if err != nil {
		return nil, err
	}
	return &pb.ListEmailSendersResponse{SenderIds: senders}, nil
}
```

- [ ] **Step 6: 在 service/service.go 加 facade 方法**

在 `ListSMSStats` facade 之后追加（或紧跟 `ListSMSByCursor` 之后，保持 service.go 文件内 RPC 注册顺序合理）：

```go
// ListEmailSenders delegates to the message subpackage.
func (s *Service) ListEmailSenders(ctx context.Context, req *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error) {
	return s.message.ListEmailSenders(ctx, req)
}
```

- [ ] **Step 7: 跑 build**

Run: `go build ./...`
Expected: 编译通过（除 testclient 外）。

- [ ] **Step 8: Commit**

```bash
git add internal/store/dal/email_record.go internal/store/dal/email_record_test.go internal/service/message/email.go internal/service/service.go
git commit -m "feat(message): add ListEmailSenders RPC for email list filter dropdown"
```

---

## Task 10: 更新 testclient

**Files:**
- Modify: `cmd/testclient/commands.go`
- Modify: `cmd/testclient/main.go`
- Modify: `cmd/testclient/client.go`

- [ ] **Step 1: 改 runSendSMS**

把 `cmd/testclient/commands.go` 中的 `runSendSMS` 改为：

```go
func runSendSMS(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("send-sms", flag.ExitOnError)
	regionCode := fs.String("region-code", "", "ISO 3166-1 alpha-2 region code, e.g. CN, US, HK (required)")
	phone := fs.String("phone", "", "local phone number without international prefix, e.g. 13800000111 (required)")
	content := fs.String("content", "", "SMS text content")
	vendor := fs.String("vendor", "", "sms vendor: aliyun|tencent|volcengine|byteplus|huawei (empty = route by country)")
	account := fs.String("account", "", "vendor account name (required if --vendor set)")
	scene := fs.String("scene", "login_code", "business scene")
	senderID := fs.String("sender", "", "sender_id (required)")
	_ = fs.Parse(args)

	if *regionCode == "" || *phone == "" || *senderID == "" {
		return fmt.Errorf("--region-code, --phone, --sender are required")
	}

	req := &pb.SendSMSRequest{
		RegionCode: *regionCode,
		Phone:      *phone,
		Content:    *content,
		Vendor:     parseSmsVendor(*vendor),
		Account:    *account,
		Scene:      parseSmsScene(*scene),
		SenderId:   *senderID,
	}
	resp, err := c.SendSMS(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("sent. id=%d status=%s\n", resp.Id, resp.Status)
	return nil
}
```

- [ ] **Step 2: 改 runListSMS / runListSMSByCursor 的输出**

把这两处输出里的 `r.Target` 改为 `r.RegionCode + " " + r.Phone`：

`runListSMS` 中：
```go
		fmt.Printf("  id=%d status=%s region=%s phone=%s\n", r.Id, r.Status, r.RegionCode, r.Phone)
```

`runListSMSByCursor` 中：同上。

- [ ] **Step 3: 加 list-sms-regions / list-sms-senders / list-email-senders 三个子命令**

在 `cmd/testclient/commands.go` 末尾追加：

```go
func runListSMSRegions(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("list-sms-regions", flag.ExitOnError)
	vendor := fs.String("vendor", "", "filter by vendor")
	scene := fs.String("scene", "", "filter by scene")
	senderID := fs.String("sender", "", "filter by sender_id")
	_ = fs.Parse(args)

	req := &pb.ListSMSRegionsRequest{SenderId: *senderID}
	if *vendor != "" {
		req.Vendor = parseSmsVendor(*vendor)
	}
	if *scene != "" {
		req.Scene = parseSmsScene(*scene)
	}
	resp, err := c.ListSMSRegions(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("regions=%d\n", len(resp.RegionCodes))
	for _, r := range resp.RegionCodes {
		fmt.Printf("  %s\n", r)
	}
	return nil
}

func runListSMSSenders(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("list-sms-senders", flag.ExitOnError)
	vendor := fs.String("vendor", "", "filter by vendor")
	scene := fs.String("scene", "", "filter by scene")
	regionCode := fs.String("region-code", "", "filter by region_code")
	_ = fs.Parse(args)

	req := &pb.ListSMSSendersRequest{RegionCode: *regionCode}
	if *vendor != "" {
		req.Vendor = parseSmsVendor(*vendor)
	}
	if *scene != "" {
		req.Scene = parseSmsScene(*scene)
	}
	resp, err := c.ListSMSSenders(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("senders=%d\n", len(resp.SenderIds))
	for _, s := range resp.SenderIds {
		fmt.Printf("  %s\n", s)
	}
	return nil
}

func runListEmailSenders(ctx context.Context, c Caller, args []string) error {
	fs := flag.NewFlagSet("list-email-senders", flag.ExitOnError)
	vendor := fs.String("vendor", "", "filter by vendor")
	scene := fs.String("scene", "", "filter by scene")
	_ = fs.Parse(args)

	req := &pb.ListEmailSendersRequest{}
	if *vendor != "" {
		req.Vendor = parseEmailVendor(*vendor)
	}
	if *scene != "" {
		req.Scene = parseEmailScene(*scene)
	}
	resp, err := c.ListEmailSenders(ctx, req)
	if err != nil {
		return err
	}
	fmt.Printf("senders=%d\n", len(resp.SenderIds))
	for _, s := range resp.SenderIds {
		fmt.Printf("  %s\n", s)
	}
	return nil
}
```

- [ ] **Step 4: 改 Caller interface + grpcClient 实现**

在 `cmd/testclient/client.go` 的 `Caller` interface 里追加三个方法签名：

```go
	ListSMSRegions(ctx context.Context, req *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error)
	ListSMSSenders(ctx context.Context, req *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error)
	ListEmailSenders(ctx context.Context, req *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error)
```

在 `grpcClient` 上追加三个实现（thin pass-through）：

```go
func (g *grpcClient) ListSMSRegions(ctx context.Context, req *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	return g.c.ListSMSRegions(ctx, req)
}

func (g *grpcClient) ListSMSSenders(ctx context.Context, req *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
	return g.c.ListSMSSenders(ctx, req)
}

func (g *grpcClient) ListEmailSenders(ctx context.Context, req *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error) {
	return g.c.ListEmailSenders(ctx, req)
}
```

- [ ] **Step 5: 在 dispatch 注册新子命令**

在 `cmd/testclient/main.go` 的 `dispatch` 函数 switch 里追加：

```go
	case "list-sms-regions":
		return runListSMSRegions(ctx, c, rest)
	case "list-sms-senders":
		return runListSMSSenders(ctx, c, rest)
	case "list-email-senders":
		return runListEmailSenders(ctx, c, rest)
```

更新 usage 字符串里的 Subcommands 列表，加这三个名字。

- [ ] **Step 6: 改 smoke-test 中的 SendSMS 请求**

把 `cmd/testclient/main.go` 中 `runSmokeTest` 里的 SendSMS 请求改为：

```go
	sr, err := c.SendSMS(ctx, &pb.SendSMSRequest{
		RegionCode: "CN",
		Phone:      "13800000000",
		Content:    fmt.Sprintf("smoke %d", stamp),
		Vendor:     pb.SmsVendor_SMS_VENDOR_ALIYUN,
		Account:    "default",
		Scene:      pb.SmsScene_SMS_SCENE_LOGIN_CODE,
		SenderId:   senderID,
	})
```

- [ ] **Step 7: 跑 build + 短测**

Run: `go build ./...`
Expected: 编译通过。

Run: `go test ./cmd/testclient/...`
Expected: PASS（如果该目录有测试）。

- [ ] **Step 8: Commit**

```bash
git add cmd/testclient/
git commit -m "feat(testclient): switch send-sms to region-code+phone, add aggregation subcommands"
```

---

## Task 11: 全量验证 + lint + fmt

**Files:** （只读 / 修复零星问题）

- [ ] **Step 1: fmt + vet + lint**

Run:
```bash
make fmt
make vet
make lint
```
Expected: 全部通过。如有 lint 错误，按提示修。

- [ ] **Step 2: 全量测试**

Run: `make test`
Expected: 所有测试 PASS，coverage 不低于改动前。

- [ ] **Step 3: migrate 跑一遍**

Run: `make migrate`
Expected: 无错误。验证 message_sms_records 表里 target 列已删除、region_code + phone 列已加（可用 `\d message_sms_records` 在 psql 里看）。

- [ ] **Step 4: 启动服务跑 smoke-test（如果方便起 docker）**

Run:
```bash
make docker-up
make docker-smoke
make docker-down
```
Expected: smoke-test 通过。

如果不起 docker，至少 `make run` + 手动 `./bin/msgclient send-sms --region-code CN --phone 13800000000 --sender smoke --vendor aliyun --account default` 验证一次。

- [ ] **Step 5: 最终 commit（如果有 fixup）**

如果前面任何 task 漏了细节，这里修完一起 commit：

```bash
git add -A
git commit -m "chore: fixups from final verification"
```

如果没有任何 fixup，跳过本步。

---

## 完成判据

- [ ] proto 中 SendSMSRequest 用 region_code + phone，不再有 to
- [ ] SMSRecord / ListSMSRequest / ListSMSByCursorRequest 同步
- [ ] SenderID 在所有相关 proto message 和 GORM model 上有"calling business service"注释
- [ ] message_sms_records 表无 target 列，有 region_code + phone 列（带索引）
- [ ] message_email_records 表 schema 未变（SenderID 注释加在 Go model 上，DB 列不变）
- [ ] validateSendSMSRequest 5 条新规则全过单测
- [ ] SendSMS 用 phonenumbers 库 parse + format E.164 传给 router
- [ ] router 与各 SMS provider 代码零改动
- [ ] ListSMSRegions / ListSMSSenders / ListEmailSenders 三个 RPC 各自有 dal + service + facade + testclient 子命令
- [ ] testclient 的 send-sms / list-sms / smoke-test 用新字段
- [ ] `make all`（fmt+vet+lint+test）全过
