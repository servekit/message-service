package email

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	pb "github.com/servekit/api/gen/go/messaging/v1"
)

// fakeProvider is a registry-level test AccountProvider that counts Send
// calls. Distinct from sender_test.go's testProvider (records last sent
// Message) — registry tests need call counts to assert that SenderFor-
// returned Senders only invoke the expected provider.
type fakeProvider struct {
	vendor    pb.EmailVendor
	account   string
	err       error
	sentCount int
}

func (p *fakeProvider) Vendor() pb.EmailVendor { return p.vendor }
func (p *fakeProvider) Account() string        { return p.account }
func (p *fakeProvider) Send(_ context.Context, _ *Message) error {
	p.sentCount++
	if p.err != nil {
		return p.err
	}
	return nil
}

// wrap builds a fakeProvider for the given (vendor enum, account, err).
func wrap(vendor pb.EmailVendor, account string, err error) *fakeProvider {
	return &fakeProvider{vendor: vendor, account: account, err: err}
}

func TestNewAccountRegistryFromProviders_empty(t *testing.T) {
	r := NewAccountRegistryFromProviders(nil)

	require.NotNil(t, r.DefaultSender())
	result, err := r.DefaultSender().Send(context.Background(), &Message{To: []*Address{{Email: "u@x.com"}}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no provider")
	require.Nil(t, result)
}

func TestNewAccountRegistryFromProviders_singleVendorAccount(t *testing.T) {
	p := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": p},
	})

	def := r.DefaultSender()
	require.NotNil(t, def)
	result, err := def.Send(context.Background(), &Message{To: []*Address{{Email: "u@x.com"}}})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, p.Vendor(), "vendor impl should report its own vendor")
	require.Equal(t, 1, p.sentCount, "default sender should have called the only provider")
}

// TestNewAccountRegistryFromProviders_sortOrder verifies that the default
// sender's fallback chain is ordered by vendor enum value asc, then account
// name asc.
//
// Map is intentionally unordered: tencent/zzz, aliyun/aaa, aliyun/bbb, tencent/aaa
// Enum values: ALIYUN=1, TENCENT=2 — so aliyun comes first.
// Account sort asc within each vendor: aaa < bbb, aaa < zzz.
// Expected fallback chain: aliyun/aaa → aliyun/bbb → tencent/aaa → tencent/zzz
func TestNewAccountRegistryFromProviders_sortOrder(t *testing.T) {
	aliyunA := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "aaa", errors.New("aliyunA down"))
	aliyunB := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "bbb", nil) // first to succeed
	tencentA := wrap(pb.EmailVendor_EMAIL_VENDOR_TENCENT, "aaa", errors.New("tencentA down"))
	tencentZ := wrap(pb.EmailVendor_EMAIL_VENDOR_TENCENT, "zzz", errors.New("tencentZ down"))

	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_TENCENT: {"zzz": tencentZ, "aaa": tencentA},
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN:  {"bbb": aliyunB, "aaa": aliyunA},
	})

	result, err := r.DefaultSender().Send(context.Background(), &Message{To: []*Address{{Email: "u@x.com"}}})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor, "should fall through to aliyun/bbb")
	require.Equal(t, "bbb", result.Account)
	require.Equal(t, 2, result.Attempts, "should try aliyunA then aliyunB (tencent not reached)")

	require.Equal(t, 1, aliyunA.sentCount)
	require.Equal(t, 1, aliyunB.sentCount)
	require.Equal(t, 0, tencentA.sentCount, "tencent comes after aliyun; never reached")
	require.Equal(t, 0, tencentZ.sentCount, "tencent comes after aliyun; never reached")
}

func TestSenderFor_bothEmpty(t *testing.T) {
	p := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", nil)
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": p},
	})

	got, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED, "")
	require.NoError(t, err)
	require.Same(t, r.DefaultSender(), got, "both empty should return DefaultSender")
}

func TestSenderFor_bothSet(t *testing.T) {
	target := wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", errors.New("target down"))
	other := wrap(pb.EmailVendor_EMAIL_VENDOR_TENCENT, "primary", nil) // should not be called

	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN:  {"primary": target},
		pb.EmailVendor_EMAIL_VENDOR_TENCENT: {"primary": other},
	})

	got, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary")
	require.NoError(t, err)
	require.NotSame(t, r.DefaultSender(), got, "specific selection should NOT be the default fallback sender")

	result, err := got.Send(context.Background(), &Message{To: []*Address{{Email: "u@x.com"}}})
	require.Error(t, err)
	require.False(t, result.Success)
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, result.Vendor)
	require.Equal(t, "primary", result.Account)
	require.Equal(t, 1, result.Attempts, "no fallback — should only try the selected provider")
	require.Equal(t, 1, target.sentCount)
	require.Equal(t, 0, other.sentCount, "other provider should not have been tried")
}

func TestSenderFor_partialVendorOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_partialAccountOnly(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_UNSPECIFIED, "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be set together")
}

func TestSenderFor_unknownVendor(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_TENCENT, "primary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email vendor")
}

func TestSenderFor_unknownAccount(t *testing.T) {
	r := NewAccountRegistryFromProviders(map[pb.EmailVendor]map[string]AccountProvider{
		pb.EmailVendor_EMAIL_VENDOR_ALIYUN: {"primary": wrap(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary", nil)},
	})

	_, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "secondary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email account")
}

// TestNewAccountRegistry_indexesByVendorAndAccount verifies that
// NewAccountRegistry stores vendor impls directly so the internal map carries
// (vendor, account) identity via the vendor impl's own methods.
func TestNewAccountRegistry_indexesByVendorAndAccount(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "aliyun", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	ap, ok := r.vendors[pb.EmailVendor_EMAIL_VENDOR_ALIYUN]["primary"]
	require.True(t, ok, "vendors map should index by (vendor, account)")
	require.Equal(t, pb.EmailVendor_EMAIL_VENDOR_ALIYUN, ap.Vendor())
	require.Equal(t, "primary", ap.Account())
}

func TestNewAccountRegistry_emptyConfig(t *testing.T) {
	r, err := NewAccountRegistry(nil)
	require.NoError(t, err)
	require.NotNil(t, r)
}

func TestNewAccountRegistry_aliyunSuccess(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "aliyun", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
		},
	}

	r, err := NewAccountRegistry(cfg)
	require.NoError(t, err)
	require.NotNil(t, r)

	sender, err := r.SenderFor(pb.EmailVendor_EMAIL_VENDOR_ALIYUN, "primary")
	require.NoError(t, err)
	require.NotNil(t, sender)
}

func TestNewAccountRegistry_requiresHost(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "aliyun", Port: 587, From: "noreply@example.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "host is required")
}

func TestNewAccountRegistry_smtpInvalidPort(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "aliyun", Host: "smtp.example.com", Port: 0, From: "noreply@example.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "smtp")
}

func TestNewAccountRegistry_duplicateAccountName(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "aliyun", Host: "a.example.com", Port: 587, From: "noreply@x.com"},
			{Name: "primary", Vendor: "aliyun", Host: "b.example.com", Port: 587, From: "noreply@y.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate")
}

func TestNewAccountRegistry_unknownVendor(t *testing.T) {
	cfg := &Config{
		Accounts: []*AccountConfig{
			{Name: "primary", Vendor: "mailgun", Host: "smtp.example.com", Port: 587, From: "noreply@example.com"},
		},
	}

	_, err := NewAccountRegistry(cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown email vendor")
}
