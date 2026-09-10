// Package service contains message-service business logic.
//
// Layering contract (see golang-service-development skill §2):
//   - This is the SERVICE ROOT. It holds Service struct + New + Start/Stop +
//     resource resolve helpers + one-line facade methods (one per RPC).
//   - Business logic lives in SUBPACKAGES (internal/service/<domain>/).
//   - handler calls service.X; service.X is a one-line facade.
//   - Service methods take proto types DIRECTLY and return proto types.
package service

import (
	"context"
	"log/slog"
	"time"

	"gorm.io/gorm"

	commonv1 "github.com/servekit/api/gen/go/common/v1"
	pb "github.com/servekit/api/gen/go/messaging/v1"
	gidservice "github.com/servekit/gid-service/pkg"
	gidconfig "github.com/servekit/gid-service/pkg/config"
	"github.com/servekit/message-service/internal/idempotency"
	"github.com/servekit/message-service/internal/jobs"
	"github.com/servekit/message-service/internal/quota"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/service/admin"
	svcemail "github.com/servekit/message-service/internal/service/email"
	svcsms "github.com/servekit/message-service/internal/service/sms"
	"github.com/servekit/message-service/internal/tenantres"
	"github.com/servekit/message-service/internal/version"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/option"

	"github.com/servekit/go-common/cronx"
	"github.com/servekit/go-common/dbx"
	"github.com/servekit/go-common/lifecycle"
	"github.com/servekit/go-common/redisx"

	"google.golang.org/protobuf/types/known/emptypb"
)

// registryRefreshSpec is the snapshot-convergence cron: cross-node admin
// mutations (or a failed local refresh) converge within a minute.
const registryRefreshSpec = "*/1 * * * *"

// Service holds message-service business state: one subpackage instance per
// channel (email + sms) plus the admin surface. The root itself only does
// resource resolve, app authentication for the send facades, and one-line
// RPC delegation; business logic lives in the subpackages.
type Service struct {
	cfg *config.Config
	mgr *lifecycle.Manager

	db  *gorm.DB
	gid gidservice.Service

	reg   *registry.Registry
	quota *quota.Checker
	email *svcemail.Service
	sms   *svcsms.Service
	admin *admin.Service
	// tenants resolves the send path's caller during the ③ dual-stack
	// window (trusted x-tenant-key vs legacy ak/sk, D-③1).
	tenants *tenantres.Resolver

	// startedAt is set once in New; Ping returns it for uptime.
	startedAt int64
}

