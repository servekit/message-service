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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gorm.io/gorm"

	pb "github.com/servekit/message-service/gen/message/v1"
	"github.com/servekit/message-service/internal/idempotency"
	provemail "github.com/servekit/message-service/internal/provider/email"
	provesms "github.com/servekit/message-service/internal/provider/sms"
	svcemail "github.com/servekit/message-service/internal/service/email"
	svcsms "github.com/servekit/message-service/internal/service/sms"
	"github.com/servekit/message-service/internal/version"
	"github.com/servekit/message-service/pkg/config"
	"github.com/servekit/message-service/pkg/option"
	"github.com/servekit/message-service/pkg/thirdcall"

	"github.com/servekit/go-common/lifecycle"
)

// Service holds message-service business state: one subpackage instance per
// channel (email + sms). The root itself only does resource resolve and
// one-line RPC delegation; business logic lives in the subpackages.
type Service struct {
	cfg *config.Config
	mgr *lifecycle.Manager

	db  *gorm.DB
	gid thirdcall.GIDService

	email *svcemail.Service
	sms   *svcsms.Service

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
	redisClient, err := resolveRedis(cfg, o.Redis, mgr)
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

	db, err := resolveDB(cfg, o.DB, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after db resolve failure", "error", cerr)
		}
		return nil, err
	}
	gid, err := resolveGID(cfg, o.GIDService, mgr)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after gid resolve failure", "error", cerr)
		}
		return nil, err
	}

	smsCfg, err := cookSMSConfig(cfg.SMS)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after sms config cook failure", "error", cerr)
		}
		return nil, fmt.Errorf("sms config: %w", err)
	}

	emailRegistry, err := provemail.NewAccountRegistry(cfg.Email.Config)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after email registry failure", "error", cerr)
		}
		return nil, fmt.Errorf("email registry: %w", err)
	}
	smsRegistry, err := provesms.NewAccountRegistry(smsCfg)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after sms registry failure", "error", cerr)
		}
		return nil, fmt.Errorf("sms registry: %w", err)
	}
	smsRouter, err := provesms.BuildRouter(smsCfg, smsRegistry)
	if err != nil {
		if cerr := mgr.Stop(); cerr != nil {
			slog.Error("rollback after sms router failure", "error", cerr)
		}
		return nil, fmt.Errorf("sms router: %w", err)
	}

	// option override: option.EmailPersistence / SMSPersistence are
	// module-mode overrides on top of the yaml-loaded defaults. Apply in
	// place — option is override-by-semantics, so modifying cfg is correct.
	if o.EmailPersistence != nil {
		cfg.Email.Persistence = *o.EmailPersistence
	}
	if o.SMSPersistence != nil {
		cfg.SMS.Persistence = *o.SMSPersistence
	}

	httpClient := &http.Client{
		Timeout: cfg.Email.Attachment.FetchTimeoutDuration(),
		// Do not follow redirects — mitigates SSRF (a malicious or compromised
		// attachment endpoint could otherwise redirect the fetch to internal
		// addresses such as the cloud metadata service). Attachment URLs are
		// expected to be direct (OSS pre-signed).
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	svc := &Service{
		cfg: cfg,
		mgr: mgr,
		db:  db,
		gid: gid,
		email: svcemail.New(db, idemChecker, gid, emailRegistry,
			cfg.Email.Persistence, cfg.Email.Attachment, httpClient),
		sms: svcsms.New(db, idemChecker, gid, smsRegistry, smsRouter, cfg.SMS.Persistence),
		startedAt: time.Now().UnixMilli(),
	}

	return svc, nil
}

// Start starts all owned components concurrently.
func (s *Service) Start() error { return s.mgr.Start() }

// Stop stops all owned components in reverse registration order.
func (s *Service) Stop() error { return s.mgr.Stop() }

// Ping is a health-check RPC. Returns only public, non-sensitive info.
func (s *Service) Ping(ctx context.Context) (*pb.Pong, error) {
	v := version.Get()
	return &pb.Pong{
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

// --- facade methods (one per RPC, delegate to subpackage) ---

// SendEmail delegates to the message subpackage.
func (s *Service) SendEmail(ctx context.Context, req *pb.SendEmailRequest) (*pb.SendResponse, error) {
	return s.email.SendEmail(ctx, req)
}

// SendSMS delegates to the message subpackage.
func (s *Service) SendSMS(ctx context.Context, req *pb.SendSMSRequest) (*pb.SendResponse, error) {
	return s.sms.SendSMS(ctx, req)
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

// ListEmailSenders delegates to the message subpackage.
func (s *Service) ListEmailSenders(ctx context.Context, req *pb.ListEmailSendersRequest) (*pb.ListEmailSendersResponse, error) {
	return s.email.ListEmailSenders(ctx, req)
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

// ListSMSSenders delegates to the message subpackage.
func (s *Service) ListSMSSenders(ctx context.Context, req *pb.ListSMSSendersRequest) (*pb.ListSMSSendersResponse, error) {
	return s.sms.ListSMSSenders(ctx, req)
}
