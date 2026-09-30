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
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

func noExportRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitInit(t, root)
	_, err := os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err))
	return root
}

func assertNoPlansDir(t *testing.T, root string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, "plans"))
	require.True(t, os.IsNotExist(err), "execution must not create a plans/ directory")
}

func assertSnapshot(t *testing.T, p *planstore.Plan, wantTasks int) {
	t.Helper()
	require.NotNil(t, p.ActiveExecution)
	require.NotNil(t, p.ActiveExecution.Snapshot, "execution must capture snapshot-at-start")
	require.Equal(t, p.ID, p.ActiveExecution.PlanID)
	require.Equal(t, p.Revision, p.ActiveExecution.Snapshot.Revision)
	require.NotEmpty(t, p.ActiveExecution.Snapshot.ContentHash)
	require.Len(t, p.ActiveExecution.Snapshot.Tasks, wantTasks)
}

func TestExecutionNoExport_Manual(t *testing.T) {
	root := noExportRoot(t)
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	life := &fakeLife{}
	srv := &Server{store: newFakeStore(), life: life, plans: plans, projects: projects}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	p := seedCanonicalPlan(t, plans, root, "manual-db", nil)
	resp := postJSON(t, planURL(ts.URL, root, "/"+p.ID+"/run"), map[string]any{"mode": "manual"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	updated, err := plans.Get(t.Context(), p.ID)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusInProgress, updated.Status)
	require.Equal(t, "", updated.FilePath)
	assertSnapshot(t, updated, 1)
	assertNoPlansDir(t, root)
	require.NotNil(t, life.spawned)
	require.Contains(t, life.spawned.Prompt, p.ID)
	require.NotContains(t, life.spawned.Prompt, "Plan file:")
}

func TestExecutionNoExport_Orchestrator(t *testing.T) {
	root := noExportRoot(t)
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	life := &fakeLife{}
	srv := &Server{store: newFakeStore(), life: life, plans: plans, projects: projects}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	p := seedCanonicalPlan(t, plans, root, "orch-db", []planstore.PlanTask{
		{ID: "a", Prompt: "analyze"},
		{ID: "b", Prompt: "implement", After: []string{"a"}},
	})
	resp := postJSON(t, planURL(ts.URL, root, "/"+p.ID+"/run"), map[string]any{"mode": "orchestrator_worker"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	updated, err := plans.Get(t.Context(), p.ID)
	require.NoError(t, err)
	assertSnapshot(t, updated, 2)
	assertNoPlansDir(t, root)
	require.Equal(t, "O:orch-db", life.spawned.Name)
}

func TestExecutionNoExport_Pipeline(t *testing.T) {
	root := noExportRoot(t)
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	pips, err := pipeline.NewStore(t.TempDir())
	require.NoError(t, err)
	cs, err := ctxstore.New(t.TempDir())
	require.NoError(t, err)
	ss := newFakeStore()
	fl := &fakeLife{}
	exec := NewExecutor(pips, ss, fl, cs, func() {})
	srv := &Server{store: ss, life: fl, plans: plans, exec: exec, projects: projects}
	exec.SetPlanPipelineHook(srv)
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	p := seedCanonicalPlan(t, plans, root, "pipe-db", []planstore.PlanTask{
		{ID: "t1", Prompt: "first"},
		{ID: "t2", Prompt: "second", After: []string{"t1"}},
	})
	resp := postJSON(t, planURL(ts.URL, root, "/"+p.ID+"/run"), map[string]any{"mode": "pipeline"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	updated, err := plans.Get(t.Context(), p.ID)
	require.NoError(t, err)
	assertSnapshot(t, updated, 2)
	assertNoPlansDir(t, root)

	pl, err := pips.Get(updated.PipelineID)
	require.NoError(t, err)
	require.Len(t, pl.Jobs, 2)
	require.Equal(t, "t1", pl.Jobs[0].ID)
	require.Equal(t, []string{"t1"}, pl.Jobs[1].DependsOn)
}

func TestExecutionNoExport_AutopilotAndRestart(t *testing.T) {
	root := noExportRoot(t)
	data := t.TempDir()
	plans, err := planstore.New(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	projects, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = projects.Close() })
	_, err = projects.OpenProject(root, "test", root)
	require.NoError(t, err)

	live, err := autopilotstore.New(data)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })
	runs, err := autopilot.NewRunStore(data)
	require.NoError(t, err)
	ctrl := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: root, DataDir: data, RunStore: runs, LiveStore: live, PlanSource: plans, Gate: "local",
	}, autopilot.NewExecEnv())

	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: plans, projects: projects, autopilot: ctrl}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)

	p := seedCanonicalPlan(t, plans, root, "ap-db", []planstore.PlanTask{
		{ID: "ship", Prompt: "ship it"},
	})
	resp := postJSON(t, planURL(ts.URL, root, "/"+p.ID+"/run"), map[string]any{"mode": "autopilot"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var got planstore.Plan
	require.NoError(t, json.Unmarshal(body, &got))
	require.NotEmpty(t, got.AutopilotRunID)
	updated, err := plans.Get(t.Context(), p.ID)
	require.NoError(t, err)
	assertSnapshot(t, updated, 1)
	assertNoPlansDir(t, root)

	// Close the first controller before opening a successor on the same stores.
	require.NoError(t, ctrl.Close())

	// Simulate daemon restart: new controller recovers from live Autopilot + Plan store.
	runs2, err := autopilot.NewRunStore(data)
	require.NoError(t, err)
	ctrl2 := autopilot.NewController(autopilot.ControllerConfig{
		BaseDir: root, DataDir: data, RunStore: runs2, LiveStore: live, PlanSource: plans, Gate: "local",
	}, autopilot.NewExecEnv())
	t.Cleanup(func() { require.NoError(t, ctrl2.Close()) })
	require.NoError(t, ctrl2.RecoverLiveAutopilots(context.Background()))

	progress := ctrl2.PlanTaskProgress(context.Background(), got.AutopilotRunID)
	require.Contains(t, progress, "ship")

	st := ctrl2.Status()
	found := false
	for _, r := range st.Runs {
		if r.PlanID == p.ID {
			found = true
			require.Len(t, r.PlanTasks, 1)
			require.Equal(t, "ship", r.PlanTasks[0].ID)
			require.Equal(t, "ship it", r.PlanTasks[0].Prompt)
		}
	}
	require.True(t, found, "restart must recover plan-bound autopilot from ScrivaDB")
	assertNoPlansDir(t, root)
}

func TestExecutionRejectsStructuralEditWhileInProgress(t *testing.T) {
	root := noExportRoot(t)
	plans, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = plans.Close() })
	svc := planstore.NewPlanService(plans, planstore.WithProjectRoot(func(string) string { return root }))

	created, err := svc.Create(t.Context(), root, planstore.CreateRequest{
		Name: "locked", Goal: "g",
		Tasks: []planstore.TaskSpec{{ID: "t1", Prompt: "one"}},
	})
	require.NoError(t, err)
	_, err = svc.Transition(t.Context(), created.ID, planstore.PlanStatusInProgress, planstore.TransitionOptions{
		ExecutionMode: planstore.PlanModeManual,
	})
	require.NoError(t, err)

	goal := "mutated"
	_, err = svc.Update(t.Context(), created.ID, planstore.UpdateRequest{Goal: &goal})
	require.ErrorIs(t, err, planstore.ErrNotPending)
}
