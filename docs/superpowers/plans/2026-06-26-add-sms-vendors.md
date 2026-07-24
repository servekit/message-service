# SMS 厂商扩展实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 message-service 加 4 个 SMS vendor(腾讯云 / 火山引擎 / BytePlus / 华为云),同时把 `AccountConfig` fat-struct 重构为嵌入式 per-vendor struct。

**Architecture:** 沿用 2026-06-25 inline 迁移建立的扁平 vendor layout — 每个 vendor 一个独立文件(`internal/provider/sms/<vendor>.go`),定义 `<Vendor>Config` / `<Vendor>Provider` / `New<Vendor>Provider`,实现 `AccountProvider` interface。火山引擎国内 / BytePlus 海外是两个独立 Go SDK 包,自然映射到两个 Provider;腾讯 / 华为各自的 SDK 一个包同时支持国内+国际,各一个 Provider 搞定。`AccountConfig` 改为持有 5 个 `<Vendor>Config` 指针(aliyun/tencent/volcengine/byteplus/huawei),`buildProvider` switch 分发,nil-check 保护"vendor 配置缺失"。

**Tech Stack:** Go 1.26、GORM、proto enum `pb.SmsVendor`、`github.com/tencentcloud/tencentcloud-sdk-go` / `github.com/volcengine/volc-sdk-golang` / `github.com/byteplus-sdk/byteplus-sdk-golang` / `github.com/huaweicloud/huaweicloud-sdk-go-v3`。

**Spec:** [`docs/superpowers/specs/2026-06-26-add-sms-vendors-design.md`](../specs/2026-06-26-add-sms-vendors-design.md)

---

## File Structure

**新增**(8 个文件,4 vendor × 2):
- `internal/provider/sms/tencent.go` — `TencentConfig` / `TencentProvider` / `NewTencentProvider`
- `internal/provider/sms/tencent_test.go` — mock SDK client + 7+ test cases
- `internal/provider/sms/volcengine.go` — `VolcengineConfig` / `VolcengineProvider` / `NewVolcengineProvider`
- `internal/provider/sms/volcengine_test.go` — 同上
- `internal/provider/sms/byteplus.go` — `ByteplusConfig` / `ByteplusProvider` / `NewByteplusProvider`
- `internal/provider/sms/byteplus_test.go` — 同上
- `internal/provider/sms/huawei.go` — `HuaweiConfig` / `HuaweiProvider` / `NewHuaweiProvider`
- `internal/provider/sms/huawei_test.go` — 同上

**改造**(7 个文件):
- `api/proto/message/v1/message.proto` — `SmsVendor` enum 加 4 个值
- `gen/message/v1/message.pb.go` 等 — protoc 重生成
- `internal/provider/sms/registry.go` — `AccountConfig` 重构 + `buildProvider` switch 加 4 个 case
- `internal/provider/sms/registry_test.go` — 适配新 `AccountConfig` 形状
- `internal/provider/sms/aliyun.go` — `<Vendor>Config` 已存在,但 `buildProvider` 调用方式从字段 copy 改成传 `*AliyunConfig` 指针(aliyun.go 本身无逻辑改动)
- `internal/service/setup.go` — `parseSMSVendorName` switch 加 4 个 case
- `config.yaml.example` — 加 4 个 vendor 配置示例(aliyun 字段嵌套化)
- `CLAUDE.md` — "已定义的枚举"小节,SMS vendor 列表扩成 5 个

**依赖新增**:`go.mod` 加 4 个 SDK require。

---

## Task 1: 重构 AccountConfig 为嵌入式 per-vendor struct(atomic)

把 `AccountConfig` 从 fat-struct(aliyun 字段直接平铺)改为持有 `*AliyunConfig` 指针。这一步独立 commit,是后续加 vendor 的基础。**Atomic — 中间状态不编译**,所有改动放一个 commit。

**Files:**
- Modify: `internal/provider/sms/registry.go`(`AccountConfig` 改形状 + `buildProvider` 改调用方式)
- Modify: `internal/provider/sms/registry_test.go`(测试适配新 `AccountConfig`)
- Modify: `config.yaml.example`(aliyun 字段嵌套化)

- [ ] **Step 1: 改 `AccountConfig` + `buildProvider`**

打开 `internal/provider/sms/registry.go`。找到 `AccountConfig` 定义(大约 35-42 行),整个 struct 替换为:

```go
// AccountConfig is a single named SMS account. Each vendor's config lives in
// its own struct (defined in the vendor impl file, e.g. AliyunConfig in
// aliyun.go). AccountConfig holds pointers to whichever vendor configs are
// relevant; buildProvider picks the right one based on the parent vendor enum.
//
// Adding a new vendor = add a `<Vendor>Config` field here + a case in
// buildProvider + the vendor impl file. No fat-struct field bloat.
type AccountConfig struct {
	Name       string
	Aliyun     *AliyunConfig
	Tencent    *TencentConfig
	Volcengine *VolcengineConfig
	Byteplus   *ByteplusConfig
	Huawei     *HuaweiConfig
}
```

注意:`TencentConfig` / `VolcengineConfig` / `ByteplusConfig` / `HuaweiConfig` 这 4 个类型**还没在 Task 1 创建**。这一步会让代码无法编译,Task 3-6 加 vendor 时会补齐。这是 atomic 的预期行为 — 所有改动放一个 commit,直到 Task 6 完成。

但为了让 Task 1 本身能独立验证编译,这一步先**只放 Aliyun**,其他 4 个 vendor 字段先注释掉:

```go
type AccountConfig struct {
	Name    string
	Aliyun  *AliyunConfig
	// Tencent    *TencentConfig    // Task 3 加
	// Volcengine *VolcengineConfig // Task 4 加
	// Byteplus   *ByteplusConfig   // Task 5 加
	// Huawei     *HuaweiConfig     // Task 6 加
}
```

然后改 `buildProvider` 的 aliyun case(大约 145-152 行),从字段 copy 改成传指针:

```go
case pb.SmsVendor_SMS_VENDOR_ALIYUN:
    if ac.Aliyun == nil {
        return nil, fmt.Errorf("sms vendor %s: aliyun config missing", vendor)
    }
    return NewAliyunProvider(ac.Name, ac.Aliyun)
```

删掉原来的 aliyun 字段 copy 逻辑(`AccessKeyID: ac.AccessKeyID` 等)。

- [ ] **Step 2: 改 `registry_test.go` 适配**

打开 `internal/provider/sms/registry_test.go`。所有 `&AccountConfig{Name: ..., AccessKeyID: ..., ...}` 改成嵌套形式:

旧:
```go
{Name: "primary", AccessKeyID: "xxx", AccessKeySecret: "yyy", SignName: "sign", RegionID: "cn-hangzhou"}
```

新:
```go
{Name: "primary", Aliyun: &AliyunConfig{AccessKeyID: "xxx", AccessKeySecret: "yyy", SignName: "sign", RegionID: "cn-hangzhou"}}
```

具体改动点用 grep 定位:`grep -n "AccessKeyID\|SignName\|RegionID" internal/provider/sms/registry_test.go`,每处都改成嵌套形式。

