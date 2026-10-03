package autopilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/store"
)

type fakePlanSource struct {
	plans map[string]*planstore.Plan
}

func (f *fakePlanSource) Get(_ context.Context, planID string) (*planstore.Plan, error) {
	p, ok := f.plans[planID]
	if !ok {
		return nil, planstore.ErrNotFound
	}
	return p, nil
}

type planRuntime struct {
	managers []BrainSpec
	brains   []ConsultBrainRuntimeSpec
	nextID   int
}

func (r *planRuntime) SpawnBrain(_ context.Context, spec BrainSpec) (BrainHandle, error) {
	r.managers = append(r.managers, spec)
	r.nextID++
	return BrainHandle{AgentID: ManagerSlotID(spec.SlotScope), Backend: spec.Backend}, nil
}
func (r *planRuntime) RotateBrain(context.Context, RotateBrainSpec) (BrainHandle, error) {
	return BrainHandle{}, nil
}
func (r *planRuntime) TerminateBrain(context.Context, string) error { return nil }
func (r *planRuntime) NewLedger(string) *Ledger                     { return nil }
func (r *planRuntime) DigestSources() DigestSources                 { return nil }
func (r *planRuntime) NotifyOwner(string, string)                   {}
func (r *planRuntime) InstallDefaultAutoApprovePolicy()             {}
func (r *planRuntime) SpawnConsultBrain(_ context.Context, spec ConsultBrainRuntimeSpec) (BrainHandle, error) {
	r.brains = append(r.brains, spec)
	r.nextID++
	return BrainHandle{AgentID: "brain-ondemand-" + spec.RunID, Backend: spec.Backend}, nil
}

func planControllerFixture(t *testing.T) (*Controller, *autopilotstore.Store, *planRuntime, string, string) {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "plans", "pending"), 0o755))
	planPath := writePlan(t, filepath.Join(repo, "plans", "pending"), "ship.yaml", "ship it")

	live, err := autopilotstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })

	runs, err := NewRunStore(dir)
	require.NoError(t, err)
	c := NewController(ControllerConfig{
		BaseDir:   repo,
		DataDir:   dir,
		RunStore:  runs,
		LiveStore: live,
		Gate:      "local",
	}, &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }})
	rt := &planRuntime{}
	c.SetRuntime(rt)
	return c, live, rt, repo, planPath
}

func TestStartFromPlan_RequiresPlanID(t *testing.T) {
	c, _, _, repo, planPath := planControllerFixture(t)
	_, err := c.StartFromPlan(context.Background(), PlanStartRequest{
		Repo: repo, PlanFile: planPath, Name: "ship",
	})
	require.ErrorIs(t, err, ErrPlanIDRequired)
}

