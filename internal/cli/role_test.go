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
	require.Contains(t, out, "DESCRIPTION")
	require.Contains(t, out, "implementer")
	require.Contains(t, out, "reviewer")
}

func TestRoleTierList(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "tier", "list")
	require.NoError(t, err)
	require.Contains(t, out, "ROLE")
	require.Contains(t, out, "DEFAULT TIER")
	require.Contains(t, out, "analysis")
	require.Contains(t, out, "tier-1")
	require.Contains(t, out, "implementation")
	require.Contains(t, out, "tier-2")
}

func TestRoleTierListDefault(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "tier")
	require.NoError(t, err)
	require.Contains(t, out, "ROLE")
	require.Contains(t, out, "DEFAULT TIER")
	require.Contains(t, out, "implementation")
}

func TestRoleTierListJSON(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "tier", "list", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"role_name": "analysis"`)
	require.Contains(t, out, `"default_tier": "tier-1"`)
}

func TestRoleSetTier(t *testing.T) {
	st := withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "role", "set-tier", "implementation", "tier-1")
	require.NoError(t, err)
	require.Contains(t, out, `role "implementation" default tier set to tier-1`)

	tier, err := st.GetRoleTier("implementation")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier1, tier)
}

func TestRoleSetTierInvalid(t *testing.T) {
	_ = withTestBackendStore(t)

	_, err := runGit(t, "127.0.0.1:0", "role", "set-tier", "implementation", "tier-bogus")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid tier")
}