- [ ] **Step 3: 改 `config.yaml.example`**

打开 `config.yaml.example`(项目根)。找到 sms.vendors.aliyun.accounts 段,把 aliyun 字段嵌套到 `aliyun:` 子节点下:

旧:
```yaml
sms:
  vendors:
    aliyun:
      accounts:
        - name: primary
          access_key_id: ${ALIYUN_AK_ID}
          access_key_secret: ${ALIYUN_AK_SECRET}
          sign_name: ${ALIYUN_SIGN_NAME}
          region_id: cn-hangzhou
```

新:
```yaml
sms:
  vendors:
    aliyun:
      accounts:
        - name: primary
          aliyun:
            access_key_id: ${ALIYUN_AK_ID}
            access_key_secret: ${ALIYUN_AK_SECRET}
            sign_name: ${ALIYUN_SIGN_NAME}
            region_id: cn-hangzhou
```

- [ ] **Step 4: 验证编译 + 测试**

```bash
gofmt -w internal/provider/sms/ internal/service/setup.go config.yaml.example
goimports -w internal/provider/sms/
go build ./...
go test -race ./internal/provider/sms/...
```

Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/provider/sms/registry.go internal/provider/sms/registry_test.go config.yaml.example
git commit -m "refactor(provider/sms): AccountConfig from fat-struct to embedded per-vendor struct"
```

---

## Task 2: 扩展 proto enum + parseSMSVendorName

加 4 个 `SmsVendor` enum 值 + 对应 YAML 字符串映射。proto 改完后跑 protoc 重生成。

**Files:**
- Modify: `api/proto/message/v1/message.proto`
- Modify: `gen/message/v1/message.pb.go`(自动生成)
- Modify: `internal/service/setup.go`

- [ ] **Step 1: 改 proto 文件**

打开 `api/proto/message/v1/message.proto`。找到 `SmsVendor` enum 定义,加 4 个值:

```proto
enum SmsVendor {
  SMS_VENDOR_UNSPECIFIED = 0;
  SMS_VENDOR_ALIYUN = 1;
  SMS_VENDOR_TENCENT = 2;
  SMS_VENDOR_VOLCENGINE = 3;
  SMS_VENDOR_BYTEPLUS = 4;
  SMS_VENDOR_HUAWEI = 5;
}
```

- [ ] **Step 2: 跑 protoc 重生成**

项目根有 Makefile 或 buf 配置。先看一下:

```bash
ls Makefile buf.yaml buf.gen.yaml 2>&1
```

如果有 Makefile,看 proto 生成 target:

```bash
grep -A2 "proto\|buf" Makefile 2>&1 | head -20
```

执行生成命令(可能是 `make proto` 或 `buf generate` 或 `protoc --go_out=... --go-grpc_out=...`)。

Expected: `gen/message/v1/message.pb.go` 里的 `SmsVendor_value` map 包含 `TENCENT` / `VOLCENGINE` / `BYTEPLUS` / `HUAWEI`。

验证:
```bash
grep "SMS_VENDOR_TENCENT\|SMS_VENDOR_VOLCENGINE\|SMS_VENDOR_BYTEPLUS\|SMS_VENDOR_HUAWEI" gen/message/v1/message.pb.go | head -10
```

- [ ] **Step 3: 改 parseSMSVendorName**

打开 `internal/service/setup.go`。找到 `parseSMSVendorName`(约 106-115 行),switch 加 4 个 case:

```go
func parseSMSVendorName(s string) (pb.SmsVendor, error) {
    switch s {
    case "aliyun":
        return pb.SmsVendor_SMS_VENDOR_ALIYUN, nil
    case "tencent":
        return pb.SmsVendor_SMS_VENDOR_TENCENT, nil
    case "volcengine":
        return pb.SmsVendor_SMS_VENDOR_VOLCENGINE, nil
    case "byteplus":
        return pb.SmsVendor_SMS_VENDOR_BYTEPLUS, nil
    case "huawei":
        return pb.SmsVendor_SMS_VENDOR_HUAWEI, nil
    default:
        return 0, fmt.Errorf("unknown vendor %q", s)
    }
}
```

同时更新上方 doc comment 里的 valid keys 列表(约 50 行):
```go
// Vendors maps a vendor name to its accounts. Valid keys (parsed to
// pb.SmsVendor by service.New): "aliyun", "tencent", "volcengine",
// "byteplus", "huawei".
Vendors map[string]*sms.VendorConfig
```

- [ ] **Step 4: 验证编译**

```bash
go build ./...
```

Expected: PASS。proto 改了但还没 vendor impl 引用新 enum,所以编译通过。

- [ ] **Step 5: Commit**

```bash
git add api/proto/message/v1/message.proto gen/ internal/service/setup.go
git commit -m "feat(proto): add Tencent/Volcengine/BytePlus/Huawei to SmsVendor enum"
```

---

## Task 3: Tencent vendor impl

加 `tencent.go` + `tencent_test.go`。SDK 包:`github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711`。先 `go get` 装包,再用 `go doc` 核对类型,然后写代码。

**Files:**
- Create: `internal/provider/sms/tencent.go`
- Create: `internal/provider/sms/tencent_test.go`
- Modify: `go.mod` / `go.sum`(自动)
- Modify: `internal/provider/sms/registry.go`(`AccountConfig` 加 `Tencent *TencentConfig` + `buildProvider` 加 case)

- [ ] **Step 1: 加 SDK 依赖**

```bash
go get github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711@latest
```

注意:腾讯云主仓 20w+ tag,如果 `@latest` 拉取超时,改用具体版本 `@v3.0.1293`(或当时最新稳定版),或临时改 GOPROXY=`https://goproxy.cn,direct`。

- [ ] **Step 2: 用 `go doc` 核对 SDK 类型**

```bash
go doc github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711 SendSmsRequest
go doc github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711 SendSmsResponse
go doc github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711 SendStatus
go doc github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711 NewClient
go doc github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711 NewSendSmsRequest
go doc github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711 Client.SendSms
```

记录下来:
- `SendSmsRequest` 有哪些字段(SmsSdkAppId / SignName / PhoneNumberSet / TemplateId / TemplateParamSet 等)
- `SendSmsResponse` 结构(`Response.SendStatusSet` 数组,每个元素有 Code / Message / IsoCode)
- `Client.SendSms(req)` 的签名
- `NewClient(credential, region, profile)` 的入参

如果实际类型与本 plan 后续给出的代码不同,**以 go doc 为准调整代码**,并在最终报告里 DONE_WITH_CONCERNS 注明差异。

- [ ] **Step 3: 创建 tencent.go**

