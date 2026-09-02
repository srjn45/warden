package autopilot

// WP1 characterization tests freeze pre-hierarchy behavior before WP4–WP6 and WP9
// change semantics. Seam inventory: docs/specs/2026-09-01-autopilot-plan-scoped-hierarchy-wp1-seams.md

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/stretchr/testify/require"
)

// hexBrainRuntime mints monotonic agent-<hex> ids across controller restarts,
// matching production's resolveID behavior (lifecycle.go:808-812) better than
// the package fakeRuntime's per-instance brain-1 reset.
type hexBrainRuntime struct {
	store    *fakeStore
	sources  fakeSources
	spawned  []BrainSpec
	killed   []string
	notified []string
	spawnErr error
	installs int
}

var hexBrainSeq atomic.Uint64

func newHexBrainRuntime() *hexBrainRuntime { return &hexBrainRuntime{store: newFakeStore()} }

func (r *hexBrainRuntime) nextAgentID() string {
	n := hexBrainSeq.Add(1)
	return fmt.Sprintf("agent-%08x", n)
}

func (r *hexBrainRuntime) SpawnBrain(_ context.Context, spec BrainSpec) (BrainHandle, error) {
	if r.spawnErr != nil {
		return BrainHandle{}, r.spawnErr
	}
	r.spawned = append(r.spawned, spec)
	return BrainHandle{AgentID: r.nextAgentID(), Backend: spec.Backend}, nil
}

func (r *hexBrainRuntime) TerminateBrain(_ context.Context, agentID string) error {
	r.killed = append(r.killed, agentID)
	return nil
}

func (r *hexBrainRuntime) NewLedger(runID string) *Ledger   { return NewLedger(r.store, runID) }
func (r *hexBrainRuntime) DigestSources() DigestSources     { return r.sources }
func (r *hexBrainRuntime) NotifyOwner(_ string, msg string) { r.notified = append(r.notified, msg) }
func (r *hexBrainRuntime) InstallDefaultAutoApprovePolicy() { r.installs++ }

// TestCharacterization_RotateBrainMintsNewAgentID records that guardian rotation
// terminates the current manager and spawns a fresh agent-<hex> id (run.go:147-158).
// WP5 replaces this terminate-then-spawn path with in-place HotSwap on the slot.
func TestCharacterization_RotateBrainMintsNewAgentID(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	fake := newGuardianFake()
	c := NewController(ControllerConfig{
		Plans:    []string{plan},
		BaseDir:  dir,
		Resolver: cyclicResolver("a", "free"),
	}, &fakeEnv{})
	c.SetRuntime(fake)
	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	runID := st.Runs[0].RunID
	r := c.runs[runID]
	require.Equal(t, "brain-1", r.brain.AgentID)

	require.NoError(t, c.rotateBrain(context.Background(), r, "a"))
	require.Equal(t, "brain-2", r.brain.AgentID, "rotation mints a new manager id")
	require.Equal(t, []string{"brain-1"}, fake.killed)
	require.Len(t, fake.spawned, 2)
	require.NotEqual(t, fake.spawned[0].RunID, "", "spawn carries run id")
}

// TestCharacterization_RestartSpawnsWithoutAdoptingStoredBrainID records that
// restoreStoredRuns discards BrainID (lifecycle.go:44-48) and SetRuntime always
// spawns a rival manager (controller.go:260-289). WP4 adds Ticket-based adopt.
func TestCharacterization_RestartSpawnsWithoutAdoptingStoredBrainID(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "plan.yaml", "durable")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c1 := NewController(ControllerConfig{
		DataDir:           data,
		BaseDir:           repo,
		IntegrationBranch: "autopilot/integration",
		Resolver:          &fakeResolver{backendID: "a", tier: "free"},
	}, env)
	rt1 := newHexBrainRuntime()
	c1.SetRuntime(rt1)
	r, err := c1.Register(context.Background(), RegisterRequest{PlanFile: plan})
	require.NoError(t, err)
	_, err = c1.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	require.Len(t, rt1.spawned, 1)
	storedBrainID := c1.Status().Runs[0].Brain.AgentID
	require.Contains(t, storedBrainID, "agent-")

	rec, err := c1.store.Get(r.RunID)
	require.NoError(t, err)
	require.Equal(t, storedBrainID, rec.BrainID)
	require.NoError(t, c1.Close())

	c2 := NewController(ControllerConfig{
		DataDir:           data,
		BaseDir:           repo,
		IntegrationBranch: "autopilot/integration",
		Resolver:          &fakeResolver{backendID: "a", tier: "free"},
	}, env)
	rt2 := newHexBrainRuntime()
	c2.SetRuntime(rt2)
	t.Cleanup(func() { require.NoError(t, c2.Close()) })

	require.Len(t, rt2.spawned, 1, "boot reconciliation spawns without adopting stored BrainID")
	newBrainID := c2.Status().Runs[0].Brain.AgentID
	require.Contains(t, newBrainID, "agent-")
	require.NotEqual(t, storedBrainID, newBrainID, "adopt path does not exist today")

	rec2, err := c2.store.Get(r.RunID)
	require.NoError(t, err)
	require.Equal(t, newBrainID, rec2.BrainID, "persisted BrainID tracks the new rival manager")
}

