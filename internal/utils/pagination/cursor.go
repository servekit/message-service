// Package pagination encodes and decodes opaque page cursors for the
// ListEmailsByCursor / ListSMSByCursor RPCs.
//
// The token carries the last row's (id, created_at) so that ORDER BY
// created_at pages without dropping or duplicating rows under tied
// timestamps. The on-wire format is `v1.<base64url(json)>`; the version
// prefix lets future formats coexist with already-issued tokens.
package pagination

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/servekit/go-common/jsonx"
)

// PageCursor is the in-memory shape of a page token.
type PageCursor struct {
	ID        int64  `json:"id"`
	CreatedAt string `json:"ca,omitempty"` // RFC3339Nano
}

// cursorVersion is the prefix tag for the current encoding. Bumping this
// constant requires a migration plan: DecodePageCursor must keep accepting
// tokens issued under the previous version (or issue a fresh first-page
// request) until all in-flight cursors have expired.
const cursorVersion = "v1"

// EncodePageCursor serializes a cursor into an opaque token.
func EncodePageCursor(c PageCursor) string {
	payload, err := jsonx.Marshal(c)
	if err != nil {
		// PageCursor only holds primitive types; jsonx.Marshal cannot fail
		// in practice. Panic so any future field-type change that breaks
		// this invariant surfaces immediately, rather than silently
		// producing a token that decodes to a zero-ID cursor.
		panic("pagination: PageCursor marshal failed: " + err.Error())
	}
	return cursorVersion + "." + base64.RawURLEncoding.EncodeToString(payload)
}

// DecodePageCursor parses a token produced by EncodePageCursor.
func DecodePageCursor(token string) (PageCursor, error) {
	if token == "" {
		return PageCursor{}, fmt.Errorf("empty page token")
	}
	if !strings.HasPrefix(token, cursorVersion+".") {
		return PageCursor{}, fmt.Errorf("malformed page token")
	}
	payload := strings.TrimPrefix(token, cursorVersion+".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return PageCursor{}, fmt.Errorf("decode page token base64: %w", err)
	}
	var c PageCursor
	if err := jsonx.Unmarshal(raw, &c); err != nil {
		return PageCursor{}, fmt.Errorf("decode page token json: %w", err)
	}
	return c, nil
}

// CursorFromCreatedAt formats a time.Time into the RFC3339Nano string
// carried by PageCursor.CreatedAt. Returns "" for the zero time so the
// field is omitted from the encoded token.
func CursorFromCreatedAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// CursorToCreatedAt reverses CursorFromCreatedAt.
func CursorToCreatedAt(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
