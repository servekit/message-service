# SMS 厂商扩展设计(腾讯云 / 火山引擎 / BytePlus / 华为云)

**日期**:2026-06-26
**状态**:设计已确认,待写实施计划
**作者**:moss

## 背景

2026-06-25 完成 `go-common/message` 内联后,SMS vendor 实现已扁平化到 `internal/provider/sms/`:
- `aliyun.go` — 唯一在用 vendor,只支持国内(`dysmsapi-20170525`)
- 每个 vendor 一个独立文件,实现 `AccountProvider` interface
- proto enum `SmsVendor` 当前只有 `UNSPECIFIED` / `ALIYUN` 两个值

业务侧现有海外短信需求,需要在 SMS 层补齐主流云厂商:

| 厂商 | 区域 | SDK 包 |
|---|---|---|
| 腾讯云 | 国内 + 国际 | `tencentcloud-sdk-go/tencentcloud/sms/v20190711`(同一 SDK,SDK 内按手机号前缀自动区分区域) |
| 火山引擎 | 国内 | `volcengine/volc-sdk-golang/service/sms` |
| BytePlus(火山海外品牌) | 国际 | `byteplus-sdk/byteplus-sdk-golang`(v2 主要面向 LLM,不含 SMS,故用 v1) |
| 华为云 | 国内 + 国际 | `huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2`(同一 SDK,通过 region + 手机号 prefix 自动区分) |

火山引擎国内 / BytePlus 海外是两个完全独立的 Go 包(import path 不一样),必须拆成两个 Provider 类。腾讯 / 华为各自的 SDK 包同时支持国内+国际,一个 Provider 即可,SDK 内部根据手机号 prefix 走对应路径。

## 目标

1. **新增 4 个 SMS vendor**:`SMS_VENDOR_TENCENT` / `SMS_VENDOR_VOLCENGINE` / `SMS_VENDOR_BYTEPLUS` / `SMS_VENDOR_HUAWEI`。
2. **每个 vendor 一个独立 Provider 类**,沿用 2026-06-25 inline 迁移已确立的扁平模式(vendor impl 文件直接放 `internal/provider/sms/` 下,实现 `AccountProvider` interface)。
3. **重构 `AccountConfig` 为嵌入式 per-vendor struct**,消除 fat-struct 与各 `<Vendor>Config` 的镜像重复。
4. **测试沿用 mock-based unit test 模式**(参照 `aliyun_test.go`),每个 vendor 至少 7 个 case:Vendor / Account / Send 成功 / Send withParams(模板参数序列化) / Send 参数验证失败(emptyPhone / noTemplate 两个) / Send SDK 错误 / Send 业务错误。
5. **配置示例同步更新**(`config.yaml.example`、`CLAUDE.md`)。

## 非目标

- ❌ 不动 email vendor —— 用户需求只覆盖 SMS。
- ❌ 不修上次迁移遗留的 `SMTPProvider.Vendor()` 硬编码 `CUSTOM_SMTP` 问题(那是 email 层的 tech debt,跟本次 SMS 扩展无关)。
- ❌ 不补阿里云国际版 —— 现有 `AliyunProvider` 只支持国内 `dysmsapi-20170525`;如果未来有阿里云国际站需求,走独立 spec。
- ❌ 不做真实 SDK 集成测试 —— 现有 aliyun 也是纯 mock,真测要 AK/SK,credentials 管理成本高;真测由手动脚本 + 真实账号验证。
- ❌ 不改 `AccountProvider` interface 本身 —— 沿用 inline 迁移后的 3 方法(`Vendor() / Account() / Send()`)。

## 目录结构

**新增 8 个文件**(4 vendor × 2 文件):

```
internal/provider/sms/
├── tencent.go              [新] TencentConfig / TencentProvider / NewTencentProvider
├── tencent_test.go         [新] mock SDK + 5+ test cases
├── volcengine.go           [新] VolcengineConfig / VolcengineProvider / NewVolcengineProvider
├── volcengine_test.go      [新] mock SDK + 5+ test cases
├── byteplus.go             [新] ByteplusConfig / ByteplusProvider / NewByteplusProvider
├── byteplus_test.go        [新] mock SDK + 5+ test cases
├── huawei.go               [新] HuaweiConfig / HuaweiProvider / NewHuaweiProvider
└── huawei_test.go          [新] mock SDK + 5+ test cases
```

**改造 6 个文件**:

