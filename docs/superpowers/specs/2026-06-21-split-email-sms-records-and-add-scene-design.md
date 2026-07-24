# 拆分 Email/SMS 记录表 + 新增 Scene 场景枚举 — 设计文档

**日期**: 2026-06-21
**主题**: 把混合的 `message_records` 表拆分为 `email_records` 与 `sms_records` 两张表；同时在发送请求和记录中新增 `scene` 字段（登录验证码 / 找回密码 / 注册验证等），并在 SendEmail/SendSMS 接口加 `sender_id` 标识发送归属。

## 1. 背景与目标

### 现状

当前所有发送记录（邮件 + 短信）都写入单张 `message_records` 表，通过 `channel` 字段区分；vendor 用两个互斥列 `email_vendor` 和 `sms_vendor` 存放（因为两个 enum 的 int 空间有重叠，必须分两列）；proto 里 `MessageRecord` 也是统一消息体，vendor 走 oneof。

存在的问题：

1. **表结构混淆**：email 独有字段（`cc` / `bcc` / `subject` / `html_body` / `reply_to`）在 SMS 行里永远空着；SMS 独有字段（`template_params`）在 email 行里也永远空着
2. **vendor 列互斥的 workaround**：`COALESCE(NULLIF(email_vendor, 0), sms_vendor)` 这种 raw SQL 是为了绕开 int 空间重叠，拆表后自然消失
3. **缺少业务场景标识**：所有发送记录都是"一条消息"，无法按业务场景（登录验证码 / 找回密码 / 注册）查询、统计、追溯
4. **缺少发送归属**：`sender_id` 字段在原表已存在，但 service 层从未赋值，无法回答"这条消息是谁触发的"

### 目标

- 把 `message_records` 拆为 `email_records` 和 `sms_records` 两张表，字段各自对齐业务语义
- proto 层也彻底拆分：删除 `Channel` / `MessageRecord` / 统一 stats，引入 `EmailRecord` / `SMSRecord` / `EmailScene` / `SmsScene`
- 查询接口按渠道拆为 6 个 RPC（Get/List/Stats × Email/SMS）
- 新增 `EmailScene` / `SmsScene` 两个 enum，在 SendEmail / SendSMS 请求中**必填**
- 新增 `sender_id` 字段，记录每次发送的业务归属（user_id / admin_id / system 等），**必填**
- 邮件侧补齐 `template_id` / `template_params` 字段，对称 SMS 设计；go-common 同步扩展 `email.Message` 增加这两个可选字段

### 非目标

- **不做数据迁移**：项目尚在开发期，直接 drop `message_records` 表
- **不做发送频控**：scene-based rate-limit 属于 `captcha` 包职责，不在 message-service 范围
- **不做邮件模板渲染**：本次只在数据/proto/go-common 层"预留" template 字段，实际渲染逻辑等未来需要时再做
- **不改 vendor 配置 / 注册逻辑**：`internal/provider/{email,sms}` 保持现状
- **不改 cmd/server、cmd/migrate 启动结构**：仅 AllModels 列表更新

## 2. 方案对比（决策记录）

### 已选方案：彻底拆分（A）

- DB: 删 `message_records`，新增 `email_records` + `sms_records`
- Proto: 删 `Channel` / `MessageRecord` / `EmailVendorStats` / `SmsVendorStats`；新增 `EmailRecord` / `SMSRecord` / `EmailScene` / `SmsScene`
- RPC: `SendEmail` / `SendSMS` 保留；新增 `Get/List/Stats × Email/SMS` 共 6 个；删除 `Get/List/Stats × Messages` 共 3 个
- vendor 直接用对应 enum 字段（不再 oneof）

### 否决方案

- **B（表拆 proto 不拆）**：保留统一 `MessageRecord` + oneof vendor，查询接口内部路由。proto 改动最小但 oneof 仍然存在、字段语义混乱（Cc/Bcc 在 SMS 行永远空）。前面已否决 union 查询、选了 6 个 RPC，此方案与决策冲突。
- **C（A + 保留 channel 字段）**：EmailRecord/SMSRecord 都保留 `channel` 字段方便调试。纯冗余（表名已蕴含），违背 YAGNI。

