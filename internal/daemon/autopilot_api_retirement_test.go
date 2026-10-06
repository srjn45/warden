package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

func TestControlPlanPauseResumeStopAutopilot(t *testing.T) {
	root := t.TempDir()
	gitInit(t, root)

	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	data := t.TempDir()
	runs, err := autopilot.NewRunStore(data)
	require.NoError(t, err)
	live, err := autopilotstore.New(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })
	controller := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: root, RunStore: runs, LiveStore: live, PlanSource: plans, Gate: "local",
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: root})
	t.Cleanup(func() { require.NoError(t, controller.Close()) })

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: plans, projects: projects, autopilot: controller}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	seedPlanYAML(t, root, "plans/pending/ctrl-plan.yaml", "ctrl-plan")
	id := planstore.PlanID(root, "ctrl-plan")
	require.NoError(t, plans.Create(t.Context(), &planstore.Plan{
		ID: id, ProjectID: root, Name: "ctrl-plan",
		FilePath: "plans/pending/ctrl-plan.yaml", Status: planstore.PlanStatusPending,
	}))

	resp := postJSON(t, ts.URL+"/api/v1/plans/"+id+"/run", map[string]any{"execution_mode": "autopilot"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var got planstore.Plan
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotEmpty(t, got.AutopilotRunID)

	for _, action := range []string{"pause", "resume", "stop"} {
		r, err := http.Post(ts.URL+"/api/v1/plans/"+id+"/"+action, "application/json", http.NoBody)
		require.NoError(t, err)
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		require.Equal(t, http.StatusOK, r.StatusCode, "action=%s body=%s", action, b)
	}
}

// TestRetiredAutopilotAliasRoutesAreGone pins the removal of the deprecated
// write aliases: plan execution is driven only through /plans/{plan_id}/....
// GET /api/v1/autopilot remains the status surface.
func TestRetiredAutopilotAliasRoutesAreGone(t *testing.T) {
	dir := t.TempDir()
	c := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir:  dir,
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(c)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	// Status GET stays.
	var st autopilot.Status
	apGetJSON(t, ts.URL+"/api/v1/autopilot", &st)
	require.Empty(t, st.Runs)

	// Retired write/list aliases must not be JSON API handlers. POSTs get
	// 404/405 from chi; the retired GET falls through to the SPA catch-all
	// (non-JSON 200) because nothing under /api/v1/autopilot/runs is registered.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/autopilot"},
		{http.MethodPost, "/api/v1/autopilot/runs"},
		{http.MethodPost, "/api/v1/autopilot/runs/ap-x/start"},
		{http.MethodPost, "/api/v1/autopilot/runs/ap-x/pause"},
		{http.MethodPost, "/api/v1/autopilot/runs/ap-x/unregister"},
		{http.MethodPost, "/api/v1/autopilot/runs/ap-x/rename"},
		{http.MethodPost, "/api/v1/autopilot/runs/ap-x/retarget"},
	} {
		req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(`{}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, resp.StatusCode,
			"%s %s must not be routed", tc.method, tc.path)
	}

	resp, err := http.Get(ts.URL + "/api/v1/autopilot/runs")
	require.NoError(t, err)
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	require.NotContains(t, ct, "application/json",
		"GET /api/v1/autopilot/runs must not be a JSON API route (got Content-Type %q)", ct)
}