- `api/proto/message/v1/message.proto` — 加 4 个 SmsVendor enum 值
- `gen/message/v1/message.pb.go` 等 — protoc 重新生成
- `internal/provider/sms/registry.go` — `AccountConfig` 重构为嵌入式 struct;`buildProvider` switch 加 4 个 case
- `internal/provider/sms/aliyun.go` — 无逻辑改动,但 `NewAliyunProvider` 签名不变,只是上层调用方式从 fat-struct copy 改成传 `*AliyunConfig` 指针
- `internal/service/setup.go` — `parseSMSVendorName` switch 加 4 个 case
- `config.yaml.example` — 加 4 个 vendor 配置示例
- `CLAUDE.md` — "已定义的枚举"小节更新

**`go.mod` 新增 4 个 SDK 依赖**。

## 实现细节

### §1. proto enum 扩展

`api/proto/message/v1/message.proto`:

```proto
enum SmsVendor {
  SMS_VENDOR_UNSPECIFIED = 0;
  SMS_VENDOR_ALIYUN = 1;
  SMS_VENDOR_TENCENT = 2;       // 新
  SMS_VENDOR_VOLCENGINE = 3;    // 新(火山引擎国内)
  SMS_VENDOR_BYTEPLUS = 4;      // 新(火山海外)
  SMS_VENDOR_HUAWEI = 5;        // 新(华为云,国内+国际)
}
```

YAML vendor 字符串映射(`parseSMSVendorName`):

| YAML 字符串 | enum | SDK 包 |
|---|---|---|
| `"aliyun"`(现有) | `SMS_VENDOR_ALIYUN` | `dysmsapi-20170525` |
| `"tencent"`(新) | `SMS_VENDOR_TENCENT` | `tencentcloud-sdk-go/tencentcloud/sms/v20190711` |
| `"volcengine"`(新) | `SMS_VENDOR_VOLCENGINE` | `volcengine/volc-sdk-golang/service/sms` |
| `"byteplus"`(新) | `SMS_VENDOR_BYTEPLUS` | `byteplus-sdk/byteplus-sdk-golang` |
| `"huawei"`(新) | `SMS_VENDOR_HUAWEI` | `huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2` |

命名约定:字符串跟 SDK 包名一致(volcengine / byteplus),tencent / huawei 是惯用 short name。**不区分 `_domestic` / `_international`** —— 腾讯/华为的 SDK 一个就够;火山国内/海外是两个独立 SDK 包,自然映射到不同 enum。

### §2. AccountConfig 重构(方案 B — 嵌入式 per-vendor struct)

**现状**(fat-struct,与 `<Vendor>Config` 镜像):

```go
// registry.go (现状)
type AccountConfig struct {
    Name            string
    AccessKeyID     string // aliyun
    AccessKeySecret string // aliyun
    SignName        string // aliyun
    RegionID        string // aliyun
}

// aliyun.go (现状)
type AliyunConfig struct {
    AccessKeyID     string
    AccessKeySecret string
    SignName        string
    RegionID        string
}

// buildProvider 内部把 AccountConfig 字段 copy 到 AliyunConfig — 冗余
```

**重构后**(`AccountConfig` 直接持有 `<Vendor>Config` 指针):

```go
// registry.go (新)
type AccountConfig struct {
    Name       string
    Aliyun     *AliyunConfig     // nil 表示未配置
    Tencent    *TencentConfig
    Volcengine *VolcengineConfig
    Byteplus   *ByteplusConfig
    Huawei     *HuaweiConfig
}

func buildProvider(vendor pb.SmsVendor, ac *AccountConfig) (AccountProvider, error) {
    switch vendor {
    case pb.SmsVendor_SMS_VENDOR_ALIYUN:
        if ac.Aliyun == nil {
            return nil, fmt.Errorf("sms vendor %s: aliyun config missing", vendor)
        }
        return NewAliyunProvider(ac.Name, ac.Aliyun)
    case pb.SmsVendor_SMS_VENDOR_TENCENT:
        if ac.Tencent == nil {
            return nil, fmt.Errorf("sms vendor %s: tencent config missing", vendor)
        }
        return NewTencentProvider(ac.Name, ac.Tencent)
    case pb.SmsVendor_SMS_VENDOR_VOLCENGINE:
        if ac.Volcengine == nil {
            return nil, fmt.Errorf("sms vendor %s: volcengine config missing", vendor)
        }
        return NewVolcengineProvider(ac.Name, ac.Volcengine)
    case pb.SmsVendor_SMS_VENDOR_BYTEPLUS:
        if ac.Byteplus == nil {
            return nil, fmt.Errorf("sms vendor %s: byteplus config missing", vendor)
        }
        return NewByteplusProvider(ac.Name, ac.Byteplus)
    case pb.SmsVendor_SMS_VENDOR_HUAWEI:
        if ac.Huawei == nil {
            return nil, fmt.Errorf("sms vendor %s: huawei config missing", vendor)
        }
        return NewHuaweiProvider(ac.Name, ac.Huawei)
    default:
        return nil, fmt.Errorf("unknown sms vendor %s", vendor)
    }
}
```

