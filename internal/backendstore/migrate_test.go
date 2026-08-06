package backendstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// seedDetected upserts a plain installed+enabled, unclassified CLI backend row.
func seedDetected(t *testing.T, s *Store, id string) {
	t.Helper()
	require.NoError(t, s.Upsert(Backend{ID: id, Installed: true, Enabled: true, Tier: TierUnclassified}))
}

func TestMigrateAutopilotLadder(t *testing.T) {
	s := newTestStore(t)
	seedDetected(t, s, "antigravity")
	seedDetected(t, s, "claude")
	seedDetected(t, s, "gpt")
	sentinel := filepath.Join(t.TempDir(), AutopilotLadderMarker)

	// A config-listed id with no store row ("ghost") is skipped, not created.
	ran, err := MigrateAutopilotLadder(s, sentinel,
		[]string{"antigravity", "ghost"}, []string{"claude"}, []string{"gpt"}, true)
	require.NoError(t, err)
	require.True(t, ran, "first migration runs")

	// Tiers were imported from config.
	for id, want := range map[string]string{"antigravity": TierFree, "claude": TierSubscription, "gpt": TierPayPerUse} {
		b, err := s.Get(id)
		require.NoError(t, err)
		require.Equal(t, want, b.Tier, "tier for %s", id)
	}
	_, err = s.Get("ghost")
	require.ErrorIs(t, err, ErrNotFound, "unknown backend is not materialised")

	// The paid-autopilot gate was seeded from allow_pay_per_use.
	st, err := s.Settings()
	require.NoError(t, err)
	require.True(t, st.AllowPaidAutopilot)

	// Sentinel written last ⇒ present after a successful import.
	_, statErr := os.Stat(sentinel)
	require.NoError(t, statErr)
}

func TestMigrateAutopilotLadderRunsOnceAndDoesNotClobber(t *testing.T) {
	s := newTestStore(t)
	seedDetected(t, s, "antigravity")
	seedDetected(t, s, "claude")
	sentinel := filepath.Join(t.TempDir(), AutopilotLadderMarker)

	ran, err := MigrateAutopilotLadder(s, sentinel, []string{"antigravity"}, []string{"claude"}, nil, false)
	require.NoError(t, err)
	require.True(t, ran)

	// Simulate a LATER user edit in the store: retier antigravity + flip the gate.
	require.NoError(t, s.SetTier("antigravity", TierSubscription))
	require.NoError(t, s.SetAllowPaidAutopilot(true))

	// A second boot re-runs the importer with the ORIGINAL config values. The
	// sentinel must short-circuit it so the user's store edits stay authoritative.
	ran, err = MigrateAutopilotLadder(s, sentinel, []string{"antigravity"}, []string{"claude"}, nil, false)
	require.NoError(t, err)
	require.False(t, ran, "second migration is a no-op")

	b, err := s.Get("antigravity")
	require.NoError(t, err)
	require.Equal(t, TierSubscription, b.Tier, "user edit preserved, not clobbered by config")
	st, err := s.Settings()
	require.NoError(t, err)
	require.True(t, st.AllowPaidAutopilot, "user gate edit preserved")
}

func TestMigrateAutopilotLadderSkipsReservedIDs(t *testing.T) {
	s := newTestStore(t)
	// The reserved local row exists (reconcile-style) but must never be retiered by
	// the config import even if config names it.
	require.NoError(t, s.Upsert(Backend{ID: IDLocal, IsLocal: true, Installed: true, Enabled: true, Tier: TierLocal}))
	sentinel := filepath.Join(t.TempDir(), AutopilotLadderMarker)

	ran, err := MigrateAutopilotLadder(s, sentinel, []string{IDLocal, "terminal"}, nil, nil, false)
	require.NoError(t, err)
	require.True(t, ran)

	b, err := s.Get(IDLocal)
	require.NoError(t, err)
	require.Equal(t, TierLocal, b.Tier, "reserved local row keeps its system tier")
}

func TestAutopilotLadderDerivation(t *testing.T) {
	s := newTestStore(t)
	// Eligible free (sorted by id: antigravity, codex).
	seedDetected(t, s, "codex")
	require.NoError(t, s.SetTier("codex", TierFree))
	seedDetected(t, s, "antigravity")
	require.NoError(t, s.SetTier("antigravity", TierFree))
	// Subscription + pay_per_use.
	seedDetected(t, s, "claude")
	require.NoError(t, s.SetTier("claude", TierSubscription))
	seedDetected(t, s, "gpt")
	require.NoError(t, s.SetTier("gpt", TierPayPerUse))
	// Excluded: uninstalled, disabled, unclassified, and the local row.
	require.NoError(t, s.Upsert(Backend{ID: "aider", Installed: false, Enabled: true, Tier: TierFree}))
	require.NoError(t, s.Upsert(Backend{ID: "crush", Installed: true, Enabled: false, Tier: TierFree}))
	seedDetected(t, s, "goose") // stays unclassified
	require.NoError(t, s.Upsert(Backend{ID: IDLocal, IsLocal: true, Installed: true, Enabled: true, Tier: TierLocal}))
	require.NoError(t, s.SetAllowPaidAutopilot(true))

	free, sub, ppu, allowPaid, err := s.AutopilotLadder()
	require.NoError(t, err)
	require.Equal(t, []string{"antigravity", "codex"}, free, "installed+enabled free, id-sorted")
	require.Equal(t, []string{"claude"}, sub)
	require.Equal(t, []string{"gpt"}, ppu)
	require.True(t, allowPaid)
}