## 3. Proto 改动（§1）

### 3.1 新增 enum

```proto
enum EmailScene {
  EMAIL_SCENE_UNSPECIFIED = 0;
  EMAIL_SCENE_LOGIN_CODE = 1;
  EMAIL_SCENE_FORGOT_PASSWORD = 2;
  EMAIL_SCENE_REGISTER = 3;
  EMAIL_SCENE_CHANGE_PASSWORD = 4;
  EMAIL_SCENE_BIND_ACCOUNT = 5;
  EMAIL_SCENE_NOTIFICATION = 6;
}

enum SmsScene {
  SMS_SCENE_UNSPECIFIED = 0;
  SMS_SCENE_LOGIN_CODE = 1;
  SMS_SCENE_FORGOT_PASSWORD = 2;
  SMS_SCENE_REGISTER = 3;
  SMS_SCENE_CHANGE_PASSWORD = 4;
  SMS_SCENE_BIND_ACCOUNT = 5;
}
```

初始 6 个（用户提到的登录/找回密码/注册 + 3 个常用扩展），后续按需新增。EmailScene 比 SmsScene 多一个 NOTIFICATION（营销/通知邮件常见，短信一般不发系统通知）。

### 3.2 删除

- `enum Channel`（拆表后无意义）
- `message MessageRecord`（拆为 EmailRecord + SMSRecord）

注：`EmailVendorStats` / `SmsVendorStats` 保留为顶层 message（沿用现状），不改为嵌套定义。

### 3.3 新增 message

```proto
message EmailRecord {
  int64 id = 1;
  EmailVendor vendor = 2;
  string account = 3;
  EmailScene scene = 4;
  MessageStatus status = 5;
  string target = 6;
  string sender_id = 7;             // 发送归属（user_id / admin_id / system 等）
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
```

字段差异说明：
- `EmailRecord` 独有：`cc` / `bcc` / `subject` / `html_body` / `reply_to`
- `SMSRecord` 独有：无（template 字段两边对称）
- 共有：`id` / `vendor` / `account` / `scene` / `status` / `target` / `sender_id` / `content` / `template_id` / `template_params` / `error_message` / `attempts` / `sent_at` / `created_at` / `updated_at`

注：proto 不暴露 `deleted_at`（软删除对调用方透明）。

### 3.4 SendEmailRequest / SendSMSRequest

```proto
message SendEmailRequest {
  string to = 1 [(buf.validate.field).string.email = true];
  repeated string cc = 2 [(buf.validate.field).repeated.items.string.email = true];
  repeated string bcc = 3 [(buf.validate.field).repeated.items.string.email = true];
  string subject = 4 [(buf.validate.field).string.min_len = 1];
  string body = 5;
  string html_body = 6;
  string reply_to = 7 [(buf.validate.field).string.email = true];
  EmailVendor vendor = 8;
  string account = 9;
  string template_id = 10;             // 新增，可选
  map<string, string> template_params = 11;  // 新增，可选
  EmailScene scene = 12;               // 新增，必填
  string sender_id = 13;               // 新增，必填

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

message SendSMSRequest {
  string to = 1 [(buf.validate.field).string.min_len = 1];
  string content = 2;
  string template_id = 3;
  map<string, string> template_params = 4;
  SmsVendor vendor = 5;
  string account = 6;
  SmsScene scene = 7;                  // 新增，必填
  string sender_id = 8;                // 新增，必填

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
```

### 3.5 SendResponse

```proto
message SendResponse {
  int64 id = 1;
  MessageStatus status = 2;
  // vendor 仍用 oneof，因为响应同时承载 email/sms 的成功结果
  oneof vendor {
    EmailVendor email_vendor = 3;
    SmsVendor sms_vendor = 4;
  }
}
```

### 3.6 查询接口（6 个新 RPC）

