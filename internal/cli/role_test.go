package cli

import (
	"testing"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

func TestRoleList(t *testing.T) {
	out, err := runGit(t, "127.0.0.1:0", "role", "list")
	require.NoError(t, err)
	require.Contains(t, out, "ROLE")
	require.Contains(t, out, "general")
	require.Contains(t, out, "worker")
	require.Contains(t, out, "orchestrator")
}

func TestRoleTierList(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "tier", "list")
	require.NoError(t, err)
	require.Contains(t, out, "ROLE")
	require.Contains(t, out, "DEFAULT TIER")
	require.Contains(t, out, "orchestrator")
	require.Contains(t, out, "tier-1")
	require.Contains(t, out, "general")
	require.Contains(t, out, "tier-2")
}

func TestRoleTierListDefault(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "tier")
	require.NoError(t, err)
	require.Contains(t, out, "ROLE")
	require.Contains(t, out, "DEFAULT TIER")
	require.Contains(t, out, "general")
}

func TestRoleTierListJSON(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "tier", "list", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"role_name": "orchestrator"`)
	require.Contains(t, out, `"default_tier": "tier-1"`)
}

func TestRoleSetTier(t *testing.T) {
	st := withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "set-tier", "worker", "tier-1")
	require.NoError(t, err)
	require.Contains(t, out, `role "worker" default tier set to tier-1`)

	tier, err := st.GetRoleTier("worker")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier1, tier)
}

func TestRoleSetTierInvalid(t *testing.T) {
	_ = withTestBackendStore(t)

	_, err := runGit(t, "127.0.0.1:0", "role", "set-tier", "worker", "tier-bogus")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid tier")
}
