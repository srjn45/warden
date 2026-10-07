package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// prStub serves /sessions/{id}/create-pr, recording the path and decoded body.
func prStub(t *testing.T, created bool) (addr string, path *string, body *map[string]string) {
	t.Helper()
	var p string
	var b map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&b)
		_ = json.NewEncoder(w).Encode(map[string]any{"branch": "feat", "base": "integ", "url": "https://github.com/o/r/pull/9", "created": created})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &p, &b
}

func TestGitPRFlagWiring(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "env-agent")
	addr, path, body := prStub(t, true)
	bf := filepath.Join(t.TempDir(), "body.md")
	require.NoError(t, os.WriteFile(bf, []byte("from file"), 0o644))

	out, err := runGit(t, addr, "git", "pr", "--base", "integ", "--title", "T", "--body-file", bf)
	require.NoError(t, err)
	require.Contains(t, out, "PR opened https://github.com/o/r/pull/9")
	require.Equal(t, "/api/v1/sessions/env-agent/create-pr", *path)
	require.Equal(t, map[string]string{"base": "integ", "title": "T", "body": "from file"}, *body)
}

func TestGitPRPositionalAgentAndExisting(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "env-agent")
	addr, path, body := prStub(t, false)
	out, err := runGit(t, addr, "git", "pr", "other-agent", "--body", "B", "--json")
	require.NoError(t, err)
	require.Equal(t, "/api/v1/sessions/other-agent/create-pr", *path)
	require.Equal(t, map[string]string{"base": "", "body": "B"}, *body, "omitted base/title stay unset for daemon-side resolution")
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Equal(t, false, res["created"])

	out, err = runGit(t, addr, "git", "pr", "other-agent")
	require.NoError(t, err)
	require.Contains(t, out, "already open:")
}

func TestGitPRNoSessionIsClearError(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	addr, path, _ := prStub(t, true)
	_, err := runGit(t, addr, "git", "pr")
	require.ErrorContains(t, err, "gh pr create")
	require.Empty(t, *path, "no daemon call without a session")
}

func TestGitPRBodyFlagsMutuallyExclusive(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "a")
	addr, _, _ := prStub(t, true)
	_, err := runGit(t, addr, "git", "pr", "--body", "x", "--body-file", "y")
	require.ErrorContains(t, err, "mutually exclusive")
}

func TestNoRootPRShortcut(t *testing.T) {
	for _, c := range newRootCmd().Commands() {
		require.NotEqual(t, "pr", c.Name(), "wd git pr has no root shortcut")
	}
}
