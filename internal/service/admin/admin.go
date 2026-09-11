// Package admin implements messaging.v1.MessageAdminService: CRUD over the
// platform resource model (apps / channel accounts / signatures / templates
// / policies). Every mutation writes the DB, then refreshes the in-process
// registry snapshot immediately — cross-node convergence is handled by the
// cron refresh.
//
// Actor scope (phase ④ T5): every RPC resolves the caller's scope first
// (internal/service/admin/actor.go) — an injected tenant key pins the
// caller to that tenant (two-layer list domain, create clamping,
// ownership-checked mutations), a PLATFORM actor without injection gets the
// cross-view, anything else fails closed.
package admin

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"

	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	"github.com/servekit/go-common/jsonx"
	provemail "github.com/servekit/message-service/internal/provider/email"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/send"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"

	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
)

// Service is the admin domain service.
type Service struct {
	db  *gorm.DB
	reg *registry.Registry
	gid gidservice.Service
}

// New constructs the admin service.
func New(db *gorm.DB, reg *registry.Registry, gid gidservice.Service) *Service {
	return &Service{db: db, reg: reg, gid: gid}
}

// refresh reloads the snapshot after a committed mutation. Failure is
// logged, not returned: the write committed, and the cron refresh
// converges (telemetry admin pattern).
func (s *Service) refresh(ctx context.Context) {
	if err := s.reg.Refresh(ctx); err != nil {
		slog.Error("admin: registry refresh after mutation (cron will converge)", "error", err)
	}
}

func (s *Service) nextID(ctx context.Context) (int64, error) {
	return gidservice.NextID(ctx, s.gid)
}

// --- Apps ---

// CreateTenantConfig registers a tenant config row. The credential column
// is gone (④ window close, spec §9.1.3) — nothing secret is minted or
// returned; the data plane authenticates by the trusted x-tenant-key. The
// row is stamped with its tenant mapping: a scoped caller
// (injected key) is clamped to that key; the cross-view keeps an explicit
// tenant_key when given, else the app_key literal (the legacy→tenant
// fallback value; T10 总装 remaps). Phase ④ T6: the admin surface speaks
// tenant — the app_key identity is minted server-side and never named by
// the wire anymore.
func (s *Service) CreateTenantConfig(ctx context.Context, req *pb.CreateTenantConfigRequest) (*pb.CreateTenantConfigResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	// Server-generated identity: "app_" + 8 random base36 chars.
	// Collision odds are negligible; retry defensively anyway.
	appKey := ""
	for i := 0; i < 3; i++ {
		candidate := mintAppKey()
		if _, err := dal.GetAppByKey(ctx, s.db, candidate); err != nil {
			appKey = candidate
			break
		}
	}
	if appKey == "" {
		return nil, xcodes.ErrInternal.New("generate app_key: exhausted retries")
	}
	tenantKey := clampTenantKey(scope, req.GetTenantKey())
	if tenantKey == "" {
		tenantKey = appKey
	}
	if app := s.reg.Current().AppByTenant(tenantKey); app != nil {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf(
			"tenant_key %q already mapped to app %q", tenantKey, app.AppKey))
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	app := &models.MessageApp{
		ID:              id,
		AppKey:          appKey,
		TenantKey:       models.TenantKeyPtr(tenantKey),
		Name:            req.GetName(),
		SMSDailyLimit:   req.GetSmsDailyLimit(),
		EmailDailyLimit: req.GetEmailDailyLimit(),
	}
	if err := dal.CreateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateTenantConfigResponse{Config: appToProto(app)}, nil
}

