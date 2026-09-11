// Package registry holds the immutable platform snapshot: apps, channel
// accounts (with LIVE vendor clients), signature bindings, templates, and
// policies. The send path reads it lock-free via Current(); admin mutations
// trigger an immediate Refresh, and a cron job converges cross-node drift
// (telemetry-service registry pattern).
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	"github.com/servekit/go-common/jsonx"
	provemail "github.com/servekit/message-service/internal/provider/email"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"

	"gorm.io/gorm"
)

// policyKey indexes policies by (tenant, channel, scene). Phase ③: the
// policy key moved from app_id to tenant_key — sends resolve the caller's
// tenant once (tenantres) and look the policy up through it.
func policyKey(tenantKey string, channel, scene int32) string {
	return fmt.Sprintf("%s:%d:%d", tenantKey, channel, scene)
}

// Snapshot is an immutable point-in-time view of the platform resources.
// Readers grab it once per request via Registry.Current().
type Snapshot struct {
	apps         map[string]*models.MessageApp // by app_key
	tenants      map[string]*models.MessageApp // by resolved tenant key (tenant_key column, app_key fallback)
	accounts     map[int64]*models.MessageChannelAccount
	signatures   map[int64]*models.MessageSignature
	sigAccounts  map[int64]map[int64]bool // signatureID -> accountID set
	templates    map[int64]*models.MessageTemplate
	policies     map[string]*models.MessagePolicy // "tenant:channel:scene"
	policyTenant map[int64]string                 // policyID -> resolved tenant (AppScenes listing)

	// Live vendor clients, rebuilt on every Refresh. Accounts that fail to
	// build (bad credentials) are skipped with a log line — a corrupt row
	// must not take the whole send path down.
	smsProviders   map[int64]provesms.AccountProvider
	emailProviders map[int64]provemail.AccountProvider
}

// AppByTenant resolves the tenant's config row by resolved tenant key
// (tenant_key column, app_key literal fallback for un-backfilled rows);
// nil when no app maps to the tenant.
func (s *Snapshot) AppByTenant(tenantKey string) *models.MessageApp {
	return s.tenants[tenantKey]
}

