// Package xcodes defines message-service error codes.
//
// Only codes actually used by the codebase are defined — add new ones when
// business code requires them (YAGNI). Defined locally via xerr.New so the
// package is self-contained; errors.Is still works across packages because
// xerr.Error.Is compares by reason string.
package xcodes

import "github.com/servekit/go-common/xerr"

// ErrBadRequest indicates the caller passed an invalid request (vendor/
// account mismatch, missing required field, etc.).
var ErrBadRequest = xerr.New("BAD_REQUEST", xerr.CategoryBadRequest, 400, "bad request")

// ErrInternal indicates an unexpected internal failure (DB error,
// serialization failure, etc.).
var ErrInternal = xerr.New("INTERNAL_ERROR", xerr.CategoryInternal, 500, "internal server error")

// ErrEmailNotFound indicates no email record matches the requested ID.
var ErrEmailNotFound = xerr.New("EMAIL_NOT_FOUND", xerr.CategoryNotFound, 404, "email record not found")

// ErrSMSNotFound indicates no SMS record matches the requested ID.
var ErrSMSNotFound = xerr.New("SMS_NOT_FOUND", xerr.CategoryNotFound, 404, "sms record not found")

// ErrMessageSendFailed indicates the underlying provider rejected the send
// request after exhausting fallback chain.
var ErrMessageSendFailed = xerr.New("MESSAGE_SEND_FAILED", xerr.CategoryInternal, 500, "message send failed")

// ErrSMSRoutesNotConfigured indicates the deployment has no SMS routing
// configured (no default routes and the caller did not pick vendor+account).
// This is a server-side configuration gap, not a caller error — hence 503,
// letting callers back off or alert instead of "fixing" their request.
var ErrSMSRoutesNotConfigured = xerr.New(
	"SMS_ROUTES_NOT_CONFIGURED",
	xerr.CategoryServiceUnavailable,
	503,
	"sms routes not configured; specify vendor and account explicitly",
)

// ErrPersistenceDisabled indicates the caller invoked a query method on a
// channel whose persistence has been disabled in config. The send path still
// works (vendor call + Redis idempotency, which is always on regardless of
// the persistence toggle); only Get/List/Stats return this error.
var ErrPersistenceDisabled = xerr.New(
	"PERSISTENCE_DISABLED",
	xerr.CategoryServiceUnavailable,
	503,
	"persistence is disabled for this channel",
)

// ErrIdempotencyConflict indicates a send with the same idempotency_key is
// currently in flight (another caller holds the Redis reservation). Caller
// can retry the same request after the in-flight call completes or its TTL
// expires.
var ErrIdempotencyConflict = xerr.New(
	"IDEMPOTENCY_CONFLICT",
	xerr.CategoryConflict,
	409,
	"idempotency_key is in flight",
)

// ErrInvalidAttachment indicates an attachment in SendEmailRequest failed
// validation (kind UNSPECIFIED, empty url, empty filename, or conflicting
// inline settings).
var ErrInvalidAttachment = xerr.New(
	"INVALID_ATTACHMENT",
	xerr.CategoryBadRequest,
	400,
	"invalid attachment",
)

// ErrAttachmentTooLarge indicates a MIME-mode attachment exceeded the
// configured max_bytes after download (or pre-download when size_bytes was
// provided and exceeded).
var ErrAttachmentTooLarge = xerr.New(
	"ATTACHMENT_TOO_LARGE",
	xerr.CategoryBadRequest,
	413,
	"attachment exceeds size limit",
)

// ErrAttachmentFetchFailed indicates the HTTP GET against the attachment URL
// failed (non-2xx status, timeout, transport error). Network-side failure;
// caller may retry.
var ErrAttachmentFetchFailed = xerr.New(
	"ATTACHMENT_FETCH_FAILED",
	xerr.CategoryInternal,
	502,
	"failed to fetch attachment",
)

// ErrAppUnauthorized indicates the request carried no trusted credential
// (x-tenant-key), a malformed one, or a disabled config row. Policy-driven
// sends require an authenticated tenant identity.
var ErrAppUnauthorized = xerr.New(
	"APP_UNAUTHORIZED",
	xerr.CategoryUnauthorized,
	401,
	"missing or invalid app credentials",
)

// ErrPolicyNotFound indicates no enabled policy is configured for the
// calling app + channel + scene. The message lists the app's configured
// scenes so the caller can self-correct. Fail-closed by design — no silent
// default route.
var ErrPolicyNotFound = xerr.New(
	"POLICY_NOT_FOUND",
	xerr.CategoryNotFound,
	404,
	"no send policy configured for this app + scene",
)

// ErrDailyQuotaExceeded indicates the app hit its per-channel daily
// send-attempt cap. Attempts (not deliveries) are counted; the counter
// resets at UTC midnight.
var ErrDailyQuotaExceeded = xerr.New(
	"DAILY_QUOTA_EXCEEDED",
	xerr.CategoryTooManyRequests,
	429,
	"daily send quota exceeded",
)

// ErrTemplateParamMissing indicates template_params is missing a parameter
// the template declares as required.
var ErrTemplateParamMissing = xerr.New(
	"TEMPLATE_PARAM_MISSING",
	xerr.CategoryBadRequest,
	400,
	"required template parameter missing",
)

// --- admin resource errors ---

// ErrUnauthorized indicates the admin surface was reached without any
// trusted identity: no injected tenant key and no verified actor (phase ④
// T5 closure — the internal-network-only window is closed).
var ErrUnauthorized = xerr.New("UNAUTHORIZED", xerr.CategoryUnauthorized, 401, "management plane requires a trusted identity")

// ErrForbidden indicates the caller's resolved scope may not perform the
// operation (e.g. a tenant-scoped caller writing a platform-pool resource,
// or a non-platform actor on a platform-only surface).
var ErrForbidden = xerr.New("FORBIDDEN", xerr.CategoryForbidden, 403, "operation outside caller scope")

// ErrAppNotFound indicates no app matches the requested ID.
var ErrAppNotFound = xerr.New("APP_NOT_FOUND", xerr.CategoryNotFound, 404, "app not found")

// ErrChannelAccountNotFound indicates no channel account matches the ID.
var ErrChannelAccountNotFound = xerr.New(
	"CHANNEL_ACCOUNT_NOT_FOUND",
	xerr.CategoryNotFound,
	404,
	"channel account not found",
)

// ErrSignatureNotFound indicates no signature matches the ID.
var ErrSignatureNotFound = xerr.New(
	"SIGNATURE_NOT_FOUND",
	xerr.CategoryNotFound,
	404,
	"signature not found",
)

// ErrTemplateNotFound indicates no template matches the ID.
var ErrTemplateNotFound = xerr.New(
	"TEMPLATE_NOT_FOUND",
	xerr.CategoryNotFound,
	404,
	"template not found",
)

// ErrInvalidTemplateContent indicates the template definition failed
// validation (content shape does not match channel/kind, missing vendor
// code, etc.).
var ErrInvalidTemplateContent = xerr.New(
	"INVALID_TEMPLATE_CONTENT",
	xerr.CategoryBadRequest,
	400,
	"invalid template definition",
)

// ErrSignatureNotBound indicates a policy route pairs a signature with an
// account it is not registered (报备) on.
var ErrSignatureNotBound = xerr.New(
	"SIGNATURE_NOT_BOUND",
	xerr.CategoryBadRequest,
	400,
	"signature is not bound to the route account",
)
