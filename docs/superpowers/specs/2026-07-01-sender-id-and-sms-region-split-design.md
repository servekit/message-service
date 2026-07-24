# SenderID 语义收紧 + SMS region_code/phone 拆分 设计

日期：2026-07-01
状态：已与用户对齐，待写实施计划

## 背景

当前 message-service 存在两个语义/设计问题：

1. **SenderID 语义模糊**：proto 注释把 `sender_id` 描述为既可以是 `user_id` / `admin_id`，也可以是 `system` / `module name`。多义性让调用方填写标准不一，也让基于 `sender_id` 的幂等命名空间含义不清。models 里完全没有注释。
2. **SMS 号码合并为一个字段**：`SendSMSRequest.to` 是单个字符串，调用方如果在自家系统里分开存国家/地区码和本地号码，发送时还得自己拼成 E.164（如 `+8613800138000`）。DB 里也只有 `target` 一个列存原始字符串，无法按 region 独立查询。

## 目标

- 把 SenderID 含义收紧为"调用方业务服务的标识"（user-service、pay-service 这一层），文档/注释明确，逻辑不变。
- 把 SMS 收件人拆为 `region_code`（ISO 3166-1 alpha-2）+ `phone`（本地号码）两个独立字段，从 API 一路贯穿到 DB。调用方不再需要预处理。
- 新增 `ListSMSRegions` RPC，供前端按 region 筛选时拿去重列表。

## 非目标

- 不改 email 的 target 字段（用户未要求）。
- 不动 `internal/provider/sms/router.go` 的 `defaultCountry` config 和 router 内部解析逻辑（service 层保证传 E.164，router 行为不变）。
- 不补回已删除的 `idempotency_key` DB 列。
- 不引入对终端 actor（user_id/admin_id）的存储——按用户决定，这一层信息由调用方自己在审计日志里留底。

## 设计决策

### SenderID = 调用方服务名（仅此一层）

选择把语义从"多种值"收窄为"仅业务服务标识"。Trade-off：

- **得到**：边界清晰；`msg:idem:{channel}:{senderID}:{key}` 的命名空间含义自然清晰（按业务服务做幂等）；`ListSMS by sender_id` 的结果可预测。
- **失去**：终端 actor 信息不在 message-service。但 actor 追溯本来就是调用方的职责，message-service 不应该持有它。

### SMS 拆分：API → DB 一致的 region_code + phone

API 层接受 `region_code`（alpha-2）+ `phone`（本地号，不带 `+`），服务层用 `phonenumbers` 库归一化为 E.164 后传给 router/providers。DB 直接按 API 字段存（删 target，加 region_code + phone）。

字段命名选 `region_code` 而非 `country_code`——HK、TW、MO 按 ISO 3166-1 是 alpha-2 编码但不是"国家"，phonenumbers 库与 CLDR 都用 region 这个概念，更严谨。

### ListSMSRegions：filter 各字段独立 AND，不做特殊关联

`sender_id`、`region_code` 等都是普通可选 AND 过滤；不做"按 sender_id 自动关联 region"这类智能逻辑——业务方需要时自己传 filter。

## 详细设计

### 1. SenderID 注释收紧（零逻辑改动）

`internal/store/models/email_record.go`、`internal/store/models/sms_record.go`：

```go
// SenderID identifies the calling business service (e.g. "user-service",
// "pay-service"). Required. NOT the end-user/admin id — the caller must
// record that in its own audit trail.
SenderID string `gorm:"size:64;column:sender_id;index"`
```

`api/proto/message/v1/message.proto`（`SendEmailRequest.sender_id`、`SendSMSRequest.sender_id`、`EmailRecord.sender_id`、`SMSRecord.sender_id`、`ListEmailsRequest.sender_id`、`ListSMSRequest.sender_id`、`ListEmailsByCursorRequest.sender_id`、`ListSMSByCursorRequest.sender_id`）：

```proto
// SenderID identifies the calling business service (e.g. "user-service",
// "pay-service"). Required. NOT the end-user/admin id — the caller must
// record that in its own audit trail.
string sender_id = ...;
```

### 2. SMS proto 改动（破坏性，项目"无生产数据"可接受）

`SendSMSRequest` 字段重排（`to` 删除，加 `region_code` + `phone`）：

```proto
message SendSMSRequest {
  // region_code is the ISO 3166-1 alpha-2 region code used to parse phone
  // (e.g. "CN", "US", "HK"). Required. Acts as defaultRegion for phonenumber
  // parsing — NOT a dialing code like "86".
  string region_code = 1 [(buf.validate.field).string.pattern = "^[A-Z]{2}$"];

  // phone is the local phone number WITHOUT the international prefix
  // (e.g. "13800138000", "5551234567"). Must NOT start with "+".
  string phone = 2 [(buf.validate.field).string.min_len = 1];

  string content = 3;
  string template_id = 4;
  map<string, string> template_params = 5;
  SmsVendor vendor = 6;
  string account = 7;
  SmsScene scene = 8;
  string sender_id = 9;
  string idempotency_key = 10 [(buf.validate.field).string.max_len = 64];

  // existing CEL rules: vendor_account_pair, scene_required, sender_required
  // new CEL rule: phone_no_plus — phone must not start with '+'
  //   (provide local number only; region_code disambiguates the country)
}
```

