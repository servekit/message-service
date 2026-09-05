package sms

import (
	"context"
	"fmt"

	bpsms "github.com/byteplus-sdk/byteplus-sdk-golang/service/sms"
	"github.com/servekit/go-common/jsonx"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// ByteplusConfig holds the configuration for the BytePlus SMS provider
// (Volcano Engine's international brand). The SDK shape mirrors Volcengine's
// (same lineage), but the import path is a separate module.
type ByteplusConfig struct {
	AccessKID  string // BytePlus account AccessKey ID
	SecretKey  string
	SmsAccount string // BytePlus SMS platform "account" (SmsAccount)
	Region     string `default:"ap-singapore-1"` // BytePlus primary region
}

// ByteplusProvider sends SMS via the BytePlus (international) SDK.
type ByteplusProvider struct {
	account string
	config  *ByteplusConfig
	client  byteplusSmsSender
}

// byteplusSmsSender abstracts the SDK client for testability.
//
// Note: byteplus-sdk-golang's SMS.Send does not accept a context.Context
// (same limitation as Volcengine — they share the SDK lineage). The interface
// matches the SDK shape; context cancellation will not short-circuit in-flight
// requests for this vendor. Tracked as a known limitation.
type byteplusSmsSender interface {
	Send(req *bpsms.SmsRequest) (*bpsms.SmsResponse, int, error)
}

// NewByteplusProvider creates a BytePlus SMS provider.
//
// The account parameter carries the account identity so the registry does
// not need to wrap the provider. SmsAccount is a per-vendor constant baked
// into the wrapper client.
func NewByteplusProvider(account string, cfg *ByteplusConfig) (*ByteplusProvider, error) {
	instance := bpsms.NewInstance()
	instance.SetRegion(cfg.Region)
	instance.Client.SetAccessKey(cfg.AccessKID)
	instance.Client.SetSecretKey(cfg.SecretKey)

	return &ByteplusProvider{
		account: account,
		config:  cfg,
		client:  &byteplusSdkClient{instance: instance, account: cfg.SmsAccount},
	}, nil
}

// byteplusSdkClient adapts *bpsms.SMS to the byteplusSmsSender interface.
type byteplusSdkClient struct {
	instance *bpsms.SMS
	account  string
}

func (c *byteplusSdkClient) Send(req *bpsms.SmsRequest) (*bpsms.SmsResponse, int, error) {
	req.SmsAccount = c.account
	return c.instance.Send(req)
}

// newByteplusProviderWithClient creates a ByteplusProvider with a mock client (for testing).
func newByteplusProviderWithClient(account string, cfg *ByteplusConfig, sender byteplusSmsSender) *ByteplusProvider {
	return &ByteplusProvider{account: account, config: cfg, client: sender}
}

// Vendor identifies this provider as BYTEPLUS in the proto enum.
func (*ByteplusProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_BYTEPLUS }

// Account returns the account name this provider was constructed with.
func (p *ByteplusProvider) Account() string { return p.account }

// Send is not supported — Byteplus is an international-only vendor and the
// domestic path must not route to it. The router picks Byteplus only for
// non-CN destinations (via SendInternational).
func (*ByteplusProvider) Send(_ context.Context, _ *Message) error {
	return fmt.Errorf("byteplus sms: domestic send not supported (international-only vendor)")
}

// SendInternational sends an international SMS via the BytePlus SDK.
//
// BytePlus's SDK exposes only a template-based SendSms API (no raw-content
// endpoint), so this adapter requires InternationalMessage.TemplateID — the
// Content field is not consulted. If the caller supplied Content instead of
// TemplateID, that is a caller/vendor mismatch: switch to a raw-content
// vendor (Aliyun SendMessageToGlobe, Twilio, ...) or pre-register a template
// with BytePlus.
//
// Context is accepted for interface conformance but not passed through — the
// underlying SDK does not support context.
//
// TemplateParam is a single JSON string (not a map). message-service carries
// params as map[string]string; we JSON-marshal it for cross-vendor parity.
func (p *ByteplusProvider) SendInternational(_ context.Context, msg *InternationalMessage) error {
	if msg.To == "" {
		return fmt.Errorf("byteplus sms: phone number is empty")
	}
	if msg.TemplateID == "" {
		return fmt.Errorf("byteplus sms: template_id is required (Content path not supported by this SDK)")
	}

	req := &bpsms.SmsRequest{
		PhoneNumbers: msg.To,
		TemplateID:   msg.TemplateID,
	}
	if msg.TemplateParams != nil {
		paramBytes, err := jsonx.Marshal(msg.TemplateParams)
		if err != nil {
			return fmt.Errorf("byteplus sms: marshal template params: %w", err)
		}
		req.TemplateParam = string(paramBytes)
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
