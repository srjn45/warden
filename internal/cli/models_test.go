package cli

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

// TestModelsDegradesForNonListerBackend: a backend with a static model set (Claude)
// must not be offered the verb — it exits non-zero pointing at --model with a known
// id, never trying to exec a list subcommand.
func TestModelsDegradesForNonListerBackend(t *testing.T) {
	_, err := runGit(t, "127.0.0.1:0", "models", "--backend", "claude")
	require.Error(t, err, "a non-lister backend must exit non-zero")
	require.Contains(t, err.Error(), "no live model menu")
	require.Contains(t, err.Error(), "--model")
}

// TestModelsUnknownBackend surfaces the registry error for an unknown --backend.
func TestModelsUnknownBackend(t *testing.T) {
	_, err := runGit(t, "127.0.0.1:0", "models", "--backend", "nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown agent backend")
}

// TestModelsResolvesBackendFromSession: with no --backend, the verb reads the owning
// agent's recorded backend (here Claude via the session stub) and degrades on it —
// proving session resolution goes through the existing read, no new daemon surface.
func TestModelsResolvesBackendFromSession(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "code-1")
	addr := sessionStub(t, "claude")
	_, err := runGit(t, addr, "models")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no live model menu")
}

func withTestBackendStore(t *testing.T) *backendstore.Store {
	t.Helper()
	st, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	orig := openBackendStore
	openBackendStore = func(_ *cobra.Command) (*backendstore.Store, error) {
		return st, nil
	}
	t.Cleanup(func() { openBackendStore = orig })
	return st
}

func TestModelsList(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "models", "list")
	require.NoError(t, err)
	require.Contains(t, out, "BACKEND")
	require.Contains(t, out, "MODEL")
	require.Contains(t, out, "TIER")
	require.Contains(t, out, "opus")
	require.Contains(t, out, "tier-1")
}

func TestModelsListByTier(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "models", "list", "--by-tier")
	require.NoError(t, err)
	require.Contains(t, out, "=== TIER-1 ===")
	require.Contains(t, out, "=== TIER-2 ===")
	require.Contains(t, out, "=== TIER-3 ===")
	require.Contains(t, out, "opus")
	require.Contains(t, out, "sonnet")
}

func TestModelsListFilterTier(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "models", "list", "--tier", "tier-1")
	require.NoError(t, err)
	require.Contains(t, out, "opus")
	require.NotContains(t, out, "sonnet")
	require.NotContains(t, out, "haiku")
}

func TestModelsListJSON(t *testing.T) {
	_ = withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "models", "list", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"backend_id": "claude"`)
	require.Contains(t, out, `"model_id": "opus"`)
	require.Contains(t, out, `"tier": "tier-1"`)
}

func TestModelsListInvalidTier(t *testing.T) {
	_ = withTestBackendStore(t)

	_, err := runGit(t, "127.0.0.1:0", "models", "list", "--tier", "tier-99")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid tier")
}

func TestModelsTierSet(t *testing.T) {
	st := withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "models", "tier", "claude", "sonnet", "tier-1")
	require.NoError(t, err)
	require.Contains(t, out, "model claude/sonnet tiered as tier-1")

	m, err := st.GetModel("claude", "sonnet")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier1, m.Tier)
}

func TestModelsTierInvalid(t *testing.T) {
	_ = withTestBackendStore(t)

	_, err := runGit(t, "127.0.0.1:0", "models", "tier", "claude", "sonnet", "tier-invalid")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid tier")
}
