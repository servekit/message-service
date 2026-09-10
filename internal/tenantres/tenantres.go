// Package tenantres resolves the send path's calling identity during the
// phase ③ dual-stack window (recipe step 1, rule D-③1):
//
//   - SourceTrusted (x-tenant-key, injected by the portal proxy): the tenant
//     key IS the tenant context. The per-tenant config row (message_apps row
//     carrying the daily limits) is resolved by the tenant_key mapping and
//     lazily upserted on first sight with default (unlimited) limits.
//   - SourceLegacy (x-app-key/x-app-secret): the pre-③ app validation,
//     unchanged; the validated app is then converted to the tenant its row
//     maps to (tenant_key column; empty column falls back to the app_key
//     literal — T10 总装 clears the empties).
//   - SourceNone: unauthenticated, fail closed.
//
// The whole package (and the legacy half of appauth) is deleted when the
// window closes (phase ④).
package tenantres

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"

	"gorm.io/gorm"

	"github.com/servekit/go-common/dualauth"

	"github.com/servekit/message-service/internal/appauth"
	"github.com/servekit/message-service/internal/registry"
	"github.com/servekit/message-service/internal/store/dal"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"
)

// Caller is the resolved send-path identity: the tenant context every
// downstream keying hangs off (policy lookup, daily quota, idempotency
// namespace) plus the config row that owns the caller's daily limits.
type Caller struct {
	// TenantKey is the authoritative tenant context ("ten_..." or a legacy
	// app_key literal through the window).
	TenantKey string
	// App is the config row: the validated app on the legacy path, the
	// tenant's (possibly just-created) row on the trusted path.
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

// Require classifies the caller's credential stack (appauth.Resolve, D-③1)
// and resolves the Caller, failing closed with ErrAppUnauthorized on missing
// credentials, unknown/disabled apps, or a bad secret. Call at the entry of
// every credential-presenting surface (SendEmail / SendSMS).
func (r *Resolver) Require(ctx context.Context) (*Caller, error) {
	tenantKey, appKey, appSecret, source := appauth.Resolve(ctx)
	switch source {
	case dualauth.SourceTrusted:
		return r.ensureTrusted(ctx, tenantKey)
	case dualauth.SourceLegacy:
		return r.verifyLegacy(ctx, appKey, appSecret)
	default:
		return nil, xcodes.ErrAppUnauthorized.New(
			"missing caller credentials (x-tenant-key or x-app-key / x-app-secret metadata)")
	}
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

// verifyLegacy is the pre-③ appauth path, kept verbatim through the
// dual-stack window: unknown/disabled app or a bad secret fails closed (the
// registry snapshot is the read path, same as pre-③ — the cron converges
// cross-node writes). The validated app is then converted to its mapped
// tenant_key (empty column → app_key literal fallback; T10 总装 clears the
// empties).
func (r *Resolver) verifyLegacy(_ context.Context, appKey, appSecret string) (*Caller, error) {
	app := r.reg.Current().App(appKey)
	if app == nil || app.Disabled {
		return nil, xcodes.ErrAppUnauthorized.New(fmt.Sprintf("unknown or disabled app %q", appKey))
	}
	if app.AppSecret != appSecret {
		return nil, xcodes.ErrAppUnauthorized.New("invalid app secret")
	}
	return &Caller{TenantKey: models.AppTenantKey(app), App: app}, nil
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
