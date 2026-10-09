package cli

import (
	"bytes"
	"context"
	"github.com/srjn45/warden/internal/backendstore"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateCommandFlags(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"update"})
	require.NoError(t, err)
	require.Equal(t, "update", cmd.Name())
	require.NotNil(t, cmd.Flags().Lookup("check"))
	require.NotNil(t, cmd.Flags().Lookup("force"))
	require.NotNil(t, cmd.Flags().Lookup("version"))
}

func TestUpdateCommandHelpMentionsAtomicSwap(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"update", "--help"})
	require.NoError(t, root.Execute())
	require.Contains(t, out.String(), "atomically")
	require.Contains(t, out.String(), "--check")
}

func TestUpdateCommandReadyTimeoutFlag(t *testing.T) {
	t.Parallel()
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"update"})
	require.NoError(t, err)
	f := cmd.Flags().Lookup("ready-timeout")
	require.NotNil(t, f)
	require.Equal(t, "1m30s", f.DefValue)
}

func TestBackendPreflightMissingDirIsClean(t *testing.T) {
	t.Parallel()
	res, err := backendPreflight(context.Background(), filepath.Join(t.TempDir(), "backends"))
	require.NoError(t, err)
	require.Empty(t, res.Blockers)
	require.Empty(t, res.Notes)
}

func TestBackendPreflightCleanStore(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "backends")
	s, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.Upsert(backendstore.Backend{ID: "claude", Installed: true, Enabled: true}))
	require.NoError(t, s.Close())
	res, err := backendPreflight(context.Background(), dir)
	require.NoError(t, err)
	require.Empty(t, res.Blockers)
}

// A store whose segment bytes are damaged cannot be auto-repaired: the update
// must be blocked with the repair command, before anything is swapped.
func TestBackendPreflightCorruptStoreBlocks(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "backends")
	s, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.Upsert(backendstore.Backend{ID: "claude", Installed: true, Enabled: true}))
	require.NoError(t, s.Close())
	segs, _ := filepath.Glob(filepath.Join(dir, "*", "seg_*.ndjson"))
	require.NotEmpty(t, segs)
	for _, seg := range segs {
		b, err := os.ReadFile(seg)
		require.NoError(t, err)
		if len(b) > 20 {
			b[len(b)/2] ^= 0xFF
			b[len(b)/2+1] ^= 0xFF
			require.NoError(t, os.WriteFile(seg, b, 0o644))
		}
	}
	res, err := backendPreflight(context.Background(), dir)
	require.NoError(t, err)
	require.NotEmpty(t, res.Blockers, "damaged store must block the update")
	require.Equal(t, backendstore.RepairCommand, res.RepairCommand)
}
