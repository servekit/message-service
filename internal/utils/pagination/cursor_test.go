package pagination

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecode_RoundTrip(t *testing.T) {
	c := PageCursor{
		ID:        12345,
		CreatedAt: "2026-06-23T10:00:00.123456789Z",
	}
	token := EncodePageCursor(c)
	decoded, err := DecodePageCursor(token)
	require.NoError(t, err)
	assert.Equal(t, c.ID, decoded.ID)
	assert.Equal(t, c.CreatedAt, decoded.CreatedAt)
}

func TestDecodePageCursor_Empty(t *testing.T) {
	_, err := DecodePageCursor("")
	require.Error(t, err)
}

func TestDecodePageCursor_Malformed(t *testing.T) {
	_, err := DecodePageCursor("v1.not-base64-!!!")
	require.Error(t, err)
}

// TestDecodePageCursor_UnsupportedVersion verifies that the version-prefix
// machinery rejects tokens with a future version tag. Bumping cursorVersion
// requires shipping a decoder that still accepts the prior version.
func TestDecodePageCursor_UnsupportedVersion(t *testing.T) {
	token := "v2." + base64.RawURLEncoding.EncodeToString([]byte(`{}`))
	_, err := DecodePageCursor(token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed page token")
}

func TestCursorFromCreatedAt_ZeroTime(t *testing.T) {
	assert.Equal(t, "", CursorFromCreatedAt(time.Time{}))
}

func TestCursorRoundTrip_CreatedAt(t *testing.T) {
	original := time.Date(2026, 6, 23, 10, 0, 0, 123456789, time.UTC)
	s := CursorFromCreatedAt(original)
	assert.NotEmpty(t, s)
	back := CursorToCreatedAt(s)
	assert.True(t, original.Equal(back))
}

func TestCursorToCreatedAt_Empty(t *testing.T) {
	assert.True(t, CursorToCreatedAt("").IsZero())
}
