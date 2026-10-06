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

func TestRepairAuthorityAndUnavailable(t *testing.T) {
	require.NoError(t, CheckRepairAuthority(t.TempDir()))
	require.Error(t, CheckRepairAuthority(t.TempDir()+"/missing"))
	if os.Geteuid() != 0 {
		require.Error(t, CheckRepairAuthority("/"), "root-owned dir is denied to a normal user")
	}
	require.False(t, RepairAvailable, "no repair primitive exists; flip only when ScrivaDB ships Verify/Repair")
	err := &RepairUnavailableError{}
	require.ErrorIs(t, err, ErrRepairUnavailable)
	require.Contains(t, err.Error(), "scriva#107")
	require.Contains(t, unhealthyHint, "not yet available", "hint must not promise a repair that does not exist")
}
