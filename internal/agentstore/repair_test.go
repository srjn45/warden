package agentstore

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestProbeOwnership(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, ProbeOwnership(dir), "free store probes clean")
	st, err := New(dir)
	require.NoError(t, err)
	require.ErrorIs(t, ProbeOwnership(dir), store.ErrStoreOwned, "live-owned store is refused")
	require.NoError(t, st.Close())
	require.NoError(t, ProbeOwnership(dir), "released on close")
}

func TestRepairAuthorityAndAvailability(t *testing.T) {
	require.NoError(t, CheckRepairAuthority(t.TempDir()))
	require.Error(t, CheckRepairAuthority(t.TempDir()+"/missing"))
	if os.Geteuid() != 0 {
		require.Error(t, CheckRepairAuthority("/"), "root-owned dir is denied to a normal user")
	}
	require.True(t, RepairAvailable)
	require.Contains(t, unhealthyHint, "repair agents --dry-run")
}
