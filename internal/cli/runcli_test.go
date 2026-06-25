package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubDaemon starts an httptest server using handler and returns its host:port
// address (no scheme), suitable for the --addr flag. The server is closed at
// test cleanup.
func stubDaemon(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// runCLI executes the root command with args plus a stub --addr and an isolated
// --config (so the test never touches the user's real config). It returns the
// combined stdout+stderr and the execution error.
func runCLI(t *testing.T, addr string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	full := append([]string{}, args...)
	if addr != "" {
		full = append(full, "--addr", addr)
	}
	full = append(full, "--config", t.TempDir()+"/none.yaml")
	root.SetArgs(full)
	err := root.Execute()
	return out.String(), err
}