```proto
service MessageService {
  // 保留
  rpc SendEmail(SendEmailRequest) returns (SendResponse) { ... }
  rpc SendSMS(SendSMSRequest) returns (SendResponse) { ... }

  // 新增（替换 GetMessage / ListMessages / GetMessageStats）
  rpc GetEmail(GetEmailRequest) returns (EmailRecord) {
    option (google.api.http) = { get: "/v1/emails/{id}" };
  }
  rpc ListEmails(ListEmailsRequest) returns (ListEmailsResponse) {
    option (google.api.http) = { get: "/v1/emails" };
  }
  rpc GetEmailStats(GetEmailStatsRequest) returns (EmailStatsResponse) {
    option (google.api.http) = { get: "/v1/emails:stats" };
  }
  rpc GetSMS(GetSMSRequest) returns (SMSRecord) {
    option (google.api.http) = { get: "/v1/sms/{id}" };
  }
  rpc ListSMS(ListSMSRequest) returns (ListSMSResponse) {
    option (google.api.http) = { get: "/v1/sms" };
  }
  rpc GetSMSStats(GetSMSStatsRequest) returns (SMSStatsResponse) {
    option (google.api.http) = { get: "/v1/sms:stats" };
  }
}
```

### 3.7 Request / Response 消息定义

```proto
message GetEmailRequest {
  int64 id = 1 [(buf.validate.field).int64.gt = 0];
}

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

message ListEmailsResponse {
  repeated EmailRecord records = 1;
  int32 total = 2;
}

message GetEmailStatsRequest {
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  int64 start_time = 3;
  int64 end_time = 4;
}

message EmailStatsResponse {
  int64 total = 1;
  int64 sent = 2;
  int64 failed = 3;
  double success_rate = 4;
  repeated EmailVendorStats vendors = 5;
}

message EmailVendorStats {
  EmailVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}

// SMS 对称定义
message GetSMSRequest { int64 id = 1 [(buf.validate.field).int64.gt = 0]; }
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
message ListSMSResponse {
  repeated SMSRecord records = 1;
  int32 total = 2;
}
message GetSMSStatsRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  int64 start_time = 3;
  int64 end_time = 4;
}
message SMSStatsResponse {
  int64 total = 1;
  int64 sent = 2;
  int64 failed = 3;
  double success_rate = 4;
  repeated SmsVendorStats vendors = 5;
}

message SmsVendorStats {
  SmsVendor vendor = 1;
  int64 total = 2;
  int64 sent = 3;
  int64 failed = 4;
}
```

vendor stats 仍走顶层 message（与原 `EmailVendorStats` / `SmsVendorStats` 风格一致），不改为嵌套定义。

### 3.8 路由总览

| 方法 | 路径 |
|------|------|
| POST | `/v1/messages:email` |
| POST | `/v1/messages:sms` |
| GET | `/v1/emails/{id}` |
| GET | `/v1/emails` |
| GET | `/v1/emails:stats` |
| GET | `/v1/sms/{id}` |
| GET | `/v1/sms` |
| GET | `/v1/sms:stats` |

## 4. go-common 改动（§1.5）

`go-common/message/email/sender.go` 的 `Message` 增加两个可选字段：

```go
type Message struct {
    To       string
    Cc       []string
    Bcc      []string
    Subject  string
    Body     string
    HTMLBody string
    ReplyTo  string
    Template       string            // 新增，可选
    TemplateParams map[string]string // 新增，可选
}
```

现有 SMTP / Mailgun 实现**不消费**这两个字段（保持现状，行为不变）。未来哪个 vendor 要做模板渲染，自行消费即可。message-service 把 `req.template_id/template_params` 透传到 `emailcommon.Message`，并持久化到 `email_records`。

go-common 改动属于本次 spec 范围内的硬性依赖，必须先于 message-service 改动完成。

## 5. GORM Model（§2）

### 5.1 `internal/store/models/email_record.go`

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
    Vendor         int32           `gorm:"not null;default:0;index"`   // EmailVendor enum int32
    Account        string          `gorm:"size:64;column:account"`
    Scene          int32           `gorm:"not null;default:0;index"`   // EmailScene enum int32
    Status         int32           `gorm:"not null;default:0;index"`
    Target         string          `gorm:"size:255;not null;index"`   // to
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

