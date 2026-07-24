package sms

import (
	"context"
	"fmt"
	"testing"

	client "github.com/alibabacloud-go/dysmsapi-20170525/v5/client"
	"github.com/alibabacloud-go/tea/dara"
	"github.com/stretchr/testify/require"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// aliyunMockSender is a mock implementation of aliyunSmsSender.
type aliyunMockSender struct {
	resp *client.SendSmsResponse
	err  error
}

func (m *aliyunMockSender) SendSmsWithContext(_ context.Context, _ *client.SendSmsRequest, _ *dara.RuntimeOptions) (*client.SendSmsResponse, error) {
	return m.resp, m.err
}

func aliyunOkResponse() *client.SendSmsResponse {
	return &client.SendSmsResponse{
		Body: &client.SendSmsResponseBody{
			Code:    dara.String("OK"),
			Message: dara.String("ok"),
		},
	}
}

func aliyunErrResponse(code, msg string) *client.SendSmsResponse {
	return &client.SendSmsResponse{
		Body: &client.SendSmsResponseBody{
			Code:    dara.String(code),
			Message: dara.String(msg),
		},
	}
}

func TestAliyunProvider_Vendor(t *testing.T) {
	p, err := NewAliyunProvider("primary", &AliyunConfig{
		AccessKeyID:     "test",
		AccessKeySecret: "test",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, p.Vendor())
}

func TestAliyunProvider_Account(t *testing.T) {
	p, err := NewAliyunProvider("secondary", &AliyunConfig{
		AccessKeyID:     "test",
		AccessKeySecret: "test",
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", p.Account())
}

func TestAliyunProvider_Send_success(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{
		To:         "13800138000",
		SignName:   "TestApp",
		TemplateID: "SMS_123",
	})
	require.NoError(t, err)
}

func TestAliyunProvider_Send_withParams(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{
		To:             "13800138000",
		SignName:       "TestApp",
		TemplateID:     "SMS_123",
		TemplateParams: map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}

func TestAliyunProvider_Send_emptyPhone(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{To: "", TemplateID: "SMS_123", SignName: "TestApp"})
	require.EqualError(t, err, "aliyun sms: phone number is empty")
}

func TestAliyunProvider_Send_noTemplate(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "TestApp"})
	require.EqualError(t, err, "aliyun sms: template is required")
}

func TestAliyunProvider_Send_noSignName(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.Send(context.Background(), &Message{To: "13800138000", TemplateID: "SMS_123"})
	require.EqualError(t, err, "aliyun sms: sign name is required")
}

func TestAliyunProvider_Send_sdkError(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{
		err: fmt.Errorf("network timeout"),
	})

	err := p.Send(context.Background(), &Message{
		To:         "13800138000",
		SignName:   "TestApp",
		TemplateID: "SMS_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "aliyun sms send")
	require.Contains(t, err.Error(), "network timeout")
}

func TestAliyunProvider_Send_businessError(t *testing.T) {
	p := newAliyunProviderWithClient("error-account", &AliyunConfig{}, &aliyunMockSender{
		resp: aliyunErrResponse("isv.BUSINESS_LIMIT_CONTROL", "frequency limit"),
	})

	err := p.Send(context.Background(), &Message{
		To:         "13800138000",
		SignName:   "TestApp",
		TemplateID: "SMS_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "BUSINESS_LIMIT_CONTROL")
	require.Contains(t, err.Error(), "frequency limit")
}

func TestAliyunProvider_SendInternational_contentNotSupported(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.SendInternational(context.Background(), &InternationalMessage{
		To:       "15551234567",
		SignName: "TestApp",
		Content:  "Your code is 123456",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "template_id is required for international send")
}

func TestAliyunProvider_SendInternational_withTemplate(t *testing.T) {
	p := newAliyunProviderWithClient("primary", &AliyunConfig{}, &aliyunMockSender{resp: aliyunOkResponse()})

	err := p.SendInternational(context.Background(), &InternationalMessage{
		To:             "15551234567",
		SignName:       "TestApp",
		TemplateID:     "SMS_INTL_123",
		TemplateParams: map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}
