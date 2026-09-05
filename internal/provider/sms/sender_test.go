package sms

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/require"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// testProvider is a sender-level test AccountProvider that records the last
// sent Message. Distinct from registry_test.go's fakeProvider (counts Send
// calls).
type testProvider struct {
	vendor   pb.SmsVendor
	account  string
	err      error
	sent     *Message
	intlSent *InternationalMessage
}

func (p *testProvider) Vendor() pb.SmsVendor { return p.vendor }
func (p *testProvider) Account() string      { return p.account }
func (p *testProvider) Send(_ context.Context, msg *Message) error {
	if p.err != nil {
		return p.err
	}
	p.sent = msg
	return nil
}
func (p *testProvider) SendInternational(_ context.Context, msg *InternationalMessage) error {
	if p.err != nil {
		return p.err
	}
	p.intlSent = msg
	return nil
}

func TestSender_Send_singleProvider(t *testing.T) {
	p := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	s := NewSender([]AccountProvider{p})

	msg := &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "default", result.Account)
	require.Equal(t, 1, result.Attempts)
	if diff := cmp.Diff(msg, p.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_fallback(t *testing.T) {
	// SMS proto only defines ALIYUN; re-use ALIYUN as the fallback vendor —
	// the Sender logic does not branch on vendor identity.
	p1 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: errors.New("aliyun down")}
	p2 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "backup"}
	s := NewSender([]AccountProvider{p1, p2})

	msg := &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Nil(t, p1.sent)
	if diff := cmp.Diff(msg, p2.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_allFail(t *testing.T) {
	p1 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: errors.New("timeout")}
	p2 := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "backup", err: errors.New("rate limited")}
	s := NewSender([]AccountProvider{p1, p2})

	result, err := s.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "all providers failed")
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Error(t, result.Error)
}

func TestSender_Send_noProvider(t *testing.T) {
	s := NewSender(nil)
	result, err := s.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_emptyPhone(t *testing.T) {
	s := NewSender([]AccountProvider{
		&testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})
	result, err := s.Send(context.Background(), &Message{To: ""})
	require.Error(t, err)
	require.Contains(t, err.Error(), "phone number is empty")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_cancelledContext(t *testing.T) {
	p := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	s := NewSender([]AccountProvider{p})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := s.Send(ctx, &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
}

func TestSender_Send_recordsVendorAndAccount(t *testing.T) {
	p := &testProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary"}
	s := NewSender([]AccountProvider{p})

	result, err := s.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
}