### 5.2 `internal/store/models/sms_record.go`

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
    Vendor         int32           `gorm:"not null;default:0;index"`   // SmsVendor enum int32
    Account        string          `gorm:"size:64;column:account"`
    Scene          int32           `gorm:"not null;default:0;index"`   // SmsScene enum int32
    Status         int32           `gorm:"not null;default:0;index"`
    Target         string          `gorm:"size:255;not null;index"`   // phone
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

### 5.3 自定义类型 & genconfig

`internal/store/models/types.go`（新文件，从 `message_record.go` 抽出共享类型）：

```go
package models

import (
    "database/sql/driver"
    "encoding/json"
    "fmt"
)

// MapStringString is a JSONB-compatible map for template parameters.
type MapStringString map[string]string

func (m *MapStringString) Scan(value any) error { /* unchanged */ }
func (m MapStringString) Value() (driver.Value, error) { /* unchanged */ }

// StringSlice is a JSONB-compatible string slice for list fields like Cc/Bcc.
type StringSlice []string

func (s *StringSlice) Scan(value any) error { /* unchanged */ }
func (s StringSlice) Value() (driver.Value, error) { /* unchanged */ }
```

`internal/store/models/genconfig.go` 改 `AllModels()`：

```go
func AllModels() []any {
    return []any{
        &EmailRecord{},
        &SMSRecord{},
    }
}
```

### 5.4 删除

- `internal/store/models/message_record.go` 整文件删除（含 `MessageStatsRow`、`MessageRecordStatsQuery`、原 `MapStringString` / `StringSlice` 定义）

### 5.5 新增 stats 查询模板

`internal/store/models/email_record_query.go`：

```go
//go:generate gorm gen — see Makefile target `generate`

type EmailStatsRow struct {
    Vendor int32
    Total  int64
    Sent   int64
    Failed int64
}

// 拆表后不再需要 COALESCE/NULLIF：每张表只有一个 vendor 列
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

`internal/store/models/sms_record_query.go` 对称定义 `SmsStatsRow` 和 `SMSRecordStatsQuery[T]`。

## 6. DAL 层（§3）

### 6.1 `internal/store/dal/email_record.go`

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

type EmailListFilter struct {
    Vendor   pb.EmailVendor
    Scene    pb.EmailScene
    Status   pb.MessageStatus
    Target   string
    SenderID string
    StartTime *time.Time
    EndTime   *time.Time
    Page     int32
    PageSize int32
}

type EmailStatsFilter struct {
    Vendor    pb.EmailVendor
    Scene     pb.EmailScene
    StartTime *time.Time
    EndTime   *time.Time
}

type EmailVendorStat struct {
    Vendor pb.EmailVendor
    Total  int64
    Sent   int64
    Failed int64
}

func CreateEmailRecord(ctx context.Context, tx *gorm.DB, record *models.EmailRecord) error { /* ... */ }
func GetEmailRecord(ctx context.Context, tx *gorm.DB, id int64) (*models.EmailRecord, error) { /* ... */ }
func ListEmailRecords(ctx context.Context, tx *gorm.DB, filter EmailListFilter) ([]*models.EmailRecord, int64, error) { /* ... */ }
func CountEmailStats(ctx context.Context, tx *gorm.DB, filter EmailStatsFilter) (*Stats, error) { /* ... */ }
func ListEmailVendorStats(ctx context.Context, tx *gorm.DB, filter EmailStatsFilter) ([]EmailVendorStat, error) { /* ... */ }
```

`Stats` 类型（含 Total/Sent/Failed/SuccessRate）保留在 dal 包内（共享给 email 和 sms）。

### 6.2 `internal/store/dal/sms_record.go`

对称定义：

```go
type SmsListFilter struct {
    Vendor   pb.SmsVendor
    Scene    pb.SmsScene
    Status   pb.MessageStatus
    Target   string
    SenderID string
    StartTime *time.Time
    EndTime   *time.Time
    Page     int32
    PageSize int32
}

type SmsStatsFilter struct {
    Vendor    pb.SmsVendor
    Scene     pb.SmsScene
    StartTime *time.Time
    EndTime   *time.Time
}

type SmsVendorStat struct {
    Vendor pb.SmsVendor
    Total  int64
    Sent   int64
    Failed int64
}

func CreateSMSRecord(...) error
func GetSMSRecord(...) (*models.SMSRecord, error)
func ListSMSRecords(...) ([]*models.SMSRecord, int64, error)
func CountSMSStats(...) (*Stats, error)
func ListSMSVendorStats(...) ([]SmsVendorStat, error)
```

