# Vendor 字段 enum 化设计

**Date:** 2026-06-16
**Status:** Design approved, pending implementation plan
**Related:** Refines enum policy from `2026-06-11-proto-enum-unification-design.md`; reverses string-vendor tradeoff recorded in `2026-06-16-go-common-message-slimdown-design.md`.

## Goal

把 `SendEmailRequest.vendor` 和 `SendSMSRequest.vendor` 从 `string` 改为 proto enum，让业务方在 API 层就知道可用的 vendor 集合，避免传未知字符串导致运行时失败。同时清理 `Provider` enum 与 vendor 之间的语义重叠。

## Background

当前 proto 定义了 `Provider` enum（`PROVIDER_SMTP`、`PROVIDER_ALIYUN`），但 `SendEmailRequest.vendor` / `SendSMSRequest.vendor` 字段是 `string`。原因有二：

1. 早期为了 config-driven 扩展性（加 vendor 不动 proto）
2. 历史误解：把 SMTP 当作 vendor

实际上 **SMTP 是 protocol，不是 vendor**。同一个 SMTP 协议可以连阿里云、腾讯、网易、自建 MTA 等不同服务商，差异只是 host/port。把 SMTP 当 vendor 是建模错误。

vendor 真正的语义是**服务商品牌**：阿里云、腾讯、网易、Mailgun、SendGrid 等。当前用 string 表达时业务方需要查文档/源码才知道有哪些合法值，违反 CLAUDE.md「有限集合的字段必须使用 proto enum」规则。

## Core Design Decisions

### 决策 1：vendor 按 channel 分两个 enum

`EmailVendor` 和 `SmsVendor` 独立。proto 层就防止 email 请求传 SMS vendor。新加 vendor 只动对应 enum，互不影响。

### 决策 2：删除 Provider enum，vendor 统一

`Provider` enum 当前被 5 处使用（SendResponse、MessageRecord、ProviderStats、ListMessagesRequest、GetMessageStatsRequest），全部语义与 vendor 重叠。删除 Provider，所有"厂商"信息都用 vendor enum 表达。

### 决策 3：MessageRecord 等跨 channel 字段用 oneof vendor

`MessageRecord` 一张表存 email 和 sms 记录，单个 vendor 字段装不下两个 enum。用 `oneof vendor { EmailVendor / SmsVendor }`，依赖 `channel` 字段决定哪个 oneof case 生效。

`ProviderStats` 拆为 `EmailVendorStats` + `SmsVendorStats`，`MessageStatsResponse` 用两个 repeated 字段。

`ListMessagesRequest` / `GetMessageStatsRequest` 的 vendor filter 拆为 `email_vendor` + `sms_vendor` 两个 optional 字段，业务方按已选 channel 填对应字段。

### 决策 4：方案 C — vendor 是元数据，protocol 由 vendor 决定

- 绝大多数 email vendor 走 SMTP 协议，共用 `go-common/message/email/smtp/`（通用 SMTP client）
- vendor 决定**默认 host**（如 ALIYUN → smtp.aliyun.com）；config 可覆盖
- 少数 email vendor 走专属 HTTP API（Mailgun/SendGrid），各自需要 subpackage（未来按需加，当前不实现）
- SMS vendor 各家 API 不同，每个 vendor 一个 go-common subpackage（当前只有 `aliyun/`）
- **`CUSTOM_SMTP` 是兜底**：通用 SMTP，host 完全由 config 提供，不绑定品牌

**go-common 不动。** 所有 vendor 概念、默认 host 映射表、enum 定义都在 message-service 内部。

### 决策 5：DB 重置（项目未上线）

`MessageRecord.Provider int32` 列拆分为 `EmailVendor int32` + `SmsVendor int32` 两列（互斥，按 Channel 填一列），避免 email/sms vendor enum 共享 int 空间导致 raw SQL 歧义。项目未上线，drop & recreate。AutoMigrate 处理新 schema。

## Proto 改动

### 删除

```proto
enum Provider { ... }  // 整个 enum 删除
```

### 新增

```proto
enum EmailVendor {
  EMAIL_VENDOR_UNSPECIFIED = 0;
  EMAIL_VENDOR_CUSTOM_SMTP = 1;  // 通用 SMTP，host 必须由 config 提供
  EMAIL_VENDOR_ALIYUN = 2;        // smtp.aliyun.com
  EMAIL_VENDOR_TENCENT = 3;       // smtp.exmail.qq.com
  EMAIL_VENDOR_NETEASE = 4;       // smtp.qiye.163.com
}

enum SmsVendor {
  SMS_VENDOR_UNSPECIFIED = 0;
  SMS_VENDOR_ALIYUN = 1;  // 已实现
  // TENCENT/HUAWEI 等待实际接入时按需加，不预留占位
}
```

