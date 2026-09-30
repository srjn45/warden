package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

func seedFinalizeReadyPlan(t *testing.T, plans *planstore.Store, root, name, agentID string) *planstore.Plan {
	t.Helper()
	ctx := context.Background()
	id := planstore.PlanID(root, name)
	dir := filepath.Join(root, "plans", "in_progress")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name+".yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\nname: "+name+"\ngoal: g\ntasks:\n  - id: t1\n    prompt: p1\n  - id: t2\n    prompt: p2\n"), 0o644))

	execID := "pe-fin01"
	now := time.Now().UTC()
	p := &planstore.Plan{
		ID: id, ProjectID: root, Name: name,
		FilePath:       "plans/in_progress/" + name + ".yaml",
		Status:         planstore.PlanStatusInProgress,
		ExecutionMode:  planstore.PlanModeManual,
		OrchestratorID: agentID,
		TaskProgress:   map[string]string{"t1": "done", "t2": "done"},
		ActiveExecution: &planstore.PlanExecution{
			ID: execID, PlanID: id, ExecutionMode: planstore.PlanModeManual,
			ExecutorID: agentID, StartedAt: now, TerminalStatus: planstore.ExecutionStatusRunning,
		},
	}
	require.NoError(t, plans.Create(ctx, p))
	require.NoError(t, plans.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		DedupKey: id + ":" + execID + ":execution_started",
		PlanID:   id, ExecutionID: execID, Kind: planstore.EventKindExecutionStarted, OccurredAt: now,
		Payload: &planstore.EventPayload{PlanName: name, ExecutionMode: string(planstore.PlanModeManual), TasksTotal: 2, ExecutorID: agentID},
	}))
	return p
}

func TestFinalizePlan_DeletesExecutorAfterSummary(t *testing.T) {
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

	fs := newFakeStore()
	life := &fakeLife{}
	agent := &agentstore.Agent{
		ID: "agent-fin", PlanID: planstore.PlanID(root, "fin-del"), Name: "M:fin-del",
		Status: store.StatusWorking, TmuxSession: "tmux-fin",
		Worktree: root + "/.worktrees/fin", Branch: "feat/fin", BranchCreated: true,
	}
	fs.data[agent.ID] = agent
	p := seedFinalizeReadyPlan(t, plans, root, "fin-del", agent.ID)

	var sawSummaryBeforeDelete bool
	srv := &Server{store: fs, life: life, plans: plans, projects: projects}
	srv.finalizeCleanupHook = func(ctx context.Context, pl *planstore.Plan) planstore.CleanupEvidence {
		require.NotNil(t, pl.ExecutionSummary)
		sawSummaryBeforeDelete = true
		return srv.cleanupPlanExecutors(ctx, pl)
	}

	res, err := srv.FinalizePlan(context.Background(), p.ID)
	require.NoError(t, err)
	require.True(t, sawSummaryBeforeDelete)
	require.Equal(t, planstore.PlanStatusCompleted, res.Plan.Status)
	require.NotNil(t, res.Plan.ExecutionSummary)

	_, err = fs.Get(context.Background(), agent.ID)
	require.ErrorIs(t, err, agentstore.ErrNotFound, "root agent must be archived/deleted")
	require.Equal(t, agent.TmuxSession, life.terminated)
	require.Equal(t, agent.ID, life.removedWT)

	events, err := plans.ListEvents(context.Background(), p.ID, "pe-fin01")
	require.NoError(t, err)
	require.NotEmpty(t, events, "execution events must be preserved")
}