// New constructs a Service from config and functional options.
//
// Resources not injected via options are created from cfg, wrapped as
// lifecycle.Stoppers, and registered with the internal Manager. Stop will
// stop them in reverse order. Injected resources are NOT registered — caller
// owns their lifecycle.
//
// On partial failure (any resolve returns an error), already-registered
// components are stopped via mgr.Stop() before returning the error.
func New(cfg *config.Config, opts ...option.Option) (*Service, error) {
	o := option.Apply(opts...)
	mgr := lifecycle.NewManager()

	// Redis is required for service-level idempotency. Resolve first —
	// it has no dependencies, and placing it before resolveDB keeps the
	// rollback chain simple (nothing prior to roll back on its failure).
	redisClient, err := redisx.Connect(cfg.Redis, o.Redis, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after redis resolve failure", "error", cerr)
		}
		return nil, err
	}
	prefix := cfg.IdempotencyKeyPrefix
	if prefix == "" {
		prefix = config.DefaultIdempotencyKeyPrefix
	}
	idemChecker := idempotency.NewRedisChecker(redisClient, &idempotency.Config{
		KeyPrefix: prefix,
		EmailTTL:  cfg.Email.IdempotencyTTLDuration(),
		SMSTTL:    cfg.SMS.IdempotencyTTLDuration(),
	})

	db, err := dbx.Connect(cfg.Database, o.DB, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after db resolve failure", "error", cerr)
		}
		return nil, err
	}
	var gidCfg *config.RemoteServiceConfig[*gidconfig.Config]
	if cfg.ThirdParty != nil {
		gidCfg = cfg.ThirdParty.GID
	}
	gid, err := resolveGID(&o, gidCfg, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after gid resolve failure", "error", cerr)
		}
		return nil, err
	}

	// Platform registry: DB-backed snapshot of apps/accounts/signatures/
	// templates/policies with live provider clients. Vendor accounts are
	// NO LONGER loaded from YAML — manage them via MessageAdminService
	// (or the one-shot `migrate --seed-from-config` importer).
	reg := registry.New(db)
	quotaChecker := quota.NewChecker(redisClient, "msg:quota")

	// Snapshot-convergence cron. jobs.Scheduler owns the cron lifecycle.
	// Module-mode callers may construct Config without going through Load
	// (nil Cron) — cronx.New defaults an empty config.
	cronCfg := cfg.Cron
	if cronCfg == nil {
		cronCfg = &cronx.Config{}
	}
	scheduler, err := jobs.New(&jobs.Deps{Config: cronCfg})
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after scheduler init failure", "error", cerr)
		}
		return nil, err
	}
	if err := scheduler.AddFunc(registryRefreshSpec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Refresh(ctx); err != nil {
			slog.Error("registry cron refresh", "error", err)
		}
	}); err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after scheduler job registration", "error", cerr)
		}
		return nil, err
	}
	mgr.Add("jobs-scheduler", scheduler)

	// option override: option.EmailPersistence / SMSPersistence are
	// module-mode overrides on top of the yaml-loaded defaults. Apply in
	// place — option is override-by-semantics, so modifying cfg is correct.
	if o.EmailPersistence != nil {
		cfg.Email.Persistence = *o.EmailPersistence
	}
	if o.SMSPersistence != nil {
		cfg.SMS.Persistence = *o.SMSPersistence
	}

	svc := &Service{
		cfg:   cfg,
		mgr:   mgr,
		db:    db,
		gid:   gid,
		reg:   reg,
		quota: quotaChecker,
		email: svcemail.New(db, idemChecker, gid, reg, quotaChecker,
			cfg.Email.Persistence, cfg.Email.Attachment),
		sms:       svcsms.New(db, idemChecker, gid, reg, quotaChecker, cfg.SMS.Persistence),
		admin:     admin.New(db, reg, gid),
		tenants:   tenantres.New(db, reg),
		startedAt: time.Now().UnixMilli(),
	}

	return svc, nil
}

// Start starts all owned components concurrently.
func (s *Service) Start() error { return s.mgr.Start() }

// Stop stops all owned components in reverse registration order.
func (s *Service) Stop() error { return s.mgr.Stop() }

// Ping is a health-check RPC. Returns only public, non-sensitive info.
func (s *Service) Ping(_ context.Context) (*commonv1.Pong, error) {
	v := version.Get()
	return &commonv1.Pong{
		Service:   "message-service",
		Version:   v.Version,
		GitCommit: v.GitCommit,
		GitBranch: v.GitBranch,
		BuildTime: v.BuildTime,
		GoVersion: v.GoVersion,
		Status:    "SERVING",
		Now:       time.Now().UnixMilli(),
		StartedAt: s.startedAt,
	}, nil
}

// resolveCaller resolves the caller's tenant context from the trusted
// x-tenant-key — the only credential stack since the ④ window close:
//
//   - the key is format-validated and its config row lazily upserted on
//     first sight (tenantres);
//   - missing or malformed: unauthenticated.
//
// Works identically for gRPC (metadata arrives from the wire) and
// module-mode (caller wrapped ctx via tenantctx.WithTenant).
func (s *Service) resolveCaller(ctx context.Context) (*tenantres.Caller, error) {
	return s.tenants.Require(ctx)
}

// --- facade methods (one per RPC, delegate to subpackage) ---

// SendEmail authenticates the caller, then delegates to the email
// subpackage with the tenant context (policy lookup, quota, idempotency
// namespace all hang off tenant_key).
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	caller, err := s.resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	return s.email.SendEmail(ctx, caller.App, caller.TenantKey, req)
}

// SendSMS authenticates the caller, then delegates to the SMS subpackage.
// See SendEmail.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	caller, err := s.resolveCaller(ctx)
	if err != nil {
		return nil, err
	}
	return s.sms.SendSMS(ctx, caller.App, caller.TenantKey, req)
}