`SMSRecord`：删 `target`，加 `region_code` + `phone`。
`ListSMSRequest` / `ListSMSByCursorRequest`：删 `target`，加 `region_code` + `phone`（都是可选过滤）。

### 3. SMS DB schema 改动

`internal/store/models/sms_record.go`：

```go
type MessageSMSRecord struct {
    ID             int64           `gorm:"primaryKey"`
    Vendor         int32           `gorm:"not null;default:0;index"`
    Account        string          `gorm:"size:64;column:account"`
    Scene          int32           `gorm:"not null;default:0;index"`
    Status         int32           `gorm:"not null;default:0;index"`
    RegionCode     string          `gorm:"size:2;column:region_code;not null;index"`
    Phone          string          `gorm:"size:32;column:phone;not null;index"`
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

Email model 的 `Target` 字段不动。AutoMigrate 自动处理增删列。

### 4. 校验规则

`internal/service/message/util.go` 的 `validateSendSMSRequest` 新增：

1. `region_code` 匹配 `^[A-Z]{2}$`（proto 已用 pattern，这里是 defense-in-depth）
2. `phone` 不以 `+` 开头
3. `phonenumbers.Parse(phone, region_code)` 必须成功
4. `phonenumbers.IsValidNumber(num)` 为 true
5. 一致性检查：`phonenumbers.GetRegionCodeForNumber(num) == region_code`（捕获 region_code="CN"、phone 实为美国号这类不匹配）

**phone 输入容忍**：phonenumbers 库能解析 `"138-0013-8000"`、`"138 0013 8000"` 这类带分隔符的形式。DB 里**按调用方原样存**（不做去空格/横线归一化），归一化只发生在解析→E.164→传给 router/providers 这条临时路径上。这样存档字段与调用方输入一致，便于排查问题；查询时如需 loose match，业务方自己 normalize 后查询。

### 5. 服务层流程

`internal/service/message/sms.go` 的 `SendSMS`：

```
1. validateSendSMSRequest(req)            // 含上面 5 条规则
2. num, _ := phonenumbers.Parse(req.Phone, req.RegionCode)
3. e164 := phonenumbers.Format(num, phonenumbers.E164)
4. msg := &sms.Message{To: e164, Content: ..., Template: ..., Params: ...}
5. router / sender 流程不变
6. persistSMSRecord:
     record.RegionCode = req.RegionCode
     record.Phone      = req.Phone
```

`toProtoSMSRecord`：填 `RegionCode` / `Phone`，删 `Target`。

### 6. ListSMS / ListSMSByCursor filter

`internal/store/dal/sms_record.go` 的 `SmsListFilter`：

```go
type SmsListFilter struct {
    Vendor        pb.SmsVendor
    Scene         pb.SmsScene
    Status        pb.MessageStatus
    RegionCode    string  // 新增；空字符串不过滤
    Phone         string  // 新增；空字符串不过滤
    SenderID      string
    SortField     pb.SortField
    SortDirection pb.SortDirection
    StartTime     *time.Time
    EndTime       *time.Time
}
```

service 层把 `req.GetRegionCode()` / `req.GetPhone()` 填进 filter。

### 7. 新增聚合 RPC：ListSMSRegions / ListSMSSenders / ListEmailSenders

三个 RPC 用途一致：返回某个 list-filter 范围内的去重字段值，给前端 list 页面的下拉筛选框用。filter 各字段独立 AND，谁传了按谁过滤；被聚合的那个字段本身不出现在 filter 里。

#### 7.1 proto 定义

```proto
service MessageService {
  // ...
  rpc ListSMSRegions(ListSMSRegionsRequest) returns (ListSMSRegionsResponse) {
    option (google.api.http) = {get: "/v1/sms:regions"};
  }
  rpc ListSMSSenders(ListSMSSendersRequest) returns (ListSMSSendersResponse) {
    option (google.api.http) = {get: "/v1/sms:senders"};
  }
  rpc ListEmailSenders(ListEmailSendersRequest) returns (ListEmailSendersResponse) {
    option (google.api.http) = {get: "/v1/emails:senders"};
  }
}

message ListSMSRegionsRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string sender_id = 4;
  int64 start_time = 5;
  int64 end_time = 6;
  // 注意：不含 region_code —— 这是被聚合的字段
}

message ListSMSRegionsResponse {
  repeated string region_codes = 1;
}

message ListSMSSendersRequest {
  SmsVendor vendor = 1;
  SmsScene scene = 2;
  MessageStatus status = 3;
  string region_code = 4;
  int64 start_time = 5;
  int64 end_time = 6;
  // 注意：不含 sender_id —— 这是被聚合的字段
  // 不含 phone —— 高基数，对下拉 scope 没意义
}