```go
package sms

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	tcsms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711"

	pb "message-service/gen/message/v1"
)

// TencentConfig holds the configuration for the Tencent Cloud SMS provider.
type TencentConfig struct {
	SecretID    string
	SecretKey   string
	SmsSdkAppID string // 控制台"短信应用" SdkAppId,如 "1400000000"
	SignName    string
	Region      string `default:"ap-guangzhou"` // 国内常用 ap-guangzhou / ap-beijing;国际站 region 不同
	Endpoint    string // 可选,默认由 SDK 按 region 推导
}

// TencentProvider sends SMS via Tencent Cloud SDK. One provider handles both
// domestic + international (SDK routes by phone number prefix automatically).
type TencentProvider struct {
	account string
	config  *TencentConfig
	client  tencentSmsSender
}

// tencentSmsSender abstracts the SDK client for testability.
type tencentSmsSender interface {
	SendSmsWithContext(ctx context.Context, req *tcsms.SendSmsRequest) (*tcsms.SendSmsResponse, error)
}

// NewTencentProvider creates a Tencent Cloud SMS provider.
func NewTencentProvider(account string, cfg *TencentConfig) (*TencentProvider, error) {
	credential := common.NewCredential(cfg.SecretID, cfg.SecretKey)
	cpf := profile.NewClientProfile()
	if cfg.Endpoint != "" {
		cpf.HttpProfile.Endpoint = cfg.Endpoint
	}

	client, err := tcsms.NewClient(credential, cfg.Region, cpf)
	if err != nil {
		return nil, fmt.Errorf("tencent: create client: %w", err)
	}
	return &TencentProvider{account: account, config: cfg, client: client}, nil
}

// newTencentProviderWithClient creates a TencentProvider with a mock client (for testing).
func newTencentProviderWithClient(account string, cfg *TencentConfig, sender tencentSmsSender) *TencentProvider {
	return &TencentProvider{account: account, config: cfg, client: sender}
}

// Vendor identifies this provider as TENCENT in the proto enum.
func (*TencentProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_TENCENT }

// Account returns the account name this provider was constructed with.
func (p *TencentProvider) Account() string { return p.account }

// Send sends an SMS via Tencent Cloud SDK.
//
// Tencent's SendSms API supports batch sends (PhoneNumberSet is a slice), but
// message-service always sends one phone at a time — we extract the first
// element of SendStatusSet for the per-message result.
func (p *TencentProvider) Send(ctx context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("tencent sms: phone number is empty")
	}
	if msg.Template == "" {
		return fmt.Errorf("tencent sms: template is required")
	}

	req := tcsms.NewSendSmsRequest()
	req.SmsSdkAppId = common.StringPtr(p.config.SmsSdkAppID)
	req.SignName = common.StringPtr(p.config.SignName)
	req.PhoneNumberSet = common.StringPtrs([]string{msg.To})
	req.TemplateId = common.StringPtr(msg.Template)

	if msg.Params != nil {
		paramBytes, err := json.Marshal(msg.Params)
		if err != nil {
			return fmt.Errorf("tencent sms: marshal template params: %w", err)
		}
		req.TemplateParamSet = common.StringPtrs([]string{string(paramBytes)})
	}

	resp, err := p.client.SendSmsWithContext(ctx, req)
	if err != nil {
		return fmt.Errorf("tencent sms send: %w", err)
	}

	if len(resp.Response.SendStatusSet) == 0 {
		return fmt.Errorf("tencent sms: empty SendStatusSet")
	}
	status := resp.Response.SendStatusSet[0]
	if status.Code == nil || *status.Code != "Ok" {
		code := ""
		if status.Code != nil {
			code = *status.Code
		}
		message := ""
		if status.Message != nil {
			message = *status.Message
		}
		return fmt.Errorf("tencent sms: code=%s, message=%s", code, message)
	}
	return nil
}
```

**核对点**(以 Step 2 的 `go doc` 输出为准):
- `tcsms.NewSendSmsRequest()` 是否存在?
- `req.SmsSdkAppId` / `req.SignName` / `req.PhoneNumberSet` / `req.TemplateId` / `req.TemplateParamSet` 字段名是否准确?(腾讯云字段大小写可能在 SDK 版本间变化)
- `resp.Response.SendStatusSet[0].Code` 的类型是 `*string` 还是 `string`?(下面 test 用 `common.StringPtr("Ok")`)
- `Client.SendSmsWithContext(ctx, req)` 是否存在?(若只有 `SendSms(req)`,改为不带 ctx 的版本,或包一层 context-ware 适配)

如不一致,调整代码并 commit 时在 message 里注明。

- [ ] **Step 4: 创建 tencent_test.go**

```go
package sms

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	tcsms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711"

	pb "message-service/gen/message/v1"
)

type tencentMockSender struct {
	resp *tcsms.SendSmsResponse
	err  error
}

func (m *tencentMockSender) SendSmsWithContext(_ context.Context, _ *tcsms.SendSmsRequest) (*tcsms.SendSmsResponse, error) {
	return m.resp, m.err
}

func tencentOkResponse() *tcsms.SendSmsResponse {
	return &tcsms.SendSmsResponse{
		Response: &tcsms.SendSmsResponseParamsForMock{
			SendStatusSet: []*tcsms.SendStatus{
				{Code: common.StringPtr("Ok"), Message: common.StringPtr("send success")},
			},
		},
	}
}

func tencentErrResponse(code, msg string) *tcsms.SendSmsResponse {
	return &tcsms.SendSmsResponse{
		Response: &tcsms.SendSmsResponseParamsForMock{
			SendStatusSet: []*tcsms.SendStatus{
				{Code: common.StringPtr(code), Message: common.StringPtr(msg)},
			},
		},
	}
}
```

**重要**:`tcsms.SendSmsResponse.Response` 的实际类型不是 `SendSmsResponseParamsForMock` — 上面是占位符。`go doc` 看实际类型(通常是匿名 struct 直接嵌在 SendSmsResponse 里,字段名 `Response *struct{...SendStatusSet []*SendStatus...}`)。调整 test helper 让类型匹配。

继续:

```go
func TestTencentProvider_Vendor(t *testing.T) {
	p, err := NewTencentProvider("primary", &TencentConfig{
		SecretID: "x", SecretKey: "y", SmsSdkAppID: "1400000000", SignName: "TestApp", Region: "ap-guangzhou",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_TENCENT, p.Vendor())
}

func TestTencentProvider_Account(t *testing.T) {
	p, err := NewTencentProvider("secondary", &TencentConfig{
		SecretID: "x", SecretKey: "y", SmsSdkAppID: "1400000000", SignName: "TestApp", Region: "ap-guangzhou",
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", p.Account())
}

func TestTencentProvider_Send_success(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400", SignName: "App"}, &tencentMockSender{resp: tencentOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", Template: "SMS_123"})
	require.NoError(t, err)
}

func TestTencentProvider_Send_withParams(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400", SignName: "App"}, &tencentMockSender{resp: tencentOkResponse()})
	err := p.Send(context.Background(), &Message{
		To:       "13800138000",
		Template: "SMS_123",
		Params:   map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}

func TestTencentProvider_Send_emptyPhone(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400", SignName: "App"}, &tencentMockSender{resp: tencentOkResponse()})
	err := p.Send(context.Background(), &Message{To: "", Template: "SMS_123"})
	require.EqualError(t, err, "tencent sms: phone number is empty")
}

func TestTencentProvider_Send_noTemplate(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400", SignName: "App"}, &tencentMockSender{resp: tencentOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000"})
	require.EqualError(t, err, "tencent sms: template is required")
}

func TestTencentProvider_Send_sdkError(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400", SignName: "App"}, &tencentMockSender{
		err: fmt.Errorf("network timeout"),
	})
	err := p.Send(context.Background(), &Message{To: "13800138000", Template: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "tencent sms send")
	require.Contains(t, err.Error(), "network timeout")
}

func TestTencentProvider_Send_businessError(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400", SignName: "App"}, &tencentMockSender{
		resp: tencentErrResponse("LimitExceed.Sms", "frequency limit"),
	})
	err := p.Send(context.Background(), &Message{To: "13800138000", Template: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "LimitExceed.Sms")
	require.Contains(t, err.Error(), "frequency limit")
}
```

