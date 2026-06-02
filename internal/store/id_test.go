package store

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewSessionIDFormatAndUniqueness(t *testing.T) {
	a := NewSessionID()
	b := NewSessionID()
	require.Regexp(t, uuidV4Re, a, "must be a v4 UUID")
	require.Regexp(t, uuidV4Re, b)
	require.NotEqual(t, a, b, "two ids must differ")
}
