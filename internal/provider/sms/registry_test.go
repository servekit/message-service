package sms

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	pb "github.com/servekit/message-service/gen/message/v1"
)

// fakeProvider is a registry-level test AccountProvider that counts Send
// calls. Distinct from sender_test.go's testProvider (records last sent
// Message) and router_test.go's trackProvider (records only whether Send was
// called).
type fakeProvider struct {
	vendor        pb.SmsVendor
	account       string
	err           error
	sentCount     int
	intlSentCount int
}

func (p *fakeProvider) Vendor() pb.SmsVendor { return p.vendor }
func (p *fakeProvider) Account() string      { return p.account }
func (p *fakeProvider) Send(_ context.Context, _ *Message) error {
	p.sentCount++
	if p.err != nil {
		return p.err
	}
	return nil
}
func (p *fakeProvider) SendInternational(_ context.Context, _ *InternationalMessage) error {
	p.intlSentCount++
	if p.err != nil {
		return p.err
	}
	return nil
}

// wrap builds a fakeProvider for the given (account, err). All SMS fakes use
// ALIYUN as the vendor — SMS proto only defines ALIYUN.
func wrap(account string, err error) *fakeProvider {
	return &fakeProvider{vendor: pb.SmsVendor_SMS_VENDOR_ALIYUN, account: account, err: err}
}

func TestNewAccountRegistryFromProviders_empty(t *testing.T) {
	r := NewAccountRegistryFromProviders(nil)

	require.NotNil(t, r.DefaultSender())
	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "13800138000"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result)
}

func TestNewAccountRegistryFromProviders_singleVendorAccount(t *testing.T) {
	p := wrap("primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": p},
	})

	def := r.DefaultSender()
	require.NotNil(t, def)
	result, err := def.Send(context.Background(), &Message{To: "13800138000"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, p.Vendor(), "vendor impl should report its own vendor")
	require.Equal(t, 1, p.sentCount, "default sender should have called the only provider")
}

// TestNewAccountRegistryFromProviders_sortOrder verifies same-vendor
// multi-account ordering (SMS proto only has ALIYUN; multi-vendor sort is
// exercised in email registry tests).
//
// Map is intentionally unordered: aliyun/zzz, aliyun/aaa, aliyun/bbb
// Expected fallback chain: aliyun/aaa → aliyun/bbb → aliyun/zzz
func TestNewAccountRegistryFromProviders_sortOrder(t *testing.T) {
	alA := wrap("aaa", errors.New("alA down"))
	alB := wrap("bbb", errors.New("alB down"))
	alZ := wrap("zzz", nil) // first to succeed

	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {
			"zzz": alZ,
			"aaa": alA,
			"bbb": alB,
		},
	})

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: "13800138000"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor, "should fall through to aliyun/zzz after aaa/bbb fail")
	require.Equal(t, "zzz", result.Account)
	require.Equal(t, 3, result.Attempts, "should try aaa → bbb → zzz")

	require.Equal(t, 1, alA.sentCount)
	require.Equal(t, 1, alB.sentCount)
	require.Equal(t, 1, alZ.sentCount)
}

func TestSenderFor_bothEmpty(t *testing.T) {
	p := wrap("primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": p},
	})

	got, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_UNSPECIFIED, "")
	require.NoError(t, err)
	require.Same(t, r.DefaultSender(), got, "both empty should return DefaultSender")
}

func TestSenderFor_bothSet(t *testing.T) {
	target := wrap("primary", errors.New("target down"))
	other := wrap("backup", nil) // should not be called

	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {
			"primary": target,
			"backup":  other,
		},
	})

	got, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "primary")
	require.NoError(t, err)
	require.NotSame(t, r.DefaultSender(), got, "specific selection should NOT be the default fallback sender")

	result, err := got.Send(context.Background(), &Message{To: "13800138000"})
	require.Error(t, err)
	require.False(t, result.Success)
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, 1, result.Attempts, "no fallback — should only try the selected provider")
	require.Equal(t, 1, target.sentCount)
	require.Equal(t, 0, other.sentCount, "other provider should not have been tried")
}

func TestSenderFor_partialVendorOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_partialAccountOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_UNSPECIFIED, "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_unknownVendor(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor(99), "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms vendor")
}

func TestSenderFor_unknownAccount(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.SmsVendor]map[string]AccountProvider{
		pb.SmsVendor_SMS_VENDOR_ALIYUN: {"primary": wrap("primary", nil)},
	})

	_, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "secondary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown sms account")
}

