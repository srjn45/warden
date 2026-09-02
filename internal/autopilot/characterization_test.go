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
	if spec.SlotScope != "" {
		return BrainHandle{AgentID: ManagerSlotID(spec.SlotScope), Backend: spec.Backend}, nil
	}
	return BrainHandle{AgentID: r.nextAgentID(), Backend: spec.Backend}, nil
}

func (r *hexBrainRuntime) RotateBrain(_ context.Context, spec RotateBrainSpec) (BrainHandle, error) {
	backend := spec.Backend
	if backend == "" {
		backend = "claude"
	}
	return BrainHandle{AgentID: spec.AgentID, Backend: backend}, nil
}

func (r *hexBrainRuntime) TerminateBrain(_ context.Context, agentID string) error {
	r.killed = append(r.killed, agentID)
	return nil
}

func (r *hexBrainRuntime) NewLedger(runID string) *Ledger   { return NewLedger(r.store, runID) }
func (r *hexBrainRuntime) DigestSources() DigestSources     { return r.sources }
func (r *hexBrainRuntime) NotifyOwner(_ string, msg string) { r.notified = append(r.notified, msg) }
func (r *hexBrainRuntime) InstallDefaultAutoApprovePolicy() { r.installs++ }

// TestCharacterization_RotateBrainHotSwapsInPlace records WP5 guardian rotation:
// in-place HotSwap on the manager slot (same id, no terminate+spawn).
func TestCharacterization_RotateBrainHotSwapsInPlace(t *testing.T) {
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

	require.NoError(t, c.rotateBrain(context.Background(), r, "a", RotateReasonHeal))
	require.Equal(t, "brain-1", r.brain.AgentID, "rotation keeps the manager slot id")
	require.Empty(t, fake.killed, "guardian rotation must not terminate the manager")
	require.Len(t, fake.rotated, 1)
	require.Equal(t, "brain-1", fake.rotated[0].AgentID)
	require.Equal(t, "a", fake.rotated[0].Backend)
}

// TestSlotSpawnAdoptsStoredBrainOnRestart verifies daemon restart adopts the
// manager slot id instead of minting a rival agent-<hex> (WP4).
func TestSlotSpawnAdoptsStoredBrainOnRestart(t *testing.T) {
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
	require.Equal(t, ManagerSlotID("plan"), storedBrainID)

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

	require.Len(t, rt2.spawned, 1, "boot reconciliation calls SpawnBrain to adopt the slot")
	newBrainID := c2.Status().Runs[0].Brain.AgentID
	require.Equal(t, storedBrainID, newBrainID, "restart adopts the stable slot id")

	rec2, err := c2.store.Get(r.RunID)
	require.NoError(t, err)
	require.Equal(t, storedBrainID, rec2.BrainID)
}

// TestSlotSpawnCanBrainCompleteAfterRestart verifies CanBrainComplete accepts the
// stable slot manager after daemon restart (WP4).
func TestSlotSpawnCanBrainCompleteAfterRestart(t *testing.T) {
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
	require.Equal(t, survivingBrainID, newBrainID)

	require.True(t, c2.CanBrainComplete(r.RunID, survivingBrainID),
		"slot manager remains authorized after restart")
}

// TestCharacterization_ConcurrentRunsShareGlobalIntegrationBranch is superseded
// by WP9 (TestTwoPlansInOneRepoResolveDistinctBranches). Kept as a thin alias
// so WP1 seam inventory still names a test; it now asserts the new contract.
func TestCharacterization_ConcurrentRunsShareGlobalIntegrationBranch(t *testing.T) {
	TestTwoPlansInOneRepoResolveDistinctBranches(t)
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

	require.Equal(t, []string{"brain-1", "brain-1", "brain-1"}, ids,
		"WP5 heal escalations HotSwap in place on the manager slot id")
	require.Empty(t, fake.killed, "guardian heal ladder must not terminate the manager")
	require.Len(t, fake.rotated, 2, "restart and rotate each call RotateBrain")
}
