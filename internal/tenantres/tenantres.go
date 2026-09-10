// Package tenantres resolves the send path's calling identity. Since the
// ④ window close the trusted x-tenant-key (injected by the portal proxy)
// is the ONLY credential stack:
//
//   - the key is format-validated first (tenantctx.ValidTenantKey — the
//     canonical ten_[0-9a-z]{12} shape plus the reserved ten_platform /
//     ten_legacy literals); malformed keys fail closed before any lookup
//     or lazy create;
//   - the per-tenant config row (message_apps row carrying the daily
//     limits) is resolved by the tenant_key mapping and lazily upserted
//     on first sight with default (unlimited) limits.
//
// Verification is service-layer (not an interceptor) so module-mode
// in-process callers share the same path as gRPC clients.
package tenantres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"github.com/servekit/go-common/tenantctx"

	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"
)

// Caller is the resolved send-path identity: the tenant context every
// downstream keying hangs off (policy lookup, daily quota, idempotency
// namespace) plus the config row that owns the caller's daily limits.
type Caller struct {
	// TenantKey is the authoritative tenant context ("ten_..." or one of
	// the reserved literals).
	TenantKey string
	// App is the tenant's (possibly just-created) config row.
	App *models.MessageApp
}

// Resolver resolves callers. Reads go through the registry snapshot; only
// the first-sight trusted path touches the DB (and refreshes the snapshot so
// subsequent sends stay lock-free).
type Resolver struct {
	db  *gorm.DB
	reg *registry.Registry
}

// New constructs a Resolver.
func New(db *gorm.DB, reg *registry.Registry) *Resolver {
	return &Resolver{db: db, reg: reg}
}

// Require resolves the Caller from the trusted x-tenant-key, failing closed
// with ErrAppUnauthorized when the credential is missing or malformed, or
// the resolved config row is unknown or disabled. Call at the entry of
// every credential-presenting surface (SendEmail / SendSMS).
func (r *Resolver) Require(ctx context.Context) (*Caller, error) {
	tenantKey, ok := tenantctx.TrustedKeyFromIncoming(ctx)
	if !ok {
		return nil, xcodes.ErrAppUnauthorized.New(
			"missing trusted caller credential (x-tenant-key metadata)")
	}
	if !tenantctx.ValidTenantKey(tenantKey) {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("malformed x-tenant-key %q", tenantKey))
	}
	return r.ensureTrusted(ctx, tenantKey)
}

// ensureTrusted resolves the tenant's config row through the snapshot; on a
// miss it falls back to the DB (covers rows written by other nodes and
// un-backfilled app_key-equal rows) and finally lazily creates the default
// config row. The minted secret satisfies the not-null column without being
// handed to anyone — trusted callers authenticate by network position, never
// by secret. A disabled config row fails closed.
func (r *Resolver) ensureTrusted(ctx context.Context, tenantKey string) (*Caller, error) {
	if app := r.reg.Current().AppByTenant(tenantKey); app != nil {
		if app.Disabled {
			return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("unknown or disabled app %q", tenantKey))
		}
		return &Caller{TenantKey: tenantKey, App: app}, nil
	}

	app, err := dal.GetAppForTenant(ctx, r.db, tenantKey)
	if err != nil {
		return nil, err
	}
	if app == nil {
		secret, mintErr := mintTenantSecret()
		if mintErr != nil {
			return nil, xcodes.ErrInternal.Wrap(mintErr)
		}
		if err := dal.EnsureTenantApp(ctx, r.db, &models.MessageApp{
			AppKey:    tenantKey,
			TenantKey: models.TenantKeyPtr(tenantKey),
			AppSecret: secret,
			Name:      tenantKey,
			// daily limits default 0 = unlimited (recipe step 4)
		}); err != nil {
			return nil, err
		}
		app, err = dal.GetAppForTenant(ctx, r.db, tenantKey)
		if err != nil {
			return nil, err
		}
		if app == nil {
			return nil, xcodes.ErrInternal.New("ensure tenant config row: row absent after insert")
		}
		// converge the snapshot so subsequent sends resolve lock-free (the
		// cron refresh would converge within a minute regardless; a refresh
		// failure must not fail the send — the row is committed)
		if err := r.reg.Refresh(ctx); err != nil {
			slog.Warn("tenantres: snapshot refresh after first-sight config row (cron will converge)", "error", err)
		}
	}
	if app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("unknown or disabled app %q", tenantKey))
	}
	return &Caller{TenantKey: tenantKey, App: app}, nil
}

// mintTenantSecret mints "msg_" + 32 random bytes (base64url) — same shape
// as admin-minted app secrets; never handed to anyone (trusted callers
// authenticate by network position).
func mintTenantSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("mint tenant secret: %w", err)
	}
	return "msg_" + base64.RawURLEncoding.EncodeToString(buf), nil
}
