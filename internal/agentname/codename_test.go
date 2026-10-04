package agentname

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestListsDistinct(t *testing.T) {
	for _, l := range [][]string{adjectives, nouns} {
		require.GreaterOrEqual(t, len(l), 100)
		seen := map[string]bool{}
		for _, w := range l {
			require.False(t, seen[w], "duplicate %q", w)
			seen[w] = true
			require.Equal(t, strings.ToLower(w), w)
			require.NotContains(t, w, "-")
		}
	}
}

func TestGenerateCodenameValid(t *testing.T) {
	for i := 0; i < 2000; i++ {
		n := GenerateCodename()
		require.NoError(t, store.ValidateName(n), n)
		require.Len(t, strings.Split(n, "-"), 2)
	}
}

func TestAllCombinationsFitLimit(t *testing.T) {
	for _, a := range adjectives {
		for _, n := range nouns {
			require.LessOrEqual(t, len(a+"-"+n), 32)
		}
	}
}
