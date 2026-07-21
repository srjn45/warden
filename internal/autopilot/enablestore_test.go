package autopilot

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// enableStoreContract exercises the behavior every EnableStore impl must share, so
// the filesystem store and the in-memory double stay interchangeable.
func enableStoreContract(t *testing.T, s EnableStore) {
	t.Helper()
	require.Empty(t, s.List())
	require.False(t, s.IsEnabled("/repo/a"))

	require.NoError(t, s.Enable("/repo/a"))
	require.True(t, s.IsEnabled("/repo/a"))
	require.Equal(t, []string{"/repo/a"}, s.List())

	// Idempotent enable + a second repo; List is sorted.
	require.NoError(t, s.Enable("/repo/a"))
	require.NoError(t, s.Enable("/repo/b"))
	require.Equal(t, []string{"/repo/a", "/repo/b"}, s.List())

	// Blank repo is a no-op, never an error.
	require.NoError(t, s.Enable(""))
	require.Equal(t, []string{"/repo/a", "/repo/b"}, s.List())

	// Disable is scoped and idempotent — disabling an unknown repo is not an error.
	require.NoError(t, s.Disable("/repo/a"))
	require.False(t, s.IsEnabled("/repo/a"))
	require.True(t, s.IsEnabled("/repo/b"))
	require.Equal(t, []string{"/repo/b"}, s.List())
	require.NoError(t, s.Disable("/repo/a"))
	require.NoError(t, s.Disable("/nope"))
	require.Equal(t, []string{"/repo/b"}, s.List())
}

func TestMemEnableStoreContract(t *testing.T) {
	enableStoreContract(t, newMemEnableStore())
}

func TestFSEnableStoreContract(t *testing.T) {
	enableStoreContract(t, newFSEnableStore(filepath.Join(t.TempDir(), "autopilot", "enabled")))
}

// TestFSEnableStorePersists proves the marker files survive a fresh store over the
// same directory — the daemon-restart guarantee.
func TestFSEnableStorePersists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "autopilot", "enabled")
	s1 := newFSEnableStore(dir)
	require.NoError(t, s1.Enable("/work/repo"))

	// A brand-new store over the same dir sees the persisted set.
	s2 := newFSEnableStore(dir)
	require.True(t, s2.IsEnabled("/work/repo"))
	require.Equal(t, []string{"/work/repo"}, s2.List())
}

// TestNewEnableStoreSelectsImpl proves the factory picks a filesystem store when a
// data dir is given and the in-memory fallback otherwise.
func TestNewEnableStoreSelectsImpl(t *testing.T) {
	require.IsType(t, &memEnableStore{}, newEnableStore(""))
	require.IsType(t, &memEnableStore{}, newEnableStore("   "))
	require.IsType(t, &fsEnableStore{}, newEnableStore(t.TempDir()))
}

// TestEnableMarkerStable proves the marker name is a stable, non-empty hash of the
// repo root (mirrors the RunID hashing style).
func TestEnableMarkerStable(t *testing.T) {
	a := enableMarker("/repo/x")
	require.Equal(t, a, enableMarker("/repo/x"))
	require.NotEqual(t, a, enableMarker("/repo/y"))
	require.NotEmpty(t, a)
}