// GetTenantConfig returns one tenant config by row id. A scoped caller
// sees only the row mapped to their tenant (foreign rows answer
// not-found — anti-enumeration).
func (s *Service) GetTenantConfig(ctx context.Context, req *pb.GetTenantConfigRequest) (*pb.GetTenantConfigResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	app, err := dal.GetApp(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, app.TenantKey,
		xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	return &pb.GetTenantConfigResponse{Config: appToProto(app)}, nil
}

// UpdateTenantConfig tweaks config metadata; absent optional fields keep
// their values. Ownership-checked against the caller's scope.
func (s *Service) UpdateTenantConfig(ctx context.Context, req *pb.UpdateTenantConfigRequest) (*pb.UpdateTenantConfigResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	app, err := dal.GetApp(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, app.TenantKey,
		xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if req.Name != nil {
		app.Name = req.GetName()
	}
	if req.Disabled != nil {
		app.Disabled = req.GetDisabled()
	}
	if req.SmsDailyLimit != nil {
		app.SMSDailyLimit = req.GetSmsDailyLimit()
	}
	if req.EmailDailyLimit != nil {
		app.EmailDailyLimit = req.GetEmailDailyLimit()
	}
	if err := dal.UpdateApp(ctx, s.db, app); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateTenantConfigResponse{Config: appToProto(app)}, nil
}

// RotateTenantConfigSecret is retired: the app_secret column was dropped
// when the ④ window closed (spec §9.1.3) — config rows carry no credential
// to rotate. Ownership-checked against the caller's scope before refusing,
// so a foreign row still answers not-found rather than the retirement
// error.
func (s *Service) RotateTenantConfigSecret(ctx context.Context, req *pb.RotateTenantConfigSecretRequest) (*pb.RotateTenantConfigSecretResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	app, err := dal.GetApp(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, app.TenantKey,
		xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	return nil, xcodes.ErrSecretRetired.New("app_secret was retired with the ④ window close; the data plane authenticates via the trusted x-tenant-key")
}

// ListTenantConfigs returns the tenant configs in the caller's scope: the
// two-layer domain for an injected key (platform pool + own mapping — in
// practice configs always carry a tenant, so this is the own mapping), all
// rows for the cross-view.
func (s *Service) ListTenantConfigs(ctx context.Context, _ *pb.ListTenantConfigsRequest) (*pb.ListTenantConfigsResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	apps := s.reg.Current().Apps()
	out := make([]*pb.MessageTenantConfigInfo, 0, len(apps))
	for _, a := range apps {
		if !visibleInTenant(scope, models.AppTenantKey(a)) {
			continue
		}
		out = append(out, appToProto(a))
	}
	return &pb.ListTenantConfigsResponse{Configs: out}, nil
}

// DeleteTenantConfig soft-deletes a tenant config; its sends fail
// immediately. Ownership-checked against the caller's scope.
func (s *Service) DeleteTenantConfig(ctx context.Context, req *pb.DeleteTenantConfigRequest) (*emptypb.Empty, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	app, err := dal.GetApp(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, app.TenantKey,
		xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if err := dal.DeleteApp(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// --- Channel accounts ---

// CreateChannelAccount adds a vendor account to the pool: the platform
// pool when tenant_key is absent, tenant-private when set (phase ③
// resource domain — a NULL tenant_key stays usable by every tenant). A
// scoped caller is clamped to the injected key (pool writes are
// PLATFORM-only).
func (s *Service) CreateChannelAccount(ctx context.Context, req *pb.CreateChannelAccountRequest) (*pb.CreateChannelAccountResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	ac, vendor, channel, err := credentialsToModel(req.GetName(), req.GetCredentials())
	if err != nil {
		return nil, err
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	account := &models.MessageChannelAccount{
		ID:        id,
		Channel:   int32(channel),
		Vendor:    vendor,
		Name:      req.GetName(),
		Remark:    req.GetRemark(),
		Config:    ac,
		TenantKey: models.TenantKeyPtr(clampTenantKey(scope, req.GetTenantKey())),
	}
	if err := dal.CreateChannelAccount(ctx, s.db, account); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateChannelAccountResponse{Account: s.accountToProto(ctx, account)}, nil
}

// UpdateChannelAccount edits remark/disabled or replaces credentials.
// Ownership-checked after the row load: foreign rows answer not-found,
// the platform pool is read-only for scoped callers.
func (s *Service) UpdateChannelAccount(ctx context.Context, req *pb.UpdateChannelAccountRequest) (*pb.UpdateChannelAccountResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	account, err := dal.GetChannelAccount(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, account.TenantKey,
		xcodes.ErrChannelAccountNotFound.New(fmt.Sprintf("account %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if req.Remark != nil {
		account.Remark = req.GetRemark()
	}
	if req.Disabled != nil {
		account.Disabled = req.GetDisabled()
	}
	if req.GetCredentials() != nil {
		ac, vendor, channel, err := credentialsToModel(account.Name, req.GetCredentials())
		if err != nil {
			return nil, err
		}
		account.Config = ac
		account.Vendor = vendor
		account.Channel = int32(channel)
	}
	if err := dal.UpdateChannelAccount(ctx, s.db, account); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateChannelAccountResponse{Account: s.accountToProto(ctx, account)}, nil
}

// DeleteChannelAccount soft-deletes an account from the pool.
// Ownership-checked after the row load.
func (s *Service) DeleteChannelAccount(ctx context.Context, req *pb.DeleteChannelAccountRequest) (*emptypb.Empty, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	account, err := dal.GetChannelAccount(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, account.TenantKey,
		xcodes.ErrChannelAccountNotFound.New(fmt.Sprintf("account %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if err := dal.DeleteChannelAccount(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListChannelAccounts returns the pool with secrets masked, scoped to the
// caller: the two-layer domain (platform pool + own rows) for an injected
// key, the full pool for the cross-view.
func (s *Service) ListChannelAccounts(ctx context.Context, _ *pb.ListChannelAccountsRequest) (*pb.ListChannelAccountsResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	accounts := s.reg.Current().ChannelAccounts()
	out := make([]*pb.ChannelAccountInfo, 0, len(accounts))
	for _, a := range accounts {
		if !visibleInTenant(scope, models.TenantKeyOf(a.TenantKey)) {
			continue
		}
		out = append(out, s.accountToProto(ctx, a))
	}
	return &pb.ListChannelAccountsResponse{Accounts: out}, nil
}

// accountToProto renders a DB account row. Credentials are decoded and
// echoed with SECRET FIELDS EMPTIED (write-only); a corrupt config JSON
// yields an account with an empty credentials oneof rather than an error
// (list must stay usable for triage).
func (s *Service) accountToProto(_ context.Context, a *models.MessageChannelAccount) *pb.ChannelAccountInfo {
	info := &pb.ChannelAccountInfo{
		Id:        a.ID,
		Name:      a.Name,
		Disabled:  a.Disabled,
		Remark:    a.Remark,
		TenantKey: models.TenantKeyOf(a.TenantKey),
		// created_at/updated_at filled below
	}
	if !a.CreatedAt.IsZero() {
		info.CreatedAt = a.CreatedAt.Unix()
	}
	if !a.UpdatedAt.IsZero() {
		info.UpdatedAt = a.UpdatedAt.Unix()
	}
	switch pb.TemplateChannel(a.Channel) {
	case pb.TemplateChannel_TEMPLATE_CHANNEL_SMS:
		info.Vendor = &pb.ChannelAccountInfo_SmsVendor{SmsVendor: pb.SmsVendor(a.Vendor)}
		var ac provesms.AccountConfig
		if err := jsonx.Unmarshal(a.Config, &ac); err == nil {
			info.Credentials = smsAccountToProtoCredentials(&ac)
		}
	case pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL:
		info.Vendor = &pb.ChannelAccountInfo_EmailVendor{EmailVendor: pb.EmailVendor(a.Vendor)}
		var ac provemail.AccountConfig
		if err := jsonx.Unmarshal(a.Config, &ac); err == nil {
			info.Credentials = emailAccountToProtoCredentials(&ac)
		}
	}
	return info
}

// --- Signatures ---

// CreateSignature registers a signature with its account bindings:
// platform pool when tenant_key is absent, tenant-private when set. A
// scoped caller is clamped to the injected key (pool writes are
// PLATFORM-only).
func (s *Service) CreateSignature(ctx context.Context, req *pb.CreateSignatureRequest) (*pb.CreateSignatureResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	tenantKey := clampTenantKey(scope, req.GetTenantKey())
	if err := s.validateBindingAccounts(ctx, tenantKey, req.GetAccountIds()); err != nil {
		return nil, err
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	sig := &models.MessageSignature{
		ID: id, Name: req.GetName(), Remark: req.GetRemark(),
		TenantKey: models.TenantKeyPtr(tenantKey),
	}
	if err := dal.CreateSignature(ctx, s.db, sig, req.GetAccountIds()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateSignatureResponse{Signature: s.signatureToProto(sig, req.GetAccountIds())}, nil
}

// UpdateSignature replaces remark/disabled/bindings. Ownership-checked
// after the row load.
func (s *Service) UpdateSignature(ctx context.Context, req *pb.UpdateSignatureRequest) (*pb.UpdateSignatureResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	sig, err := dal.GetSignature(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, sig.TenantKey,
		xcodes.ErrSignatureNotFound.New(fmt.Sprintf("signature %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if req.Remark != nil {
		sig.Remark = req.GetRemark()
	}
	if req.Disabled != nil {
		sig.Disabled = req.GetDisabled()
	}
	accounts := s.reg.Current().SignatureAccounts(sig.ID)
	if req.GetAccountIds() != nil {
		accounts = req.GetAccountIds().GetIds()
		if err := s.validateBindingAccounts(ctx, models.TenantKeyOf(sig.TenantKey), accounts); err != nil {
			return nil, err
		}
	}
	if err := dal.UpdateSignature(ctx, s.db, sig, accounts); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateSignatureResponse{Signature: s.signatureToProto(sig, accounts)}, nil
}

// DeleteSignature soft-deletes a signature and its bindings.
// Ownership-checked after the row load.
func (s *Service) DeleteSignature(ctx context.Context, req *pb.DeleteSignatureRequest) (*emptypb.Empty, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	sig, err := dal.GetSignature(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, sig.TenantKey,
		xcodes.ErrSignatureNotFound.New(fmt.Sprintf("signature %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if err := dal.DeleteSignature(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListSignatures returns signatures with bindings, scoped to the caller:
// the two-layer domain (platform pool + own rows) for an injected key, all
// signatures for the cross-view.
func (s *Service) ListSignatures(ctx context.Context, _ *pb.ListSignaturesRequest) (*pb.ListSignaturesResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	sigs := s.reg.Current().Signatures()
	out := make([]*pb.SignatureInfo, 0, len(sigs))
	for _, sig := range sigs {
		if !visibleInTenant(scope, models.TenantKeyOf(sig.TenantKey)) {
			continue
		}
		out = append(out, s.signatureToProto(sig, s.reg.Current().SignatureAccounts(sig.ID)))
	}
	return &pb.ListSignaturesResponse{Signatures: out}, nil
}

// validateBindingAccounts checks every bound account exists, is an SMS
// channel account (signatures are SMS-only concepts), and stays inside the
// signature's resource domain: the platform pool (tenant_key NULL) or the
// signature's own tenant (phase ③ — binding another tenant's private
// account is a cross-tenant reference).
func (s *Service) validateBindingAccounts(ctx context.Context, tenantKey string, accountIDs []int64) error {
	snap := s.reg.Current()
	for _, id := range accountIDs {
		account := snap.ChannelAccount(id)
		if account == nil {
			return xcodes.ErrChannelAccountNotFound.New(fmt.Sprintf("account %d not found (or deleted)", id))
		}
		if pb.TemplateChannel(account.Channel) != pb.TemplateChannel_TEMPLATE_CHANNEL_SMS {
			return xcodes.ErrBadRequest.New(fmt.Sprintf("account %d (%s) is not an SMS account", id, account.Name))
		}
		if err := checkResourceDomain("account", id, account.Name, account.TenantKey, tenantKey); err != nil {
			return err
		}
	}
	return nil
}

// checkResourceDomain enforces the phase ③ resource-domain rule for a
// referenced resource: its tenant_key must be NULL (platform pool / shared)
// or equal the referencing tenant's key. Anything else is a cross-tenant
// reference → BadRequest.
func checkResourceDomain(kind string, id int64, name string, resourceTenant *string, tenantKey string) error {
	owner := models.TenantKeyOf(resourceTenant)
	if owner == "" || owner == tenantKey {
		return nil
	}
	return xcodes.ErrBadRequest.New(fmt.Sprintf(
		"%s %d (%s) belongs to tenant %q — cross-tenant reference rejected", kind, id, name, owner))
}

// --- Templates ---

// CreateTemplate registers a template definition. app_id 0 = shared
// (tenant_key NULL); otherwise the template is stamped with the app's
// mapped tenant (the app must exist — the tenant is derived from it). A
// scoped caller must reference an app mapped to their own tenant — shared
// templates are PLATFORM-only and foreign apps answer not-found.
func (s *Service) CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.CreateTemplateResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	tenantKey, err := s.templateTenant(scope, req.GetAppId())
	if err != nil {
		return nil, err
	}
	t, err := templateToModel(req.GetTemplate())
	if err != nil {
		return nil, err
	}
	t.TenantKey = models.TenantKeyPtr(tenantKey)
	if req.GetTemplate().GetName() != "" {
		t.Name = req.GetTemplate().GetName()
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	t.ID = id
	if err := dal.CreateTemplate(ctx, s.db, t); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreateTemplateResponse{Template: templateToProto(s.reg.Current().AppIDForTenant, t)}, nil
}

// templateTenant resolves the owning tenant for a template scoped to
// appID: "" (shared) for app_id 0, the app's mapped tenant otherwise.
// Unknown apps are refused — the tenant cannot be derived. Under a scope
// the reference is verified against the caller's tenant (app_id 0 shared
// is PLATFORM-only; foreign apps answer not-found so existence of another
// tenant's app mapping never leaks).
func (s *Service) templateTenant(scope string, appID int64) (string, error) {
	if appID == 0 {
		if scope != "" {
			return "", xcodes.ErrForbidden.New("shared (app_id 0) templates are platform-only")
		}
		return "", nil
	}
	app := s.reg.Current().AppByID(appID)
	if app == nil {
		return "", xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", appID))
	}
	tenant := models.AppTenantKey(app)
	if scope != "" && tenant != scope {
		return "", xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", appID))
	}
	return tenant, nil
}

// UpdateTemplate fully replaces a template definition. Ownership (app_id /
// tenant_key) is immutable. Ownership-checked against the caller's scope.
func (s *Service) UpdateTemplate(ctx context.Context, req *pb.UpdateTemplateRequest) (*pb.UpdateTemplateResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := dal.GetTemplate(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, existing.TenantKey,
		xcodes.ErrTemplateNotFound.New(fmt.Sprintf("template %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	t, err := templateToModel(req.GetTemplate())
	if err != nil {
		return nil, err
	}
	t.ID = existing.ID
	t.TenantKey = existing.TenantKey
	if err := dal.UpdateTemplate(ctx, s.db, t); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdateTemplateResponse{Template: templateToProto(s.reg.Current().AppIDForTenant, t)}, nil
}

// DeleteTemplate soft-deletes a template. Ownership-checked against the
// caller's scope.
func (s *Service) DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := dal.GetTemplate(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageRow(scope, existing.TenantKey,
		xcodes.ErrTemplateNotFound.New(fmt.Sprintf("template %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if err := dal.DeleteTemplate(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListTemplates filters by app (0 = all) and channel (0 = all), scoped to
// the caller: the two-layer domain (shared + own rows) for an injected
// key, everything for the cross-view.
func (s *Service) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	templates := s.reg.Current().Templates(req.GetAppId(), int32(req.GetChannel()))
	out := make([]*pb.TemplateInfo, 0, len(templates))
	for _, t := range templates {
		if !visibleInTenant(scope, models.TenantKeyOf(t.TenantKey)) {
			continue
		}
		out = append(out, templateToProto(s.reg.Current().AppIDForTenant, t))
	}
	return &pb.ListTemplatesResponse{Templates: out}, nil
}

// --- Policies ---

// CreatePolicy binds (tenant, channel, scene) to a template + route chains.
// The policy belongs to the app's mapped tenant (phase ③ re-keying — unique
// per (tenant_key, channel, scene)); its template and route references are
// domain-checked: the platform pool plus the tenant's own private resources
// only, cross-tenant references are a BadRequest. Under a scope the app
// must map to the caller's tenant (foreign apps answer not-found).
func (s *Service) CreatePolicy(ctx context.Context, req *pb.CreatePolicyRequest) (*pb.CreatePolicyResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	channel, scene, err := sceneFromProto(req.GetScene())
	if err != nil {
		return nil, err
	}
	app := s.reg.Current().AppByID(req.GetAppId())
	if app == nil {
		return nil, xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", req.GetAppId()))
	}
	tenantKey := models.AppTenantKey(app)
	if scope != "" && tenantKey != scope {
		return nil, xcodes.ErrAppNotFound.New(fmt.Sprintf("app %d not found", req.GetAppId()))
	}
	if s.reg.Current().Policy(tenantKey, int32(channel), scene) != nil {
		return nil, xcodes.ErrBadRequest.New(fmt.Sprintf("policy for tenant %q scene already exists", tenantKey))
	}
	if err := s.validatePolicy(ctx, channel, tenantKey, req.GetTemplateId(), req.GetRoutes(), req.GetIntlRoutes()); err != nil {
		return nil, err
	}
	id, err := s.nextID(ctx)
	if err != nil {
		return nil, xcodes.ErrInternal.Wrap(err)
	}
	routes, err := send.MarshalRoutes(routesFromProto(req.GetRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	intlRoutes, err := send.MarshalRoutes(routesFromProto(req.GetIntlRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	policy := &models.MessagePolicy{
		ID:         id,
		TenantKey:  models.TenantKeyPtr(tenantKey),
		Channel:    int32(channel),
		Scene:      scene,
		TemplateID: req.GetTemplateId(),
		Routes:     routes,
		IntlRoutes: intlRoutes,
	}
	if err := dal.CreatePolicy(ctx, s.db, policy); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.CreatePolicyResponse{Policy: policyToProto(s.reg.Current().AppIDForTenant, policy, req.GetRoutes(), req.GetIntlRoutes())}, nil
}

// UpdatePolicy fully replaces template + route chains. Tenant / channel /
// scene are immutable — delete and recreate to move a policy.
// Ownership-checked after the row load.
func (s *Service) UpdatePolicy(ctx context.Context, req *pb.UpdatePolicyRequest) (*pb.UpdatePolicyResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := dal.GetPolicy(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageTenant(scope, s.policyTenant(policy),
		xcodes.ErrPolicyNotFound.New(fmt.Sprintf("policy %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	tenantKey := s.policyTenant(policy)
	if err := s.validatePolicy(ctx, pb.TemplateChannel(policy.Channel), tenantKey, req.GetTemplateId(), req.GetRoutes(), req.GetIntlRoutes()); err != nil {
		return nil, err
	}
	routes, err := send.MarshalRoutes(routesFromProto(req.GetRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	intlRoutes, err := send.MarshalRoutes(routesFromProto(req.GetIntlRoutes()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	policy.TemplateID = req.GetTemplateId()
	if req.Disabled != nil {
		policy.Disabled = req.GetDisabled()
	}
	policy.Routes = routes
	policy.IntlRoutes = intlRoutes
	if err := dal.UpdatePolicy(ctx, s.db, policy); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &pb.UpdatePolicyResponse{Policy: policyToProto(s.reg.Current().AppIDForTenant, policy, req.GetRoutes(), req.GetIntlRoutes())}, nil
}

// DeletePolicy removes the binding; sends fail closed from then on.
// Ownership-checked after the row load.
func (s *Service) DeletePolicy(ctx context.Context, req *pb.DeletePolicyRequest) (*emptypb.Empty, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := dal.GetPolicy(ctx, s.db, req.GetId())
	if err != nil {
		return nil, err
	}
	if err := authorizeManageTenant(scope, s.policyTenant(policy),
		xcodes.ErrPolicyNotFound.New(fmt.Sprintf("policy %d not found", req.GetId()))); err != nil {
		return nil, err
	}
	if err := dal.DeletePolicy(ctx, s.db, req.GetId()); err != nil {
		return nil, err
	}
	s.refresh(ctx)
	return &emptypb.Empty{}, nil
}

// ListPolicies filters by app (0 = all) and channel (0 = all), scoped to
// the caller: the two-layer domain (shared leftovers + own rows) for an
// injected key, everything for the cross-view.
func (s *Service) ListPolicies(ctx context.Context, req *pb.ListPoliciesRequest) (*pb.ListPoliciesResponse, error) {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	policies := s.reg.Current().Policies(req.GetAppId(), int32(req.GetChannel()))
	out := make([]*pb.PolicyInfo, 0, len(policies))
	for _, p := range policies {
		if !visibleInTenant(scope, s.policyTenant(p)) {
			continue
		}
		routes, _ := send.ParseRoutes(p.Routes)
		intlRoutes, _ := send.ParseRoutes(p.IntlRoutes)
		out = append(out, policyToProto(s.reg.Current().AppIDForTenant, p, routesToProto(routes), routesToProto(intlRoutes)))
	}
	return &pb.ListPoliciesResponse{Policies: out}, nil
}

// policyTenant resolves a stored policy's tenant from its tenant_key
// column (the registry load's chain minus the deleted legacy pointer).
func (s *Service) policyTenant(p *models.MessagePolicy) string {
	return models.TenantKeyOf(p.TenantKey)
}

// validatePolicy checks: template exists with matching channel and stays in
// the tenant's resource domain; every route account exists, is enabled,
// matches the channel, and stays in the domain; SMS routes carry a signature
// bound to that account (and in the domain); email routes carry none.
// Domain rule (phase ③): tenant_key NULL (platform pool / shared) or equal
// to tenantKey; anything else is cross-tenant → BadRequest. An empty
// tenantKey (unresolvable, pre-③ dangling row) skips the domain check —
// the platform editing its own leftovers.
func (s *Service) validatePolicy(ctx context.Context, channel pb.TemplateChannel, tenantKey string, templateID int64, routes, intlRoutes []*pb.RouteRule) error {
	snap := s.reg.Current()
	t := snap.Template(templateID)
	if t == nil || t.Disabled {
		return xcodes.ErrTemplateNotFound.New(fmt.Sprintf("template %d not found (or disabled)", templateID))
	}
	if pb.TemplateChannel(t.Channel) != channel {
		return xcodes.ErrBadRequest.New(fmt.Sprintf("template %d channel %s does not match policy channel %s",
			templateID, pb.TemplateChannel(t.Channel), channel))
	}
	if err := checkResourceDomain("template", t.ID, t.Name, t.TenantKey, tenantKey); err != nil {
		return err
	}
	if channel == pb.TemplateChannel_TEMPLATE_CHANNEL_SMS && len(intlRoutes) == 0 {
		// Allowed: policy rejects intl destinations at send time. Recorded
		// here as a no-op check for readability.
		_ = intlRoutes
	}
	check := func(rs []*pb.RouteRule, requireIntlEligible bool) error {
		for _, r := range rs {
			account := snap.ChannelAccount(r.GetAccountId())
			if account == nil || account.Disabled {
				return xcodes.ErrChannelAccountNotFound.New(fmt.Sprintf("route account %d not found or disabled", r.GetAccountId()))
			}
			if pb.TemplateChannel(account.Channel) != channel {
				return xcodes.ErrBadRequest.New(fmt.Sprintf("route account %d (%s) channel mismatch", r.GetAccountId(), account.Name))
			}
			if err := checkResourceDomain("account", account.ID, account.Name, account.TenantKey, tenantKey); err != nil {
				return err
			}
			if channel == pb.TemplateChannel_TEMPLATE_CHANNEL_SMS {
				if r.GetSignatureId() == 0 {
					return xcodes.ErrBadRequest.New("sms routes require a signature")
				}
				sig := snap.Signature(r.GetSignatureId())
				if sig == nil || sig.Disabled {
					return xcodes.ErrSignatureNotFound.New(fmt.Sprintf("route signature %d not found or disabled", r.GetSignatureId()))
				}
				if err := checkResourceDomain("signature", sig.ID, sig.Name, sig.TenantKey, tenantKey); err != nil {
					return err
				}
				if !snap.SignatureBound(r.GetSignatureId(), r.GetAccountId()) {
					return xcodes.ErrSignatureNotBound.New(fmt.Sprintf(
						"signature %q is not bound to account %q", sig.Name, account.Name))
				}
			} else if r.GetSignatureId() != 0 {
				return xcodes.ErrBadRequest.New("email routes must not set a signature")
			}
		}
		return nil
	}
	if err := check(routes, false); err != nil {
		return err
	}
	return check(intlRoutes, true)
}

// --- conversion helpers ---

func appToProto(a *models.MessageApp) *pb.MessageTenantConfigInfo {
	info := &pb.MessageTenantConfigInfo{
		Id:              a.ID,
		AppKey:          a.AppKey,
		Name:            a.Name,
		Disabled:        a.Disabled,
		TenantKey:       models.TenantKeyOf(a.TenantKey),
		SmsDailyLimit:   a.SMSDailyLimit,
		EmailDailyLimit: a.EmailDailyLimit,
	}
	if !a.CreatedAt.IsZero() {
		info.CreatedAt = a.CreatedAt.Unix()
	}
	if !a.UpdatedAt.IsZero() {
		info.UpdatedAt = a.UpdatedAt.Unix()
	}
	return info
}

func (s *Service) signatureToProto(sig *models.MessageSignature, accountIDs []int64) *pb.SignatureInfo {
	info := &pb.SignatureInfo{
		Id:         sig.ID,
		Name:       sig.Name,
		Disabled:   sig.Disabled,
		Remark:     sig.Remark,
		AccountIds: accountIDs,
		TenantKey:  models.TenantKeyOf(sig.TenantKey),
	}
	if !sig.CreatedAt.IsZero() {
		info.CreatedAt = sig.CreatedAt.Unix()
	}
	if !sig.UpdatedAt.IsZero() {
		info.UpdatedAt = sig.UpdatedAt.Unix()
	}
	return info
}

// sceneFromProto extracts (channel, scene int32) from the PolicyScene
// oneof.
func sceneFromProto(ps *pb.PolicyScene) (pb.TemplateChannel, int32, error) {
	switch sc := ps.GetScene().(type) {
	case *pb.PolicyScene_EmailScene:
		if sc.EmailScene == pb.EmailScene_EMAIL_SCENE_UNSPECIFIED {
			return 0, 0, xcodes.ErrBadRequest.New("email_scene is required")
		}
		return pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL, int32(sc.EmailScene), nil
	case *pb.PolicyScene_SmsScene:
		if sc.SmsScene == pb.SmsScene_SMS_SCENE_UNSPECIFIED {
			return 0, 0, xcodes.ErrBadRequest.New("sms_scene is required")
		}
		return pb.TemplateChannel_TEMPLATE_CHANNEL_SMS, int32(sc.SmsScene), nil
	default:
		return 0, 0, xcodes.ErrBadRequest.New("scene is required")
	}
}

func routesFromProto(rs []*pb.RouteRule) []send.Route {
	out := make([]send.Route, len(rs))
	for i, r := range rs {
		out[i] = send.Route{AccountID: r.GetAccountId(), SignatureID: r.GetSignatureId(), Weight: r.GetWeight()}
	}
	return out
}

func routesToProto(rs []send.Route) []*pb.RouteRule {
	out := make([]*pb.RouteRule, len(rs))
	for i, r := range rs {
		out[i] = &pb.RouteRule{AccountId: r.AccountID, SignatureId: r.SignatureID, Weight: r.Weight}
	}
	return out
}

func policyToProto(appIDFor func(string) int64, p *models.MessagePolicy, routes, intlRoutes []*pb.RouteRule) *pb.PolicyInfo {
	info := &pb.PolicyInfo{
		Id:         p.ID,
		AppId:      appIDFor(models.TenantKeyOf(p.TenantKey)),
		TenantKey:  models.TenantKeyOf(p.TenantKey),
		Channel:    pb.TemplateChannel(p.Channel),
		TemplateId: p.TemplateID,
		Routes:     routes,
		IntlRoutes: intlRoutes,
		Disabled:   p.Disabled,
	}
	switch pb.TemplateChannel(p.Channel) {
	case pb.TemplateChannel_TEMPLATE_CHANNEL_EMAIL:
		info.EmailScene = pb.EmailScene(p.Scene)
	case pb.TemplateChannel_TEMPLATE_CHANNEL_SMS:
		info.SmsScene = pb.SmsScene(p.Scene)
	}
	if !p.CreatedAt.IsZero() {
		info.CreatedAt = p.CreatedAt.Unix()
	}
	if !p.UpdatedAt.IsZero() {
		info.UpdatedAt = p.UpdatedAt.Unix()
	}
	return info
}

func templateToModel(t *pb.TemplateInfo) (*models.MessageTemplate, error) {
	channel := t.GetChannel()
	kind := t.GetKind()
	var content models.RawJSON
	var err error
	switch c := t.GetContent().(type) {
	case *pb.TemplateInfo_Email:
		content, err = send.MarshalEmailContent(&send.EmailContent{
			Subject:  c.Email.GetSubject(),
			TextBody: c.Email.GetTextBody(),
			HTMLBody: c.Email.GetHtmlBody(),
		})
	case *pb.TemplateInfo_VendorCodes:
		codes := make([]send.VendorCode, 0, len(c.VendorCodes.GetCodes()))
		for _, vc := range c.VendorCodes.GetCodes() {
			codes = append(codes, send.VendorCode{Vendor: vc.GetVendor(), TemplateCode: vc.GetTemplateCode()})
		}
		content, err = send.MarshalVendorCodes(codes)
	case *pb.TemplateInfo_SmsContent:
		content, err = send.MarshalSmsContent(&send.SmsContent{Content: c.SmsContent.GetContent()})
	default:
		err = xcodes.ErrInvalidTemplateContent.New("content is required")
	}
	if err != nil {
		return nil, xcodes.ErrInvalidTemplateContent.Wrap(err)
	}
	if err := send.ValidateContentShape(channel, kind, content); err != nil {
		return nil, err
	}
	params, err := send.MarshalParamSpecs(paramSpecsToModel(t.GetParams()))
	if err != nil {
		return nil, xcodes.ErrBadRequest.Wrap(err)
	}
	return &models.MessageTemplate{
		Name:     t.GetName(),
		Channel:  int32(channel),
		Kind:     int32(kind),
		Disabled: t.GetDisabled(),
		Params:   params,
		Content:  content,
	}, nil
}

func templateToProto(appIDFor func(string) int64, t *models.MessageTemplate) *pb.TemplateInfo {
	info := &pb.TemplateInfo{
		Id:        t.ID,
		AppId:     appIDFor(models.TenantKeyOf(t.TenantKey)),
		Name:      t.Name,
		Channel:   pb.TemplateChannel(t.Channel),
		Kind:      pb.TemplateKind(t.Kind),
		Disabled:  t.Disabled,
		TenantKey: models.TenantKeyOf(t.TenantKey),
	}
	specs, _ := send.ParseParamSpecs(t.Params)
	info.Params = paramSpecsToProto(specs)
	switch pb.TemplateKind(t.Kind) {
	case pb.TemplateKind_TEMPLATE_KIND_EMAIL_RENDER:
		if c, err := send.DecodeEmailContent(t.Content); err == nil {
			info.Content = &pb.TemplateInfo_Email{Email: &pb.EmailTemplateContent{
				Subject:  c.Subject,
				TextBody: c.TextBody,
				HtmlBody: c.HTMLBody,
			}}
		}
	case pb.TemplateKind_TEMPLATE_KIND_SMS_VENDOR_CODES:
		if codes, err := send.DecodeVendorCodes(t.Content); err == nil {
			pbCodes := make([]*pb.VendorTemplateCode, 0, len(codes))
			for _, vc := range send.SortVendorCodes(codes) {
				pbCodes = append(pbCodes, &pb.VendorTemplateCode{Vendor: vc.Vendor, TemplateCode: vc.TemplateCode})
			}
			info.Content = &pb.TemplateInfo_VendorCodes{VendorCodes: &pb.SmsVendorCodesContent{Codes: pbCodes}}
		}
	case pb.TemplateKind_TEMPLATE_KIND_SMS_CONTENT:
		if c, err := send.DecodeSmsContent(t.Content); err == nil {
			info.Content = &pb.TemplateInfo_SmsContent{SmsContent: &pb.SmsContentTemplate{Content: c.Content}}
		}
	}
	if !t.CreatedAt.IsZero() {
		info.CreatedAt = t.CreatedAt.Unix()
	}
	if !t.UpdatedAt.IsZero() {
		info.UpdatedAt = t.UpdatedAt.Unix()
	}
	return info
}

func paramSpecsToModel(specs []*pb.TemplateParamSpec) []models.TemplateParamSpec {
	out := make([]models.TemplateParamSpec, len(specs))
	for i, s := range specs {
		out[i] = models.TemplateParamSpec{Name: s.GetName(), Required: s.GetRequired(), Description: s.GetDescription()}
	}
	return out
}

func paramSpecsToProto(specs []models.TemplateParamSpec) []*pb.TemplateParamSpec {
	out := make([]*pb.TemplateParamSpec, len(specs))
	for i, s := range specs {
		out[i] = &pb.TemplateParamSpec{Name: s.Name, Required: s.Required, Description: s.Description}
	}
	return out
}

// mintAppKey generates a server-side app identity: "app_" + 8 base36 chars.
func mintAppKey() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "app_" + fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 8)
	for i, b := range buf {
		out[i] = base36[int(b)%36]
	}
	return "app_" + string(out)
}
