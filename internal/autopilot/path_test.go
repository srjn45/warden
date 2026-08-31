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

func TestEnableStoresTreatAliasesAsOneRepo(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "repo-alias")
	require.NoError(t, os.Symlink(real, alias))

	for _, store := range []EnableStore{newMemEnableStore(), newFSEnableStore(filepath.Join(t.TempDir(), "enabled"))} {
		require.NoError(t, store.Enable(alias))
		require.True(t, store.IsEnabled(real))
		require.Equal(t, []string{canonicalPath(real)}, store.List())
		require.NoError(t, store.Disable(real))
		require.False(t, store.IsEnabled(alias))
	}
}

func TestFSEnableStoreReadsAndRemovesLegacyAliasMarker(t *testing.T) {
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "repo-alias")
	require.NoError(t, os.Symlink(real, alias))
	store := newFSEnableStore(filepath.Join(t.TempDir(), "enabled"))
	require.NoError(t, os.MkdirAll(store.dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(store.dir, enableMarker(alias)), []byte(alias), 0o644))

	require.True(t, store.IsEnabled(real), "pre-canonicalization markers remain readable")
	require.Equal(t, []string{canonicalPath(real)}, store.List())
	require.NoError(t, store.Disable(real))
	require.False(t, store.IsEnabled(alias))
}