// GetEmail delegates to the message subpackage.
func (s *Service) GetEmail(ctx context.Context, req *pb.GetEmailRequest) (*pb.EmailRecord, error) {
	return s.email.GetEmail(ctx, req)
}

// ListEmails delegates to the message subpackage.
func (s *Service) ListEmails(ctx context.Context, req *pb.ListEmailsRequest) (*pb.ListEmailsResponse, error) {
	return s.email.ListEmails(ctx, req)
}

// ListEmailsByCursor delegates to the message subpackage.
func (s *Service) ListEmailsByCursor(ctx context.Context, req *pb.ListEmailsByCursorRequest) (*pb.ListEmailsByCursorResponse, error) {
	return s.email.ListEmailsByCursor(ctx, req)
}

// GetEmailStats delegates to the message subpackage.
func (s *Service) GetEmailStats(ctx context.Context, req *pb.GetEmailStatsRequest) (*pb.EmailStatsResponse, error) {
	return s.email.GetEmailStats(ctx, req)
}

// GetSMS delegates to the message subpackage.
func (s *Service) GetSMS(ctx context.Context, req *pb.GetSMSRequest) (*pb.SMSRecord, error) {
	return s.sms.GetSMS(ctx, req)
}

// ListSMS delegates to the message subpackage.
func (s *Service) ListSMS(ctx context.Context, req *pb.ListSMSRequest) (*pb.ListSMSResponse, error) {
	return s.sms.ListSMS(ctx, req)
}

// ListSMSByCursor delegates to the message subpackage.
func (s *Service) ListSMSByCursor(ctx context.Context, req *pb.ListSMSByCursorRequest) (*pb.ListSMSByCursorResponse, error) {
	return s.sms.ListSMSByCursor(ctx, req)
}

// GetSMSStats delegates to the message subpackage.
func (s *Service) GetSMSStats(ctx context.Context, req *pb.GetSMSStatsRequest) (*pb.SMSStatsResponse, error) {
	return s.sms.GetSMSStats(ctx, req)
}

// ListSMSRegions delegates to the message subpackage.
func (s *Service) ListSMSRegions(ctx context.Context, req *pb.ListSMSRegionsRequest) (*pb.ListSMSRegionsResponse, error) {
	return s.sms.ListSMSRegions(ctx, req)
}

// --- admin facades (MessageAdminService; internal-network trusted) ---

// CreateTenantConfig registers a tenant's config row and returns the
// plaintext app_secret exactly once.
func (s *Service) CreateTenantConfig(ctx context.Context, req *pb.CreateTenantConfigRequest) (*pb.CreateTenantConfigResponse, error) {
	return s.admin.CreateTenantConfig(ctx, req)
}

// GetTenantConfig returns one tenant config by row id.
func (s *Service) GetTenantConfig(ctx context.Context, req *pb.GetTenantConfigRequest) (*pb.GetTenantConfigResponse, error) {
	return s.admin.GetTenantConfig(ctx, req)
}

// UpdateTenantConfig tweaks config metadata.
func (s *Service) UpdateTenantConfig(ctx context.Context, req *pb.UpdateTenantConfigRequest) (*pb.UpdateTenantConfigResponse, error) {
	return s.admin.UpdateTenantConfig(ctx, req)
}

// RotateTenantConfigSecret invalidates the current secret; new plaintext
// returned exactly once.
func (s *Service) RotateTenantConfigSecret(ctx context.Context, req *pb.RotateTenantConfigSecretRequest) (*pb.RotateTenantConfigSecretResponse, error) {
	return s.admin.RotateTenantConfigSecret(ctx, req)
}

// ListTenantConfigs returns the tenant configs in the caller's scope.
func (s *Service) ListTenantConfigs(ctx context.Context, req *pb.ListTenantConfigsRequest) (*pb.ListTenantConfigsResponse, error) {
	return s.admin.ListTenantConfigs(ctx, req)
}

// DeleteTenantConfig soft-deletes a tenant config.
func (s *Service) DeleteTenantConfig(ctx context.Context, req *pb.DeleteTenantConfigRequest) (*emptypb.Empty, error) {
	return s.admin.DeleteTenantConfig(ctx, req)
}

