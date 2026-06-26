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

// runHookGuard drives `warden hook guard` with stdin against a stub daemon at
// addr, returning stdout.
func runHookGuard(t *testing.T, addr, stdin string) string {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"hook", "guard", "--addr", addr, "--config", t.TempDir() + "/none.yaml"})
	require.NoError(t, root.Execute())
	return out.String()
}

func stubGuard(t *testing.T, decision, reason string) (addr string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/hooks/guard", r.URL.Path)
		json.NewEncoder(w).Encode(map[string]string{"decision": decision, "reason": reason})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func preToolUseJSON(tool, path string) string {
	b, _ := json.Marshal(map[string]any{
		"tool_name":  tool,
		"tool_input": map[string]string{"file_path": path},
	})
	return string(b)
}

func TestHookGuardDenyEmitsDecision(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "code-1")
	addr := stubGuard(t, "deny", "work in your worktree at /repo/.worktrees/code-1")
	out := runHookGuard(t, addr, preToolUseJSON("Edit", "/repo/main.go"))

	var dec struct {
		HookSpecificOutput struct {
			HookEventName            string `json:"hookEventName"`
			PermissionDecision       string `json:"permissionDecision"`
			PermissionDecisionReason string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &dec))
	require.Equal(t, "PreToolUse", dec.HookSpecificOutput.HookEventName)
	require.Equal(t, "deny", dec.HookSpecificOutput.PermissionDecision)
	require.Contains(t, dec.HookSpecificOutput.PermissionDecisionReason, "worktree")
}

func TestHookGuardAllowEmitsNothing(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "code-1")
	addr := stubGuard(t, "allow", "")
	out := runHookGuard(t, addr, preToolUseJSON("Edit", "/repo/.worktrees/code-1/main.go"))
	require.Empty(t, strings.TrimSpace(out), "allow must produce no stdout")
}

func TestHookGuardNoSessionFailsOpen(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "")
	t.Setenv("AGENTCTL_SESSION_ID", "")
	// Daemon at an unroutable addr — must never be called when there is no session.
	out := runHookGuard(t, "127.0.0.1:1", preToolUseJSON("Edit", "/repo/main.go"))
	require.Empty(t, strings.TrimSpace(out))
}

func TestHookGuardDaemonErrorFailsOpen(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "code-1")
	// Unroutable daemon → Guard errors → allow (no output), agent never wedged.
	out := runHookGuard(t, "127.0.0.1:1", preToolUseJSON("Edit", "/repo/main.go"))
	require.Empty(t, strings.TrimSpace(out))
}

func TestToolInputPath(t *testing.T) {
	require.Equal(t, "/a/b.go", toolInputPath(json.RawMessage(`{"file_path":"/a/b.go"}`)))
	require.Equal(t, "/a/nb.ipynb", toolInputPath(json.RawMessage(`{"notebook_path":"/a/nb.ipynb"}`)))
	require.Empty(t, toolInputPath(json.RawMessage(`{"other":"x"}`)))
	require.Empty(t, toolInputPath(json.RawMessage(`not json`)))
}
