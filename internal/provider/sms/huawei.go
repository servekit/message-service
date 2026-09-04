package sms

import (
	"context"
	"fmt"
	"sort"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/def"
	hwsms "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/smsapi/v1"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/smsapi/v1/model"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/smsapi/v1/region"
	"github.com/servekit/go-common/jsonx"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// HuaweiConfig holds the configuration for the Huawei Cloud SMS provider.
//
// Huawei MSGSMS has two layers of credentials:
//   - Account AK/SK (IAM-style, signs the HTTP request — here we treat
//     AppKey/AppSecret as the SMS-platform AK/SK per the legacy smsapi/v1
//     auth flow; SMSApiCredentials signs the request with AK=AppKey/SK=AppSecret)
//   - The smsapi/v1 SDK uses application/x-www-form-urlencoded body and signs
//     via SMSApiCredentials (not the standard Basic credentials)
//
// Region defaults to cn-north-4 (mainland China SMS endpoint).
type HuaweiConfig struct {
	AppKey    string // MSGSMS application key (used as AK)
	AppSecret string // MSGSMS application secret (used as SK)
	Sign      string // SMS signature (overridden if message body sets Signature explicitly)
	Endpoint  string `default:"https://smsapi.cn-north-4.myhuaweicloud.com"`
	Region    string `default:"cn-north-4"`
}

// HuaweiProvider sends SMS via the Huawei Cloud smsapi/v1 SDK
// (BatchSendSms — same template, single recipient).
type HuaweiProvider struct {
	account string
	config  *HuaweiConfig
	client  huaweiSmsSender
}

// huaweiSmsSender abstracts the SDK client for testability.
// The smsapi/v1 SDK does not accept a context.Context; context cancellation
// will not short-circuit in-flight requests for this vendor. Tracked as a
// known limitation (same as Volcengine/BytePlus lineage).
type huaweiSmsSender interface {
	BatchSendSms(req *model.BatchSendSmsRequest) (*model.BatchSendSmsResponse, error)
}

// NewHuaweiProvider creates a Huawei Cloud SMS provider.
//
// The account parameter carries the account identity so the registry does
// not need to wrap the provider. Sign is per-message (msg.SignName);
// From/To/TemplateId/TemplateParas are per-message.
func NewHuaweiProvider(account string, cfg *HuaweiConfig) (*HuaweiProvider, error) {
	creds := hwsms.NewSMSApiCredentialsBuilder().
		WithAk(cfg.AppKey).
		WithSk(cfg.AppSecret).
		Build()

	builder := hwsms.SMSApiClientBuilder().WithCredential(creds)
	if cfg.Endpoint != "" {
		// Endpoint takes precedence over region; if both are empty SafeBuild
		// succeeds but the resulting client has no endpoint, so every send
		// fails at request time. Callers must set at least one.
		builder = builder.WithEndpoint(cfg.Endpoint)
	} else if cfg.Region != "" {
		r, err := region.SafeValueOf(cfg.Region)
		if err != nil {
			return nil, fmt.Errorf("huawei: invalid region %q: %w", cfg.Region, err)
		}
		builder = builder.WithRegion(r)
	}

	httpClient, err := builder.SafeBuild()
	if err != nil {
		return nil, fmt.Errorf("huawei: build http client: %w", err)
	}

	client := hwsms.NewSMSApiClient(httpClient)
	return &HuaweiProvider{
		account: account,
		config:  cfg,
		client:  &huaweiSdkClient{client: client},
	}, nil
}

// huaweiSdkClient adapts *hwsms.SMSApiClient. Sign is set per-request by
// Provider.Send from msg.SignName (as Signature in the request body).
type huaweiSdkClient struct {
	client *hwsms.SMSApiClient
}

func (c *huaweiSdkClient) BatchSendSms(req *model.BatchSendSmsRequest) (*model.BatchSendSmsResponse, error) {
	return c.client.BatchSendSms(req)
}

// newHuaweiProviderWithClient creates a HuaweiProvider with a mock client (for testing).
func newHuaweiProviderWithClient(account string, cfg *HuaweiConfig, sender huaweiSmsSender) *HuaweiProvider {
	return &HuaweiProvider{account: account, config: cfg, client: sender}
}

// Vendor identifies this provider as HUAWEI in the proto enum.
func (*HuaweiProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_HUAWEI }

// Account returns the account name this provider was constructed with.
func (p *HuaweiProvider) Account() string { return p.account }

// Send sends a domestic SMS via the Huawei Cloud smsapi/v1 BatchSendSms API.
//
// Context is accepted for interface conformance but not passed through — the
// underlying SDK does not support context (see huaweiSmsSender doc).
//
// TemplateParas is a single JSON-encoded string of a positional string array,
// e.g. '["123456","alice"]', mapped to ${1}, ${2}, ... placeholders in the
// template body. message-service carries params as map[string]string (designed
// for Aliyun's named-param style); we sort keys alphabetically to derive a
// deterministic positional mapping before marshalling.
//
// Success indicator: response Code == "000000" (Huawei MSGSMS API convention).
// Any other Code is a business error and is surfaced with Code+Description.
func (p *HuaweiProvider) Send(_ context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("huawei sms: phone number is empty")
	}
	if msg.TemplateID == "" {
		return fmt.Errorf("huawei sms: template is required")
	}
	if msg.SignName == "" {
		return fmt.Errorf("huawei sms: sign name is required")
	}

	body := &model.BatchSendSmsRequestBody{
		To:         def.NewMultiPart(msg.To),
		TemplateId: def.NewMultiPart(msg.TemplateID),
		Signature:  def.NewMultiPart(msg.SignName),
	}
	if msg.TemplateParams != nil {
		// Huawei expects positional JSON array of values, e.g. ["123456","alice"],
		// mapped to ${1}, ${2}, ... in the template body. message-service carries
		// params as map[string]string (designed for Aliyun's named-param style).
		// Sort keys alphabetically to give a deterministic positional mapping.
		keys := make([]string, 0, len(msg.TemplateParams))
		for k := range msg.TemplateParams {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		values := make([]string, 0, len(msg.TemplateParams))
		for _, k := range keys {
			values = append(values, msg.TemplateParams[k])
		}
		paramBytes, err := jsonx.Marshal(values)
		if err != nil {
			return fmt.Errorf("huawei sms: marshal template params: %w", err)
		}
		body.TemplateParas = def.NewMultiPart(string(paramBytes))
	}

	resp, err := p.client.BatchSendSms(&model.BatchSendSmsRequest{Body: body})
	if err != nil {
		return fmt.Errorf("huawei sms send: %w", err)
	}

	// Business error: per Huawei MSGSMS API convention, Code "000000" = success.
	// Any other code (e.g. E000020, E000027 frequency limit) is a failure.
	code := ""
	if resp.Code != nil {
		code = *resp.Code
	}
	if code != "000000" {
		desc := ""
		if resp.Description != nil {
			desc = *resp.Description
		}
		return fmt.Errorf("huawei sms: code=%s, description=%s", code, desc)
	}
	return nil
}

// SendInternational is not supported — Huawei MSGSMS is a China-only SMS
// vendor. The router will fall back to other international vendors.
func (*HuaweiProvider) SendInternational(_ context.Context, _ *InternationalMessage) error {
	return fmt.Errorf("huawei sms: international send not supported (domestic-only vendor)")
}
