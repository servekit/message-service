package sms

import (
	"context"
	"fmt"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	client "github.com/alibabacloud-go/dysmsapi-20170525/v5/client"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/servekit/go-common/jsonx"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// AliyunConfig holds the configuration for the Aliyun SMS provider.
type AliyunConfig struct {
	AccessKeyID     string
	AccessKeySecret string
	RegionID        string `default:"cn-hangzhou"`
}

// AliyunProvider sends SMS via the Aliyun SDK. Implements AccountProvider
// once Task 5 turns AccountProvider into an interface.
type AliyunProvider struct {
	account   string
	config    *AliyunConfig
	smsClient aliyunSmsSender
}

// aliyunSmsSender abstracts the Aliyun SMS SDK client for testability.
type aliyunSmsSender interface {
	SendSmsWithContext(ctx context.Context, req *client.SendSmsRequest, runtime *dara.RuntimeOptions) (*client.SendSmsResponse, error)
}

// NewAliyunProvider creates an Aliyun SMS provider.
//
// The account parameter carries the account identity so the registry does
// not need to wrap the provider.
func NewAliyunProvider(account string, config *AliyunConfig) (*AliyunProvider, error) {
	smsClient, err := client.NewClient(&openapi.Config{
		AccessKeyId:     dara.String(config.AccessKeyID),
		AccessKeySecret: dara.String(config.AccessKeySecret),
		RegionId:        dara.String(config.RegionID),
	})
	if err != nil {
		return nil, fmt.Errorf("create aliyun sms client: %w", err)
	}

	return &AliyunProvider{
		account:   account,
		config:    config,
		smsClient: smsClient,
	}, nil
}

// newAliyunProviderWithClient creates an AliyunProvider with a mock client (for testing).
func newAliyunProviderWithClient(account string, config *AliyunConfig, sender aliyunSmsSender) *AliyunProvider {
	return &AliyunProvider{account: account, config: config, smsClient: sender}
}

// Vendor identifies this provider as ALIYUN in the proto enum.
func (*AliyunProvider) Vendor() pb.SmsVendor { return pb.SmsVendor_SMS_VENDOR_ALIYUN }

// Account returns the account name this provider was constructed with.
func (p *AliyunProvider) Account() string { return p.account }

// Send sends a domestic SMS via the Aliyun SendSms API (template-required).
func (p *AliyunProvider) Send(ctx context.Context, msg *Message) error {
	if msg.To == "" {
		return fmt.Errorf("aliyun sms: phone number is empty")
	}

	if msg.TemplateID == "" {
		return fmt.Errorf("aliyun sms: template is required")
	}
	if msg.SignName == "" {
		return fmt.Errorf("aliyun sms: sign name is required")
	}

	req := &client.SendSmsRequest{
		PhoneNumbers: dara.String(msg.To),
		SignName:     dara.String(msg.SignName),
		TemplateCode: dara.String(msg.TemplateID),
	}

	if msg.TemplateParams != nil {
		templateParam, err := jsonx.Marshal(msg.TemplateParams)
		if err != nil {
			return fmt.Errorf("marshal template params: %w", err)
		}
		req.TemplateParam = dara.String(string(templateParam))
	}

	resp, err := p.smsClient.SendSmsWithContext(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("aliyun sms send: %w", err)
	}

	code := dara.StringValue(resp.Body.Code)
	if code != "OK" {
		return fmt.Errorf("aliyun sms: code=%s, message=%s",
			code, dara.StringValue(resp.Body.Message))
	}

	return nil
}

// SendInternational sends an international SMS via the same Aliyun SendSms
// API. Mainland Aliyun accounts route international SMS through SendSms
// itself — the phone number's country code triggers international routing,
// and the template must be pre-registered for international use in the
// console. No separate SDK or endpoint is required.
//
// Template-based only: Aliyun's SendSms always requires a template. The
// Content (raw text) path is not supported — callers needing raw-content
// international delivery must use a vendor that exposes a raw-content API
// (e.g. Aliyun international account via dysmsapiintl, Twilio, AWS SNS).
func (p *AliyunProvider) SendInternational(ctx context.Context, msg *InternationalMessage) error {
	if msg.To == "" {
		return fmt.Errorf("aliyun sms: phone number is empty")
	}
	if msg.TemplateID == "" {
		return fmt.Errorf("aliyun sms: template_id is required for international send (Content path not supported)")
	}

	req := &client.SendSmsRequest{
		PhoneNumbers: dara.String(msg.To),
		TemplateCode: dara.String(msg.TemplateID),
	}
	// SignName is optional for international SMS — some region/format
	// combinations don't accept it. Forward only if the caller set it.
	if msg.SignName != "" {
		req.SignName = dara.String(msg.SignName)
	}

	if msg.TemplateParams != nil {
		templateParam, err := jsonx.Marshal(msg.TemplateParams)
		if err != nil {
			return fmt.Errorf("marshal template params: %w", err)
		}
		req.TemplateParam = dara.String(string(templateParam))
	}

	resp, err := p.smsClient.SendSmsWithContext(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("aliyun sms intl send: %w", err)
	}

	code := dara.StringValue(resp.Body.Code)
	if code != "OK" {
		return fmt.Errorf("aliyun sms intl: code=%s, message=%s",
			code, dara.StringValue(resp.Body.Message))
	}
	return nil
}
