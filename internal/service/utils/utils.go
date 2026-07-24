// Package utils holds cross-domain helpers shared by the email and sms
// subpackages under internal/service. Kept as a sibling subpackage (not a
// parent-package util.go) to avoid an import cycle: the service root imports
// both domain subpackages, so they cannot import back into the parent.
//
// Only truly domain-agnostic helpers live here. Business semantics stay in
// the relevant subpackage — e.g. validateSendEmailRequest stays in email,
// validateSendSMSRequest + regionCodePattern stay in sms.
package utils

import "time"

// MaxErrorMessageLen caps the persisted error_message column to match the
// DB schema (model.EmailRecord.ErrorMessage / model.SMSRecord.ErrorMessage
// gorm:"size:1024").
const MaxErrorMessageLen = 1024

// MaxIdempotencyKeyLen matches the proto field's max_len=64 constraint.
// Idempotency is enforced in Redis (see internal/idempotency); the DB no
// longer stores this column. Both channels share the same proto constraint.
const MaxIdempotencyKeyLen = 64

// PersistTimeout bounds the independent-context DB write so a slow DB does
// not leak goroutines past request lifetime. Used by persistEmailRecord /
// persistSMSRecord which deliberately decouple from the RPC ctx so client
// cancellation does not lose FAILED records.
const PersistTimeout = 3 * time.Second

// TruncateErrorMessage caps vendor error strings so a multi-KB HTML error
// page doesn't bloat the DB row. The model column is varchar(1024).
func TruncateErrorMessage(s string) string {
	if len(s) <= MaxErrorMessageLen {
		return s
	}
	return s[:MaxErrorMessageLen]
}
