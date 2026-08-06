package daemon

import (
	"testing"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

// newBackendStore opens a fresh registry seeded with rows for defaultBackend tests.
func newBackendStore(t *testing.T, rows ...backendstore.Backend) *backendstore.Store {
	t.Helper()
	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { bs.Close() })
	for _, b := range rows {
		require.NoError(t, bs.Upsert(b))
	}
	return bs
}

func TestDefaultBackendNilStore(t *testing.T) {
	s := &Server{}
	require.Equal(t, "", s.defaultBackend(), "no registry ⇒ keep the compile-time claude default")
}

func TestDefaultBackendNoDefaultSet(t *testing.T) {
	s := &Server{backends: newBackendStore(t,
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree},
	)}
	require.Equal(t, "", s.defaultBackend(), "no row flagged default ⇒ keep claude")
}

func TestDefaultBackendOverridesClaude(t *testing.T) {
	s := &Server{backends: newBackendStore(t,
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierSubscription, Default: true},
	)}
	require.Equal(t, "codex", s.defaultBackend(), "an installed+enabled default overrides claude")
}

func TestDefaultBackendDriftedDefaultFallsBackToClaude(t *testing.T) {
	// Default was set, then the backend was uninstalled: fall back to claude
	// rather than failing every unspecified-backend spawn.
	s := &Server{backends: newBackendStore(t,
		backendstore.Backend{ID: "codex", Installed: false, Enabled: true, Tier: backendstore.TierSubscription, Default: true},
	)}
	require.Equal(t, "", s.defaultBackend())

	// A disabled default is likewise ignored.
	s2 := &Server{backends: newBackendStore(t,
		backendstore.Backend{ID: "codex", Installed: true, Enabled: false, Tier: backendstore.TierFree, Default: true},
	)}
	require.Equal(t, "", s2.defaultBackend())
}