`<Vendor>Config` 字段(vendor impl 文件里定义):

```go
// sms/aliyun.go(已有,无改动)
type AliyunConfig struct {
    AccessKeyID     string
    AccessKeySecret string
    SignName        string
    RegionID        string `default:"cn-hangzhou"`
}

// sms/tencent.go(新)
type TencentConfig struct {
    SecretID    string
    SecretKey   string
    SmsSdkAppID string // 腾讯云控制台"短信应用"的 SdkAppId,如 "1400000000"
    SignName    string
    Region      string `default:"ap-guangzhou"` // 国内常用 ap-guangzhou / ap-beijing
    Endpoint    string // 可选,默认 sms.tencentcloudapi.com
}

// sms/volcengine.go(新)
type VolcengineConfig struct {
    AccessKID   string // 火山引擎账号的 AccessKey ID
    SecretKey   string
    SmsAccount  string // 火山引擎短信平台"账号"
    Sign        string // 短信签名
    Region      string `default:"cn-north-1"`
}

// sms/byteplus.go(新)
type ByteplusConfig struct {
    AccessKID  string // BytePlus 控制台的 AccessKey ID
    SecretKey  string
    SmsAccount string // BytePlus 短信"账号"
    Sign       string
    Region     string `default:"ap-singapore-1"` // BytePlus 主要 region
}

// sms/huawei.go(新)
type HuaweiConfig struct {
    Ak         string // 华为云账号 AK
    Sk         string // 华为云账号 SK
    AppKey     string // MSGSMS 项目的 app_key
    AppSecret  string // MSGSMS 项目的 app_secret
    Endpoint   string `default:"https://msgsms.cn-north-4.myhuaweicloud.com"` // 国内
    Region     string `default:"cn-north-4"`
}
```

字段含义的最终命名以实施时核对 SDK 文档为准,spec 只列出预期字段。

### §3. 各 vendor impl 设计

每个 vendor 文件结构一致(参照 `aliyun.go` 模板):

```go
package sms

import (...)

// <Vendor>Config holds the configuration for the <Vendor> SMS provider.
type <Vendor>Config struct { ... }

// <Vendor>Provider sends SMS via the <Vendor> SDK. Implements AccountProvider.
type <Vendor>Provider struct {
    account string
    config  *<Vendor>Config
    client  <vendor>SmsSender  // 未导出的 SDK client interface
}

// <vendor>SmsSender 抽象 SDK client 以便测试。
type <vendor>SmsSender interface {
    SendSmsWithContext(ctx context.Context, req *<sdk>.SendSmsRequest, ...) (*<sdk>.SendSmsResponse, error)
}

// New<Vendor>Provider 创建生产用 provider。
func New<Vendor>Provider(account string, cfg *<Vendor>Config) (*<Vendor>Provider, error) {
    client, err := <sdk>.NewClient(...)
    if err != nil { return nil, fmt.Errorf("<vendor>: create client: %w", err) }
    return &<Vendor>Provider{account: account, config: cfg, client: client}, nil
}

// new<Vendor>ProviderWithClient 用于测试,注入 mock client。
func new<Vendor>ProviderWithClient(account string, cfg *<Vendor>Config, sender <vendor>SmsSender) *<Vendor>Provider {
    return &<Vendor>Provider{account: account, config: cfg, client: sender}
}

func (*<Vendor>Provider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_<VENDOR> }
func (p *<Vendor>Provider) Account() string     { return p.account }

func (p *<Vendor>Provider) Send(ctx context.Context, msg *Message) error {
    // 1. validate msg.To / msg.Template
    // 2. 构造 SDK request
    // 3. 调 p.client.SendSmsWithContext(ctx, req, ...)
    // 4. SDK 错误 → fmt.Errorf("<vendor> sms send: %w", err)
    // 5. 业务 code != OK → fmt.Errorf("<vendor> sms: code=%s, message=%s", ...)
    // 6. 成功 → return nil
}
```

