package cli

import (
	"bytes"
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

func TestRepairAgentsDryRunHealthyStore(t *testing.T) {
	dir := t.TempDir()
	st, err := agentstore.New(dir)
	require.NoError(t, err)
	require.NoError(t, st.Close())

	cmd := repairAgentsCmdFor(t, dir)
	require.NoError(t, cmd.Flags().Set("dry-run", "true"))
	var out bytes.Buffer
	cmd.SetOut(&out)
	require.NoError(t, cmd.RunE(cmd, nil))
	require.Contains(t, out.String(), "0 finding(s)")
	require.Contains(t, out.String(), "no files changed")
}

func TestRepairAgentsMissingStoreFails(t *testing.T) {
	cmd := repairAgentsCmdFor(t, t.TempDir())
	require.Error(t, cmd.RunE(cmd, nil))
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
