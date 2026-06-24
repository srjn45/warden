package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// runGit drives a wd git subcommand against a stub daemon at addr, returning stdout.
func runGit(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	full := append(args, "--addr", addr, "--config", t.TempDir()+"/none.yaml")
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}

// gitStub serves the three /git/* endpoints, echoing a canned JSON body and
// recording the last decoded request.
func gitStub(t *testing.T, body any) (addr string, last *map[string]string) {
	t.Helper()
	rec := map[string]string{}
	last = &rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.URL.Path, "/git/"), "unexpected path %s", r.URL.Path)
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		for k, v := range in {
			rec[k] = v
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), last
}

func TestCommitCmdReportsSHA(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "code-1")
	addr, last := gitStub(t, map[string]any{"committed": true, "sha": "abc1234", "branch": "feat", "files": []string{"a.go"}})
	out, err := runGit(t, addr, "commit", "-m", "do x")
	require.NoError(t, err)
	require.Contains(t, out, "committed abc1234 on feat")
	require.Equal(t, "code-1", (*last)["session"], "session id from WARDEN_SESSION_ID is forwarded")
	require.Equal(t, "do x", (*last)["message"])
}

func TestCommitCmdRequiresMessage(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{})
	_, err := runGit(t, addr, "commit")
	require.Error(t, err, "commit must require -m")
}

func TestCommitCmdHookFailure(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{"hook_failed": true, "hook_output": "gofmt failed on a.go", "branch": "feat"})
	out, err := runGit(t, addr, "commit", "-m", "x")
	require.NoError(t, err)
	require.Contains(t, out, "rejected by a pre-commit hook")
	require.Contains(t, out, "gofmt failed")
}

func TestCommitCmdCleanTree(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{"committed": false, "branch": "feat"})
	out, err := runGit(t, addr, "commit", "-m", "x")
	require.NoError(t, err)
	require.Contains(t, out, "nothing to commit")
}

func TestCommitCmdJSON(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{"committed": true, "sha": "deadbee", "branch": "feat"})
	out, err := runGit(t, addr, "commit", "-m", "x", "--json")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, "deadbee", got["sha"])
}

func TestPushCmd(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{"branch": "feat", "remote": "origin", "pushed": true})
	out, err := runGit(t, addr, "push")
	require.NoError(t, err)
	require.Contains(t, out, "pushed feat -> origin")
}

func TestSyncCmdClean(t *testing.T) {
	addr, last := gitStub(t, map[string]any{"branch": "feat", "base": "main", "updated": true})
	out, err := runGit(t, addr, "sync", "--base", "develop")
	require.NoError(t, err)
	require.Contains(t, out, "rebased feat onto origin/main")
	require.Equal(t, "develop", (*last)["base"], "--base is forwarded")
}

func TestSyncCmdConflicts(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{"branch": "feat", "base": "main", "conflicts": []string{"a.go", "b.go"}})
	out, err := runGit(t, addr, "sync")
	require.NoError(t, err)
	require.Contains(t, out, "hit conflicts")
	require.Contains(t, out, "a.go")
	require.Contains(t, out, "b.go")
}
