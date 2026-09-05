package sms

import (
	"context"
	"testing"

	"github.com/byteplus-sdk/byteplus-sdk-golang/base"
	bpsms "github.com/byteplus-sdk/byteplus-sdk-golang/service/sms"
	"github.com/stretchr/testify/require"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// byteplusMockSender is a mock implementation of byteplusSmsSender.
// After the domestic/international split, Byteplus implements neither path
// against the live SDK (both stubs return not-supported); the mock remains
// for Vendor/Account tests and future use when raw-content wiring lands.
type byteplusMockSender struct {
	resp       *bpsms.SmsResponse
	statusCode int
	err        error
}

func (m *byteplusMockSender) Send(_ *bpsms.SmsRequest) (*bpsms.SmsResponse, int, error) {
	return m.resp, m.statusCode, m.err
}

// byteplusOkResponse builds a SmsResponse with no ResponseMetadata.Error
// (the SDK's success indicator).
func byteplusOkResponse() *bpsms.SmsResponse {
	return &bpsms.SmsResponse{
		ResponseMetadata: base.ResponseMetadata{},
		Result:           &bpsms.SmsResult{MessageID: []string{"msg-1"}},
	}
}

func TestByteplusProvider_Vendor(t *testing.T) {
	p, err := NewByteplusProvider("primary", &ByteplusConfig{
		AccessKID: "x", SecretKey: "y", SmsAccount: "acc",
	})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_BYTEPLUS, p.Vendor())
}

func TestByteplusProvider_Account(t *testing.T) {
	p, err := NewByteplusProvider("secondary", &ByteplusConfig{
		AccessKID: "x", SecretKey: "y", SmsAccount: "acc",
	})
	require.NoError(t, err)
	require.Equal(t, "secondary", p.Account())
}

// Byteplus is an international-only vendor: Send (domestic) is intentionally
// not implemented. The router only routes to Byteplus for non-CN destinations
// (via SendInternational).
func TestByteplusProvider_Send_notSupported(t *testing.T) {
	p := newByteplusProviderWithClient("primary", &ByteplusConfig{SmsAccount: "acc"}, &byteplusMockSender{resp: byteplusOkResponse()})
	err := p.Send(context.Background(), &Message{
		To: "13800138000", SignName: "sign", TemplateID: "SMS_123",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "domestic send not supported")
}

// BytePlus's SDK is template-based, so SendInternational requires TemplateID;
// Content (raw) is rejected with a clear error so the caller knows to switch
// to a raw-content vendor or pre-register a template.
func TestByteplusProvider_SendInternational_contentNotSupported(t *testing.T) {
	p := newByteplusProviderWithClient("primary", &ByteplusConfig{SmsAccount: "acc"}, &byteplusMockSender{resp: byteplusOkResponse()})
	err := p.SendInternational(context.Background(), &InternationalMessage{
		To: "15551234567", SignName: "sign", Content: "Your code is 123456",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "template_id is required")
}

func TestByteplusProvider_SendInternational_withTemplate(t *testing.T) {
	p := newByteplusProviderWithClient("primary", &ByteplusConfig{SmsAccount: "acc"}, &byteplusMockSender{resp: byteplusOkResponse()})
	err := p.SendInternational(context.Background(), &InternationalMessage{
		To:             "15551234567",
		SignName:       "sign",
		TemplateID:     "SMS_INTL_123",
		TemplateParams: map[string]string{"code": "123456"},
	})
	require.NoError(t, err)
}
