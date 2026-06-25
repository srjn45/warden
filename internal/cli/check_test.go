package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// errStubServer serves a daemon error envelope ({"error": msg}) with the given
// status on every path, returning its addr.
func errStubServer(t *testing.T, status int, msg string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestCheckCmdReportsPass(t *testing.T) {
	t.Setenv("WARDEN_SESSION_ID", "code-1")
	addr, last := gitStub(t, map[string]any{
		"passed": true,
		"checks": []map[string]any{{"name": "test", "cmd": "go test ./...", "passed": true}},
	})
	out, err := runGit(t, addr, "check", "test")
	require.NoError(t, err)
	require.Contains(t, out, "✓ test")
	require.Equal(t, "code-1", (*last)["session"], "session id is forwarded")
	require.Equal(t, "test", (*last)["name"], "the check name is forwarded")
}

func TestCheckCmdFailureExitsNonZeroWithOutput(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{
		"passed": false,
		"checks": []map[string]any{
			{"name": "test", "cmd": "go test ./...", "passed": false, "exit_code": 1, "output": "--- FAIL: TestX"},
		},
	})
	out, err := runGit(t, addr, "check")
	require.Error(t, err, "a failed check must exit non-zero")
	require.Contains(t, out, "✗ test")
	require.Contains(t, out, "--- FAIL: TestX")
}

func TestCheckCmdJSON(t *testing.T) {
	addr, _ := gitStub(t, map[string]any{"passed": true, "checks": []map[string]any{}})
	out, err := runGit(t, addr, "check", "--json")
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Equal(t, true, got["passed"])
}

func TestCheckCmdSurfacesNoConfigError(t *testing.T) {
	// The daemon returns the friendly no-config message as a 4xx; the CLI surfaces it.
	addr := errStubServer(t, 422, "no .warden/check.yml in this project")
	_, err := runGit(t, addr, "check")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no .warden/check.yml")
}
