package sms

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// trackProvider records whether Send / SendInternational was called.
type trackProvider struct {
	vendor     pb.SmsVendor
	account    string
	err        error
	called     bool
	intlCalled bool
}

func (p *trackProvider) Vendor() pb.SmsVendor { return p.vendor }
func (p *trackProvider) Account() string      { return p.account }
func (p *trackProvider) Send(_ context.Context, _ *Message) error {
	p.called = true
	return p.err
}
func (p *trackProvider) SendInternational(_ context.Context, _ *InternationalMessage) error {
	p.intlCalled = true
	return p.err
}

func TestRouter_Send_chinaNumber(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, cn.called)
	require.False(t, def.called)
}

func TestRouter_Send_chinaNumberWithoutPlus(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
	)

	result, err := router.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, cn.called)
	require.False(t, def.called)
}

func TestRouter_Send_internationalNumber(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+819012345678", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.False(t, cn.called)
	require.True(t, def.called)
}

func TestRouter_Send_multipleCountries(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	hk := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "hk"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{cn}},
		Route{Country: "HK", Targets: []AccountProvider{hk}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+85291234567", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.True(t, hk.called)
	require.False(t, cn.called)
	require.False(t, def.called)
}

func TestRouter_Send_fallbackWithinCountry(t *testing.T) {
	p1 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: fmt.Errorf("timeout")}
	p2 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "secondary"}
	def := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}

	router := NewRouter("CN", []AccountProvider{def},
		Route{Country: "CN", Targets: []AccountProvider{p1, p2}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "secondary", result.Account)
	require.Equal(t, 2, result.Attempts)
	require.True(t, p1.called)
	require.True(t, p2.called)
	require.False(t, def.called)
}

func TestRouter_Send_allProvidersFail(t *testing.T) {
	p1 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "primary", err: fmt.Errorf("timeout")}
	p2 := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "secondary", err: fmt.Errorf("rate limit")}

	router := NewRouter("CN", nil,
		Route{Country: "CN", Targets: []AccountProvider{p1, p2}},
	)

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "all targets failed")
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "secondary", result.Account)
	require.Equal(t, 2, result.Attempts)
}

func TestRouter_Send_emptyPhone(t *testing.T) {
	router := NewRouter("CN", []AccountProvider{
		&trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})
	result, err := router.Send(context.Background(), &Message{To: ""})
	require.EqualError(t, err, "sms: phone number is empty")
	require.Nil(t, result, "validation errors return nil result")
}

func TestRouter_Send_invalidPhone(t *testing.T) {
	router := NewRouter("CN", []AccountProvider{
		&trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})
	result, err := router.Send(context.Background(), &Message{To: "not-a-number", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid phone number")
	require.Nil(t, result, "validation errors return nil result")
}

func TestRouter_Send_noProvider(t *testing.T) {
	router := NewRouter("CN", nil)
	result, err := router.Send(context.Background(), &Message{To: "+819012345678", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no route target for")
	require.Nil(t, result, "validation errors return nil result")
}

func TestRouter_Send_cancelledContext(t *testing.T) {
	router := NewRouter("CN", []AccountProvider{
		&trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := router.Send(ctx, &Message{To: "+8613800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.Error(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
}

func TestRouter_Send_recordsAccount(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "cn"}
	router := NewRouter("CN", nil, Route{Country: "CN", Targets: []AccountProvider{cn}})

	result, err := router.Send(context.Background(), &Message{To: "+8613800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.Equal(t, "cn", result.Account)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
}
