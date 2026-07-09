package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/stretchr/testify/require"
)

// apFakeEnv is a minimal autopilot.Env for route tests: any dir resolves to a
// fixed repo, gh is OK, and the integration branch is auto-created.
type apFakeEnv struct {
	repo    string
	ghErr   error
	created bool
}

func (e *apFakeEnv) GitToplevel(context.Context, string) (string, error) { return e.repo, nil }
func (e *apFakeEnv) DefaultBranch(context.Context, string) (string, error) {
	return "main", nil
}
func (e *apFakeEnv) BranchExists(context.Context, string, string) (bool, error) {
	return e.created, nil
}
func (e *apFakeEnv) CreateBranch(context.Context, string, string, string) error {
	e.created = true
	return nil
}
func (e *apFakeEnv) GHAuthOK(context.Context) error { return e.ghErr }

func newAutopilotServer(t *testing.T, env autopilot.Env, plans []string) *httptest.Server {
	t.Helper()
	srv := &Server{store: newFakeStore(), hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(autopilot.NewController(autopilot.ControllerConfig{
		Plans:             plans,
		IntegrationBranch: "autopilot/integration",
		Gate:              "auto",
	}, env))
	return httptest.NewServer(srv.router())
}

func TestAutopilotEnableStatusDisable(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	ts := newAutopilotServer(t, &apFakeEnv{repo: dir}, []string{plan})
	defer ts.Close()

	// GET before enabling → disabled, no runs.
	var st autopilot.Status
	apGetJSON(t, ts.URL+"/api/v1/autopilot", &st)
	require.False(t, st.Enabled)
	require.Empty(t, st.Runs)

	// Enable → active, one run, no brain (inert).
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot", `{"enabled":true}`, &st)
	require.Equal(t, http.StatusOK, code)
	require.True(t, st.Enabled)
	require.Len(t, st.Runs, 1)
	require.Equal(t, autopilot.StateActive, st.Runs[0].State)
	require.Nil(t, st.Runs[0].Brain)

	// Disable → kill switch.
	code = apPostJSON(t, ts.URL+"/api/v1/autopilot", `{"enabled":false}`, &st)
	require.Equal(t, http.StatusOK, code)
	require.False(t, st.Enabled)
	require.Empty(t, st.Runs)
}

func TestAutopilotEnable409ListsFailures(t *testing.T) {
	dir := t.TempDir() // plan file intentionally absent
	ts := newAutopilotServer(t, &apFakeEnv{repo: dir}, []string{filepath.Join(dir, "missing.yaml")})
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/autopilot", "application/json", strings.NewReader(`{"enabled":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)

	var body struct {
		Error    string   `json:"error"`
		Failures []string `json:"failures"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.NotEmpty(t, body.Failures)
	require.Contains(t, strings.Join(body.Failures, " "), "plan file not found")
}

func TestAutopilotUnconfigured(t *testing.T) {
	// No Controller wired: GET reports disabled, POST is 403.
	srv := &Server{store: newFakeStore(), hub: newHub(), done: make(chan struct{})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	var st autopilot.Status
	apGetJSON(t, ts.URL+"/api/v1/autopilot", &st)
	require.False(t, st.Enabled)

	resp, err := http.Post(ts.URL+"/api/v1/autopilot", "application/json", strings.NewReader(`{"enabled":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

// --- small JSON helpers ---

func apGetJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
}

func apPostJSON(t *testing.T, url, body string, out any) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	return resp.StatusCode
}
