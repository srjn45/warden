package autopilot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalPathAliasHasOneRunIdentity(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "repo-alias")
	require.NoError(t, os.Symlink(real, alias))

	realPlan := filepath.Join(real, "plans", "nightly.yaml")
	aliasPlan := filepath.Join(alias, "plans", "nightly.yaml")
	require.Equal(t, canonicalPath(realPlan), canonicalPath(aliasPlan),
		"a missing leaf beneath a symlinked ancestor must still canonicalize")
	require.Equal(t, RunID(real, realPlan), RunID(alias, aliasPlan))
}
