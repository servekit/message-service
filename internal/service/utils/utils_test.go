package utils

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTruncateErrorMessage_Short(t *testing.T) {
	assert.Equal(t, "hello", TruncateErrorMessage("hello"))
}

func TestTruncateErrorMessage_Long(t *testing.T) {
	long := strings.Repeat("x", 2000)
	got := TruncateErrorMessage(long)
	assert.Equal(t, MaxErrorMessageLen, len(got))
}

func TestTruncateErrorMessage_ExactBoundary(t *testing.T) {
	// Length equal to the cap should be returned unchanged.
	exact := strings.Repeat("y", MaxErrorMessageLen)
	assert.Equal(t, exact, TruncateErrorMessage(exact))
}

func TestTruncateErrorMessage_OneOverBoundary(t *testing.T) {
	over := strings.Repeat("z", MaxErrorMessageLen+1)
	got := TruncateErrorMessage(over)
	assert.Equal(t, MaxErrorMessageLen, len(got))
}
