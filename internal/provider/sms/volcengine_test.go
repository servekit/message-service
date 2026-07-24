package sms

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/volcengine/volc-sdk-golang/base"
	vocsms "github.com/volcengine/volc-sdk-golang/service/sms"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// volcengineMockSender is a mock implementation of volcengineSmsSender.
type volcengineMockSender struct {
	resp       *vocsms.SmsResponse
	statusCode int
	err        error
}

func (m *volcengineMockSender) Send(_ *vocsms.SmsRequest) (*vocsms.SmsResponse, int, error) {
	return m.resp, m.statusCode, m.err
}

// volcengineOkResponse builds a SmsResponse with no ResponseMetadata.Error
// (the SDK's success indicator). Result.MessageID echoes the SDK's success
// shape.
func volcengineOkResponse() *vocsms.SmsResponse {
	return &vocsms.SmsResponse{
		ResponseMetadata: base.ResponseMetadata{},
		Result:           &vocsms.SmsResult{MessageID: []string{"msg-1"}},
	}
}

// volcengineErrResponse builds a SmsResponse carrying a business error in
// ResponseMetadata.Error (e.g. LimitExceeded frequency control).
func volcengineErrResponse(code, msg string) *vocsms.SmsResponse {
	return &vocsms.SmsResponse{
		ResponseMetadata: base.ResponseMetadata{
			Error: &base.ErrorObj{
				Code:    code,
				Message: msg,
			},
		},
	}
}

func TestVolcengineProvider_Vendor(t *testing.T) {
	p, err := NewVolcengineProvider("primary", &VolcengineConfig{
		AccessKID: "x", SecretKey: "y", SmsAccount: "acc",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_VOLCENGINE, p.Vendor())
}

func TestVolcengineProvider_Account(t *testing.T) {
	p, err := NewVolcengineProvider("secondary", &VolcengineConfig{
		AccessKID: "x", SecretKey: "y", SmsAccount: "acc",
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", p.Account())
}

func TestVolcengineProvider_Send_success(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
}

func TestVolcengineProvider_Send_withParams(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{
		To: "13800138000", SignName: "sign", TemplateID: "SMS_123",
		TemplateParams: map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}

func TestVolcengineProvider_Send_emptyPhone(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "", TemplateID: "SMS_123", SignName: "sign"})
	require.EqualError(t, err, "volcengine sms: phone number is empty")
}

func TestVolcengineProvider_Send_noTemplate(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign"})
	require.EqualError(t, err, "volcengine sms: template is required")
}

func TestVolcengineProvider_Send_noSignName(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.Send(context.Background(), &Message{To: "13800138000", TemplateID: "SMS_123"})
	require.EqualError(t, err, "volcengine sms: sign name is required")
}

func TestVolcengineProvider_Send_sdkError(t *testing.T) {
	p := newVolcengineProviderWithClient("error-account", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{err: fmt.Errorf("network timeout")})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "volcengine sms send")
	require.Contains(t, err.Error(), "network timeout")
}

func TestVolcengineProvider_Send_businessError(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{
		resp: volcengineErrResponse("LimitExceeded", "frequency limit"),
	})
	err := p.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "LimitExceeded")
	require.Contains(t, err.Error(), "frequency limit")
}

func TestVolcengineProvider_SendInternational_notSupported(t *testing.T) {
	p := newVolcengineProviderWithClient("primary", &VolcengineConfig{SmsAccount: "acc"}, &volcengineMockSender{resp: volcengineOkResponse()})
	err := p.SendInternational(context.Background(), &InternationalMessage{
		To: "15551234567", SignName: "sign", Content: "Your code is 123456",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "international send not supported")
}
