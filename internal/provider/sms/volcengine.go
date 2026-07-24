package sms

import (
	"context"
	"fmt"

	"github.com/servekit/go-common/jsonx"
	vocsms "github.com/volcengine/volc-sdk-golang/service/sms"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// VolcengineConfig holds the configuration for the Volcengine SMS provider.
type VolcengineConfig struct {
	AccessKID  string // 火山引擎账号 AccessKey ID
	SecretKey  string
	SmsAccount string // 火山引擎短信平台"账号" (SmsAccount)
	Region     string `default:"cn-north-1"`
}

// VolcengineProvider sends SMS via the Volcengine (domestic) SDK.
type VolcengineProvider struct {
	account string
	config  *VolcengineConfig
	client  volcengineSmsSender
}

// volcengineSmsSender abstracts the SDK client for testability.
//
// Note: volc-sdk-golang's SMS.Send does not accept a context.Context (issue
// #30 in volcengine/volc-sdk-golang). The interface matches the SDK shape;
// context cancellation will not short-circuit in-flight requests for this
// vendor. Tracked as a known limitation.
type volcengineSmsSender interface {
	Send(req *vocsms.SmsRequest) (*vocsms.SmsResponse, int, error)
}

// NewVolcengineProvider creates a Volcengine SMS provider.
//
// The account parameter carries the account identity so the registry does
// not need to wrap the provider. SmsAccount is a per-vendor constant baked
// into the wrapper client; Sign is per-message (msg.SignName) and
// PhoneNumber/TemplateID/TemplateParam are per-message.
func NewVolcengineProvider(account string, cfg *VolcengineConfig) (*VolcengineProvider, error) {
	instance := vocsms.NewInstance()
	instance.SetRegion(cfg.Region)
	instance.Client.SetAccessKey(cfg.AccessKID)
	instance.Client.SetSecretKey(cfg.SecretKey)

	return &VolcengineProvider{
		account: account,
		config:  cfg,
		client:  &volcengineSdkClient{instance: instance, account: cfg.SmsAccount},
	}, nil
}

// volcengineSdkClient adapts *vocsms.SMS to the volcengineSmsSender interface.
// SmsAccount is baked in at construction (per-vendor constant); Sign is set
// per-request by the Provider.Send method from msg.SignName.
type volcengineSdkClient struct {
	instance *vocsms.SMS
	account  string
}

func (c *volcengineSdkClient) Send(req *vocsms.SmsRequest) (*vocsms.SmsResponse, int, error) {
	req.SmsAccount = c.account
	return c.instance.Send(req)
}

// newVolcengineProviderWithClient creates a VolcengineProvider with a mock client (for testing).
func newVolcengineProviderWithClient(account string, cfg *VolcengineConfig, sender volcengineSmsSender) *VolcengineProvider {
	return &VolcengineProvider{account: account, config: cfg, client: sender}
}

// Vendor identifies this provider as VOLCENGINE in the proto enum.
func (*VolcengineProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_VOLCENGINE }

// Account returns the account name this provider was constructed with.
func (p *VolcengineProvider) Account() string { return p.account }

// Send sends a domestic SMS via the Volcengine SDK.
//
// Context is accepted for interface conformance but not passed through — the
// underlying SDK does not support context (see volcengineSmsSender doc).
//
// TemplateParam is a single JSON string (not a map). message-service carries
// params as map[string]string; we JSON-marshal it the same way the Aliyun and
// Tencent providers do for cross-vendor parity. The Volcengine console expects
// the template params JSON to match the variable names declared in the
// approved template body (e.g. {"code":"123456"}).
func (p *VolcengineProvider) Send(_ context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("volcengine sms: phone number is empty")
	}
	if msg.TemplateID == "" {
		return fmt.Errorf("volcengine sms: template is required")
	}
	if msg.SignName == "" {
		return fmt.Errorf("volcengine sms: sign name is required")
	}

	req := &vocsms.SmsRequest{
		PhoneNumbers: msg.To,
		TemplateID:   msg.TemplateID,
		Sign:         msg.SignName,
	}
	if msg.TemplateParams != nil {
		paramBytes, err := jsonx.Marshal(msg.TemplateParams)
		if err != nil {
			return fmt.Errorf("volcengine sms: marshal template params: %w", err)
		}
		req.TemplateParam = string(paramBytes)
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

// SendInternational is not supported — Volcengine is a China-only SMS vendor.
// The router will fall back to other international vendors.
func (*VolcengineProvider) SendInternational(_ context.Context, _ *InternationalMessage) error {
	return fmt.Errorf("volcengine sms: international send not supported (domestic-only vendor)")
}
