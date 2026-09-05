package sms

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	tcsms "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/sms/v20190711"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// tencentMockSender is a mock implementation of tencentSmsSender.
type tencentMockSender struct {
	resp *tcsms.SendSmsResponse
	err  error
}

func (m *tencentMockSender) SendSmsWithContext(_ context.Context, _ *tcsms.SendSmsRequest) (*tcsms.SendSmsResponse, error) {
	return m.resp, m.err
}

// tencentOkResponse builds a SendSmsResponse with a successful per-phone
// status (Code == "Ok"). resp.Response is *SendSmsResponseParams, a concrete
// exported struct, so we can construct it directly.
func tencentOkResponse() *tcsms.SendSmsResponse {
	return &tcsms.SendSmsResponse{
		Response: &tcsms.SendSmsResponseParams{
			SendStatusSet: []*tcsms.SendStatus{
				{
					Code:    common.StringPtr("Ok"),
					Message: common.StringPtr("send success"),
				},
			},
			RequestId: common.StringPtr("req-1"),
		},
	}
}

// tencentErrResponse builds a SendSmsResponse carrying a per-phone business
// error (e.g. LimitExceeded.Sms frequency control).
func tencentErrResponse(code, msg string) *tcsms.SendSmsResponse {
	return &tcsms.SendSmsResponse{
		Response: &tcsms.SendSmsResponseParams{
			SendStatusSet: []*tcsms.SendStatus{
				{
					Code:    common.StringPtr(code),
					Message: common.StringPtr(msg),
				},
			},
			RequestId: common.StringPtr("req-2"),
		},
	}
}

func TestTencentProvider_Vendor(t *testing.T) {
	p, err := NewTencentProvider("primary", &TencentConfig{
		SecretID:    "x",
		SecretKey:   "y",
		SmsSdkAppID: "1400000000",
		Region:      "ap-guangzhou",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_TENCENT, p.Vendor())
}

func TestTencentProvider_Account(t *testing.T) {
	p, err := NewTencentProvider("secondary", &TencentConfig{
		SecretID:    "x",
		SecretKey:   "y",
		SmsSdkAppID: "1400000000",
		Region:      "ap-guangzhou",
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", p.Account())
}

func TestTencentProvider_Send_success(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{resp: tencentOkResponse()})

	err := p.Send(context.Background(), &Message{
		To:         "13800138000",
		SignName:   "App",
		TemplateID: "SMS_123",
	})
	require.NoError(t, err)
}

func TestTencentProvider_Send_withParams(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{resp: tencentOkResponse()})

	err := p.Send(context.Background(), &Message{
		To:             "13800138000",
		SignName:       "App",
		TemplateID:     "SMS_123",
		TemplateParams: map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}

func TestTencentProvider_Send_emptyPhone(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{resp: tencentOkResponse()})

	err := p.Send(context.Background(), &Message{To: "", TemplateID: "SMS_123", SignName: "App"})
	require.EqualError(t, err, "tencent sms: phone number is empty")
}

func TestTencentProvider_Send_noTemplate(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{resp: tencentOkResponse()})

	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "App"})
	require.EqualError(t, err, "tencent sms: template is required")
}

func TestTencentProvider_Send_noSignName(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{resp: tencentOkResponse()})

	err := p.Send(context.Background(), &Message{To: "13800138000", TemplateID: "SMS_123"})
	require.EqualError(t, err, "tencent sms: sign name is required")
}

func TestTencentProvider_Send_sdkError(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{
		err: fmt.Errorf("network timeout"),
	})

	err := p.Send(context.Background(), &Message{
		To:         "13800138000",
		SignName:   "App",
		TemplateID: "SMS_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "tencent sms send")
	require.Contains(t, err.Error(), "network timeout")
}

func TestTencentProvider_Send_businessError(t *testing.T) {
	p := newTencentProviderWithClient("error-account", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{
		resp: tencentErrResponse("LimitExceeded.Sms", "frequency limit"),
	})

	err := p.Send(context.Background(), &Message{
		To:         "13800138000",
		SignName:   "App",
		TemplateID: "SMS_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "LimitExceeded.Sms")
	require.Contains(t, err.Error(), "frequency limit")
}

func TestTencentProvider_SendInternational_contentNotSupported(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{resp: tencentOkResponse()})
	err := p.SendInternational(context.Background(), &InternationalMessage{
		To: "15551234567", SignName: "App", Content: "Your code is 123456",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "template_id is required for international send")
}

func TestTencentProvider_SendInternational_withTemplate(t *testing.T) {
	p := newTencentProviderWithClient("primary", &TencentConfig{SmsSdkAppID: "1400"}, &tencentMockSender{resp: tencentOkResponse()})
	err := p.SendInternational(context.Background(), &InternationalMessage{
		To:             "15551234567",
		SignName:       "App",
		TemplateID:     "SMS_INTL_123",
		TemplateParams: map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}