**各家 SDK 的业务错误码提取路径**(实施时以 SDK 源码为准):

| Vendor | 业务错误码提取 |
|---|---|
| Aliyun(现有) | `resp.Body.Code != "OK"`,code/message 在 `resp.Body.Code` / `.Message` |
| Tencent | `resp.Response.SendStatusSet[0].Code != "Ok"`(SDK 支持批量,单发取数组第 0 个) |
| Volcengine | `resp.ResponseMetadata.Error != nil`(SDK 错误)或 `resp.ResponseMetadata.Error.Code` / `resp.Body.Data` |
| BytePlus | 同 Volcengine 结构(`ResponseMetadata.Error`) |
| Huawei | `resp.HttpStatusCode != 200` 或 `resp.ErrorCode != ""`(华为云 SDK 风格) |

### §4. YAML 配置示例(破坏性变更)

`config.yaml.example` 加示例:

```yaml
sms:
  default_country: CN
  vendors:
    aliyun:
      accounts:
        - name: primary
          aliyun:
            access_key_id: ${ALIYUN_AK_ID}
            access_key_secret: ${ALIYUN_AK_SECRET}
            sign_name: ${ALIYUN_SIGN_NAME}
            region_id: cn-hangzhou

    tencent:
      accounts:
        - name: primary
          tencent:
            secret_id: ${TENCENT_SECRET_ID}
            secret_key: ${TENCENT_SECRET_KEY}
            sms_sdk_app_id: "1400000000"
            sign_name: ${TENCENT_SIGN_NAME}
            region: ap-guangzhou

    volcengine:
      accounts:
        - name: primary
          volcengine:
            access_kid: ${VOLC_AK_ID}
            secret_key: ${VOLC_AK_SECRET}
            sms_account: ${VOLC_SMS_ACCOUNT}
            sign: ${VOLC_SIGN}

    byteplus:
      accounts:
        - name: primary
          byteplus:
            access_kid: ${BP_AK_ID}
            secret_key: ${BP_AK_SECRET}
            sms_account: ${BP_SMS_ACCOUNT}
            sign: ${BP_SIGN}

    huawei:
      accounts:
        - name: primary
          huawei:
            ak: ${HW_AK}
            sk: ${HW_SK}
            app_key: ${HW_APP_KEY}
            app_secret: ${HW_APP_SECRET}
            endpoint: https://msgsms.cn-north-4.myhuaweicloud.com
            region: cn-north-4

  routes:
    - country: "*"
      targets:
        - vendor: huawei
          account: primary
    - country: CN
      targets:
        - vendor: aliyun
          account: primary
        - vendor: tencent
          account: primary
    - country: SG  # 新加坡走 BytePlus
      targets:
        - vendor: byteplus
          account: primary
```

### §5. 依赖管理

**go.mod 新增 require**:

```go
require (
    // 现有 aliyun 不动
    github.com/alibabacloud-go/dysmsapi-20170525/v5 v5.x.x
    github.com/alibabacloud-go/darabonba-openapi/v2 v2.x.x

    // 新增
    github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms v3.0.x  // 锁定具体版本
    github.com/volcengine/volc-sdk-golang v1.x.x
    github.com/byteplus-sdk/byteplus-sdk-golang v1.x.x  // v2 不含 SMS
    github.com/huaweicloud/huaweicloud-sdk-go-v3 v0.x.x
)
```