### 字段类型变更

| Message.field | 当前类型 | 改后类型 |
|---|---|---|
| `SendEmailRequest.vendor` (8) | `string` | `EmailVendor` |
| `SendSMSRequest.vendor` (5) | `string` | `SmsVendor` |
| `SendResponse.provider` (3) | `Provider` | 删除字段；新增 `oneof vendor { EmailVendor email_vendor = 4; SmsVendor sms_vendor = 5; }` |
| `MessageRecord.provider` (3) | `Provider` | 删除字段；新增 `oneof vendor { EmailVendor email_vendor = 19; SmsVendor sms_vendor = 20; }` |
| `ListMessagesRequest.provider` (4) | `Provider` | 删除；新增 `EmailVendor email_vendor = 9;` + `SmsVendor sms_vendor = 10;` |
| `GetMessageStatsRequest.provider` (2) | `Provider` | 删除；新增 `EmailVendor email_vendor = 5;` + `SmsVendor sms_vendor = 6;` |
| `MessageStatsResponse.provider_stats` (5) | `repeated ProviderStats` | 删除；新增 `repeated EmailVendorStats email_stats = 5;` + `repeated SmsVendorStats sms_stats = 6;` |
| `ProviderStats` | message | 删除；新增 `EmailVendorStats` 和 `SmsVendorStats`（字段相同：vendor enum + total/sent/failed） |

### CEL 校验调整

`vendor == 0` 表示未指定（enum 默认值）：

```proto
option (buf.validate.message).cel = {
  id: "vendor_account_pair",
  message: "vendor and account must both be set or both be empty",
  expression: "(this.vendor == 0 && this.account == '') || (this.vendor != 0 && this.account != '')"
};
```

## Go 代码改动

### `internal/service/send.go`

- `req.GetVendor()` 返回 enum（`pb.EmailVendor` / `pb.SmsVendor`），不再是 string
- **删除** `emailProviderToProto` / `smsProviderToProto`——vendor 已是 enum，无需转换
- `SendResponse` 构造时填对应 oneof case：email 路径设 `EmailVendor`，sms 路径设 `SmsVendor`

### `internal/message/email/registry.go`

`buildProvider` 改为按 enum 分发：

```go
func buildProvider(vendor pb.EmailVendor, ac AccountConfig) (emailcommon.Provider, error) {
    switch vendor {
    case pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP,
         pb.EmailVendor_EMAIL_VENDOR_ALIYUN,
         pb.EmailVendor_EMAIL_VENDOR_TENCENT,
         pb.EmailVendor_EMAIL_VENDOR_NETEASE:
        host := ac.Host
        if host == "" {
            host = defaultSMTPHost(vendor)
            if host == "" {
                return nil, fmt.Errorf("email vendor %v requires explicit host", vendor)
            }
        }
        return smtpprovider.NewProvider(&smtpprovider.Config{
            Host: host, Port: ac.Port, Username: ac.Username, Password: ac.Password, From: ac.From,
        })
    default:
        return nil, fmt.Errorf("unsupported email vendor %v", vendor)
    }
}

// defaultSMTPHost returns the conventional SMTP host for a branded vendor, or
// empty string if the vendor has no canonical host (e.g., CUSTOM_SMTP).
func defaultSMTPHost(vendor pb.EmailVendor) string {
    switch vendor {
    case pb.EmailVendor_EMAIL_VENDOR_ALIYUN:
        return "smtp.aliyun.com"
    case pb.EmailVendor_EMAIL_VENDOR_TENCENT:
        return "smtp.exmail.qq.com"
    case pb.EmailVendor_EMAIL_VENDOR_NETEASE:
        return "smtp.qiye.163.com"
    default:
        return ""
    }
}
```

`SenderFor` 签名：保留 `(vendor string, account string)`，由 service 层做 enum→string 转换。理由：registry 是 internal 实现细节，不该耦合 proto 类型；同时 enum→string 转换极简单（`vendor.String()` 或显式映射），不需要把 enum 渗到 internal 层。

`AccountRegistry.vendors` 的 map key 仍是 string（来自 config），但加载 config 时校验 vendor 名必须是 enum 已知值，未知报错 fail-fast。

### `internal/message/sms/registry.go`

`buildProvider` 当前按 string vendor 分发（`case "aliyun":`）。**保持 string 签名不变**，service 层做 enum→string 转换。与 email registry 一致——proto 类型不渗到 internal 层。

### `internal/store/models/message_record.go`

```go
// 旧
Provider int32 `gorm:"not null;index"`

// 新（按 channel 拆分两列）
EmailVendor int32 `gorm:"index"`
SmsVendor   int32 `gorm:"index"`
```

