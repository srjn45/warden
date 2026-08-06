package backendstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertGetList(t *testing.T) {
	s := newTestStore(t)

	_, err := s.Get("claude")
	require.ErrorIs(t, err, ErrNotFound)

	now := time.Now().Truncate(time.Second)
	require.NoError(t, s.Upsert(Backend{ID: "claude", Installed: true, BinaryPath: "/usr/bin/claude", DetectedAt: now, Tier: "free", Enabled: true}))
	require.NoError(t, s.Upsert(Backend{ID: "aider", Installed: false, Tier: TierUnclassified, Enabled: true}))

	got, err := s.Get("claude")
	require.NoError(t, err)
	require.True(t, got.Installed)
	require.Equal(t, "/usr/bin/claude", got.BinaryPath)
	require.Equal(t, "free", got.Tier)
	require.True(t, got.DetectedAt.Equal(now))

	// Upsert updates in place (no duplicate row).
	require.NoError(t, s.Upsert(Backend{ID: "claude", Installed: false, Tier: "free", Enabled: true}))
	got, err = s.Get("claude")
	require.NoError(t, err)
	require.False(t, got.Installed)

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	// Sorted by id.
	require.Equal(t, "aider", list[0].ID)
	require.Equal(t, "claude", list[1].ID)
}

func TestSetTierEnabled(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Upsert(Backend{ID: "codex", Tier: TierUnclassified, Enabled: true}))

	require.NoError(t, s.SetTier("codex", "subscription"))
	b, err := s.Get("codex")
	require.NoError(t, err)
	require.Equal(t, "subscription", b.Tier)

	require.NoError(t, s.SetEnabled("codex", false))
	b, err = s.Get("codex")
	require.NoError(t, err)
	require.False(t, b.Enabled)

	require.ErrorIs(t, s.SetTier("nope", "free"), ErrNotFound)
	require.ErrorIs(t, s.SetEnabled("nope", true), ErrNotFound)
}

func TestSingleDefaultInvariant(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Upsert(Backend{ID: "claude", Installed: true, Tier: "free", Enabled: true}))
	require.NoError(t, s.Upsert(Backend{ID: "codex", Installed: true, Tier: "subscription", Enabled: true}))
	require.NoError(t, s.Upsert(Backend{ID: "aider", Installed: true, Tier: "free", Enabled: true}))

	require.NoError(t, s.SetDefault("claude"))
	def, ok, err := s.Default()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "claude", def.ID)

	// Switching the default clears the previous one — exactly one row is default.
	require.NoError(t, s.SetDefault("codex"))
	list, err := s.List()
	require.NoError(t, err)
	var defaults []string
	for _, b := range list {
		if b.Default {
			defaults = append(defaults, b.ID)
		}
	}
	require.Equal(t, []string{"codex"}, defaults)

	def, ok, err = s.Default()
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "codex", def.ID)
}

func TestSetDefaultRejectsLocalTerminal(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Upsert(Backend{ID: idLocal, IsLocal: true, Tier: TierLocal, Enabled: true}))
	require.NoError(t, s.Upsert(Backend{ID: idTerminal, Installed: true, Tier: TierUnclassified, Enabled: true}))

	require.Error(t, s.SetDefault(idLocal))
	require.Error(t, s.SetDefault(idTerminal))

	_, ok, err := s.Default()
	require.NoError(t, err)
	require.False(t, ok)
}

func TestSetDefaultNotFound(t *testing.T) {
	s := newTestStore(t)
	require.ErrorIs(t, s.SetDefault("ghost"), ErrNotFound)
}

func TestSettingsDefaults(t *testing.T) {
	s := newTestStore(t)

	// A fresh store yields the default mode without a persisted record.
	st, err := s.Settings()
	require.NoError(t, err)
	require.Equal(t, SettingsKey, st.ID)
	require.Equal(t, ThinkingModeFreePlusLocal, st.InternalThinkingMode)

	require.NoError(t, s.SetThinkingMode(ThinkingModeLocalOnly))
	st, err = s.Settings()
	require.NoError(t, err)
	require.Equal(t, ThinkingModeLocalOnly, st.InternalThinkingMode)

	require.Error(t, s.SetThinkingMode("bogus"))
}

func TestSettingsExcludedFromList(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.Upsert(Backend{ID: "claude", Installed: true, Tier: "free", Enabled: true}))
	// Writing settings creates the reserved __settings__ record in the same
	// collection.
	require.NoError(t, s.SetThinkingMode(ThinkingModeLocalOnly))

	list, err := s.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "claude", list[0].ID)
	for _, b := range list {
		require.NotEqual(t, SettingsKey, b.ID)
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.Upsert(Backend{ID: "claude", Installed: true, Tier: "free", Enabled: true, Default: true}))
	require.NoError(t, s.SetThinkingMode(ThinkingModeLocalOnly))
	require.NoError(t, s.Close())

	s2, err := NewStore(dir)
	require.NoError(t, err)
	defer s2.Close()

	b, err := s2.Get("claude")
	require.NoError(t, err)
	require.True(t, b.Default)
	st, err := s2.Settings()
	require.NoError(t, err)
	require.Equal(t, ThinkingModeLocalOnly, st.InternalThinkingMode)
}