// CreateChannelAccount adds a vendor account to the platform pool.
func (s *Service) CreateChannelAccount(ctx context.Context, req *pb.CreateChannelAccountRequest) (*pb.CreateChannelAccountResponse, error) {
	return s.admin.CreateChannelAccount(ctx, req)
}

// UpdateChannelAccount edits remark/disabled or replaces credentials.
func (s *Service) UpdateChannelAccount(ctx context.Context, req *pb.UpdateChannelAccountRequest) (*pb.UpdateChannelAccountResponse, error) {
	return s.admin.UpdateChannelAccount(ctx, req)
}

// DeleteChannelAccount soft-deletes an account from the pool.
func (s *Service) DeleteChannelAccount(ctx context.Context, req *pb.DeleteChannelAccountRequest) (*emptypb.Empty, error) {
	return s.admin.DeleteChannelAccount(ctx, req)
}

// ListChannelAccounts returns the full pool (secrets masked).
func (s *Service) ListChannelAccounts(ctx context.Context, req *pb.ListChannelAccountsRequest) (*pb.ListChannelAccountsResponse, error) {
	return s.admin.ListChannelAccounts(ctx, req)
}

// CreateSignature registers a signature with its account bindings.
func (s *Service) CreateSignature(ctx context.Context, req *pb.CreateSignatureRequest) (*pb.CreateSignatureResponse, error) {
	return s.admin.CreateSignature(ctx, req)
}

// UpdateSignature replaces remark/disabled/bindings.
func (s *Service) UpdateSignature(ctx context.Context, req *pb.UpdateSignatureRequest) (*pb.UpdateSignatureResponse, error) {
	return s.admin.UpdateSignature(ctx, req)
}

// DeleteSignature soft-deletes a signature.
func (s *Service) DeleteSignature(ctx context.Context, req *pb.DeleteSignatureRequest) (*emptypb.Empty, error) {
	return s.admin.DeleteSignature(ctx, req)
}

// ListSignatures returns all signatures with bindings.
func (s *Service) ListSignatures(ctx context.Context, req *pb.ListSignaturesRequest) (*pb.ListSignaturesResponse, error) {
	return s.admin.ListSignatures(ctx, req)
}

// CreateTemplate registers a template definition.
func (s *Service) CreateTemplate(ctx context.Context, req *pb.CreateTemplateRequest) (*pb.CreateTemplateResponse, error) {
	return s.admin.CreateTemplate(ctx, req)
}

// UpdateTemplate fully replaces a template definition.
func (s *Service) UpdateTemplate(ctx context.Context, req *pb.UpdateTemplateRequest) (*pb.UpdateTemplateResponse, error) {
	return s.admin.UpdateTemplate(ctx, req)
}

// DeleteTemplate soft-deletes a template.
func (s *Service) DeleteTemplate(ctx context.Context, req *pb.DeleteTemplateRequest) (*emptypb.Empty, error) {
	return s.admin.DeleteTemplate(ctx, req)
}

// ListTemplates filters by app (0 = all) and channel (0 = all).
func (s *Service) ListTemplates(ctx context.Context, req *pb.ListTemplatesRequest) (*pb.ListTemplatesResponse, error) {
	return s.admin.ListTemplates(ctx, req)
}

// CreatePolicy binds (app, channel, scene) to a template + route chains.
func (s *Service) CreatePolicy(ctx context.Context, req *pb.CreatePolicyRequest) (*pb.CreatePolicyResponse, error) {
	return s.admin.CreatePolicy(ctx, req)
}

// UpdatePolicy fully replaces template + route chains.
func (s *Service) UpdatePolicy(ctx context.Context, req *pb.UpdatePolicyRequest) (*pb.UpdatePolicyResponse, error) {
	return s.admin.UpdatePolicy(ctx, req)
}

// DeletePolicy removes the binding; sends fail closed from then on.
func (s *Service) DeletePolicy(ctx context.Context, req *pb.DeletePolicyRequest) (*emptypb.Empty, error) {
	return s.admin.DeletePolicy(ctx, req)
}

// ListPolicies filters by app (0 = all) and channel (0 = all).
func (s *Service) ListPolicies(ctx context.Context, req *pb.ListPoliciesRequest) (*pb.ListPoliciesResponse, error) {
	return s.admin.ListPolicies(ctx, req)
}
