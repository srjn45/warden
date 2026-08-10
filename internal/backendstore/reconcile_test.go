package backendstore

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

func TestReconcileFirstSightAndLocalSeeding(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	det := []agentbackend.Detected{
		{ID: "claude", Binary: "claude", Path: "/usr/bin/claude", Installed: true},
		{ID: "aider", Binary: "aider", Path: "", Installed: false},
	}
	require.NoError(t, Reconcile(s, det, true, now))

	claude, err := s.Get("claude")
	require.NoError(t, err)
	require.True(t, claude.Installed)
	require.Equal(t, "/usr/bin/claude", claude.BinaryPath)
	require.Equal(t, TierUnclassified, claude.Tier) // new backend starts unclassified
	require.True(t, claude.Enabled)                 // …and enabled
	require.True(t, claude.DetectedAt.Equal(now))

	// terminal is no longer a backend (stage 6) — it is never created by reconcile.
	_, err = s.Get("terminal")
	require.ErrorIs(t, err, ErrNotFound, "terminal is not a backend row")

	// local row seeded from localInstalled, with the system-set local invariants.
	local, err := s.Get(idLocal)
	require.NoError(t, err)
	require.True(t, local.IsLocal)
	require.Equal(t, TierLocal, local.Tier)
	require.True(t, local.Installed)
	require.Empty(t, local.BinaryPath)
	require.True(t, local.LimitedUntil.IsZero())
	require.False(t, local.Default)

	// local excluded from being a candidate is enforced via SetDefault too.
	require.Error(t, s.SetDefault(idLocal))
}

// A `terminal` row persisted by a pre-stage-6 daemon is pruned on the next
// reconcile (the backend was removed) so it stops showing in GET /backends.
func TestReconcilePrunesStaleTerminalRow(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)
	require.NoError(t, s.Upsert(Backend{ID: "terminal", Installed: true, Tier: TierUnclassified, Enabled: true}))

	require.NoError(t, Reconcile(s, nil, true, now))

	_, err := s.Get("terminal")
	require.ErrorIs(t, err, ErrNotFound, "the stale terminal row is pruned")
}

func TestReconcilePreservesPreferences(t *testing.T) {
	s := newTestStore(t)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)

	// User has tiered + defaulted + disabled some backends.
	require.NoError(t, s.Upsert(Backend{ID: "claude", Installed: true, BinaryPath: "/old/claude", DetectedAt: t0, Tier: "free", Default: true, Enabled: true}))
	require.NoError(t, s.Upsert(Backend{ID: "codex", Installed: true, DetectedAt: t0, Tier: "subscription", Enabled: false}))

	now := time.Now().Truncate(time.Second)
	det := []agentbackend.Detected{
		{ID: "claude", Binary: "claude", Path: "/new/claude", Installed: true},
		{ID: "codex", Binary: "codex", Path: "", Installed: false}, // uninstalled between rescans
	}
	require.NoError(t, Reconcile(s, det, false, now))

	claude, err := s.Get("claude")
	require.NoError(t, err)
	// Detection fields updated…
	require.Equal(t, "/new/claude", claude.BinaryPath)
	require.True(t, claude.DetectedAt.Equal(now))
	// …preferences preserved.
	require.Equal(t, "free", claude.Tier)
	require.True(t, claude.Default)
	require.True(t, claude.Enabled)

	codex, err := s.Get("codex")
	require.NoError(t, err)
	require.False(t, codex.Installed) // record kept, marked uninstalled
	require.Equal(t, "subscription", codex.Tier)
	require.False(t, codex.Enabled) // disabled preference survives

	// local seeded not-installed (localInstalled=false) but still present as a row.
	local, err := s.Get(idLocal)
	require.NoError(t, err)
	require.False(t, local.Installed)
	require.True(t, local.IsLocal)
}

func TestReconcilePreservesLocalEnabled(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().Truncate(time.Second)

	require.NoError(t, Reconcile(s, nil, true, now))
	require.NoError(t, s.SetEnabled(idLocal, false))

	// A later reconcile must not re-enable the user-disabled local row.
	require.NoError(t, Reconcile(s, nil, true, now.Add(time.Minute)))
	local, err := s.Get(idLocal)
	require.NoError(t, err)
	require.False(t, local.Enabled)
	require.True(t, local.Installed)
}