**腾讯云 tag 问题**(20w+ tag,`go get` 慢,[Issue #280](https://github.com/TencentCloud/tencentcloud-sdk-go/issues/280)):

- **采用方案:锁定具体子包版本** `go get github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms@v3.0.1293`(或最新稳定版)。GOPROXY(goproxy.cn)对 tag 多的仓库有缓存优化,实测不会太慢。
- **fallback 方案**(真有问题再换):
  - 方案 B:go.mod 加 `replace github.com/tencentcloud/tencentcloud-sdk-go => gitee.com/tencentcloud/tencentcloud-sdk-go v3.0.x`
  - 方案 C:切到 [`tencentcloud-sdk-go-intl-en`](https://github.com/tencentcloud/tencentcloud-sdk-go-intl-en)(API 3.0 兼容,tag 少)

**BytePlus v1 vs v2**:
- [`byteplus-sdk-golang` (v1)](https://github.com/byteplus-sdk/byteplus-sdk-golang) 是综合 SDK,包含 SMS。
- [`byteplus-go-sdk-v2`](https://github.com/byteplus-sdk/byteplus-go-sdk-v2) 主要面向 Ark Runtime(LLM),不含 SMS 模块。
- **采用 v1**。

### §6. 测试策略

每个 vendor 文件 `<vendor>_test.go` 沿用 `aliyun_test.go` 模式:

```go
package sms

import (
    "context"
    "fmt"
    "testing"

    <sdk> "github.com/.../sms/..."
    "github.com/stretchr/testify/require"

    pb "message-service/gen/message/v1"
)

// <vendor>MockSender — 全部 helper 加 vendor 前缀,避免同包冲突
type <vendor>MockSender struct { resp *<sdk>.SendSmsResponse; err error }
func (m *<vendor>MockSender) SendSmsWithContext(...) (*<sdk>.SendSmsResponse, error) { return m.resp, m.err }

func <vendor>OkResponse() *<sdk>.SendSmsResponse { ... }   // 业务成功响应
func <vendor>ErrResponse(code, msg string) *<sdk>.SendSmsResponse { ... }

func Test<Vendor>Provider_Vendor(t *testing.T) { ... }       // 断言 enum
func Test<Vendor>Provider_Account(t *testing.T) { ... }       // account 参数透传,用 "secondary" 测,避免 unparam lint
func Test<Vendor>Provider_Send_success(t *testing.T) { ... }
func Test<Vendor>Provider_Send_withParams(t *testing.T) { ... }  // 模板参数 JSON 序列化
func Test<Vendor>Provider_Send_emptyPhone(t *testing.T) { ... }
func Test<Vendor>Provider_Send_noTemplate(t *testing.T) { ... }
func Test<Vendor>Provider_Send_sdkError(t *testing.T) { ... }    // SDK 返回 err
func Test<Vendor>Provider_Send_businessError(t *testing.T) { ... }  // code != OK
```

**业务错误码字符串**(各 vendor 业务错误测试断言):

| Vendor | 业务错误示例 |
|---|---|
| Tencent | `code="LimitExceed.Sms"`(频率限制类) |
| Volcengine | `Code="LimitExceeded"` 或类似 |
| BytePlus | 同 Volcengine 结构 |
| Huawei | `ErrorCode="E200027"` 或类似 |

具体错误码字符串实施时查 SDK 文档。

**unparam lint 处理**:`Test<Vendor>Provider_Account` 用 `"secondary"` 而非 `"primary"`,跟 2026-06-25 Task 2/3 的 lint feedback 一致。

## 兼容性 / 破坏性变更

**破坏性 YAML 重构**(2026-06-25 已确立 `project-stage-no-prod-data` 现状,无生产数据,改成本最低):

- 旧:`aliyun` 字段直接平铺在 AccountConfig 根
- 新:`aliyun` 字段嵌套在 `aliyun:` 子节点下

旧 YAML 示例:
```yaml
- name: primary
  access_key_id: xxx
  sign_name: MyApp
```

新 YAML 示例:
```yaml
- name: primary
  aliyun:
    access_key_id: xxx
    sign_name: MyApp
```

**其他不变**:
- proto wire 兼容(只加 enum 值,不改字段编号)
- DB schema 不变(Vendor 字段还是 int32,新增枚举值天然兼容)
- API 行为不变(只多几个可选 vendor)

## 实施顺序

按"先重构 AccountConfig,再加 vendor"的顺序,避免每个 vendor PR 都要改 fat-struct:

1. **Step 1**:重构 `AccountConfig` 为嵌入式 struct + 改造 aliyun 调用链(`registry.go` + `aliyun.go` 调用方式 + 现有 aliyun YAML 示例)。这一步独立 commit,迁移路径明确。
2. **Step 2**:加 proto enum 4 个值 + `parseSMSVendorName` 4 个 case + 重跑 protoc。
3. **Step 3-6**:逐个加 vendor 文件(tencent → volcengine → byteplus → huawei),每个独立 commit。
4. **Step 7**:加 4 个 SDK 依赖到 go.mod(可能需要解决腾讯云 tag 慢的问题)。
5. **Step 8**:`config.yaml.example` + `CLAUDE.md` 更新。
6. **Step 9**:全量验证(gofmt / lint / build / vet / test / coverage)。

## 关联

**前置依赖:**
- [[services/message-service/design/v1/2026-06-25-inline-go-common-message|2026-06-25 inline go-common/message]] —— 本次建立在 inline 迁移完成的扁平 vendor layout 上

**实施计划:**
- 待写(`docs/superpowers/plans/2026-06-26-add-sms-vendors.md`)