### 6.3 删除

- `internal/store/dal/message_record.go`

## 7. Service 层（§3）

### 7.1 文件组织

```
internal/service/message/
├── service.go              # Service struct + New + 公共依赖（db/gid/registries/router）
├── email.go                # SendEmail/GetEmail/ListEmails/GetEmailStats/persist/toProto
├── sms.go                  # SendSMS/GetSMS/ListSMS/GetSMSStats/persist/toProto
├── email_test.go           # 拆自 message_test.go 的 email 部分
└── sms_test.go             # 拆自 message_test.go 的 sms 部分
```

不拆 `internal/service/email/` + `internal/service/sms/` 子包的理由：
- 共享 db/gid/registries 依赖，拆开后还要在父 service 装配，徒增间接层
- 每个渠道 200 行级别，单文件足够
- "消息发送"是一个领域，email/sms 是同一领域的两个 channel
- 真的变复杂再拆（YAGNI）

### 7.2 `service.go` 内容

```go
package message

type Service struct {
    db            *gorm.DB
    gid           thirdcall.GIDService
    emailRegistry *email.AccountRegistry
    smsRegistry   *sms.AccountRegistry
    smsRouter     *sms.Router
}

func New(db *gorm.DB, gid thirdcall.GIDService, ...) *Service { /* unchanged */ }
```

业务方法（`SendEmail` / `GetEmail` / `ListEmails` / `GetEmailStats` / `SendSMS` / `GetSMS` / `ListSMS` / `GetSMSStats`）分别在 `email.go` / `sms.go` 里实现。

### 7.3 `email.go` 关键变更（相对现状）

1. `SendEmail` 持久化时增加 `Scene`、`SenderID`、`TemplateID`、`TemplateParams` 字段写入
2. `SendEmail` 透传 `req.template_id` / `req.template_params` 到 `emailcommon.Message`
3. `persistEmailRecord` 写入新表 `email_records`，新字段一并写入
4. `toProtoEmailRecord` 把 `EmailRecord` model 转 `pb.EmailRecord`

### 7.4 `sms.go` 关键变更

对称：
1. `SendSMS` 持久化时增加 `Scene`、`SenderID` 字段写入
2. `persistSMSRecord` 写入新表 `sms_records`
3. `toProtoSMSRecord` 转换

### 7.5 父 `service.Service`（`internal/service/service.go`）

facade 方法调整为新方法集：

```go
func (s *Service) SendEmail(ctx, req) (*pb.SendResponse, error) { return s.msg.SendEmail(ctx, req) }
func (s *Service) SendSMS(ctx, req) (*pb.SendResponse, error) { return s.msg.SendSMS(ctx, req) }
func (s *Service) GetEmail(ctx, req) (*pb.EmailRecord, error) { return s.msg.GetEmail(ctx, req) }
func (s *Service) ListEmails(ctx, req) (*pb.ListEmailsResponse, error) { return s.msg.ListEmails(ctx, req) }
func (s *Service) GetEmailStats(ctx, req) (*pb.EmailStatsResponse, error) { return s.msg.GetEmailStats(ctx, req) }
func (s *Service) GetSMS(ctx, req) (*pb.SMSRecord, error) { return s.msg.GetSMS(ctx, req) }
func (s *Service) ListSMS(ctx, req) (*pb.ListSMSResponse, error) { return s.msg.ListSMS(ctx, req) }
func (s *Service) GetSMSStats(ctx, req) (*pb.SMSStatsResponse, error) { return s.msg.GetSMSStats(ctx, req) }
```

## 8. Handler 层

`pkg/handler/message.go` 每个 RPC 一行委托：

