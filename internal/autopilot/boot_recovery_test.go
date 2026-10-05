package autopilot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSetRuntimeContentOnlyBootRecovery proves that a daemon restart with a
// plan whose only problems are content-level (invalid task status) proceeds
// via loadPlanLenient: the run becomes active, the coerced status is pending,
// and preflight_warnings is populated for the operator.
func TestSetRuntimeContentOnlyBootRecovery(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "plan.yaml", "ship it")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c1 := NewController(ControllerConfig{
		DataDir:  data,
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, env)
	rt1 := newFakeRuntime()
	c1.SetRuntime(rt1)
	r, err := c1.Register(context.Background(), RegisterRequest{PlanFile: plan})
	require.NoError(t, err)
	_, err = c1.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	require.NoError(t, c1.Close())

	// Corrupt the plan with a content-only failure (invalid status string).
	require.NoError(t, os.WriteFile(plan, []byte(
		"version: 1\ngoal: ship it\ntasks:\n  - id: a\n    prompt: x\n    status: completed\n",
	), 0o644))

	c2 := NewController(ControllerConfig{
		DataDir:  data,
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, env)
	t.Cleanup(func() { require.NoError(t, c2.Close()) })
	rt2 := newFakeRuntime()
	c2.SetRuntime(rt2)

	require.Len(t, rt2.spawned, 1, "content-only failure must not block brain spawn at boot")
	st := c2.Status()
	require.Len(t, st.Runs, 1)
	require.Equal(t, StateActive, st.Runs[0].State)
	require.NotEmpty(t, st.Runs[0].PreflightWarnings)
	require.Contains(t, st.Runs[0].PreflightWarnings[0], "normalized to pending")

	c2.mu.Lock()
	run := c2.runs[r.RunID]
	require.Equal(t, TaskStatusPending, run.plan.Tasks[0].Status)
	c2.mu.Unlock()
}

// TestSetRuntimeStructuralBootRecoveryBlocks proves a missing plan file is a
// structural failure: the run stays degraded and no brain is spawned.
func TestSetRuntimeStructuralBootRecoveryBlocks(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "plan.yaml", "ship it")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c1 := NewController(ControllerConfig{
		DataDir:  data,
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, env)
	c1.SetRuntime(newFakeRuntime())
	r, err := c1.Register(context.Background(), RegisterRequest{PlanFile: plan})
	require.NoError(t, err)
	_, err = c1.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	require.NoError(t, c1.Close())

	require.NoError(t, os.Remove(plan))

	c2 := NewController(ControllerConfig{
		DataDir:  data,
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, env)
	t.Cleanup(func() { require.NoError(t, c2.Close()) })
	rt2 := newFakeRuntime()
	c2.SetRuntime(rt2)

	require.Empty(t, rt2.spawned, "structural failure must not spawn a brain")
	st := c2.Status()
	require.Len(t, st.Runs, 1)
	require.Equal(t, StateDegraded, st.Runs[0].State)
	// The stored manager slot id may still appear on status (restored from the
	// durable record); what matters is no new SpawnBrain call and degraded state.
}

// TestWatchPlanDegradedContentRecovery proves that a degraded run whose plan
// has only content errors is promoted and gets a brain on the next watchPlan
// tick (via lenient load), without a daemon restart.
func TestWatchPlanDegradedContentRecovery(t *testing.T) {
	repo := t.TempDir()
	planPath := filepath.Join(repo, "plan.yaml")
	// Start with a valid plan so Register accepts it.
	require.NoError(t, os.WriteFile(planPath, []byte("version: 1\ngoal: ship it\n"), 0o644))
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c := NewController(ControllerConfig{
		DataDir:  data,
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, env)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	rt := newFakeRuntime()
	c.SetRuntime(rt)

	reg, err := c.Register(context.Background(), RegisterRequest{PlanFile: planPath})
	require.NoError(t, err)

	// Corrupt to a content-only failure, then put the run into degraded with no brain.
	require.NoError(t, os.WriteFile(planPath, []byte(
		"version: 1\ngoal: ship it\ntasks:\n  - id: a\n    prompt: x\n    status: completed\n",
	), 0o644))

	c.mu.Lock()
	run := c.runs[reg.RunID]
	run.state = StateDegraded
	run.brain = nil
	run.plan = Plan{} // empty — as if restore could not load it cleanly
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	run.cancel = cancel
	go c.watchPlan(ctx, run, 15*time.Millisecond)
	c.mu.Unlock()

	require.Eventually(t, func() bool {
		return len(rt.spawned) > 0
	}, 2*time.Second, 20*time.Millisecond, "watchPlan should recover content-only degraded run")

	st := c.Status()
	require.Equal(t, StateActive, st.Runs[0].State)
	require.NotNil(t, st.Runs[0].Brain)
	require.NotEmpty(t, st.Runs[0].PreflightWarnings)
}

// TestWatchPlanDegradedStructuralThenFix proves that a structurally-broken
// (missing) plan leaves the run degraded, and writing a valid plan back lets
// the next watchPlan tick promote + spawn.
func TestWatchPlanDegradedStructuralThenFix(t *testing.T) {
	repo := t.TempDir()
	planPath := filepath.Join(repo, "plan.yaml")
	require.NoError(t, os.WriteFile(planPath, []byte("version: 1\ngoal: ship it\n"), 0o644))
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c := NewController(ControllerConfig{
		DataDir:  data,
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, env)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	rt := newFakeRuntime()
	c.SetRuntime(rt)

	reg, err := c.Register(context.Background(), RegisterRequest{PlanFile: planPath})
	require.NoError(t, err)

	require.NoError(t, os.Remove(planPath))

	c.mu.Lock()
	run := c.runs[reg.RunID]
	run.state = StateDegraded
	run.brain = nil
	run.plan = Plan{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	run.cancel = cancel
	go c.watchPlan(ctx, run, 15*time.Millisecond)
	c.mu.Unlock()

	// Still missing — must not spawn.
	time.Sleep(60 * time.Millisecond)
	require.Empty(t, rt.spawned, "missing plan must keep the run degraded")

	// Operator restores a clean plan.
	require.NoError(t, os.WriteFile(planPath, []byte("version: 1\ngoal: ship it\n"), 0o644))

	require.Eventually(t, func() bool {
		return len(rt.spawned) > 0
	}, 2*time.Second, 20*time.Millisecond, "watchPlan should recover after the plan file is restored")

	st := c.Status()
	require.Equal(t, StateActive, st.Runs[0].State)
	require.NotNil(t, st.Runs[0].Brain)
	require.Empty(t, st.Runs[0].PreflightWarnings, "clean preflight clears warnings")
}

// TestSpawnBrainEmptyPlanGuardReloads proves spawnBrain reloads from disk when
// r.plan.Goal is empty but a valid plan file exists — closing the guardian's
// blind-spawn hole.
func TestSpawnBrainEmptyPlanGuardReloads(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(rt)

	r := &run{
		runID:             "ap-guard-reload",
		repo:              repo,
		planFile:          plan,
		absPlanFile:       plan,
		plan:              Plan{}, // empty goal
		state:             StateDegraded,
		slotScope:         "plan",
		integrationBranch: "autopilot/plan",
		tried:             map[string]bool{},
	}
	c.mu.Lock()
	err := c.spawnBrain(context.Background(), r, "antigravity")
	c.mu.Unlock()

	require.NoError(t, err)
	require.Equal(t, "ship it", r.plan.Goal)
	require.Equal(t, StateActive, r.state)
	require.NotNil(t, r.brain)
	require.Len(t, rt.spawned, 1)
	require.Contains(t, rt.spawned[0].Prompt, "ship it")
}

// TestSpawnBrainEmptyPlanGuardMissingFile proves spawnBrain returns an error
// and stays degraded when even a lenient load fails (missing file) — no blind
// spawn with an empty goal.
func TestSpawnBrainEmptyPlanGuardMissingFile(t *testing.T) {
	repo := t.TempDir()
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{
		BaseDir:  repo,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(rt)

	r := &run{
		runID:             "ap-guard-missing",
		repo:              repo,
		planFile:          filepath.Join(repo, "missing.yaml"),
		absPlanFile:       filepath.Join(repo, "missing.yaml"),
		plan:              Plan{},
		state:             StateDegraded,
		slotScope:         "plan",
		integrationBranch: "autopilot/plan",
		tried:             map[string]bool{},
	}
	c.mu.Lock()
	err := c.spawnBrain(context.Background(), r, "antigravity")
	c.mu.Unlock()

	require.Error(t, err)
	require.Contains(t, err.Error(), "plan not loadable")
	require.Equal(t, StateDegraded, r.state)
	require.Nil(t, r.brain)
	require.Empty(t, rt.spawned)
}