`EmailVendor` 和 `SmsVendor` **互斥**：每行只有与 `Channel` 匹配的列被填值，另一列为 0。这样拆分的原因：`EmailVendor`（CUSTOM_SMTP=1, ALIYUN=2, …）和 `SmsVendor`（ALIYUN=1）共享 int 空间，单列时 raw `WHERE vendor=N` 无法区分 `vendor=1` 是 email 的 CUSTOM_SMTP 还是 sms 的 ALIYUN。AutoMigrate 处理新 schema；项目未上线，drop & recreate。

DB 存对应 enum 的 int 值：
- Email 记录：`email_vendor` 存 `EmailVendor` 的 int（如 CUSTOM_SMTP=1, ALIYUN=2），`sms_vendor=0`
- SMS 记录：`sms_vendor` 存 `SmsVendor` 的 int（如 ALIYUN=1），`email_vendor=0`

`persist*Record` 填值：用 `result.Vendor`（实际处理发送的 provider 的 vendor 名）经 reverse map（`emailVendorFromString` / `smsVendorFromString`）转回 enum，**而不是** `req.GetVendor()`——后者在 fallback 路径上是 0/UNSPECIFIED，会丢失真实 vendor 信息。

`VendorStats` 查询用 `COALESCE(NULLIF(email_vendor, 0), sms_vendor)` 把两列合并成单一 `vendor` 输出列，由调用方按 `channel` 解释。`ListFilter` / `StatsFilter` 的 `EmailVendor` / `SmsVendor` 分别打到对应列，消除了旧实现里两个 filter AND 同一列导致结果恒为空的 bug。

### `pkg/config/config.go`

无需改动。`Config.Email` / `Config.SMS` 字段类型不变（仍为 `*email.Config` / `*sms.Config`，来自 internal/message/）。

## YAML 配置

`config.yaml` 保持现有结构。`vendors` 是 `map[string]VendorConfig`，key 必须 string（YAML 限制）。

约束：vendor 名必须与 enum 名的小写形式对应：

```yaml
email:
  vendors:
    custom_smtp:    # EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP
      accounts:
        - name: primary
          host: smtp.example.com
          port: 587
          ...
    aliyun:         # EmailVendor_EMAIL_VENDOR_ALIYUN
      accounts:
        - name: default
          # host 留空 → registry 自动填 smtp.aliyun.com
          username: ...
sms:
  vendors:
    aliyun:
      accounts:
        - name: default
          ...
```

registry 加载时校验 vendor 名（`pb.EmailVendor.String()` 比较），未知名报错。

## 测试改动

- `service_test.go`：所有 `Vendor: "smtp"` 改为 `Vendor: pb.EmailVendor_EMAIL_VENDOR_CUSTOM_SMTP`（或对应值）
- 所有 `pb.Provider_PROVIDER_SMTP` 断言改为对应 vendor enum 值（response oneof case）
- `registry_test.go`：vendor 字段类型从 string 改为 enum
- 测试中 mock provider 的 `name` 属性（如 `&mockEmailProvider{name: "mailgun"}`）保留字符串——这是 mock 的内部标识，与 enum 解耦；但 vendor 字段（AccountProvider.Vendor）必须用 enum 值

## 兼容性

### proto breaking change

- `Provider` enum 删除
- 多个 message 字段类型变更/重命名
- **API 客户端必须重新生成 stub**

### DB schema change

- `message_records.provider` 列 → `vendor` 列（重命名 + 索引保留）
- 项目未上线，drop & recreate

### go-common

不动。

## 影响范围

| 文件 | 改动程度 |
|---|---|
| `api/proto/message/v1/message.proto` | 中（删 1 enum，加 2 enum，5 个 message 字段调整） |
| `gen/message/v1/*.pb.go` | 自动重生成 |
| `internal/service/send.go` | 小（删 2 helper，调整 SendResponse 构造） |
| `internal/service/service_test.go` | 中（断言类型变） |
| `internal/message/email/registry.go` | 中（buildProvider 改 enum + 默认 host 表） |
| `internal/message/email/registry_test.go` | 中 |
| `internal/message/sms/registry.go` | 小（兼容 enum 输入） |
| `internal/message/sms/registry_test.go` | 小 |
| `internal/store/models/message_record.go` | 小（字段重命名） |
| `internal/store/repository/*` | 视情况（如果直接引用 Provider 列名） |
| `config.yaml` | 无（结构不变，但 vendor 名集合变） |
| `go-common` | 无 |

## 关联

**前一版设计：**
- [[services/message-service/design/v1/proto-enum-unification|proto-enum-unification]] — Provider enum 引入的初衷
- [[services/message-service/design/v1/go-common-message-slimdown|go-common-message-slimdown]] — Sender/Registry 下沉

**实现计划：** 待 writing-plans skill 生成（plan 命名 `2026-06-16-vendor-enum.md`）