message ListSMSSendersResponse {
  repeated string sender_ids = 1;
}

message ListEmailSendersRequest {
  EmailVendor vendor = 1;
  EmailScene scene = 2;
  MessageStatus status = 3;
  int64 start_time = 4;
  int64 end_time = 5;
  // 注意：不含 sender_id —— 这是被聚合的字段
  // 不含 target —— 高基数，对下拉 scope 没意义
}

message ListEmailSendersResponse {
  repeated string sender_ids = 1;
}
```

#### 7.2 dal 层

`internal/store/dal/sms_record.go`：

```go
type SmsAggregationFilter struct {
    Vendor    pb.SmsVendor
    Scene     pb.SmsScene
    Status    pb.MessageStatus
    SenderID  string  // 仅 ListSMSRegions 用
    RegionCode string // 仅 ListSMSSenders 用
    StartTime *time.Time
    EndTime   *time.Time
}

func ListSMSRegions(ctx context.Context, db *gorm.DB, f SmsAggregationFilter) ([]string, error)
func ListSMSSenderIDs(ctx context.Context, db *gorm.DB, f SmsAggregationFilter) ([]string, error)
```

`internal/store/dal/email_record.go`：

```go
type EmailAggregationFilter struct {
    Vendor    pb.EmailVendor
    Scene     pb.EmailScene
    Status    pb.MessageStatus
    StartTime *time.Time
    EndTime   *time.Time
}

func ListEmailSenderIDs(ctx context.Context, db *gorm.DB, f EmailAggregationFilter) ([]string, error)
```

三个查询都走 `SELECT DISTINCT <col> FROM message_*_records WHERE deleted_at IS NULL AND ... ORDER BY <col>`，依赖被聚合字段上的索引做 index-only scan。

#### 7.3 service 层

`internal/service/message/sms.go`：

```go
func (s *Service) ListSMSRegions(ctx context.Context, req *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
    if !s.persistSMSEnabled {
        return nil, xcodes.ErrPersistenceDisabled.Wrap(...)
    }
    f := dal.SmsAggregationFilter{Vendor: req.GetVendor(), Scene: req.GetScene(), Status: req.GetStatus(), SenderID: req.GetSenderId()}
    // 时间转换同 ListSMS
    regions, err := dal.ListSMSRegions(ctx, s.db, f)
    return &pb.ListSMSRegionsResponse{RegionCodes: regions}, nil
}

func (s *Service) ListSMSSenders(ctx context.Context, req *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
    if !s.persistSMSEnabled {
        return nil, xcodes.ErrPersistenceDisabled.Wrap(...)
    }
    f := dal.SmsAggregationFilter{Vendor: req.GetVendor(), Scene: req.GetScene(), Status: req.GetStatus(), RegionCode: req.GetRegionCode()}
    senders, err := dal.ListSMSSenderIDs(ctx, s.db, f)
    return &pb.ListSMSSendersResponse{SenderIds: senders}, nil
}
```

`internal/service/message/email.go`：同模板新增 `ListEmailSenders`。

### 8. 影响范围清单

- `api/proto/message/v1/message.proto` → 重新 `make generate`（proto + grpc-gateway）
- `internal/store/models/sms_record.go`：删 Target、加 RegionCode + Phone、加 SenderID 注释
- `internal/store/models/email_record.go`：加 SenderID 注释（字段不变）
- `internal/store/generated/sms_record.go`：`gorm gen` 重新生成（Makefile `generate` target）
- `internal/store/dal/sms_record.go`：filter 字段调整、新增 ListSMSRegions + ListSMSSenderIDs
- `internal/store/dal/email_record.go`：新增 ListEmailSenderIDs
- `internal/service/message/sms.go`：validate 改、SendSMS 加 parse/format 步骤、persist 字段调整、toProto 调整、新增 ListSMSRegions + ListSMSSenders service 方法
- `internal/service/message/email.go`：新增 ListEmailSenders service 方法
- `internal/service/message/util.go`：validateSendSMSRequest 新增 5 条规则
- `internal/service/message/service.go`（如有 service 注册）：注册三个新 RPC
- `cmd/migrate/`：AutoMigrate 自动处理（无手写 SQL）
- 测试更新：sms_test.go、util_test.go、dal/sms_record_test.go、dal/email_record_test.go、testclient（如有 SMS 请求构造）
- 三个新 RPC 各自 dal + service 单测，覆盖空 filter、各字段 filter、持久化关闭等用例

### 9. 不在范围

- email 的 target 字段（用户未要求）
- router 的 `defaultCountry` config（保留不动）
- 已删除的 `idempotency_key` DB 列（之前已处理）
- 终端 actor 信息存储（按用户决定，调用方自理）

## 关联

设计本身无前置依赖；实施计划见后续 writing-plans 输出。
