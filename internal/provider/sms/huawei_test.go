package sms

import (
	"context"
	"fmt"
	"testing"

	"github.com/huaweicloud/huaweicloud-sdk-go-v3/services/smsapi/v1/model"
	"github.com/stretchr/testify/require"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// huaweiMockSender is a mock implementation of huaweiSmsSender.
type huaweiMockSender struct {
	resp   *model.BatchSendSmsResponse
	err    error
	called bool
	req    *model.BatchSendSmsRequest
}

func (m *huaweiMockSender) BatchSendSms(req *model.BatchSendSmsRequest) (*model.BatchSendSmsResponse, error) {
	m.called = true
	m.req = req
	return m.resp, m.err
}

// huaweiOkResponse builds a BatchSendSmsResponse carrying Code="000000"
// (Huawei MSGSMS API's success indicator).
func huaweiOkResponse() *model.BatchSendSmsResponse {
	code := "000000"
	desc := "Success"
	return &model.BatchSendSmsResponse{
		Code:        &code,
		Description: &desc,
	}
}

// huaweiErrResponse builds a BatchSendSmsResponse carrying a business error
// (e.g. E000027 frequency limit). The HTTP status is typically 200 even on
// business errors — the error lives in the response body's Code/Description.
func huaweiErrResponse(code, desc string) *model.BatchSendSmsResponse {
	return &model.BatchSendSmsResponse{
		Code:        &code,
		Description: &desc,
	}
}

func TestHuaweiProvider_Vendor(t *testing.T) {
	p, err := NewHuaweiProvider("primary", &HuaweiConfig{
		AppKey: "k", AppSecret: "s", Region: "cn-north-4",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_HUAWEI, p.Vendor())
}

func TestHuaweiProvider_Account(t *testing.T) {
	p, err := NewHuaweiProvider("secondary", &HuaweiConfig{
		AppKey: "k", AppSecret: "s", Region: "cn-north-4",
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", p.Account())
}

func TestHuaweiProvider_Send_success(t *testing.T) {
	p := newHuaweiProviderWithClient("primary", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, &huaweiMockSender{resp: huaweiOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
}

func TestHuaweiProvider_Send_withParams(t *testing.T) {
	sender := &huaweiMockSender{resp: huaweiOkResponse()}
	p := newHuaweiProviderWithClient("primary", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, sender)
	err := p.Send(context.Background(), &Message{
		To: "13800138000", SignName: "sign", TemplateID: "SMS_123",
		TemplateParams: map[string]string{"code": "123456"},
	})
	require.NoError(t, err)

	// Huawei MSGSMS requires TemplateParas to be a positional JSON array of
	// values (e.g. ["123456"]) mapped to ${1}, ${2}, ... in the template body —
	// NOT a JSON object. Sending the wrong shape causes "templateParas does not
	// match the template" at the API. Guard against regression.
	require.True(t, sender.called, "expected BatchSendSms to be called")
	require.NotNil(t, sender.req.Body, "expected request body to be set")
	require.NotNil(t, sender.req.Body.TemplateParas, "expected TemplateParas to be set")
	require.Equal(t, `["123456"]`, sender.req.Body.TemplateParas.Content,
		"TemplateParas must be a positional JSON array of values, not a JSON object")
}

func TestHuaweiProvider_Send_emptyPhone(t *testing.T) {
	p := newHuaweiProviderWithClient("primary", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, &huaweiMockSender{resp: huaweiOkResponse()})
	err := p.Send(context.Background(), &Message{To: "", TemplateID: "SMS_123", SignName: "sign"})
	require.EqualError(t, err, "huawei sms: phone number is empty")
}

func TestHuaweiProvider_Send_noTemplate(t *testing.T) {
	p := newHuaweiProviderWithClient("primary", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, &huaweiMockSender{resp: huaweiOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign"})
	require.EqualError(t, err, "huawei sms: template is required")
}

func TestHuaweiProvider_Send_noSignName(t *testing.T) {
	p := newHuaweiProviderWithClient("primary", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, &huaweiMockSender{resp: huaweiOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", TemplateID: "SMS_123"})
	require.EqualError(t, err, "huawei sms: sign name is required")
}

func TestHuaweiProvider_Send_sdkError(t *testing.T) {
	p := newHuaweiProviderWithClient("error-account", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, &huaweiMockSender{err: fmt.Errorf("network timeout")})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "huawei sms send")
	require.Contains(t, err.Error(), "network timeout")
}

func TestHuaweiProvider_Send_businessError(t *testing.T) {
	p := newHuaweiProviderWithClient("primary", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, &huaweiMockSender{
		resp: huaweiErrResponse("E000027", "frequency limit"),
	})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "E000027")
	require.Contains(t, err.Error(), "frequency limit")
}

func TestHuaweiProvider_SendInternational_notSupported(t *testing.T) {
	p := newHuaweiProviderWithClient("primary", &HuaweiConfig{AppKey: "k", AppSecret: "s", Region: "cn-north-4"}, &huaweiMockSender{resp: huaweiOkResponse()})
	err := p.SendInternational(context.Background(), &InternationalMessage{
		To: "15551234567", SignName: "sign", Content: "Your code is 123456",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "international send not supported")
}