func TestStartFromPlan_CreatesLiveAutopilotAndManager(t *testing.T) {
	c, live, rt, repo, planPath := planControllerFixture(t)
	planID := "plan-aabbccdd"
	res, err := c.StartFromPlan(context.Background(), PlanStartRequest{
		PlanID: planID, ProjectID: "proj-1", Name: "ship",
		Repo: repo, PlanFile: planPath, ParentAgentID: "agent-owner",
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.AutopilotID)
	require.NotEmpty(t, res.ManagerAgentID)
	require.Equal(t, planID, res.Status.PlanID)

	ap, err := live.Get(context.Background(), res.AutopilotID)
	require.NoError(t, err)
	require.Equal(t, planID, ap.PlanID)
	require.Equal(t, "AP:ship", ap.Name)
	require.Equal(t, res.ManagerAgentID, ap.ManagerAgentID)
	require.Equal(t, "agent-owner", ap.ParentAgentID)
	require.Len(t, rt.managers, 1)
	require.Equal(t, planID, rt.managers[0].PlanID)
	require.Contains(t, rt.managers[0].Tags, "autopilot")
}

func TestStartFromPlan_ManagerWorkerParentage(t *testing.T) {
	c, _, _, repo, planPath := planControllerFixture(t)
	res, err := c.StartFromPlan(context.Background(), PlanStartRequest{
		PlanID: "plan-parent01", ProjectID: "proj-1", Name: "ship",
		Repo: repo, PlanFile: planPath,
	})
	require.NoError(t, err)

	manager := &agentstore.Agent{
		ID: res.ManagerAgentID, Role: "autopilot", PlanID: "plan-parent01",
		Tags: []string{"autopilot", "run:" + res.AutopilotID},
	}
	require.True(t, IsManagerRecord(manager))

	worker := &agentstore.Agent{
		ID: "worker-1", Role: "worker", ParentID: res.ManagerAgentID,
		PlanID: "plan-parent01", Tags: []string{"autopilot", "run:" + res.AutopilotID},
	}
	require.True(t, IsWorkerRecord(worker))
	require.Equal(t, res.ManagerAgentID, worker.ParentID)
	require.Empty(t, worker.AutopilotRunID)
	require.Empty(t, worker.AutopilotSlot)
	require.Empty(t, worker.AutopilotTaskID)
}

func TestSpawnOnDemandBrain_Headless(t *testing.T) {
	c, live, rt, repo, planPath := planControllerFixture(t)
	res, err := c.StartFromPlan(context.Background(), PlanStartRequest{
		PlanID: "plan-brain01", ProjectID: "proj-1", Name: "ship",
		Repo: repo, PlanFile: planPath,
	})
	require.NoError(t, err)

	handle, err := c.SpawnOnDemandBrain(context.Background(), ConsultBrainSpec{
		AutopilotID: res.AutopilotID,
		Prompt:      "unblock worker",
	})
	require.NoError(t, err)
	require.NotEmpty(t, handle.AgentID)
	require.Len(t, rt.brains, 1)
	require.Equal(t, "plan-brain01", rt.brains[0].PlanID)
	require.True(t, rt.brains[0].Headless)
	require.Contains(t, rt.brains[0].Tags, "system:true")
	require.Contains(t, rt.brains[0].Tags, "autopilot")

	ap, err := live.Get(context.Background(), res.AutopilotID)
	require.NoError(t, err)
	require.Equal(t, handle.AgentID, ap.BrainAgentID)

	brainAgent := &agentstore.Agent{
		ID: handle.AgentID, Role: "brain", PlanID: "plan-brain01",
		Tags:   []string{"autopilot", "run:" + res.AutopilotID, "system:true"},
		Status: store.StatusWorking,
	}
	require.True(t, IsHeadlessBrain(brainAgent))
}

func TestRecoverLiveAutopilots_TaskProgressFromPlan(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "plans", "in_progress"), 0o755))
	planPath := filepath.Join(repo, "plans", "in_progress", "recover.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte(`version: 1
name: recover
goal: recover tasks
tasks:
  - id: t1
    prompt: one
  - id: t2
    prompt: two
`), 0o644))

	live, err := autopilotstore.New(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = live.Close() })

	planID := "plan-recover1"
	runID := RunID(repo, planPath)
	require.NoError(t, live.Create(context.Background(), &autopilotstore.Autopilot{
		ID: runID, PlanID: planID, ProjectID: "proj-1", Name: "AP:recover",
		ManagerAgentID: "recover-autopilot",
		Diagnostics: autopilotstore.Diagnostics{
			State: string(StateActive), Repo: repo, PlanFile: planPath, SlotScope: "recover",
		},
	}))

	src := &fakePlanSource{plans: map[string]*planstore.Plan{
		planID: {
			ID: planID, Name: "recover", Status: planstore.PlanStatusInProgress,
			TaskProgress: map[string]string{"t1": "done", "t2": "in_progress"},
			ActiveExecution: &planstore.PlanExecution{
				ID: "pe-1", PlanID: planID, ExecutorID: runID,
				TaskProgress: map[string]string{"t2": "in_progress"},
			},
		},
	}}

	runs, err := NewRunStore(dir)
	require.NoError(t, err)
	c1 := NewController(ControllerConfig{
		BaseDir: repo, DataDir: dir, RunStore: runs, LiveStore: live, PlanSource: src, Gate: "local",
	}, &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }})
	require.NoError(t, c1.Close())

	// Simulate daemon restart: new controller, same live + plan stores.
	runs2, err := NewRunStore(dir)
	require.NoError(t, err)
	c2 := NewController(ControllerConfig{
		BaseDir: repo, DataDir: dir, RunStore: runs2, LiveStore: live, PlanSource: src, Gate: "local",
	}, &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }})
	require.NoError(t, c2.RecoverLiveAutopilots(context.Background()))

	progress := c2.PlanTaskProgress(context.Background(), runID)
	require.Equal(t, "done", progress["t1"])
	require.Equal(t, "in_progress", progress["t2"])
	st := c2.Status()
	var found bool
	for _, r := range st.Runs {
		if r.RunID == runID {
			found = true
			require.Equal(t, planID, r.PlanID)
			require.Equal(t, StateActive, r.State)
		}
	}
	require.True(t, found, "recovered run missing from status")
}