// TestNewAccountRegistry_indexesByVendorAndAccount verifies that
// NewAccountRegistry stores vendor impls directly so the internal map carries
// (vendor, account) identity via the vendor impl's own methods.
func TestNewAccountRegistry_indexesByVendorAndAccount(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.SmsVendor]*VendorConfig{
			pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
				{Name: "primary", Aliyun: &AliyunConfig{AccessKeyID: "xxx", AccessKeySecret: "yyy", RegionID: "cn-hangzhou"}},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	ap, ok := r.vendors[pb.SmsVendor_SMS_VENDOR_ALIYUN]["primary"]
	require.True(t, ok, "vendors map should index by (vendor, account)")
	require.Equal(t, pb.SmsVendor_SMS_VENDOR_ALIYUN, ap.Vendor())
	require.Equal(t, "primary", ap.Account())
}

// TestNewAccountRegistry_allFiveVendors verifies the registry can construct
// and index all 5 SMS vendors end-to-end. Catches AccountConfig shape drift,
// missing buildProvider cases, or vendor impl construction failures.
func TestNewAccountRegistry_allFiveVendors(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.SmsVendor]*VendorConfig{
			pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
				{Name: "a", Aliyun: &AliyunConfig{AccessKeyID: "x", AccessKeySecret: "y", RegionID: "cn-hangzhou"}},
			}},
			pb.SmsVendor_SMS_VENDOR_TENCENT: {Accounts: []*AccountConfig{
				{Name: "t", Tencent: &TencentConfig{SecretID: "x", SecretKey: "y", SmsSdkAppID: "1400", Region: "ap-guangzhou"}},
			}},
			pb.SmsVendor_SMS_VENDOR_VOLCENGINE: {Accounts: []*AccountConfig{
				{Name: "v", Volcengine: &VolcengineConfig{AccessKID: "x", SecretKey: "y", SmsAccount: "a"}},
			}},
			pb.SmsVendor_SMS_VENDOR_BYTEPLUS: {Accounts: []*AccountConfig{
				{Name: "b", Byteplus: &ByteplusConfig{AccessKID: "x", SecretKey: "y", SmsAccount: "a"}},
			}},
			pb.SmsVendor_SMS_VENDOR_HUAWEI: {Accounts: []*AccountConfig{
				{Name: "h", Huawei: &HuaweiConfig{AppKey: "k", AppSecret: "s", Endpoint: "https://smsapi.cn-north-4.myhuaweicloud.com", Region: "cn-north-4"}},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	for vendor, wantAccount := range map[pb.SmsVendor]string{
		pb.SmsVendor_SMS_VENDOR_ALIYUN:     "a",
		pb.SmsVendor_SMS_VENDOR_TENCENT:    "t",
		pb.SmsVendor_SMS_VENDOR_VOLCENGINE: "v",
		pb.SmsVendor_SMS_VENDOR_BYTEPLUS:   "b",
		pb.SmsVendor_SMS_VENDOR_HUAWEI:     "h",
	} {
		s, err := r.SenderFor(vendor, wantAccount)
		require.NoError(t, err, "vendor %s", vendor)
		require.NotNil(t, s)
	}
}

func TestNewAccountRegistry_emptyConfig(t *testing.T) {
	r, err := NewAccountRegistry(nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestNewAccountRegistry_aliyunSuccess(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.SmsVendor]*VendorConfig{
			pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
				{Name: "primary", Aliyun: &AliyunConfig{AccessKeyID: "xxx", AccessKeySecret: "yyy", RegionID: "cn-hangzhou"}},
			}},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	sender, err := r.SenderFor(pb.SmsVendor_SMS_VENDOR_ALIYUN, "primary")
	require.NoError(t, err)
	require.NotNil(t, sender)
}

func TestNewAccountRegistry_duplicateAccountName(t *testing.T) {
	cfg := &Config{
		Vendors: map[pb.SmsVendor]*VendorConfig{
			pb.SmsVendor_SMS_VENDOR_ALIYUN: {Accounts: []*AccountConfig{
				{Name: "primary", Aliyun: &AliyunConfig{AccessKeyID: "a", AccessKeySecret: "b", RegionID: "cn-hangzhou"}},
				{Name: "primary", Aliyun: &AliyunConfig{AccessKeyID: "c", AccessKeySecret: "d", RegionID: "cn-hangzhou"}},
			}},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

func TestNewAccountRegistry_defaultCountryPreserved(t *testing.T) {
	cfg := &Config{
		DefaultCountry: "US",
		Vendors:        map[pb.SmsVendor]*VendorConfig{},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.Equal(t, "US", cfg.DefaultCountry, "field should be preserved on the input config")
	_ = r
}
