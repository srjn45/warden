package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

func TestRepairAgentsRefusesWhileOwned(t *testing.T) {
	dir := t.TempDir()
	st, err := agentstore.New(dir)
	require.NoError(t, err)
	defer st.Close()

	cmd := repairAgentsCmdFor(t, dir)
	err = cmd.RunE(cmd, nil)
	require.ErrorIs(t, err, store.ErrStoreOwned)
	require.Contains(t, err.Error(), "next step: stop the running warden daemon")
}

func TestRepairAgentsOfflineReportsUnavailable(t *testing.T) {
	dir := t.TempDir()
	cmd := repairAgentsCmdFor(t, dir)
	err := cmd.RunE(cmd, nil)
	require.ErrorIs(t, err, agentstore.ErrRepairUnavailable)
}

func TestCheckAgentStoreDaemonOfflineOwned(t *testing.T) {
	dir := t.TempDir()
	st, err := agentstore.New(dir)
	require.NoError(t, err)
	r := checkAgentStore(t.Context(), "http://127.0.0.1:1", dir)
	require.False(t, r.ok)
	require.Contains(t, r.detail, "owned by another warden process")
	require.NoError(t, st.Close())
	r = checkAgentStore(t.Context(), "http://127.0.0.1:1", dir)
	require.True(t, r.ok, r.detail)
}

// repairAgentsCmdFor builds the command pointed at dir via a throwaway config
// file, so tests never touch the real ~/.warden.
func repairAgentsCmdFor(t *testing.T, dir string) *cobra.Command {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte(fmt.Sprintf("data_dir: %q\n", dir)), 0o600))
	cmd := newRepairAgentsCmd()
	cmd.SetContext(t.Context())
	cmd.Flags().String("config", cfg, "")
	return cmd
}
