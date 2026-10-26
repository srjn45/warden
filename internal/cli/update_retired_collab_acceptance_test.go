package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/storage"
	"github.com/srjn45/warden/internal/store"
)

// An installation last written by the final detector-era release carries the
// retired collab settings in both nested and flat spellings, next to settings
// that must survive. This drives the real update migration hook (config
// Reconcile + schema-1 -> schema-2 data cutover) against isolated temporary
// config and data directories.
//
// Simulated: the binary swap. The target binary's schema and the migrate
// subprocess are stubbed by stubUpdateSeams/realMigrate (in-process runner);
// no released binary is downloaded or executed.
const retiredCollabLegacyConfig = `addr: 127.0.0.1:4040
# operator note
keep_me: value
collab:
  enabled: false
  interval: not-a-duration
  git_reconcile_interval: 7m
  hint: false
collab_enabled: true
collab_interval: 45s
collab_hint: false
branch_track:
  enabled: true
  interval: 5m
log:
  level: debug
`

func TestUpdateAcceptance_RetiredCollabConfigAndSchemaCutover(t *testing.T) {
	dataDir := t.TempDir()
	seedReleaseAcceptanceLegacy(t, dataDir)
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(retiredCollabLegacyConfig), 0o600))

	// Before the update runs, the new binary already tolerates the old file.
	c, err := config.LoadStrict(cfgPath)
	require.NoError(t, err)
	require.True(t, c.BranchTrack.Enabled)
	require.Equal(t, "debug", c.Log.Level)

	calls := stubUpdateSeams(t, schema.Schema2, realMigrate(t))
	require.NoError(t, runUpdateMigrations(context.Background(), cfgPath, dataDir, "/fake/warden", io.Discard))
	require.Equal(t, 1, *calls)

	// Retired keys are gone; unrelated settings and comments are preserved.
	first, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	for _, key := range []string{"collab:", "collab_enabled:", "collab_interval:", "collab_hint:", "git_reconcile_interval"} {
		require.NotContains(t, string(first), key)
	}
	require.Contains(t, string(first), "# operator note")
	require.Contains(t, string(first), "keep_me: value")
	reloaded, err := config.LoadStrict(cfgPath)
	require.NoError(t, err)
	require.True(t, reloaded.BranchTrack.Enabled)
	require.Equal(t, "5m", reloaded.BranchTrack.Interval)
	require.Equal(t, "debug", reloaded.Log.Level)

	// Data needs no detector-specific migration: the normal cutover ran and
	// live agent state is intact.
	l, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, schema.Schema2, l.SchemaVersion)
	require.NoError(t, schema.Check(dataDir, l))
	s, err := storage.Open(dataDir)
	require.NoError(t, err)
	live, err := s.Agents.Get(context.Background(), "agent-live")
	require.NoError(t, err)
	require.Equal(t, store.StatusWorking, live.Status)
	require.NoError(t, s.Close())

	// A second update pass is a no-op for config and data.
	require.NoError(t, runUpdateMigrations(context.Background(), cfgPath, dataDir, "/fake/warden", io.Discard))
	second, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
	require.Equal(t, 1, *calls, "already-migrated data is not migrated again")
}