func TestFinalizePlan_AutopilotTeardown(t *testing.T) {
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
	}, autopilot.NewExecEnv())
	t.Cleanup(func() { require.NoError(t, controller.Close()) })

	apID := "ap-finalize01"
	managerID := "mgr-finalize"
	require.NoError(t, live.Create(context.Background(), &autopilotstore.Autopilot{
		ID: apID, PlanID: planstore.PlanID(root, "ap-fin"), ProjectID: root,
		Name: "AP:ap-fin", ManagerAgentID: managerID,
		Diagnostics: autopilotstore.Diagnostics{State: "active", Repo: root},
	}))

	fs := newFakeStore()
	mgr := &agentstore.Agent{
		ID: managerID, PlanID: planstore.PlanID(root, "ap-fin"), Role: "autopilot",
		Status: store.StatusWorking, TmuxSession: "tmux-mgr",
	}
	worker := &agentstore.Agent{
		ID: "worker-fin", PlanID: planstore.PlanID(root, "ap-fin"), ParentID: managerID,
		Role: "worker", Status: store.StatusWorking, TmuxSession: "tmux-w",
		Worktree: root + "/.worktrees/w", Branch: "feat/w", BranchCreated: true,
	}
	fs.data[mgr.ID] = mgr
	fs.data[worker.ID] = worker

	ctx := context.Background()
	id := planstore.PlanID(root, "ap-fin")
	dir := filepath.Join(root, "plans", "in_progress")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ap-fin.yaml"), []byte("version: 1\nname: ap-fin\ngoal: g\ntasks:\n  - id: t1\n    prompt: p\n"), 0o644))
	now := time.Now().UTC()
	execID := "pe-apfin"
	require.NoError(t, plans.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: root, Name: "ap-fin",
		FilePath: "plans/in_progress/ap-fin.yaml", Status: planstore.PlanStatusInProgress,
		ExecutionMode: planstore.PlanModeAutopilot, AutopilotRunID: apID,
		TaskProgress: map[string]string{"t1": "done"},
		ActiveExecution: &planstore.PlanExecution{
			ID: execID, PlanID: id, ExecutionMode: planstore.PlanModeAutopilot,
			ExecutorID: apID, StartedAt: now,
		},
	}))
	require.NoError(t, plans.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		DedupKey: id + ":" + execID + ":execution_started",
		PlanID:   id, ExecutionID: execID, Kind: planstore.EventKindExecutionStarted, OccurredAt: now,
		Payload: &planstore.EventPayload{PlanName: "ap-fin", ExecutionMode: "autopilot", TasksTotal: 1, ExecutorID: apID},
	}))

	srv := &Server{store: fs, life: &fakeLife{}, plans: plans, projects: projects, autopilot: controller}
	res, err := srv.FinalizePlan(ctx, id)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusCompleted, res.Plan.Status)
	require.Empty(t, res.Plan.AutopilotRunID)

	_, err = live.Get(ctx, apID)
	require.ErrorIs(t, err, autopilotstore.ErrNotFound)
	_, err = fs.Get(ctx, managerID)
	require.ErrorIs(t, err, agentstore.ErrNotFound)
	_, err = fs.Get(ctx, worker.ID)
	require.ErrorIs(t, err, agentstore.ErrNotFound)
}

func TestCompletePlanRoute_UsesFinalize(t *testing.T) {
	ts, ps, _, root := planGitServer(t)
	ctx := t.Context()

	seedPlanYAML(t, root, "plans/pending/fin-route.yaml", "fin-route")
	id := planstore.PlanID(root, "fin-route")
	require.NoError(t, ps.Create(ctx, &planstore.Plan{
		ID: id, ProjectID: root, Name: "fin-route",
		FilePath: "plans/pending/fin-route.yaml", Status: planstore.PlanStatusPending,
	}))

	require.NoError(t, os.MkdirAll(filepath.Join(root, "plans/in_progress"), 0o755))
	require.NoError(t, os.Rename(
		filepath.Join(root, "plans/pending/fin-route.yaml"),
		filepath.Join(root, "plans/in_progress/fin-route.yaml"),
	))
	now := time.Now().UTC()
	execID := "pe-route"
	require.NoError(t, ps.Update(ctx, id, func(p *planstore.Plan) error {
		p.Status = planstore.PlanStatusInProgress
		p.FilePath = "plans/in_progress/fin-route.yaml"
		p.ExecutionMode = planstore.PlanModeManual
		p.TaskProgress = map[string]string{"t1": "done"}
		p.ActiveExecution = &planstore.PlanExecution{
			ID: execID, PlanID: id, ExecutionMode: planstore.PlanModeManual,
			ExecutorID: "a1", StartedAt: now,
		}
		return nil
	}))
	require.NoError(t, ps.AppendEvent(ctx, &planstore.PlanExecutionEvent{
		DedupKey: id + ":" + execID + ":execution_started",
		PlanID:   id, ExecutionID: execID, Kind: planstore.EventKindExecutionStarted, OccurredAt: now,
		Payload: &planstore.EventPayload{PlanName: "fin-route", TasksTotal: 1, ExecutionMode: "manual"},
	}))

	resp := postJSON(t, ts.URL+"/api/v1/plans/"+id+"/complete", map[string]any{})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))

	var got oapiPlanStatus
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "completed", got.Status)

	stored, err := ps.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, planstore.PlanStatusCompleted, stored.Status)
	require.NotNil(t, stored.ExecutionSummary)
}

// oapiPlanStatus is a minimal decode of the CompletePlan JSON response.
type oapiPlanStatus struct {
	Status string `json:"status"`
}