- [ ] **Step 5: 接到 AccountConfig + buildProvider**

打开 `internal/provider/sms/registry.go`。`AccountConfig` 加 `Tencent` 字段(取消注释):

```go
type AccountConfig struct {
	Name    string
	Aliyun  *AliyunConfig
	Tencent *TencentConfig
	// Volcengine *VolcengineConfig // Task 4
	// Byteplus   *ByteplusConfig   // Task 5
	// Huawei     *HuaweiConfig     // Task 6
}
```

`buildProvider` switch 在 ALIYUN case 后加 TENCENT:

```go
case pb.SmsVendor_SMS_VENDOR_TENCENT:
    if ac.Tencent == nil {
        return nil, fmt.Errorf("sms vendor %s: tencent config missing", vendor)
    }
    return NewTencentProvider(ac.Name, ac.Tencent)
```

- [ ] **Step 6: 验证编译 + 测试**

```bash
gofmt -w internal/provider/sms/tencent.go internal/provider/sms/tencent_test.go internal/provider/sms/registry.go
goimports -w internal/provider/sms/
go build ./...
go test -race -run "Tencent" ./internal/provider/sms/...
golangci-lint run ./internal/provider/sms/...
```

Expected: 全部 PASS。如 `go doc` 发现 SDK 类型与本 plan 不同,代码已相应调整,测试 helper 类型也跟着调整。

- [ ] **Step 7: Commit**

```bash
git add internal/provider/sms/tencent.go internal/provider/sms/tencent_test.go internal/provider/sms/registry.go go.mod go.sum
git commit -m "feat(provider/sms): add Tencent vendor impl"
```

---

## Task 4: Volcengine vendor impl

加 `volcengine.go` + `volcengine_test.go`。SDK 包:`github.com/volcengine/volc-sdk-golang/service/sms`。

**Files:**
- Create: `internal/provider/sms/volcengine.go`
- Create: `internal/provider/sms/volcengine_test.go`
- Modify: `go.mod` / `go.sum`
- Modify: `internal/provider/sms/registry.go`

- [ ] **Step 1: 加 SDK 依赖**

```bash
go get github.com/volcengine/volc-sdk-golang@latest
```

- [ ] **Step 2: 用 `go doc` 核对 SDK 类型**

```bash
go doc github.com/volcengine/volc-sdk-golang/service/sms SMS
go doc github.com/volcengine/volc-sdk-golang/service/sms SmsRequest
go doc github.com/volcengine/volc-sdk-golang/service/sms SmsResponse
go doc github.com/volcengine/volc-sdk-golang/service/sms SmsResult
go doc github.com/volcengine/volc-sdk-golang/service/sms NewInstance
```

**已知特殊点**(来自 pkg.go.dev):
- SDK client 类型是 `*SMS`,通过 `sms.NewInstance()` 创建
- 不直接接受 context(`Send` 签名是 `Send(req *SmsRequest) (*SmsResponse, int, error)` — 返回 3 个值,第 2 个是 int 状态码)
- 业务结果在 `SmsResponse.ResponseMetadata.Error` + `SmsResponse.Result.MessageID`