// TestCharacterization_CanBrainCompleteAfterSimulatedRestart records that
// CanBrainComplete compares against in-memory r.brain.AgentID (controller.go:772-783),
// so a pre-restart manager is rejected after boot reconciliation spawns a rival.
func TestCharacterization_CanBrainCompleteAfterSimulatedRestart(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "plan.yaml", "durable")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	c1 := NewController(ControllerConfig{DataDir: data, BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, env)
	rt1 := newHexBrainRuntime()
	c1.SetRuntime(rt1)
	r, err := c1.Register(context.Background(), RegisterRequest{PlanFile: plan})
	require.NoError(t, err)
	_, err = c1.StartRun(context.Background(), r.RunID)
	require.NoError(t, err)
	survivingBrainID := c1.Status().Runs[0].Brain.AgentID
	require.NoError(t, c1.Close())

	c2 := NewController(ControllerConfig{DataDir: data, BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, env)
	rt2 := newHexBrainRuntime()
	c2.SetRuntime(rt2)
	t.Cleanup(func() { require.NoError(t, c2.Close()) })
	newBrainID := c2.Status().Runs[0].Brain.AgentID
	require.NotEqual(t, survivingBrainID, newBrainID)

	require.False(t, c2.CanBrainComplete(r.RunID, survivingBrainID),
		"pre-restart manager is not authorized after boot spawns a rival")
	require.True(t, c2.CanBrainComplete(r.RunID, newBrainID),
		"only the in-memory brain id may complete the run")
}

// TestCharacterization_ConcurrentRunsShareGlobalIntegrationBranch records the
// present-tense defect (spec §2.6): two active plans in one repo both resolve to
// the controller's global integrationBranch. WP9 derives per-plan branches.
func TestCharacterization_ConcurrentRunsShareGlobalIntegrationBranch(t *testing.T) {
	dir := t.TempDir()
	p1 := writePlan(t, dir, "alpha.yaml", "alpha goal")
	p2 := writePlan(t, dir, "beta.yaml", "beta goal")
	runStore, err := NewRunStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = runStore.Close() })

	const globalBranch = "autopilot/integration"
	c := NewController(ControllerConfig{
		Plans:             []string{p1, p2},
		IntegrationBranch: globalBranch,
		BaseDir:           dir,
		RunStore:          runStore,
		Resolver:          &fakeResolver{backendID: "a", tier: "free"},
	}, &fakeEnv{})
	rt := newFakeRuntime()
	c.SetRuntime(rt)

	st, err := c.Enable(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, st.Runs, 2, "V2 allows two concurrent runs in one repo")

	branches := map[string]string{}
	for _, r := range st.Runs {
		lp, ok := c.LandParams(r.RunID)
		require.True(t, ok)
		require.Equal(t, globalBranch, lp.IntegrationBranch)
		branches[r.RunID] = lp.IntegrationBranch

		rec, err := runStore.Get(r.RunID)
		require.NoError(t, err)
		require.Equal(t, globalBranch, rec.IntegrationBranch,
			"recordLocked mirrors global config, not per-plan derivation")
	}
	require.Len(t, branches, 2)
	require.Equal(t, branches[st.Runs[0].RunID], branches[st.Runs[1].RunID],
		"both runs land into the same integration branch today")
}

// TestCharacterization_GuardianRotationWalksIdChurn documents the full heal-ladder
// rotation path (guardian.go nudge→restart→rotate) as a sequence of new manager ids.
func TestCharacterization_GuardianRotationWalksIdChurn(t *testing.T) {
	t0 := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	fake := newGuardianFake()
	res := &roundRobinResolver{
		backends: []string{"a", "b"},
		tiers:    []backendstore.ModelTier{"free", "subscription"},
	}
	c, runID := enabledGuardianController(t, fake, clock, res, testGuardian())
	ctx := context.Background()

	ids := []string{c.runs[runID].brain.AgentID}
	require.Equal(t, "brain-1", ids[0])

	// Wedge → nudge → restart (same backend) → rotate (next backend).
	clock.t = t0.Add(11 * time.Minute)
	c.guardianTick(ctx)
	clock.t = t0.Add(22 * time.Minute)
	c.guardianTick(ctx)
	ids = append(ids, c.runs[runID].brain.AgentID)
	clock.t = t0.Add(33 * time.Minute)
	c.guardianTick(ctx)
	ids = append(ids, c.runs[runID].brain.AgentID)

	require.Equal(t, []string{"brain-1", "brain-2", "brain-3"}, ids,
		"each heal escalation mints a new manager id; WP5 preserves slot id")
	require.Equal(t, []string{"brain-1", "brain-2"}, fake.killed)
}