```go
func (h *Handler) GetEmail(ctx, req) (*pb.EmailRecord, error) { return h.svc.GetEmail(ctx, req) }
func (h *Handler) ListEmails(ctx, req) (*pb.ListEmailsResponse, error) { return h.svc.ListEmails(ctx, req) }
// ... 共 8 个 RPC
```

## 9. 错误码（§4）

`pkg/xcodes/message.go` 拆分错误码：

```go
var ErrEmailNotFound = xerr.New("EMAIL_NOT_FOUND", xerr.CategoryNotFound, 404, "email record not found")
var ErrSMSNotFound   = xerr.New("SMS_NOT_FOUND",   xerr.CategoryNotFound, 404, "sms record not found")

// 保留（channel 无关）
var ErrBadRequest       = xerr.New(...)
var ErrInternal         = xerr.New(...)
var ErrMessageSendFailed = xerr.New(...)
```

删除 `ErrMessageNotFound`。

## 10. 测试

### 10.1 DAL 测试

- `internal/store/dal/email_record_test.go`：Create / Get / List（含 scene 过滤）/ Stats / VendorStats
- `internal/store/dal/sms_record_test.go`：对称

测试用 `dbx.SetupTestDB(t)` 启动 PostgreSQL testcontainer，每个用例 AutoMigrate `EmailRecord` 或 `SMSRecord`。

### 10.2 Service 测试

- `internal/service/message/email_test.go`：SendEmail（success / fail）、GetEmail、ListEmails、GetEmailStats
- `internal/service/message/sms_test.go`：对称

mock provider 沿用现状；DB 走真实 dal。

### 10.3 删除

- `internal/store/dal/message_record_test.go`
- `internal/service/message/message_test.go`（拆分到 email_test.go / sms_test.go）

## 11. 实施步骤（顺序）

1. **go-common 改动**：`email.Message` 加 Template / TemplateParams 字段；提交 go-common
2. **proto 改动**：改 `api/proto/message/v1/message.proto`，跑 `make proto`（buf gen）生成 `gen/`
3. **store 层**：
   - 新增 `models/email_record.go` / `models/sms_record.go` / `models/types.go` / `models/email_record_query.go` / `models/sms_record_query.go`
   - 删除 `models/message_record.go`
   - 更新 `models/genconfig.go` 的 `AllModels()`
   - 跑 `make generate`（gorm gen）
4. **dal 层**：
   - 新增 `dal/email_record.go` / `dal/sms_record.go`
   - 删除 `dal/message_record.go`
5. **service 层**：
   - 拆 `internal/service/message/message.go` 为 `email.go` + `sms.go` + `service.go`
   - 更新父 `internal/service/service.go` 的 facade 方法集
6. **handler 层**：更新 `pkg/handler/message.go` 的 RPC 方法集
7. **xcodes**：拆 `ErrMessageNotFound` 为 `ErrEmailNotFound` + `ErrSMSNotFound`
8. **测试**：拆分并更新所有相关测试
9. **本地验证**：`make migrate` 跑迁移；`make test` 跑测试；`make lint` 跑 golangci-lint
10. **CI 验证**：`git diff --exit-code` 确保 generated 与 models 同步

## 12. 风险与缓解

| 风险 | 缓解 |
|------|------|
| proto 删除字段导致 wire 不兼容 | 项目开发期，无外部调用方依赖；接受 breaking change |
| go-common 改动需要先发布 | 使用 local replace（`=> ../go-common`），改动即时生效；message-service 改动依赖此 commit |
| 测试覆盖不全导致回归 | DAL/Service 测试覆盖全部新 RPC，包括 scene 过滤、sender_id 持久化、vendor stats 简化 |
| 字段编号冲突 | EmailRecord / SMSRecord 是新 message，从 1 开始编号；SendEmailRequest / SendSMSRequest 在原字段后追加（10-13 / 7-8）保持 wire 兼容 |

## 关联

- 上一版架构重构：[[services/message-service/design/v1/2026-06-21-service-architecture-refactor|service-architecture-refactor]]
- proto 枚举统一：[[services/message-service/design/v1/2026-06-11-proto-enum-unification|proto-enum-unification]]
- vendor 枚举设计：[[services/message-service/design/v1/2026-06-16-vendor-enum|vendor-enum]]