// Apps lists every app ordered by id.
func (s *Snapshot) Apps() []*models.MessageApp {
	out := make([]*models.MessageApp, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AppByID resolves an app by id; nil when unknown.
func (s *Snapshot) AppByID(id int64) *models.MessageApp {
	for _, a := range s.apps {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// ChannelAccount resolves an account by id; nil when unknown.
func (s *Snapshot) ChannelAccount(id int64) *models.MessageChannelAccount {
	return s.accounts[id]
}

// ChannelAccounts lists all accounts ordered by name.
func (s *Snapshot) ChannelAccounts() []*models.MessageChannelAccount {
	out := make([]*models.MessageChannelAccount, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Signature resolves a signature by id; nil when unknown.
func (s *Snapshot) Signature(id int64) *models.MessageSignature {
	return s.signatures[id]
}

// Signatures lists all signatures ordered by name.
func (s *Snapshot) Signatures() []*models.MessageSignature {
	out := make([]*models.MessageSignature, 0, len(s.signatures))
	for _, sig := range s.signatures {
		out = append(out, sig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SignatureAccounts returns the account ids a signature is bound to.
func (s *Snapshot) SignatureAccounts(signatureID int64) []int64 {
	set := s.sigAccounts[signatureID]
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// SignatureBound reports whether signatureID is bound to accountID.
func (s *Snapshot) SignatureBound(signatureID, accountID int64) bool {
	return s.sigAccounts[signatureID][accountID]
}

// Template resolves a template by id; nil when unknown.
func (s *Snapshot) Template(id int64) *models.MessageTemplate {
	return s.templates[id]
}

// Templates lists templates filtered by tenant_key ("" = all) and channel
// (0 = all), ordered by id. The rows carry tenant_key directly (the legacy
// app_id column was dropped with the ④ window close). Shared templates
// (tenant_key NULL) stay visible under every tenant filter.
func (s *Snapshot) Templates(tenantKey string, channel int32) []*models.MessageTemplate {
	var out []*models.MessageTemplate
	for _, t := range s.templates {
		if tenantKey != "" && t.TenantKey != nil && models.TenantKeyOf(t.TenantKey) != tenantKey {
			continue
		}
		if channel != 0 && t.Channel != channel {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AppIDForTenant reverse-resolves a tenant key to its tenant-config row id
// (0 when no config row maps to the tenant) — the wire still names rows by
// id, so admin echoes translate the stored tenant back to the row id.
func (s *Snapshot) AppIDForTenant(tenantKey string) int64 {
	if tenantKey == "" {
		return 0
	}
	if app := s.tenants[tenantKey]; app != nil {
		return app.ID
	}
	return 0
}

// Policy resolves the send policy for (tenant, channel, scene); nil when not
// configured or disabled. channel/scene are TemplateChannel /
// EmailScene|SmsScene enum int32 values.
func (s *Snapshot) Policy(tenantKey string, channel, scene int32) *models.MessagePolicy {
	return s.policies[policyKey(tenantKey, channel, scene)]
}

// Policies lists policies filtered by tenant_key ("" = all) and channel
// (0 = all), ordered by id. The filter matches the policy's resolved tenant
// (the rows were re-keyed to tenant_key with the ④ window close).
// Admin-surface filter — the send path uses Policy.
func (s *Snapshot) Policies(tenantKey string, channel int32) []*models.MessagePolicy {
	var out []*models.MessagePolicy
	for _, p := range s.policies {
		if tenantKey != "" && s.policyTenant[p.ID] != tenantKey {
			continue
		}
		if channel != 0 && p.Channel != channel {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AppScenes lists the scene enum values configured for a tenant on a
// channel (for the POLICY_NOT_FOUND error message).
func (s *Snapshot) AppScenes(tenantKey string, channel int32) []int32 {
	var scenes []int32
	for _, p := range s.policies {
		if s.policyTenant[p.ID] == tenantKey && p.Channel == channel {
			scenes = append(scenes, p.Scene)
		}
	}
	return scenes
}

// SMSProvider returns the live SMS client for an account; nil when the
// account is missing, disabled, or failed to build.
func (s *Snapshot) SMSProvider(accountID int64) provesms.AccountProvider {
	return s.smsProviders[accountID]
}

// EmailProvider returns the live email client for an account; nil when the
// account is missing, disabled, or failed to build.
func (s *Snapshot) EmailProvider(accountID int64) provemail.AccountProvider {
	return s.emailProviders[accountID]
}

// Registry owns the snapshot pointer and the DB it reloads from.
type Registry struct {
	db   *gorm.DB
	snap atomic.Pointer[Snapshot]

	// BuildSMS / BuildEmail construct live provider clients from account
	// rows. Exported so tests can substitute mock providers before the
	// first Refresh; production uses the vendor SDK builders below.
	BuildSMS   func(a *models.MessageChannelAccount) (provesms.AccountProvider, error)
	BuildEmail func(a *models.MessageChannelAccount) (provemail.AccountProvider, error)
}

// New loads the first snapshot synchronously. A load failure (e.g. tables
// not migrated yet) is logged and leaves an empty snapshot — sends then
// fail with clear not-found errors until the first successful refresh,
// rather than the process refusing to boot.
func New(db *gorm.DB) *Registry {
	r := &Registry{
		db:         db,
		BuildSMS:   buildSMSProvider,
		BuildEmail: buildEmailProvider,
	}
	snap := &Snapshot{
		apps:           map[string]*models.MessageApp{},
		tenants:        map[string]*models.MessageApp{},
		accounts:       map[int64]*models.MessageChannelAccount{},
		signatures:     map[int64]*models.MessageSignature{},
		sigAccounts:    map[int64]map[int64]bool{},
		templates:      map[int64]*models.MessageTemplate{},
		policies:       map[string]*models.MessagePolicy{},
		policyTenant:   map[int64]string{},
		smsProviders:   map[int64]provesms.AccountProvider{},
		emailProviders: map[int64]provemail.AccountProvider{},
	}
	r.snap.Store(snap)
	if err := r.Refresh(context.Background()); err != nil {
		slog.Error("registry initial load failed (empty snapshot until first refresh)", "error", err)
	}
	return r
}

// Current returns the latest snapshot. Lock-free.
func (r *Registry) Current() *Snapshot {
	return r.snap.Load()
}

// Refresh reloads every platform table and rebuilds the live provider
// clients, then swaps the snapshot atomically. Corrupt rows are skipped
// with a log line (the write itself may have committed elsewhere; the cron
// refresh converges).
func (r *Registry) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	apps, err := dal.ListApps(ctx, r.db)
	if err != nil {
		return fmt.Errorf("registry: load apps: %w", err)
	}
	accounts, err := dal.ListChannelAccounts(ctx, r.db)
	if err != nil {
		return fmt.Errorf("registry: load accounts: %w", err)
	}
	signatures, err := dal.ListSignatures(ctx, r.db)
	if err != nil {
		return fmt.Errorf("registry: load signatures: %w", err)
	}
	bindings, err := dal.ListAllSignatureAccounts(ctx, r.db)
	if err != nil {
		return fmt.Errorf("registry: load signature bindings: %w", err)
	}
	templates, err := dal.ListTemplates(ctx, r.db, 0)
	if err != nil {
		return fmt.Errorf("registry: load templates: %w", err)
	}
	policies, err := dal.ListPolicies(ctx, r.db, 0)
	if err != nil {
		return fmt.Errorf("registry: load policies: %w", err)
	}

	snap := &Snapshot{
		apps:           make(map[string]*models.MessageApp, len(apps)),
		tenants:        make(map[string]*models.MessageApp, len(apps)),
		accounts:       make(map[int64]*models.MessageChannelAccount, len(accounts)),
		signatures:     make(map[int64]*models.MessageSignature, len(signatures)),
		sigAccounts:    make(map[int64]map[int64]bool, len(signatures)),
		templates:      make(map[int64]*models.MessageTemplate, len(templates)),
		policies:       make(map[string]*models.MessagePolicy, len(policies)),
		policyTenant:   make(map[int64]string, len(policies)),
		smsProviders:   make(map[int64]provesms.AccountProvider, len(accounts)),
		emailProviders: make(map[int64]provemail.AccountProvider, len(accounts)),
	}
	for _, a := range apps {
		snap.apps[a.AppKey] = a
		if tenant := models.AppTenantKey(a); tenant != "" {
			if _, dup := snap.tenants[tenant]; dup {
				slog.Error("registry: duplicate tenant mapping (keeping first)", "tenant", tenant, "app", a.AppKey)
				continue
			}
			snap.tenants[tenant] = a
		}
	}
	for _, a := range accounts {
		snap.accounts[a.ID] = a
		if a.Disabled {
			continue
		}
		switch a.Channel {
		case int32(pb.TemplateChannel_TEMPLATE_CHANNEL_SMS):
			p, err := r.BuildSMS(a)
			if err != nil {
				slog.Error("registry: skip sms account (build failed)", "account", a.Name, "error", err)
				continue
			}
			snap.smsProviders[a.ID] = p
		case int32(pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL):
			p, err := r.BuildEmail(a)
			if err != nil {
				slog.Error("registry: skip email account (build failed)", "account", a.Name, "error", err)
				continue
			}
			snap.emailProviders[a.ID] = p
		}
	}
	for _, sig := range signatures {
		snap.signatures[sig.ID] = sig
	}
	for _, b := range bindings {
		set := snap.sigAccounts[b.SignatureID]
		if set == nil {
			set = make(map[int64]bool)
			snap.sigAccounts[b.SignatureID] = set
		}
		set[b.AccountID] = true
	}
	for _, t := range templates {
		snap.templates[t.ID] = t
	}
	for _, p := range policies {
		if p.Disabled {
			continue
		}
		// Resolve the policy's tenant from its tenant_key column (the ③
		// migration backfill left no NULLs; the legacy app_id pointer was
		// dropped with the ④ window close). A policy with no resolvable
		// tenant is unreachable from any caller — skip it loudly rather
		// than silently keying it under "".
		tenant := models.TenantKeyOf(p.TenantKey)
		if tenant == "" {
			slog.Error("registry: skip policy with unresolvable tenant", "policy_id", p.ID)
			continue
		}
		snap.policies[policyKey(tenant, p.Channel, p.Scene)] = p
		snap.policyTenant[p.ID] = tenant
	}

	r.snap.Store(snap)
	return nil
}

// buildSMSProvider constructs the live SMS client from a DB account row.
func buildSMSProvider(a *models.MessageChannelAccount) (provesms.AccountProvider, error) {
	var ac provesms.AccountConfig
	if len(a.Config) > 0 {
		if err := jsonx.Unmarshal(a.Config, &ac); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}
	ac.Name = a.Name
	return provesms.BuildProvider(pb.SmsVendor(a.Vendor), &ac)
}

// buildEmailProvider constructs the live email client from a DB account
// row. The vendor brand travels inside the config JSON (string form, as
// loaded from legacy YAML); the row's Vendor column is the enum.
func buildEmailProvider(a *models.MessageChannelAccount) (provemail.AccountProvider, error) {
	var ac provemail.AccountConfig
	if len(a.Config) > 0 {
		if err := jsonx.Unmarshal(a.Config, &ac); err != nil {
			return nil, fmt.Errorf("decode config: %w", err)
		}
	}
	ac.Name = a.Name
	if ac.Port == 0 {
		ac.Port = 587
	}
	return provemail.BuildProvider(pb.EmailVendor(a.Vendor), &ac)
}
