package cli

import (
	"errors"
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

func TestModelsAdd(t *testing.T) {
	st := withTestBackendStore(t)

	out, err := runGit(t, "127.0.0.1:0", "models", "add", "cursor", "brand-new",
		"--tier", "tier-2", "--display", "Brand New", "--auto-assign", "--quota-scope", "api")
	require.NoError(t, err)
	require.Contains(t, out, "model cursor/brand-new added as tier-2")

	m, err := st.GetModel("cursor", "brand-new")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier2, m.Tier)
	require.Equal(t, "Brand New", m.DisplayName)
	require.True(t, m.AutoAssign)
	require.True(t, m.IsCustom)
	require.Equal(t, "api", m.QuotaScope)

	_, err = runGit(t, "127.0.0.1:0", "models", "add", "cursor", "brand-new", "--tier", "tier-1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "already exists")
}

func TestModelsAddRequiresTier(t *testing.T) {
	_ = withTestBackendStore(t)
	_, err := runGit(t, "127.0.0.1:0", "models", "add", "cursor", "x")
	require.Error(t, err)
}

func TestModelsDiscoverImport(t *testing.T) {
	st := withTestBackendStore(t)

	origCursor, origAgy, origOC, origCrush, origPath := discoverCursorCmd, discoverAgyCmd, discoverOpencodeCmd, discoverCrushCmd, discoverCodexConfigPath
	t.Cleanup(func() {
		discoverCursorCmd, discoverAgyCmd, discoverOpencodeCmd, discoverCrushCmd = origCursor, origAgy, origOC, origCrush
		discoverCodexConfigPath = origPath
	})
	discoverCursorCmd = func() ([]byte, error) {
		return []byte("fresh-model - Fresh Model\nauto - Auto\n"), nil
	}
	discoverAgyCmd = func() ([]byte, error) { return nil, errors.New("skip") }
	discoverOpencodeCmd = func() ([]byte, error) { return nil, errors.New("skip") }
	discoverCrushCmd = func() ([]byte, error) { return nil, errors.New("skip") }
	discoverCodexConfigPath = func() string { return "" }

	out, err := runGit(t, "127.0.0.1:0", "models", "discover", "--backend", "cursor", "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"model_id": "fresh-model"`)
	require.Contains(t, out, `"model_id": "auto"`)

	out, err = runGit(t, "127.0.0.1:0", "models", "discover", "--backend", "cursor",
		"--import", "--tier", "tier-3", "--auto-assign")
	require.NoError(t, err)
	require.Contains(t, out, "imported 1 model(s)")
	require.Contains(t, out, "skipped 1 already present")

	m, err := st.GetModel("cursor", "fresh-model")
	require.NoError(t, err)
	require.Equal(t, backendstore.Tier3, m.Tier)
	require.Equal(t, "Fresh Model", m.DisplayName)
	require.True(t, m.AutoAssign)
	require.True(t, m.IsCustom)
}
