// Platform resource models: apps (caller identities), the channel-account
// pool, SMS signatures, templates, and per-(app, channel, scene) send
// policies. Managed via MessageAdminService; loaded into the live registry
// snapshot (internal/registry) for the send path.
package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// RawJSON is a JSON-column value stored/loaded verbatim. The typed decode
// (per-vendor credentials, template content, route chains) happens at the
// registry / admin boundary — the model layer keeps it opaque.
type RawJSON json.RawMessage

// Scan implements sql.Scanner for JSON across postgres/mysql/sqlite drivers.
func (r *RawJSON) Scan(value any) error {
	if value == nil {
		*r = nil
		return nil
	}
	var bytes []byte
	switch v := value.(type) {
	case []byte:
		bytes = v
	case string:
		bytes = []byte(v)
	default:
		return fmt.Errorf("failed to unmarshal JSON value: %v", value)
	}
	*r = append((*r)[0:0], bytes...)
	return nil
}

// Value implements driver.Valuer for JSON columns.
func (r RawJSON) Value() (driver.Value, error) {
	if r == nil {
		return nil, nil
	}
	return []byte(r), nil
}

// MarshalJSON renders the raw JSON verbatim (json.RawMessage semantics).
func (r RawJSON) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	return r, nil
}

// UnmarshalJSON stores the raw bytes verbatim.
func (r *RawJSON) UnmarshalJSON(data []byte) error {
	*r = append((*r)[0:0], data...)
	return nil
}

// MessageApp is a tenant's config row — the send path's per-tenant limits
// and the anchor policies/templates hang off. Since the ④ window close the
// tenant arrives via the trusted x-tenant-key; the minted secret only
// satisfies the not-null column.
type MessageApp struct {
	ID     int64  `gorm:"primaryKey"`
	AppKey string `gorm:"size:64;column:app_key;uniqueIndex;not null"`
	// AppSecret is stored PLAINTEXT — internal-trust posture (see
	// specs/2026-09-07-message-platform-design.md §8).
	AppSecret string `gorm:"size:128;column:app_secret;not null"`
	Name      string `gorm:"size:200;not null"`
	Disabled  bool   `gorm:"not null;default:false"`
	// TenantKey maps the app to its tenant (phase ③ dual-stack window).
	// Nullable transition: NULL = not yet backfilled; the send path falls
	// back to the app_key literal (T10 总装 clears the empties). Unique —
	// one config row per tenant. Lazily upserted on the first trusted
	// x-tenant-key sighting (app_key = tenant_key, default limits).
	TenantKey *string `gorm:"size:16;column:tenant_key;uniqueIndex:uniq_msg_apps_tenant_key"`
	// Daily send-attempt caps; 0 = unlimited. Counts attempts, not
	// deliveries — protects vendor accounts from runaway loops.
	SMSDailyLimit   int64 `gorm:"column:sms_daily_limit;not null;default:0"`
	EmailDailyLimit int64 `gorm:"column:email_daily_limit;not null;default:0"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       gorm.DeletedAt `gorm:"index"`
}

// MessageChannelAccount is one vendor credential set in the platform channel
// pool. Channel (1=email, 2=sms — messaging.v1.TemplateChannel numbering)
// decides how Vendor (the respective SmsVendor/EmailVendor enum value) and
// Config (the vendor-specific credential JSON) are interpreted.
type MessageChannelAccount struct {
	ID       int64  `gorm:"primaryKey"`
	Channel  int32  `gorm:"not null;default:0;index"`
	Vendor   int32  `gorm:"not null;default:0"`
	Name     string `gorm:"size:64;uniqueIndex;not null"`
	Disabled bool   `gorm:"not null;default:false"`
	Remark   string `gorm:"size:256"`
	// Config holds the vendor credential JSON: provesms.AccountConfig JSON
	// (without name) for SMS accounts, provemail.AccountConfig JSON for
	// email accounts. Secrets live here in plaintext (internal trust).
	Config    RawJSON `gorm:"type:json;column:config"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
	// TenantKey marks a tenant-private account (phase ③ resource domain).
	// NULL = platform pool — usable by every tenant's policies, behavior
	// unchanged from pre-③.
	TenantKey *string `gorm:"size:16;column:tenant_key"`
}

