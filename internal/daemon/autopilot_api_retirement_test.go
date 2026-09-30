package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

func TestEnableDoesNotRegisterWork(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "plans", "pending", "ship.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(plan), 0o755))
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\nname: ship\ngoal: go\ntasks:\n  - id: t1\n    prompt: do\n"), 0o644))

	c := autopilot.NewController(autopilot.ControllerConfig{
		Plans:             []string{plan},
		BaseDir:           dir,
		IntegrationBranch: "autopilot/integration",
		Resolver:          autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{})}
	srv.SetAutopilotController(c)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	var stResp oapi.AutopilotStatus
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot", `{"enabled":true,"repo":"`+dir+`"}`, &stResp)
	require.Equal(t, http.StatusOK, code)
	require.True(t, stResp.Enabled)

	st := c.Status()
	require.True(t, st.Enabled)
	require.Contains(t, st.EnabledRepos, dir)
	require.Empty(t, st.Runs, "enable must not register or start Autopilot work")
	require.Empty(t, stResp.Runs)
}

func TestDeprecatedRegisterReturnsMigrationWithPlanID(t *testing.T) {
	root := t.TempDir()
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	svc := planstore.NewPlanService(ps, planstore.WithProjectRoot(func(string) string { return root }))
	p, err := svc.Create(context.Background(), root, planstore.CreateRequest{
		Name:  "ship",
		Goal:  "go",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "do"}},
	})
	require.NoError(t, err)

	c := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir:           root,
		IntegrationBranch: "autopilot/integration",
		Resolver:          autopilotTestResolver{},
	}, &apFakeEnv{repo: root})
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{}), plans: ps}
	srv.SetAutopilotController(c)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	planFile := filepath.Join(root, p.FilePath)
	var body oapi.Error
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot/runs",
		`{"plan_file":"`+planFile+`","name":"ship","repo":"`+root+`"}`, &body)
	require.Equal(t, http.StatusGone, code)
	require.Contains(t, body.Error, p.ID)
	require.Contains(t, body.Error, "/run")
}

func TestDeprecatedRegisterWithoutPlanIDReturnsPreciseError(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "orphan.yaml")
	require.NoError(t, os.WriteFile(orphan, []byte("version: 1\ngoal: x\n"), 0o644))

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	c := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir:  dir,
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{}), plans: ps}
	srv.SetAutopilotController(c)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	var body oapi.Error
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot/runs",
		`{"plan_file":"`+orphan+`","name":"orphan","repo":"`+dir+`"}`, &body)
	require.Equal(t, http.StatusBadRequest, code)
	require.Contains(t, body.Error, "cannot resolve PlanID")
	require.Contains(t, body.Error, "wd plan scan")
}

func TestDeprecatedRetargetReturnsMigrationError(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "ship.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	c := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir:  dir,
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{}), plans: ps}
	srv.SetAutopilotController(c)

	r, err := c.Register(context.Background(), autopilot.RegisterRequest{
		Name: "ship", Repo: dir, PlanFile: plan, PlanID: "plan-aabbccdd",
	})
	require.NoError(t, err)

	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	var body oapi.Error
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot/runs/"+r.RunID+"/retarget",
		`{"derive":true}`, &body)
	require.Equal(t, http.StatusGone, code)
	require.Contains(t, body.Error, "retarget is retired")
	require.Contains(t, body.Error, "plan-aabbccdd")
}

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

func TestDeprecatedControlPauseTranslatesToPlanControl(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "ship.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))

	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })

	c := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir:  dir,
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{}), plans: ps}
	srv.SetAutopilotController(c)

	planID := "plan-translat1"
	r, err := c.Register(context.Background(), autopilot.RegisterRequest{
		Name: "ship", Repo: dir, PlanFile: plan, PlanID: planID,
	})
	require.NoError(t, err)
	_, err = c.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)

	require.NoError(t, ps.Create(context.Background(), &planstore.Plan{
		ID: planID, ProjectID: dir, Name: "ship",
		FilePath: "ship.yaml", Status: planstore.PlanStatusInProgress,
		AutopilotRunID: r.RunID, ExecutionMode: planstore.PlanModeAutopilot,
	}))

	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	var out autopilot.RunStatus
	code := apPostJSON(t, ts.URL+"/api/v1/autopilot/runs/"+r.RunID+"/pause", `{}`, &out)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, autopilot.StatePaused, out.State)
}
