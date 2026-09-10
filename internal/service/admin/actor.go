// Actor-scope helpers for the admin surface (tenant platform phase ④ T5 —
// server-side closure): every admin RPC resolves the caller's scope BEFORE
// any data leaves the service, mirroring portal-service's
// internal/service/admin/actor.go shape.
//
// The two trusted identity inputs are the ones the doors plant:
//
//   - x-tenant-key (tenantctx): the management-plane tenant choice the
//     testkit gate resolved and injected. Present → the caller is pinned to
//     that tenant (TENANT_ADMIN self-service and PLATFORM drill-down alike —
//     the door only injects validated choices).
//   - x-actor (grpcx): the verified console identity. A PLATFORM actor with
//     NO injection is the cross-view ("drill everywhere") posture.
//
// Anything else — a TENANT_ADMIN whose choice never resolved, an END_USER, a
// caller with neither identity — fails closed: the admin surface used to be
// internal-network trust only; that window closes here. Row-level denials
// answer in the domain not-found style (anti-enumeration, aligned with the
// ErrUserNotFound ruling), except platform-pool rows which stay visible and
// are refused with Forbidden (read-only for tenants, matching the console).
package admin

import (
	"context"

	userv1 "github.com/servekit/api/gen/go/user/v1"
	"github.com/servekit/go-common/grpcx"
	"github.com/servekit/go-common/tenantctx"
	"github.com/servekit/message-service/internal/store/models"
	"github.com/servekit/message-service/pkg/xcodes"
)

// scopeFromCtx resolves the caller's admin scope:
//
//	("", nil)  → PLATFORM cross-view (full pool, no injection)
//	(key, nil) → pinned to the injected tenant key
//	("", err)  → fail closed (no trusted identity at all)
//
// The injected key outranks the actor: a PLATFORM drill-down is scoped
// exactly like the tenant it is inspecting.
func scopeFromCtx(ctx context.Context) (string, error) {
	if key, ok := tenantctx.TenantKeyFromCtx(ctx); ok {
		return key, nil
	}
	actor, err := grpcx.MustActorFromCtx(ctx)
	if err != nil {
		return "", xcodes.ErrUnauthorized.New()
	}
	if userv1.UserType(actor.GetUserType()) != userv1.UserType_USER_TYPE_PLATFORM {
		return "", xcodes.ErrForbidden.New("management plane requires a platform actor or a tenant injection")
	}
	return "", nil
}

// requirePlatformScope admits only the PLATFORM cross-view (no injected
// key). For surfaces with no tenant dimension this is the whole closure:
// a tenant-scoped caller is refused outright, and no identity fails closed.
func requirePlatformScope(ctx context.Context) error {
	scope, err := scopeFromCtx(ctx)
	if err != nil {
		return err
	}
	if scope != "" {
		return xcodes.ErrForbidden.New("platform operators only")
	}
	return nil
}

// visibleInTenant reports whether a row in the two-layer resource domain is
// LISTABLE by the scope: the platform pool (tenant_key NULL / "") plus the
// scope's own private rows. The cross-view ("") sees everything.
func visibleInTenant(scope, rowTenant string) bool {
	if scope == "" {
		return true
	}
	return rowTenant == "" || rowTenant == scope
}

// authorizeManageTenant is the write-side rule for one row: the cross-view
// manages everything; a scoped caller manages only rows they own. The
// platform pool is visible but read-only for tenants (Forbidden — the row's
// existence is no secret), while a foreign tenant's row answers with the
// caller-supplied not-found error so existence never leaks
// (anti-enumeration, the ErrUserNotFound ruling).
func authorizeManageTenant(scope, rowTenant string, notFound error) error {
	if scope == "" {
		return nil
	}
	switch rowTenant {
	case scope:
		return nil
	case "":
		return xcodes.ErrForbidden.New("platform pool resources are read-only for tenants")
	default:
		return notFound
	}
}

// authorizeManageRow is authorizeManageTenant over a nullable column.
func authorizeManageRow(scope string, rowTenant *string, notFound error) error {
	return authorizeManageTenant(scope, models.TenantKeyOf(rowTenant), notFound)
}

// clampTenantKey applies the create-side rule: a scoped caller's rows are
// stamped with the injected key no matter what the request body claimed
// (empty = would-be platform pool → also clamped; pool writes are
// PLATFORM-only). The cross-view keeps its explicit target as given.
func clampTenantKey(scope, requested string) string {
	if scope == "" {
		return requested
	}
	return scope
}