// MessageSignature is an SMS signature (CN) / sender ID (intl).
type MessageSignature struct {
	ID        int64  `gorm:"primaryKey"`
	Name      string `gorm:"size:64;uniqueIndex;not null"`
	Disabled  bool   `gorm:"not null;default:false"`
	Remark    string `gorm:"size:256"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
	// TenantKey marks a tenant-private signature (phase ③ resource domain).
	// NULL = platform pool — usable by every tenant's policies, behavior
	// unchanged from pre-③.
	TenantKey *string `gorm:"size:16;column:tenant_key"`
}

// MessageSignatureAccount binds a signature to a channel account (报备).
// A signature may only be used with a bound account in a policy route.
type MessageSignatureAccount struct {
	ID          int64 `gorm:"primaryKey"`
	SignatureID int64 `gorm:"column:signature_id;uniqueIndex:uniq_msg_sig_acct_sig_acct;not null"`
	AccountID   int64 `gorm:"column:account_id;uniqueIndex:uniq_msg_sig_acct_sig_acct;not null"`
	CreatedAt   time.Time
	DeletedAt   gorm.DeletedAt `gorm:"index"`
}

// TemplateParamSpec declares one {{param}} placeholder of a template.
type TemplateParamSpec struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// MessageTemplate is a message template. TenantKey NULL = shared across all
// tenants (the former AppID=0); otherwise the template is private to that
// tenant. Channel/Kind are messaging.v1 TemplateChannel / TemplateKind enum
// values; Params is a JSON array of TemplateParamSpec; Content is a JSON
// document whose shape follows (channel, kind):
//   - (EMAIL, EMAIL_RENDER):     {"email": {subject, text_body, html_body}}
//   - (SMS, SMS_VENDOR_CODES):   {"vendor_codes": [{vendor, template_code}]}
//   - (SMS, SMS_CONTENT):        {"sms_content": {content}}
type MessageTemplate struct {
	ID        int64   `gorm:"primaryKey"`
	AppID     int64   `gorm:"column:app_id;not null;default:0;index"`
	Name      string  `gorm:"size:200;not null"`
	Channel   int32   `gorm:"not null;default:0"`
	Kind      int32   `gorm:"not null;default:0"`
	Disabled  bool    `gorm:"not null;default:false"`
	Params    RawJSON `gorm:"type:json;column:params"`
	Content   RawJSON `gorm:"type:json;column:content"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
	// TenantKey scopes the template to one tenant (phase ③). NULL = shared
	// across all tenants — the former AppID=0 semantics. AppID is retained
	// through the dual-stack window (④ drops it).
	TenantKey *string `gorm:"size:16;column:tenant_key"`
}

// MessagePolicy binds (tenant, channel, scene) to a template + route chains.
// Scene holds the EmailScene or SmsScene enum value matching Channel.
// Routes / IntlRoutes are JSON arrays of send.Route (account_id,
// signature_id, weight) — ordered fallback chains; IntlRoutes applies to
// international SMS destinations only.
type MessagePolicy struct {
	ID         int64   `gorm:"primaryKey"`
	AppID      int64   `gorm:"column:app_id;index"`
	Channel    int32   `gorm:"column:channel;uniqueIndex:uniq_msg_policy_tenant_ch_scene;not null"`
	Scene      int32   `gorm:"column:scene;uniqueIndex:uniq_msg_policy_tenant_ch_scene;not null"`
	TemplateID int64   `gorm:"column:template_id;not null"`
	Disabled   bool    `gorm:"not null;default:false"`
	Routes     RawJSON `gorm:"type:json;column:routes"`
	IntlRoutes RawJSON `gorm:"type:json;column:intl_routes"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  gorm.DeletedAt `gorm:"index"`
	// TenantKey re-keys the policy (phase ③): unique per (tenant_key,
	// channel, scene). NULL on rows written by pre-③ code during the deploy
	// window — resolved through the app mapping at registry load time; the
	// migration backfill leaves no NULLs. AppID is retained through the
	// window (④ drops it along with the old composite).
	TenantKey *string `gorm:"size:16;column:tenant_key;uniqueIndex:uniq_msg_policy_tenant_ch_scene"`
}

// --- tenant_key helpers (nullable-column ergonomics) ---

// TenantKeyOf dereferences a nullable tenant_key column; nil → "".
func TenantKeyOf(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

// TenantKeyPtr boxes a tenant key; "" → nil (writes NULL — the shared /
// platform-pool / not-yet-backfilled marker).
func TenantKeyPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// AppTenantKey resolves the tenant an app row maps to: the tenant_key column
// when backfilled, else the app_key literal (phase ③ window fallback —
// T10 总装 clears the empty columns).
func AppTenantKey(a *MessageApp) string {
	if a == nil {
		return ""
	}
	if tk := TenantKeyOf(a.TenantKey); tk != "" {
		return tk
	}
	return a.AppKey
}