如果 SDK 真不支持 context(参考 issue #30),我们的 `volcengineSmsSender` interface 也去掉 ctx 参数,接受这个限制。

- [ ] **Step 3: 创建 volcengine.go**

```go
package sms

import (
	"context"
	"fmt"

	vocsms "github.com/volcengine/volc-sdk-golang/service/sms"

	pb "message-service/gen/message/v1"
)

// VolcengineConfig holds the configuration for the Volcengine SMS provider.
type VolcengineConfig struct {
	AccessKID  string // 火山引擎账号 AccessKey ID
	SecretKey  string
	SmsAccount string // 火山引擎短信平台"账号"
	Sign       string // 短信签名
	Region     string `default:"cn-north-1"`
}

// VolcengineProvider sends SMS via Volcengine (domestic) SDK.
type VolcengineProvider struct {
	account string
	config  *VolcengineConfig
	client  volcengineSmsSender
}

// volcengineSmsSender abstracts the SDK client for testability.
//
// Note: volc-sdk-golang's SMS.Send does not accept a context.Context (issue
// #30). The interface matches the SDK shape; context cancellation will not
// short-circuit in-flight requests for this vendor.
type volcengineSmsSender interface {
	Send(req *vocsms.SmsRequest) (*vocsms.SmsResponse, int, error)
}

// NewVolcengineProvider creates a Volcengine SMS provider.
func NewVolcengineProvider(account string, cfg *VolcengineConfig) (*VolcengineProvider, error) {
	instance := vocsms.NewInstance()
	instance.SetRegion(cfg.Region)

	return &VolcengineProvider{
		account: account,
		config:  cfg,
		client:  &volcengineSdkClient{instance: instance, account: cfg.SmsAccount, sign: cfg.Sign},
	}, nil
}

// volcengineSdkClient adapts *vocsms.SMS to the volcengineSmsSender interface.
// SmsAccount and Sign are baked in at construction (per-vendor constants).
type volcengineSdkClient struct {
	instance *vocsms.SMS
	account  string
	sign     string
}

func (c *volcengineSdkClient) Send(req *vocsms.SmsRequest) (*vocsms.SmsResponse, int, error) {
	req.SmsAccount = c.account
	req.Sign = c.sign
	return c.instance.Send(req)
}

// newVolcengineProviderWithClient creates a VolcengineProvider with a mock client.
func newVolcengineProviderWithClient(account string, cfg *VolcengineConfig, sender volcengineSmsSender) *VolcengineProvider {
	return &VolcengineProvider{account: account, config: cfg, client: sender}
}

// Vendor identifies this provider as VOLCENGINE in the proto enum.
func (*VolcengineProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_VOLCENGINE }

// Account returns the account name this provider was constructed with.
func (p *VolcengineProvider) Account() string { return p.account }

// Send sends an SMS via Volcengine SDK.
func (p *VolcengineProvider) Send(_ context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("volcengine sms: phone number is empty")
	}
	if msg.Template == "" {
		return fmt.Errorf("volcengine sms: template is required")
	}

	req := &vocsms.SmsRequest{
		PhoneNumber: msg.To,
		TemplateID:  msg.Template,
	}
	if msg.Params != nil {
		req.TemplateParam = msg.Params
	}

	resp, _, err := p.client.Send(req)
	if err != nil {
		return fmt.Errorf("volcengine sms send: %w", err)
	}
	if resp.ResponseMetadata.Error != nil {
		return fmt.Errorf("volcengine sms: code=%s, message=%s",
			resp.ResponseMetadata.Error.Code, resp.ResponseMetadata.Error.Message)
	}
	return nil
}
```

**核对点**(以 Step 2 `go doc` 为准):
- `vocsms.NewInstance()` 是否存在?
- `SmsRequest` 的字段名:`PhoneNumber` / `TemplateID` / `TemplateParam` / `SmsAccount` / `Sign` 大小写?
- `SmsResponse.ResponseMetadata.Error.Code` / `.Message` 字段?
- `instance.SetRegion(cfg.Region)` 是否存在?

如有差异,以 go doc 为准调整。

- [ ] **Step 4: 创建 volcengine_test.go**

```go
package sms

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	vocsms "github.com/volcengine/volc-sdk-golang/service/sms"
	"github.com/volcengine/volc-sdk-golang/base"

	pb "message-service/gen/message/v1"
)

type volcengineMockSender struct {
	resp     *vocsms.SmsResponse
	statusCode int
	err      error
}

func (m *volcengineMockSender) Send(_ *vocsms.SmsRequest) (*vocsms.SmsResponse, int, error) {
	return m.resp, m.statusCode, m.err
}

func volcengineOkResponse() *vocsms.SmsResponse {
	return &vocsms.SmsResponse{
		ResponseMetadata: base.ResponseMetadata{},
		Result:           &vocsms.SmsResult{MessageID: []string{"msg-1"}},
	}
}

func volcengineErrResponse(code, msg string) *vocsms.SmsResponse {
	return &vocsms.SmsResponse{
		ResponseMetadata: base.ResponseMetadata{
			Error: &base.Error{
				Code:    code,
				Message: msg,
			},
		},
	}
}

func TestVolcengineProvider_Vendor(t *testing.T) {
	p, err := NewVolcengineProvider("primary", &VolcengineConfig{
		AccessKID: "x", SecretKey: "y", SmsAccount: "acc", Sign: "sign",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_VOLCENGINE, p.Vendor())
}

func TestVolcengineProvider_Account(t *testing.T) {
	p, err := NewVolcengineProvider("secondary", &VolcengineConfig{
		AccessKID: "x", SecretKey: "y", SmsAccount: "acc", Sign: "sign",
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", p.Account())
}

func TestVolcengineProvider_Send_success(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc", Sign: "sign"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", Template: "SMS_123"})
	require.NoError(t, err)
}

func TestVolcengineProvider_Send_withParams(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc", Sign: "sign"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", Template: "SMS_123", Params: map[string]string{"code": "123456"}})
	require.NoError(t, err)
}

func TestVolcengineProvider_Send_emptyPhone(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc", Sign: "sign"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "", Template: "SMS_123"})
	require.EqualError(t, err, "volcengine sms: phone number is empty")
}

func TestVolcengineProvider_Send_noTemplate(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc", Sign: "sign"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000"})
	require.EqualError(t, err, "volcengine sms: template is required")
}

func TestVolcengineProvider_Send_sdkError(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc", Sign: "sign"}, &volcengineMockSender{err: fmt.Errorf("network timeout")})
	err := p.Send(context.Background(), &Message{To: "13800138000", Template: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "volcengine sms send")
	require.Contains(t, err.Error(), "network timeout")
}

func TestVolcengineProvider_Send_businessError(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc", Sign: "sign"}, &volcengineMockSender{
		resp: volcengineErrResponse("LimitExceeded", "frequency limit"),
	})
	err := p.Send(context.Background(), &Message{To: "13800138000", Template: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "LimitExceeded")
	require.Contains(t, err.Error(), "frequency limit")
}
```

- [ ] **Step 5: 接到 AccountConfig + buildProvider**

`registry.go`:
```go
type AccountConfig struct {
	Name       string
	Aliyun     *AliyunConfig
	Tencent    *TencentConfig
	Volcengine *VolcengineConfig
	// Byteplus *ByteplusConfig // Task 5
	// Huawei   *HuaweiConfig   // Task 6
}
```

`buildProvider` switch 加:
```go
case pb.SmsVendor_SMS_VENDOR_VOLCENGINE:
    if ac.Volcengine == nil {
        return nil, fmt.Errorf("sms vendor %s: volcengine config missing", vendor)
    }
    return NewVolcengineProvider(ac.Name, ac.Volcengine)
```

- [ ] **Step 6: 验证 + Commit**

```bash
gofmt -w internal/provider/sms/volcengine.go internal/provider/sms/volcengine_test.go internal/provider/sms/registry.go
goimports -w internal/provider/sms/
go build ./...
go test -race -run "Volcengine" ./internal/provider/sms/...
golangci-lint run ./internal/provider/sms/...

git add internal/provider/sms/volcengine.go internal/provider/sms/volcengine_test.go internal/provider/sms/registry.go go.mod go.sum
git commit -m "feat(provider/sms): add Volcengine vendor impl"
```

---

## Task 5: BytePlus vendor impl

加 `byteplus.go` + `byteplus_test.go`。SDK 包:`github.com/byteplus-sdk/byteplus-sdk-golang`(v1,因 v2 不含 SMS)。

**Files:**
- Create: `internal/provider/sms/byteplus.go`
- Create: `internal/provider/sms/byteplus_test.go`
- Modify: `go.mod` / `go.sum`
- Modify: `internal/provider/sms/registry.go`

- [ ] **Step 1: 加 SDK 依赖**

```bash
go get github.com/byteplus-sdk/byteplus-sdk-golang@latest
```

- [ ] **Step 2: 用 `go doc` 核对 SDK 包结构**

BytePlus SDK 的 SMS 子包路径需要 `go doc` 探索。试:

```bash
go doc github.com/byteplus-sdk/byteplus-sdk-golang/service/sms 2>&1 | head -30
# 或
go doc github.com/byteplus-sdk/byteplus-sdk-golang/sms 2>&1 | head -30
# 或浏览 github.com/byteplus-sdk/byteplus-sdk-golang 仓库的 service 目录
```

BytePlus 的 SMS API 跟 Volcengine 结构高度类似(都是字节系,ResponseMetadata.Error 模式)。预期类型:
- Client type: `*SMS`,通过 `sms.NewInstance()` 创建
- Send 方法:`Send(req *SmsRequest) (*SmsResponse, int, error)`(跟 Volcengine 一样,3 返回值)

如果实际 SDK 包路径不同,先 `go doc github.com/byteplus-sdk/byteplus-sdk-golang` 看顶层包结构,再深入找 SMS 子包。

- [ ] **Step 3: 创建 byteplus.go**

按 Volcengine 模板,把 `Volcengine` 替换为 `Byteplus`、`volcengine` 替换为 `byteplus`、`vocsms` 替换为实际 BytePlus SMS 包 alias:

```go
package sms

import (
	"context"
	"fmt"

	bpsms "github.com/byteplus-sdk/byteplus-sdk-golang/service/sms"  // 路径以 go doc 为准

	pb "message-service/gen/message/v1"
)

// ByteplusConfig holds the configuration for the BytePlus SMS provider
// (Volcano Engine's international brand).
type ByteplusConfig struct {
	AccessKID  string
	SecretKey  string
	SmsAccount string
	Sign       string
	Region     string `default:"ap-singapore-1"`
}

// ByteplusProvider sends SMS via BytePlus SDK.
type ByteplusProvider struct {
	account string
	config  *ByteplusConfig
	client  byteplusSmsSender
}

type byteplusSmsSender interface {
	Send(req *bpsms.SmsRequest) (*bpsms.SmsResponse, int, error)
}

func NewByteplusProvider(account string, cfg *ByteplusConfig) (*ByteplusProvider, error) {
	instance := bpsms.NewInstance()
	instance.SetRegion(cfg.Region)

	return &ByteplusProvider{
		account: account,
		config:  cfg,
		client:  &byteplusSdkClient{instance: instance, account: cfg.SmsAccount, sign: cfg.Sign},
	}, nil
}

type byteplusSdkClient struct {
	instance *bpsms.SMS
	account  string
	sign     string
}

func (c *byteplusSdkClient) Send(req *bpsms.SmsRequest) (*bpsms.SmsResponse, int, error) {
	req.SmsAccount = c.account
	req.Sign = c.sign
	return c.instance.Send(req)
}

func newByteplusProviderWithClient(account string, cfg *ByteplusConfig, sender byteplusSmsSender) *ByteplusProvider {
	return &ByteplusProvider{account: account, config: cfg, client: sender}
}

func (*ByteplusProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_BYTEPLUS }
func (p *ByteplusProvider) Account() string     { return p.account }

func (p *ByteplusProvider) Send(_ context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("byteplus sms: phone number is empty")
	}
	if msg.Template == "" {
		return fmt.Errorf("byteplus sms: template is required")
	}

	req := &bpsms.SmsRequest{
		PhoneNumber: msg.To,
		TemplateID:  msg.Template,
	}
	if msg.Params != nil {
		req.TemplateParam = msg.Params
	}

	resp, _, err := p.client.Send(req)
	if err != nil {
		return fmt.Errorf("byteplus sms send: %w", err)
	}
	if resp.ResponseMetadata.Error != nil {
		return fmt.Errorf("byteplus sms: code=%s, message=%s",
			resp.ResponseMetadata.Error.Code, resp.ResponseMetadata.Error.Message)
	}
	return nil
}
```

**核对点**:以 `go doc` 为准。BytePlus SDK 包路径如果不是 `service/sms`,改成实际路径。

- [ ] **Step 4: 创建 byteplus_test.go**

按 `volcengine_test.go` 模板,把所有 `Volcengine` 替换为 `Byteplus`、`vocsms` 替换为 `bpsms`。Test 名 `TestByteplusProvider_*`,helper `byteplusMockSender` / `byteplusOkResponse` / `byteplusErrResponse`。

业务错误码示例用 `"LimitExceeded"`(具体值看 BytePlus 文档)。

- [ ] **Step 5: 接到 AccountConfig + buildProvider**

```go
type AccountConfig struct {
	Name       string
	Aliyun     *AliyunConfig
	Tencent    *TencentConfig
	Volcengine *VolcengineConfig
	Byteplus   *ByteplusConfig
	// Huawei *HuaweiConfig // Task 6
}
```

`buildProvider` 加:
```go
case pb.SmsVendor_SMS_VENDOR_BYTEPLUS:
    if ac.Byteplus == nil {
        return nil, fmt.Errorf("sms vendor %s: byteplus config missing", vendor)
    }
    return NewByteplusProvider(ac.Name, ac.Byteplus)
```

- [ ] **Step 6: 验证 + Commit**

```bash
gofmt -w internal/provider/sms/byteplus.go internal/provider/sms/byteplus_test.go internal/provider/sms/registry.go
goimports -w internal/provider/sms/
go build ./...
go test -race -run "Byteplus" ./internal/provider/sms/...
golangci-lint run ./internal/provider/sms/...

git add internal/provider/sms/byteplus.go internal/provider/sms/byteplus_test.go internal/provider/sms/registry.go go.mod go.sum
git commit -m "feat(provider/sms): add BytePlus vendor impl"
```

---

## Task 6: Huawei vendor impl

加 `huawei.go` + `huawei_test.go`。SDK 包:`github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2`。

**Files:**
- Create: `internal/provider/sms/huawei.go`
- Create: `internal/provider/sms/huawei_test.go`
- Modify: `go.mod` / `go.sum`
- Modify: `internal/provider/sms/registry.go`

- [ ] **Step 1: 加 SDK 依赖**

```bash
go get github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2@latest
# 主包也要,credentials 用
go get github.com/huaweicloud/huaweicloud-sdk-go-v3/core
go get github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic
```

- [ ] **Step 2: 用 `go doc` 核对 SDK 类型**

```bash
go doc github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2 MsgsmsClient
go doc github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2 SendSmsRequest
go doc github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2 SendSmsResponse
go doc github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2 NewMsgsmsClient
go doc github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2/region
```

华为云 SDK 关键模式(来自 Context7):
- `basic.NewCredentialsBuilder().WithAk(...).WithSk(...).Build()` 构造凭证
- `region.SafeValueOf("cn-north-4")` 构造 region
- `core.NewHcHttpClientBuilder().WithRegion(r).WithCredential(creds).Build()` 构造 HTTP client
- `msgsms.NewMsgsmsClient(httpClient)` 构造服务 client
- `client.SendSms(req)` 调接口

华为云 MSGSMS 还需要 `app_key` / `app_secret`(短信平台项目的),这通常通过 SDK 的 `frontend` 配置或请求头携带,具体看 SDK 源码。

- [ ] **Step 3: 创建 huawei.go**

```go
package sms

import (
	"context"
	"fmt"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic"
	hwsms "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2/model"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/msgsms/v2/region"

	pb "message-service/gen/message/v1"
)

// HuaweiConfig holds the configuration for the Huawei Cloud MSGSMS provider.
type HuaweiConfig struct {
	Ak        string
	Sk        string
	AppKey    string // MSGSMS 项目的 app_key
	AppSecret string // MSGSMS 项目的 app_secret
	Endpoint  string `default:"https://msgsms.cn-north-4.myhuaweicloud.com"`
	Region    string `default:"cn-north-4"`
}

// HuaweiProvider sends SMS via Huawei Cloud MSGSMS SDK. One provider handles
// both domestic + international (SDK routes by phone number prefix).
type HuaweiProvider struct {
	account string
	config  *HuaweiConfig
	client  huaweiSmsSender
}

type huaweiSmsSender interface {
	SendSms(ctx context.Context, req *model.SendSmsRequest) (*model.SendSmsResponse, error)
}

// NewHuaweiProvider creates a Huawei Cloud MSGSMS provider.
func NewHuaweiProvider(account string, cfg *HuaweiConfig) (*HuaweiProvider, error) {
	creds, err := basic.NewCredentialsBuilder().
		WithAk(cfg.Ak).
		WithSk(cfg.Sk).
		SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("huawei: build credentials: %w", err)
	}

	r, err := region.SafeValueOf(cfg.Region)
	if err != nil {
		return nil, fmt.Errorf("huawei: invalid region %q: %w", cfg.Region, err)
	}

	builder := core.NewHcHttpClientBuilder().
		WithRegion(r).
		WithCredential(creds)
	if cfg.Endpoint != "" {
		builder = builder.WithEndpoint(cfg.Endpoint)
	}

	httpClient, err := builder.SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("huawei: build http client: %w", err)
	}

	client := hwsms.NewMsgsmsClient(httpClient)
	return &HuaweiProvider{account: account, config: cfg, client: &huaweiSdkClient{client: client, appKey: cfg.AppKey, appSecret: cfg.AppSecret}}, nil
}

// huaweiSdkClient adapts *hwsms.MsgsmsClient to inject app_key/app_secret
// (MSGSMS requires these per-request via header).
type huaweiSdkClient struct {
	client    *hwsms.MsgsmsClient
	appKey    string
	appSecret string
}

func (c *huaweiSdkClient) SendSms(ctx context.Context, req *model.SendSmsRequest) (*model.SendSmsResponse, error) {
	// Huawei MSGSMS expects app_key/app_secret in request header. Set via
	// req.Header or via a custom SenderFunc depending on SDK shape — verify
	// with go doc model.SendSmsRequest fields.
	return c.client.SendSms(req)
}

func newHuaweiProviderWithClient(account string, cfg *HuaweiConfig, sender huaweiSmsSender) *HuaweiProvider {
	return &HuaweiProvider{account: account, config: cfg, client: sender}
}

func (*HuaweiProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_HUAWEI }
func (p *HuaweiProvider) Account() string     { return p.account }

func (p *HuaweiProvider) Send(ctx context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("huawei sms: phone number is empty")
	}
	if msg.Template == "" {
		return fmt.Errorf("huawei sms: template is required")
	}

	req := &model.SendSmsRequest{
		Body: &model.SmsApp{
			// Huawei MSGSMS request body fields — verify with go doc model.SmsApp
			// Typical: From / To / TemplateId / TemplateParas / Signature
		},
	}

	resp, err := p.client.SendSms(ctx, req)
	if err != nil {
		return fmt.Errorf("huawei sms send: %w", err)
	}
	if resp.HttpStatusCode != 200 {
		return fmt.Errorf("huawei sms: http_status=%d", resp.HttpStatusCode)
	}
	return nil
}
```

**重要**:`model.SmsApp` 的字段名 + 业务错误码字段(`resp.Code` / `resp.ErrorCode` / `resp.Message`)需要以 `go doc` 为准。上面 `Body: &model.SmsApp{...}` 内部字段是占位符,**实施时必须填实际字段**。如果实际字段名跟 plan 不同(很可能),调整代码,在 commit message 里注明。

- [ ] **Step 4: 创建 huawei_test.go**

按 `aliyun_test.go` / `tencent_test.go` 模板。`huaweiMockSender` 实现 `huaweiSmsSender` interface,`huaweiOkResponse` / `huaweiErrResponse` 构造 `*model.SendSmsResponse`。

业务错误码:`huaweiErrResponse("E200027", "frequency limit")`(具体看华为云错误码文档)。

- [ ] **Step 5: 接到 AccountConfig + buildProvider**

```go
type AccountConfig struct {
	Name       string
	Aliyun     *AliyunConfig
	Tencent    *TencentConfig
	Volcengine *VolcengineConfig
	Byteplus   *ByteplusConfig
	Huawei     *HuaweiConfig
}
```

`buildProvider` 加:
```go
case pb.SmsVendor_SMS_VENDOR_HUAWEI:
    if ac.Huawei == nil {
        return nil, fmt.Errorf("sms vendor %s: huawei config missing", vendor)
    }
    return NewHuaweiProvider(ac.Name, ac.Huawei)
```

- [ ] **Step 6: 验证 + Commit**

```bash
gofmt -w internal/provider/sms/huawei.go internal/provider/sms/huawei_test.go internal/provider/sms/registry.go
goimports -w internal/provider/sms/
go build ./...
go test -race -run "Huawei" ./internal/provider/sms/...
golangci-lint run ./internal/provider/sms/...

git add internal/provider/sms/huawei.go internal/provider/sms/huawei_test.go internal/provider/sms/registry.go go.mod go.sum
git commit -m "feat(provider/sms): add Huawei vendor impl"
```

---

## Task 7: 更新 config.yaml.example + CLAUDE.md

把 4 个新 vendor 配置示例加进 `config.yaml.example`,同时更新 `CLAUDE.md` 的 SMS vendor 列表。

**Files:**
- Modify: `config.yaml.example`
- Modify: `CLAUDE.md`

- [ ] **Step 1: 扩 config.yaml.example**

打开 `config.yaml.example`。在现有 sms.vendors.aliyun 之后,加 tencent / volcengine / byteplus / huawei 4 个示例(用 ${ENV_VAR} 形式,跟 docker-deploy 一致):

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

    # 取消注释并配置环境变量以启用对应 vendor
    # tencent:
    #   accounts:
    #     - name: primary
    #       tencent:
    #         secret_id: ${TENCENT_SECRET_ID}
    #         secret_key: ${TENCENT_SECRET_KEY}
    #         sms_sdk_app_id: "1400000000"
    #         sign_name: ${TENCENT_SIGN_NAME}
    #         region: ap-guangzhou

    # volcengine:
    #   accounts:
    #     - name: primary
    #       volcengine:
    #         access_kid: ${VOLC_AK_ID}
    #         secret_key: ${VOLC_AK_SECRET}
    #         sms_account: ${VOLC_SMS_ACCOUNT}
    #         sign: ${VOLC_SIGN}

    # byteplus:
    #   accounts:
    #     - name: primary
    #       byteplus:
    #         access_kid: ${BP_AK_ID}
    #         secret_key: ${BP_AK_SECRET}
    #         sms_account: ${BP_SMS_ACCOUNT}
    #         sign: ${BP_SIGN}

    # huawei:
    #   accounts:
    #     - name: primary
    #       huawei:
    #         ak: ${HW_AK}
    #         sk: ${HW_SK}
    #         app_key: ${HW_APP_KEY}
    #         app_secret: ${HW_APP_SECRET}
    #         endpoint: https://msgsms.cn-north-4.myhuaweicloud.com
    #         region: cn-north-4

  routes:
    - country: "*"
      targets:
        - vendor: aliyun
          account: primary
```

- [ ] **Step 2: 改 CLAUDE.md**

打开 `CLAUDE.md`。找到"有限集合的字段必须使用 proto enum"小节里 `SmsVendor` 那一行:

旧:
```
  - `SmsVendor`（ALIYUN）
```

新:
```
  - `SmsVendor`（ALIYUN / TENCENT / VOLCENGINE / BYTEPLUS / HUAWEI）
```

- [ ] **Step 3: Commit**

```bash
git add config.yaml.example CLAUDE.md
git commit -m "docs: add 4 SMS vendor config examples + update SmsVendor list in CLAUDE.md"
```

---

## Task 8: 最终验证

跑全套质量门禁,确保整个迁移无回归。

- [ ] **Step 1: 全量格式化 + lint**

```bash
gofmt -l .
goimports -l .  # 排除 gen/ 下的 .pb.go(generated)
golangci-lint run ./...
```

Expected: 三个命令都无输出或全部 PASS。`goimports -l` 可能列出 `gen/message/v1/*.pb.go`,这是预期的(generated),忽略。

- [ ] **Step 2: 全量构建**

```bash
go build ./...
go vet ./...
```

Expected: PASS

- [ ] **Step 3: 全量测试**

```bash
go test -race -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | grep "internal/provider/sms"
```

Expected: 全部 PASS。`internal/provider/sms` 覆盖率不应低于迁移前(参考 2026-06-25 完成时的 95.4%)。

- [ ] **Step 4: 验证 git 历史**

```bash
git log --oneline -10
```

Expected: 看到 Tasks 1-7 的 7 个 commit。

- [ ] **Step 5: 跨 vendor smoke 验证**

构造一个简单的 end-to-end 场景:用 mock 验证 `NewAccountRegistry` 能正确加载 5 个 vendor 的 account,`SenderFor(vendor, account)` 返回对应 Provider:

```bash
go test -race -run "TestNewAccountRegistry_indexesByVendorAndAccount\|TestSenderFor" ./internal/provider/sms/...
```

Expected: PASS。如果 registry_test.go 里没有针对所有 5 个 vendor 的索引验证,加一个:

```go
func TestNewAccountRegistry_allFiveVendors(t *testing.T) {
    cfg := &Config{
        Vendors: map[pb.SmsVendor]*VendorConfig{
            pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
                {Name: "a", Aliyun: &AliyunConfig{AccessKeyID: "x", AccessKeySecret: "y", SignName: "s", RegionID: "cn-hangzhou"}},
            }},
            pb.SmsVendor_SMS_VENDOR_TENCENT: {Accounts: []*AccountConfig{
                {Name: "t", Tencent: &TencentConfig{SecretID: "x", SecretKey: "y", SmsSdkAppID: "1400", SignName: "s", Region: "ap-guangzhou"}},
            }},
            pb.SmsVendor_SMS_VENDOR_VOLCENGINE: {Accounts: []*AccountConfig{
                {Name: "v", Volcengine: &VolcengineConfig{AccessKID: "x", SecretKey: "y", SmsAccount: "a", Sign: "s"}},
            }},
            pb.SmsVendor_SMS_VENDOR_BYTEPLUS: {Accounts: []*AccountConfig{
                {Name: "b", Byteplus: &ByteplusConfig{AccessKID: "x", SecretKey: "y", SmsAccount: "a", Sign: "s"}},
            }},
            pb.SmsVendor_SMS_VENDOR_HUAWEI: {Accounts: []*AccountConfig{
                {Name: "h", Huawei: &HuaweiConfig{Ak: "x", Sk: "y", AppKey: "k", AppSecret: "s", Endpoint: "https://msgsms.cn-north-4.myhuaweicloud.com", Region: "cn-north-4"}},
            }},
        },
    }
    r, err := NewAccountRegistry(cfg)
    require.NoError(t, err)
    require.NotNil(t, r)

    // Each vendor's account should resolve via SenderFor
    for vendor, wantAccount := range map[pb.SmsVendor]string{
        pb.SmsVendor_SMS_VENDOR_ALIYUN:     "a",
        pb.SmsVendor_SMS_VENDOR_TENCENT:    "t",
        pb.SmsVendor_SMS_VENDOR_VOLCENGINE: "v",
        pb.SmsVendor_SMS_VENDOR_BYTEPLUS:   "b",
        pb.SmsVendor_SMS_VENDOR_HUAWEI:     "h",
    } {
        s, err := r.SenderFor(vendor, wantAccount)
        require.NoError(t, err, "vendor %s", vendor)
        require.NotNil(t, s)
    }
}
```

加到 `internal/provider/sms/registry_test.go`,跑测试,如果 PASS 提交:

```bash
git add internal/provider/sms/registry_test.go
git commit -m "test(provider/sms): add registry smoke test for all 5 vendors"
```

如果测试发现某个 vendor 构造失败(`NewClient` 报错等),回到对应 Task 修。

---

## Self-Review Checklist

执行完所有 Task 后,过一遍 spec 的每个章节,确认无遗漏:

**Spec 覆盖**:
- §目录结构 → Tasks 1-6 全部覆盖(AccountConfig 重构 + proto enum + 4 vendor 文件)
- §1 proto enum + YAML 字符串 → Task 2 + parseSMSVendorName
- §2 AccountConfig 重构 → Task 1 + Task 3-6 逐个 vendor 接入
- §3 vendor impl 设计 → Tasks 3-6
- §4 YAML 配置示例 → Task 7
- §5 依赖管理 → Tasks 3-6 各自 `go get`
- §6 测试策略 → 每个 vendor 7+ test cases
- §兼容性 → Task 1 破坏性 YAML 重构已 atomic commit
- §实施顺序 → 完全按 spec §实施顺序执行

**类型一致性**:
- `AccountConfig` 5 个 vendor 字段名:`Aliyun` / `Tencent` / `Volcengine` / `Byteplus` / `Huawei`(首字母大写)
- 各 `<Vendor>Config` 字段名与 §2 spec 表格一致
- 各 `<Vendor>Provider.Vendor()` 返回值与 §1 enum 一致
- mock helper 命名全部加 vendor 前缀(`tencentMockSender` / `volcengineMockSender` / ...)
- buildProvider switch 顺序与 enum 数值顺序一致(ALIYUN → TENCENT → VOLCENGINE → BYTEPLUS → HUAWEI)

**Placeholder 扫描**:
- 每个 vendor Task 都有"用 `go doc` 核对 SDK 类型"步骤,因为 SDK 实际类型可能与本 plan 代码不同
- 这一约定明确写在 Task 3 Step 2 / Task 4 Step 2 / Task 5 Step 2 / Task 6 Step 2,实施者按 go doc 调整

**已知约束**:
- Task 1 atomic commit 重构,中间状态不编译 — 跟 2026-06-25 Task 4/5 模式一致
- 腾讯云 20w+ tag 问题,如果 `go get @latest` 超时,改用具体版本(已在 Task 3 Step 1 注明)
- BytePlus v1 而非 v2(v2 不含 SMS)— 已在 Task 5 Step 1 注明
- Volcengine SDK 不支持 context(issue #30)— 已在 Task 4 Step 3 注明
- Huawei MSGSMS 需要 app_key/app_secret — 已在 Task 6 Step 3 注明

---

## 关联

**设计文档:** [[services/message-service/design/v5/2026-06-26-add-sms-vendors|2026-06-26 SMS 厂商扩展设计]]
