package sms

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	pb "github.com/servekit/message-service/gen/message/v1"
)

func TestBuildRouter_emptyConfig(t *testing.T) {
	reg := NewAccountRegistryFromProviders(nil)
	r, err := BuildRouter(nil, reg)
	require.NoError(t, err)
	require.Nil(t, r, "empty routes → nil router (caller decides if that's error)")
}

func TestBuildRouter_emptyRoutes(t *testing.T) {
	reg := NewAccountRegistryFromProviders(nil)
	r, err := BuildRouter(&Config{}, reg)
	require.NoError(t, err)
	require.Nil(t, r)
}

func TestBuildRouter_validRoutes(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	reg := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"default": cn},
	})
	cfg := &Config{
		DefaultCountry: "CN",
		Routes: []*RouteConfig{
			{Country: "CN", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "default"}}},
			{Country: "*", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "default"}}},
		},
	}

	r, err := BuildRouter(cfg, reg)
	require.NoError(t, err)
	require.NotNil(t, r)

	result, err := r.Send(context.Background(), &Message{To: "+8613800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, "default", result.Account)
}

func TestBuildRouter_unknownAccount(t *testing.T) {
	reg := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"default": &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}},
	})
	cfg := &Config{
		Routes: []*RouteConfig{
			{Country: "CN", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "primary"}}},
		},
	}

	_, err := BuildRouter(cfg, reg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms account")
}

func TestBuildRouter_defaultCountryFallback(t *testing.T) {
	cn := &trackProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: "default"}
	reg := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"default": cn},
	})
	cfg := &Config{
		Routes: []*RouteConfig{
			{Country: "CN", Targets: []*RouteTarget{{Vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, Account: "default"}}},
		},
	}

	r, err := BuildRouter(cfg, reg)
	require.NoError(t, err)
	require.NotNil(t, r)
	result, err := r.Send(context.Background(), &Message{To: "13800138000", SignName: "sign", TemplateID: "SMS_123"})
	require.NoError(t, err)
	require.True(t, result.Success)
}
