package autopilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/planstore"
)

// brokenExportYAML mirrors the ap-5b7b085f99c9 failure: an unquoted `key: value`
// list item decodes as a map, so the export no longer parses as a Plan.
const brokenExportYAML = "version: 1\ngoal: stale\nconstraints:\n  - Zero parsing at the hub: the hub is a pure relay\ntasks:\n  - id: a\n    prompt: x\n"

type errPlanSource struct{ err error }

func (e errPlanSource) Get(context.Context, string) (*planstore.Plan, error) { return nil, e.err }

func dbPlan(id string) *planstore.Plan {
	return &planstore.Plan{
		ID: id, Name: "hub", Goal: "relay-only hub goal",
		Tasks: []planstore.PlanTask{
			{ID: "db-task-1", Prompt: "build the relay"},
			{ID: "db-task-2", Prompt: "wire the hub", After: []string{"db-task-1"}},
		},
	}
}

// TestPlanBoundRecoveryIgnoresBrokenYAMLExport reproduces ap-5b7b085f99c9: a
// plan-bound run whose YAML export no longer parses must still recover after a
// daemon restart and respawn its missing manager from the ScrivaDB definition.
func TestPlanBoundRecoveryIgnoresBrokenYAMLExport(t *testing.T) {
	repo := t.TempDir()
	planFile := filepath.Join(repo, "plans", "remote-access-mvp.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(planFile), 0o755))
	require.NoError(t, os.WriteFile(planFile, []byte(brokenExportYAML), 0o644))
	data := t.TempDir()
	const planID = "plan-96d33b9f"
	runID := PlanBoundRunID(repo, planID)
	slot := ManagerSlotID("hub")

	rs, err := NewRunStore(data)
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, rs.Create(RunRecord{
		RunID: runID, Name: "hub", Repo: repo, PlanFile: planFile, PlanID: planID,
		State: StateActive, SlotScope: "hub", BrainID: slot, IntegrationBranch: "autopilot/hub",
		CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, rs.Close())

	t0 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	src := &fakePlanSource{plans: map[string]*planstore.Plan{planID: dbPlan(planID)}}
	c := NewController(ControllerConfig{
		DataDir: data, BaseDir: repo, PlanSource: src, Gate: "local",
		Resolver: cyclicResolver("a", "free"), Guardian: testGuardian(),
	}, &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }})
	t.Cleanup(func() { _ = c.Close() })
	c.setClock(clock.now)
	require.NoError(t, c.enableStore.Enable(repo))
	c.SetRuntime(fake)
	require.NoError(t, c.RecoverLiveAutopilots(context.Background()))

	require.Equal(t, StateActive, c.Status().Runs[0].State, "broken YAML export must not degrade a plan-bound run")

	// Manager session lost: the guardian tick must respawn it from the DB plan.
	before := len(fake.spawned)
	c.mu.Lock()
	require.NotNil(t, c.runs[runID].brain, "boot recovery must have adopted/spawned a manager")
	fake.missing[c.runs[runID].brain.AgentID] = true
	c.mu.Unlock()
	clock.t = t0.Add(time.Minute)
	c.guardianTick(context.Background())

	require.Greater(t, len(fake.spawned), before, "guardian must respawn the manager")
	prompt := fake.spawned[len(fake.spawned)-1].Prompt
	require.Contains(t, prompt, "relay-only hub goal")
	require.Contains(t, prompt, "db-task-2")
	require.NotContains(t, prompt, "plan not loadable")
	require.NotEqual(t, StateDegraded, c.Status().Runs[0].State)
}

func TestSpawnBrainPlanSourceErrorNamesPlanID(t *testing.T) {
	repo := t.TempDir()
	c := NewController(ControllerConfig{
		BaseDir: repo, PlanSource: errPlanSource{err: errors.New("db down")},
		Resolver: &fakeResolver{backendID: "a", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(newFakeRuntime())
	r := &run{runID: "ap-x", planID: "plan-err1", repo: repo, state: StateDegraded,
		slotScope: "x", tried: map[string]bool{}}
	c.mu.Lock()
	err := c.spawnBrain(context.Background(), r, "a")
	c.mu.Unlock()
	require.Error(t, err)
	require.Contains(t, err.Error(), "plan-err1")
	require.Equal(t, StateDegraded, r.state)
}

func TestRotateBrainHydratesFromPlanSource(t *testing.T) {
	repo := t.TempDir()
	rt := newFakeRuntime()
	src := &fakePlanSource{plans: map[string]*planstore.Plan{"plan-rot1": dbPlan("plan-rot1")}}
	c := NewController(ControllerConfig{
		BaseDir: repo, PlanSource: src, Resolver: &fakeResolver{backendID: "a", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(rt)
	r := &run{runID: "ap-rot", planID: "plan-rot1", repo: repo, state: StateActive,
		slotScope: "rot", brain: &BrainHandle{AgentID: ManagerSlotID("rot")}, tried: map[string]bool{}}
	c.mu.Lock()
	err := c.rotateBrain(context.Background(), r, "b", "test")
	c.mu.Unlock()
	require.NoError(t, err)
	require.Len(t, rt.rotated, 1)
	require.Contains(t, rt.rotated[0].Prompt, "relay-only hub goal")
}
