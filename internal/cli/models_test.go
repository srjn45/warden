package cli

import (
	"testing"

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
