package backendstore_test

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

func TestRLCooldown_SetIsActiveExpireClear(t *testing.T) {
	s, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	until := time.Now().Add(time.Hour).UTC()
	require.NoError(t, s.SetRLCooldown("codex", "codex-model", until))
	require.True(t, s.IsRLCoolingDown("codex", "codex-model", time.Now()))
	require.False(t, s.IsRLCoolingDown("claude", "claude-model", time.Now()), "other pool must be unaffected")

	// Past expiry: not cooling down, and the expired row is cleaned up.
	require.False(t, s.IsRLCoolingDown("codex", "codex-model", until.Add(time.Second)))
	// A second check after cleanup still fails open.
	require.False(t, s.IsRLCoolingDown("codex", "codex-model", until.Add(2*time.Second)))

	// Re-stamp, then clear via a past/zero until.
	require.NoError(t, s.SetRLCooldown("codex", "codex-model", until))
	require.True(t, s.IsRLCoolingDown("codex", "codex-model", time.Now()))
	require.NoError(t, s.SetRLCooldown("codex", "codex-model", time.Time{}))
	require.False(t, s.IsRLCoolingDown("codex", "codex-model", time.Now()))
}

func TestRLCooldown_SurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).UTC()

	s1, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, s1.SetRLCooldown("claude", "claude-model", until))
	require.NoError(t, s1.Close())

	s2, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	defer s2.Close()
	require.True(t, s2.IsRLCoolingDown("claude", "claude-model", time.Now()), "cooldown must survive store reopen")
}
