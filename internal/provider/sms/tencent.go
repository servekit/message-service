package sms

import (
	"context"
	"fmt"

	"github.com/servekit/go-common/jsonx"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	tcsms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// TencentConfig holds the configuration for the Tencent Cloud SMS provider.
type TencentConfig struct {
	SecretID    string
	SecretKey   string
	SmsSdkAppID string // SdkAppid from the SMS console, e.g. "1400000000"
	Region      string `default:"ap-guangzhou"` // ap-guangzhou / ap-beijing for domestic; differs for international
	Endpoint    string // optional; SDK derives from region when empty
}

// TencentProvider sends SMS via the Tencent Cloud SDK. One provider handles
// both domestic and international numbers — the SDK routes by phone-number
// prefix automatically.
type TencentProvider struct {
	account string
	config  *TencentConfig
	client  tencentSmsSender
}

// tencentSmsSender abstracts the SDK client for testability.
//
// SendSmsWithContext is preferred so request cancellation short-circuits
// in-flight requests.
type tencentSmsSender interface {
	SendSmsWithContext(ctx context.Context, req *tcsms.SendSmsRequest) (*tcsms.SendSmsResponse, error)
}

// NewTencentProvider creates a Tencent Cloud SMS provider.
//
// The account parameter carries the account identity so the registry does
// not need to wrap the provider.
func NewTencentProvider(account string, cfg *TencentConfig) (*TencentProvider, error) {
	credential := common.NewCredential(cfg.SecretID, cfg.SecretKey)
	cpf := profile.NewClientProfile()
	if cfg.Endpoint != "" {
		cpf.HttpProfile.Endpoint = cfg.Endpoint
	}

	client, err := tcsms.NewClient(credential, cfg.Region, cpf)
	if err != nil {
		return nil, fmt.Errorf("create tencent sms client: %w", err)
	}

	return &TencentProvider{
		account: account,
		config:  cfg,
		client:  client,
	}, nil
}

// newTencentProviderWithClient creates a TencentProvider with a mock client (for testing).
func newTencentProviderWithClient(account string, cfg *TencentConfig, sender tencentSmsSender) *TencentProvider {
	return &TencentProvider{account: account, config: cfg, client: sender}
}

// Vendor identifies this provider as TENCENT in the proto enum.
func (*TencentProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_TENCENT }

// Account returns the account name this provider was constructed with.
func (p *TencentProvider) Account() string { return p.account }

// Send sends a domestic SMS via the Tencent Cloud SendSms API (template-required).
//
// Tencent's SendSms API supports batch sends (PhoneNumberSet is a slice), but
// message-service always sends one phone at a time — extract the first
// element of SendStatusSet for the per-message result.
//
// TemplateParamSet is a positional []*string. message-service carries params
// as a map[string]string, so we marshal the map to a single JSON string and
// pass it as the only element. Callers whose Tencent templates use positional
// placeholders (${1}, ${2}, ...) must order keys to match — this matches the
// JSON-blob convention used by the Aliyun provider for cross-vendor parity.
func (p *TencentProvider) Send(ctx context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("tencent sms: phone number is empty")
	}
	if msg.TemplateID == "" {
		return fmt.Errorf("tencent sms: template is required")
	}
	if msg.SignName == "" {
		return fmt.Errorf("tencent sms: sign name is required")
	}

	req := tcsms.NewSendSmsRequest()
	req.SmsSdkAppid = common.StringPtr(p.config.SmsSdkAppID)
	req.Sign = common.StringPtr(msg.SignName)
	req.PhoneNumberSet = common.StringPtrs([]string{msg.To})
	req.TemplateID = common.StringPtr(msg.TemplateID)

	if msg.TemplateParams != nil {
		paramBytes, err := jsonx.Marshal(msg.TemplateParams)
		if err != nil {
			return fmt.Errorf("tencent sms: marshal template params: %w", err)
		}
		req.TemplateParamSet = common.StringPtrs([]string{string(paramBytes)})
	}

	resp, err := p.client.SendSmsWithContext(ctx, req)
	if err != nil {
		return fmt.Errorf("tencent sms send: %w", err)
	}

	if resp.Response == nil || len(resp.Response.SendStatusSet) == 0 {
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

// SendInternational sends an international SMS via the same Tencent SendSms
// API. Tencent's international SMS uses a separate SdkAppid registered for
// the international product — configure a separate Tencent account in
// config with the intl SdkAppid, and route non-CN destinations to it.
//
// The SDK behaviour is identical to Send; the only difference is which
// account (and thus SdkAppid) is in play. International SMS almost always
// requires a pre-registered template — Content (raw text) is rejected.
func (p *TencentProvider) SendInternational(ctx context.Context, msg *InternationalMessage) error {
	if msg.To == "" {
		return fmt.Errorf("tencent sms: phone number is empty")
	}
	if msg.TemplateID == "" {
		return fmt.Errorf("tencent sms: template_id is required for international send (Content path not supported)")
	}
	// Sign is required by Tencent even for international — pass SignName as-is
	// (caller may set it to empty if their intl account doesn't require it,
	// and Tencent will return its own validation error in that case).
	sign := msg.SignName

	req := tcsms.NewSendSmsRequest()
	req.SmsSdkAppid = common.StringPtr(p.config.SmsSdkAppID)
	req.Sign = common.StringPtr(sign)
	req.PhoneNumberSet = common.StringPtrs([]string{msg.To})
	req.TemplateID = common.StringPtr(msg.TemplateID)

	if msg.TemplateParams != nil {
		paramBytes, err := jsonx.Marshal(msg.TemplateParams)
		if err != nil {
			return fmt.Errorf("tencent sms: marshal template params: %w", err)
		}
		req.TemplateParamSet = common.StringPtrs([]string{string(paramBytes)})
	}

	resp, err := p.client.SendSmsWithContext(ctx, req)
	if err != nil {
		return fmt.Errorf("tencent sms intl send: %w", err)
	}
	if resp.Response == nil || len(resp.Response.SendStatusSet) == 0 {
		return fmt.Errorf("tencent sms intl: empty SendStatusSet")
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
		return fmt.Errorf("tencent sms intl: code=%s, message=%s", code, message)
	}
	return nil
}
