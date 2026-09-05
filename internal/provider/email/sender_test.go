package email

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
// calls) — sender tests need to assert which message was forwarded.
type testProvider struct {
	vendor  pb.EmailVendor
	account string
	err     error
	sent    *Message
}

func (p *testProvider) Vendor() pb.EmailVendor { return p.vendor }
func (p *testProvider) Account() string        { return p.account }
func (p *testProvider) Send(_ context.Context, msg *Message) error {
	if p.err != nil {
		return p.err
	}
	p.sent = msg
	return nil
}

func TestSender_Send_singleProvider(t *testing.T) {
	p := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "default"}
	s := NewSender([]AccountProvider{p})

	msg := &Message{To: []*Address{{Email: "user@test.com"}}, Subject: "Hi", Body: "Hello"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "default", result.Account)
	require.Equal(t, 1, result.Attempts)
	if diff := cmp.Diff(msg, p.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_fallback(t *testing.T) {
	p1 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "primary", err: errors.New("smtp down")}
	p2 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_TENCENT, account: "backup"}
	s := NewSender([]AccountProvider{p1, p2})

	msg := &Message{To: []*Address{{Email: "user@test.com"}}, Subject: "Hi", Body: "Hello"}
	result, err := s.Send(context.Background(), msg)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_TENCENT, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Nil(t, p1.sent)
	if diff := cmp.Diff(msg, p2.sent); diff != "" {
		t.Errorf("sent message mismatch (-want +got):\n%s", diff)
	}
}

func TestSender_Send_allFail(t *testing.T) {
	p1 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "primary", err: errors.New("timeout")}
	p2 := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "backup", err: errors.New("rate limited")}
	s := NewSender([]AccountProvider{p1, p2})

	result, err := s.Send(context.Background(), &Message{To: []*Address{{Email: "user@test.com"}}, Subject: "Hi", Body: "Hello"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "all providers failed")
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "backup", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.Error(t, result.Error)
}

func TestSender_Send_noProvider(t *testing.T) {
	s := NewSender(nil)
	result, err := s.Send(context.Background(), &Message{To: []*Address{{Email: "user@test.com"}}, Body: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_emptyRecipient(t *testing.T) {
	s := NewSender([]AccountProvider{
		&testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "default"},
	})
	result, err := s.Send(context.Background(), &Message{To: nil, Body: "test"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "recipient is empty")
	require.Nil(t, result, "validation errors return nil result")
}

func TestSender_Send_cancelledContext(t *testing.T) {
	p := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "default"}
	s := NewSender([]AccountProvider{p})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := s.Send(ctx, &Message{To: []*Address{{Email: "user@test.com"}}, Body: "test"})
	require.Error(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
}

func TestSender_Send_recordsVendorAndAccount(t *testing.T) {
	p := &testProvider{vendor: pb.EmailVendor_EMAIL_VENDOR_ALIYUN, account: "primary"}
	s := NewSender([]AccountProvider{p})

	result, err := s.Send(context.Background(), &Message{To: []*Address{{Email: "user@test.com"}}, Body: "Hello"})
	require.NoError(t, err)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
}
