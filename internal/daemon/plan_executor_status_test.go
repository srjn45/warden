package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
)

func getPlanOverHTTP(t *testing.T, srv *Server, id string) map[string]any {
	t.Helper()
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/v1/plans/" + id)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

func TestGetPlanExecutorAutopilot(t *testing.T) {
	dir := t.TempDir()
	plan := filepath.Join(dir, "ship.yaml")
	require.NoError(t, os.WriteFile(plan, []byte("version: 1\ngoal: ship\n"), 0o644))
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, hub: newHub(), done: make(chan struct{}), plans: ps}
	c := autopilot.NewController(autopilot.ControllerConfig{
		Plans: []string{plan}, BaseDir: dir, IntegrationBranch: autopilot.DefaultIntegrationBranch,
		Resolver: autopilotTestResolver{},
	}, &apFakeEnv{repo: dir})
	srv.SetAutopilotController(c)
	st, err := c.ReconcileConfiguredPlans(context.Background(), dir)
	require.NoError(t, err)
	run := st.Runs[0]

	require.NoError(t, ps.Create(t.Context(), &planstore.Plan{
		ID: "plan-ap", ProjectID: dir, Name: "ap", Goal: "g", Status: planstore.PlanStatusInProgress,
		AutopilotRunID: run.RunID, Tasks: []planstore.PlanTask{{ID: "t1", Prompt: "p"}},
	}))
	got := getPlanOverHTTP(t, srv, "plan-ap")
	ex, ok := got["executor"].(map[string]any)
	require.True(t, ok, "executor block missing: %v", got)
	require.Equal(t, "autopilot", ex["kind"])
	require.Equal(t, run.RunID, ex["id"])
	require.Equal(t, string(run.State), ex["state"])
	require.Equal(t, run.IntegrationBranch, ex["integration_branch"])
	require.Equal(t, run.Brain.AgentID, ex["manager_agent_id"])
}

func TestGetPlanExecutorPipeline(t *testing.T) {
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	exec, pstore, _ := newTestExecutor(t)
	require.NoError(t, pstore.Create(&pipeline.Pipeline{
		ID: "pipe-1", Name: "pipe-1", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: "a", Status: pipeline.JobRunning, AgentID: "agent-a", Branch: "br-a"},
			{ID: "b", Status: pipeline.JobPending},
		},
	}))
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps, exec: exec}
	require.NoError(t, ps.Create(t.Context(), &planstore.Plan{
		ID: "plan-pl", ProjectID: "p", Name: "pl", Goal: "g", Status: planstore.PlanStatusInProgress,
		PipelineID: "pipe-1", Tasks: []planstore.PlanTask{{ID: "a", Prompt: "p"}},
	}))
	ex := srv.planExecutorStatus(t.Context(), mustGetPlan(t, ps, "plan-pl"))
	require.NotNil(t, ex)
	require.Equal(t, oapi.PlanExecutorStatus{
		Kind: "pipeline", Id: "pipe-1", State: "running",
		Tasks: []oapi.PlanExecutorTask{
			{Id: "a", State: "running", WorkerAgentId: "agent-a", Branch: "br-a"},
			{Id: "b", State: "pending"},
		},
	}, *ex)
}

func TestGetPlanNoExecutorOmitsBlock(t *testing.T) {
	ps, err := planstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ps.Close() })
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, plans: ps}
	for id, status := range map[string]planstore.PlanStatus{
		"plan-pend": planstore.PlanStatusPending, "plan-done": planstore.PlanStatusCompleted,
	} {
		require.NoError(t, ps.Create(t.Context(), &planstore.Plan{
			ID: id, ProjectID: "p", Name: id, Goal: "g", Status: status,
			Tasks: []planstore.PlanTask{{ID: "t1", Prompt: "p"}},
		}))
		got := getPlanOverHTTP(t, srv, id)
		_, has := got["executor"]
		require.False(t, has, "%s must not carry an executor block", id)
	}
}

func mustGetPlan(t *testing.T, ps *planstore.Store, id string) *planstore.Plan {
	t.Helper()
	p, err := ps.Get(t.Context(), id)
	require.NoError(t, err)
	return p
}
