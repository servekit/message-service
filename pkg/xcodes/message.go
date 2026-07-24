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
