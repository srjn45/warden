package agentname

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestDisambiguate(t *testing.T) {
	require.Equal(t, "foo", Disambiguate("foo", nil))
	require.Equal(t, "foo", Disambiguate("foo", map[string]bool{"bar": true}))
	require.Equal(t, "foo-2", Disambiguate("foo", map[string]bool{"foo": true}))
	require.Equal(t, "foo-3", Disambiguate("foo", map[string]bool{"foo": true, "foo-2": true}))
}

func TestDisambiguateTruncates(t *testing.T) {
	long := strings.Repeat("a", 32)
	got := Disambiguate(long, map[string]bool{long: true})
	require.Equal(t, strings.Repeat("a", 30)+"-2", got)
	require.NoError(t, store.ValidateName(got))
}

func TestDisambiguateEmptyOrInvalid(t *testing.T) {
	for _, c := range []string{"", "  ", "has space", strings.Repeat("x", 40)} {
		got := Disambiguate(c, nil)
		require.NoError(t, store.ValidateName(got), c)
	}
}

func TestDisambiguatePrefixed(t *testing.T) {
	got := Disambiguate("wkr:task-1", map[string]bool{"wkr:task-1": true})
	require.Equal(t, "wkr:task-1-2", got)
	require.NoError(t, store.ValidateName(got))
}
