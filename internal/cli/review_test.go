package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sessionStub serves GET /api/v1/sessions/{id} with a canned session body so the
// review resolver can read the owning agent's backend without a live daemon.
func sessionStub(t *testing.T, backend string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.Contains(r.URL.Path, "/sessions/"), "unexpected path %s", r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "code-1", "backend": backend})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// TestReviewDegradesForNonReviewerBackend: a backend without native review (Claude)
// must not be offered the verb — it exits non-zero with the pointer to wd check /
// pr-review, never trying to exec anything.
func TestReviewDegradesForNonReviewerBackend(t *testing.T) {
	out, err := runGit(t, "127.0.0.1:0", "review", "--backend", "claude")
	require.Error(t, err, "a non-reviewer backend must exit non-zero")
	require.Contains(t, err.Error(), "no native review")
	require.Contains(t, err.Error(), "wd check")
	_ = out
}

// TestReviewUnknownBackend surfaces the registry error for an unknown --backend.
func TestReviewUnknownBackend(t *testing.T) {
	_, err := runGit(t, "127.0.0.1:0", "review", "--backend", "nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown agent backend")
}

// TestReviewResolvesBackendFromSession: with no --backend, the verb reads the owning
// agent's recorded backend (here Claude via the session stub) and degrades on it —
// proving session resolution goes through the existing read, no new daemon surface.
func TestReviewResolvesBackendFromSession(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "code-1")
	addr := sessionStub(t, "claude")
	_, err := runGit(t, addr, "review")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no native review")
}
