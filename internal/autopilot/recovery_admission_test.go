package autopilot

import (
	"context"
	"fmt"
	"github.com/srjn45/warden/internal/planstore"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/stretchr/testify/require"
)

// blockingRecoveryRuntime is deliberately synchronization-safe: it models a
// dependency which does not return until released, while allowing the test to
// assert controller responsiveness without reading a racing fake slice.
type blockingRecoveryRuntime struct {
	store   *fakeStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockingDefaultBranchEnv struct {
	*fakeEnv
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingDefaultBranchEnv) DefaultBranch(ctx context.Context, repo string) (string, error) {
	e.once.Do(func() { close(e.entered) })
	<-e.release
	return e.fakeEnv.DefaultBranch(ctx, repo)
}

func (r *blockingRecoveryRuntime) SpawnBrain(context.Context, BrainSpec) (BrainHandle, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return BrainHandle{AgentID: "manager"}, nil
}
func (r *blockingRecoveryRuntime) RotateBrain(context.Context, RotateBrainSpec) (BrainHandle, error) {
	return BrainHandle{}, nil
}
func (r *blockingRecoveryRuntime) TerminateBrain(context.Context, string) error { return nil }
func (r *blockingRecoveryRuntime) NewLedger(id string) *Ledger                  { return NewLedger(r.store, id) }
func (r *blockingRecoveryRuntime) DigestSources() DigestSources                 { return fakeSources{} }
func (r *blockingRecoveryRuntime) NotifyOwner(string, string)                   {}
func (r *blockingRecoveryRuntime) InstallDefaultAutoApprovePolicy()             {}

func TestRecoveryAdmissionDoesNotBlockStatusAndPauseFencesApply(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "plan.yaml", "ship it")
	rt := &blockingRecoveryRuntime{store: newFakeStore(), entered: make(chan struct{}), release: make(chan struct{})}
	c := NewController(ControllerConfig{BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, &fakeEnv{})
	c.SetRuntime(rt)
	r := &run{runID: RunID(repo, plan), repo: repo, planFile: plan, absPlanFile: plan, state: StateDegraded, slotScope: "blocked", tried: map[string]bool{}}
	c.mu.Lock()
	c.runs[r.runID] = r
	require.True(t, c.admitRecoveryLocked(r))
	c.mu.Unlock()
	c.launchRecovery(r.runID)
	select {
	case <-rt.entered:
	case <-time.After(time.Second):
		t.Fatal("recovery never reached runtime")
	}

	statusDone := make(chan struct{})
	go func() { _ = c.Status(); close(statusDone) }()
	select {
	case <-statusDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Status blocked behind stalled recovery")
	}

	_, err := c.PauseRun(context.Background(), r.runID)
	require.NoError(t, err)
	close(rt.release)
	require.Eventually(t, func() bool { return c.Status().Runs[0].State == StatePaused }, time.Second, 10*time.Millisecond)
	require.Nil(t, c.Status().Runs[0].Brain, "stale completion must not resurrect a paused run")
}

func TestRecoveryAdmissionDefaultBranchDoesNotBlockOtherRun(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "plan.yaml", "ship it")
	env := &blockingDefaultBranchEnv{fakeEnv: &fakeEnv{}, entered: make(chan struct{}), release: make(chan struct{})}
	rt := &blockingRecoveryRuntime{store: newFakeStore(), entered: make(chan struct{}), release: make(chan struct{})}
	c := NewController(ControllerConfig{BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, env)
	c.SetRuntime(rt)
	r := &run{runID: RunID(repo, plan), repo: repo, planFile: plan, absPlanFile: plan, state: StateDegraded, slotScope: "default", tried: map[string]bool{}}
	other := &run{runID: "ap-other", repo: repo, state: StateActive, slotScope: "other", tried: map[string]bool{}}
	c.mu.Lock()
	c.runs[r.runID], c.runs[other.runID] = r, other
	require.True(t, c.admitRecoveryLocked(r))
	c.mu.Unlock()
	c.launchRecovery(r.runID)
	select {
	case <-env.entered:
	case <-time.After(time.Second):
		t.Fatal("default branch lookup did not start")
	}
	statusDone := make(chan struct{})
	go func() { _, _ = c.LookupRun(other.runID); close(statusDone) }()
	select {
	case <-statusDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("other run status blocked behind DefaultBranch")
	}
	close(env.release)
	select {
	case <-rt.entered:
	case <-time.After(time.Second):
		t.Fatal("spawn did not follow DefaultBranch")
	}
	close(rt.release)
}

// gatedRecoveryRuntime gates actual controller entry dependencies, including
// context-ignoring ones. All observations cross channels or the controller lock.
type gatedRecoveryRuntime struct {
	*guardianFake
	gate    string
	entered chan struct{}
	release chan struct{}
	cleaned chan BrainHandle
	handle  BrainHandle
	ledger  CtxStore
	once    sync.Once
}

func (r *gatedRecoveryRuntime) NewLedger(id string) *Ledger {
	if r.ledger != nil {
		return NewLedger(r.ledger, id)
	}
	return r.guardianFake.NewLedger(id)
}
func (r *gatedRecoveryRuntime) block(point string) {
	if r.gate == point {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
}
func (r *gatedRecoveryRuntime) SpawnBrain(context.Context, BrainSpec) (BrainHandle, error) {
	r.block("spawn")
	return r.handle, nil
}
func (r *gatedRecoveryRuntime) RotateBrain(context.Context, RotateBrainSpec) (BrainHandle, error) {
	r.block("rotate")
	return r.handle, nil
}
func (r *gatedRecoveryRuntime) BrainSession(context.Context, string) SessionPresence {
	r.block("session")
	if r.gate == "activity" {
		return SessionPresent
	}
	return SessionMissing
}
func (r *gatedRecoveryRuntime) BrainActivity(context.Context, string) (time.Time, bool) {
	r.block("activity")
	return time.Time{}, false
}
func (r *gatedRecoveryRuntime) AuditRunEvent(context.Context, string, string, string, string) {}
func (r *gatedRecoveryRuntime) ReconcileSessions(context.Context, []BootReconcileRun) error {
	r.block("reconcile")
	return nil
}
func (r *gatedRecoveryRuntime) CleanupRecoveryBrain(_ context.Context, _ string, h BrainHandle) error {
	r.cleaned <- h
	r.block("cleanup")
	return nil
}

type gatedPlanSource struct {
	plan             *planstore.Plan
	entered, release chan struct{}
	once             sync.Once
}

func (s *gatedPlanSource) Get(context.Context, string) (*planstore.Plan, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.plan, nil
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("dependency did not reach gate")
	}
}
func assertRecoveryResponsive(t *testing.T, c *Controller, healthy string) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = c.Status(); _, _ = c.LookupRun(healthy); close(done) }()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("controller locked by external recovery work")
	}
}
func recoveryFixture(t *testing.T, gate string) (*Controller, *run, *gatedRecoveryRuntime) {
	t.Helper()
	repo := t.TempDir()
	path := writePlan(t, repo, "plan.yaml", "recover safely")
	rt := &gatedRecoveryRuntime{guardianFake: newGuardianFake(), gate: gate,
		entered: make(chan struct{}), release: make(chan struct{}), cleaned: make(chan BrainHandle, 1),
		handle: BrainHandle{AgentID: "plan-autopilot", Backend: "a"}}
	c := NewController(ControllerConfig{BaseDir: repo, Resolver: &fakeResolver{backendID: "a", tier: "free"}}, &fakeEnv{})
	setRuntimeAndWait(t, c, rt)
	r := &run{runID: RunID(repo, path), repo: repo, absPlanFile: path, planFile: path,
		integrationBranch: "autopilot/plan", state: StateDegraded, slotScope: "plan", tried: map[string]bool{}}
	c.mu.Lock()
	c.runs[r.runID] = r
	c.runs["healthy"] = &run{runID: "healthy", state: StatePaused}
	c.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-rt.release:
		default:
			close(rt.release)
		}
		waitRecoveryIdle(t, c)
		_ = c.Close()
	})
	return c, r, rt
}

func TestRecoveryRealGuardianDependenciesDoNotHoldController(t *testing.T) {
	for _, point := range []string{"session", "activity"} {
		t.Run(point, func(t *testing.T) {
			c, r, rt := recoveryFixture(t, point)
			c.mu.Lock()
			r.state = StateActive
			r.brain = &BrainHandle{AgentID: "old"}
			c.mu.Unlock()
			c.guardianTick(context.Background())
			awaitSignal(t, rt.entered)
			assertRecoveryResponsive(t, c, "healthy")
			// A second tick must neither queue a waiter nor duplicate the observation.
			c.guardianTick(context.Background())
			require.Equal(t, 1, c.Status().Recovery.InFlight)
			_, err := c.PauseRun(context.Background(), r.runID)
			require.NoError(t, err)
			close(rt.release)
			waitRecoveryIdle(t, c)
			require.Equal(t, StatePaused, c.Status().Runs[0].State)
		})
	}
}

func TestRecoveryLateLaunchCleanupAndAdoption(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprint("new=", fresh), func(t *testing.T) {
			c, r, rt := recoveryFixture(t, "spawn")
			rt.handle.NewLaunch, rt.handle.LaunchID = fresh, "owned-launch"
			// Use the real watcher entry rather than calling the admission helper.
			c.watchPlanTick(context.Background(), r.runID)
			awaitSignal(t, rt.entered)
			_, err := c.PauseRun(context.Background(), r.runID)
			require.NoError(t, err)
			_, err = c.ResumeRun(context.Background(), r.runID)
			require.ErrorIs(t, err, ErrRunConflict)
			_, err = c.RestartRun(context.Background(), r.runID, RestartRequest{Force: true})
			require.ErrorIs(t, err, ErrRunConflict)
			require.Equal(t, 1, c.Status().Recovery.InFlight)
			close(rt.release)
			waitRecoveryIdle(t, c)
			if fresh {
				require.Equal(t, rt.handle, <-rt.cleaned)
			} else {
				require.Empty(t, rt.cleaned, "an adopted manager must survive canceled recovery")
			}
			st, err := c.LookupRun(r.runID)
			require.NoError(t, err)
			require.Equal(t, StatePaused, st.State)
		})
	}
}

func TestRecoveryRemovedAfterAdmissionReleasesReservation(t *testing.T) {
	c, r, _ := recoveryFixture(t, "")
	c.mu.Lock()
	require.True(t, c.admitRecoveryLocked(r))
	delete(c.runs, r.runID)
	// Replacing the map entry must not permit a second recovery while the old
	// reservation is waiting to start, even when its per-run counter is reset.
	replacement := &run{runID: r.runID, state: StateDegraded}
	c.runs[r.runID] = replacement
	require.False(t, c.admitRecoveryLocked(replacement))
	delete(c.runs, r.runID)
	c.mu.Unlock()
	c.launchRecovery(r.runID)
	require.Eventually(t, func() bool { return c.Status().Recovery.InFlight == 0 }, time.Second, time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Empty(t, c.recoveryOwners)
	require.Empty(t, c.recoverySem)
}

func TestRecoveryWatcherAndGuardianReleaseDeadlineTimers(t *testing.T) {
	for _, guardian := range []bool{false, true} {
		t.Run(fmt.Sprint("guardian=", guardian), func(t *testing.T) {
			c, r, _ := recoveryFixture(t, "")
			if guardian {
				c.guardianTick(context.Background())
			} else {
				c.watchPlanTick(context.Background(), r.runID)
			}
			c.mu.Lock()
			ctx := r.recoveryCtx
			c.mu.Unlock()
			require.NotNil(t, ctx)
			waitRecoveryIdle(t, c)
			require.ErrorIs(t, ctx.Err(), context.Canceled, "finished attempts must not fire deadline counters later")
		})
	}
}

func TestRecoveryRemovedRunReservationFencesLifecycleReplacement(t *testing.T) {
	c, r, _ := recoveryFixture(t, "")
	live, err := autopilotstore.New(t.TempDir())
	require.NoError(t, err)
	defer live.Close()
	c.SetLiveStore(live)
	c.mu.Lock()
	delete(c.runs, r.runID)
	r.planID = "plan-replacement"
	r.runID = PlanBoundRunID(r.repo, r.planID)
	c.runs[r.runID] = r
	require.True(t, c.admitRecoveryLocked(r))
	reservedCtx := r.recoveryCtx
	c.mu.Unlock()
	require.NoError(t, c.TeardownLive(context.Background(), r.runID))
	require.ErrorIs(t, reservedCtx.Err(), context.Canceled)
	_, err = c.StartFromPlan(context.Background(), PlanStartRequest{PlanID: r.planID, Repo: r.repo})
	require.ErrorIs(t, err, ErrRunConflict)
	replacement := &run{runID: r.runID, state: StateRegistered}
	c.mu.Lock()
	c.runs[r.runID] = replacement
	c.mu.Unlock()
	_, err = c.StartRun(context.Background(), r.runID)
	require.ErrorIs(t, err, ErrRunConflict)
	c.mu.Lock()
	replacement.state = StatePaused
	c.mu.Unlock()
	_, err = c.ResumeRun(context.Background(), r.runID)
	require.ErrorIs(t, err, ErrRunConflict)
	_, err = c.RestartRun(context.Background(), r.runID, RestartRequest{Force: true})
	require.ErrorIs(t, err, ErrRunConflict)
	c.launchRecovery(r.runID)
	require.Eventually(t, func() bool { return c.Status().Recovery.InFlight == 0 }, time.Second, time.Millisecond)
}

func TestRecoveryGuardianObservationCannotApplyToReplacementRun(t *testing.T) {
	c, r, rt := recoveryFixture(t, "session")
	c.mu.Lock()
	r.state, r.brain = StateActive, &BrainHandle{AgentID: "old"}
	c.mu.Unlock()
	c.guardianTick(context.Background())
	awaitSignal(t, rt.entered)
	c.mu.Lock()
	replacement := &run{runID: r.runID, state: StateActive, brain: &BrainHandle{AgentID: "replacement"},
		recoveryGeneration: r.recoveryGeneration, persistRevision: r.persistRevision}
	c.runs[r.runID] = replacement
	c.mu.Unlock()
	close(rt.release)
	require.Eventually(t, func() bool { return c.Status().Recovery.InFlight == 0 }, time.Second, time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Equal(t, "replacement", replacement.brain.AgentID)
	require.Equal(t, StateActive, replacement.state)
	require.Zero(t, replacement.persistRevision)
}

func TestRecoveryLateRotationAfterStopCleansReplacement(t *testing.T) {
	c, r, rt := recoveryFixture(t, "rotate")
	rt.handle.NewLaunch, rt.handle.LaunchID = true, "replacement-token"
	c.mu.Lock()
	r.state, r.brain = StateActive, &BrainHandle{AgentID: rt.handle.AgentID}
	require.True(t, c.admitRotationLocked(r, "a", RotateReasonHeal))
	c.mu.Unlock()
	c.launchRecovery(r.runID)
	awaitSignal(t, rt.entered)
	_, err := c.StopRun(context.Background(), r.runID)
	require.NoError(t, err)
	close(rt.release)
	waitRecoveryIdle(t, c)
	require.Equal(t, rt.handle, <-rt.cleaned, "Stop owns late replacement cleanup")
	st, err := c.LookupRun(r.runID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, st.State)
	require.Nil(t, st.Brain)
}

func TestRecoveryLateRotationAfterPausePreservesReplacementIdentity(t *testing.T) {
	c, r, rt := recoveryFixture(t, "rotate")
	rt.handle.NewLaunch, rt.handle.LaunchID, rt.handle.Backend = true, "replacement-token", "b"
	c.mu.Lock()
	r.state, r.brain = StateActive, &BrainHandle{AgentID: rt.handle.AgentID, Backend: "a"}
	require.True(t, c.admitRotationLocked(r, "b", RotateReasonHeal))
	c.mu.Unlock()
	c.launchRecovery(r.runID)
	awaitSignal(t, rt.entered)
	_, err := c.PauseRun(context.Background(), r.runID)
	require.NoError(t, err)
	close(rt.release)
	waitRecoveryIdle(t, c)
	require.Empty(t, rt.cleaned)
	st, err := c.LookupRun(r.runID)
	require.NoError(t, err)
	require.Equal(t, StatePaused, st.State)
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotNil(t, r.brain)
	require.Equal(t, "b", r.brain.Backend)
	require.Equal(t, rt.handle.LaunchID, r.brain.LaunchID)
}

func TestRecoveryDeadlineRetainsGlobalAndPerRunCapacity(t *testing.T) {
	c, r, rt := recoveryFixture(t, "spawn")
	c.mu.Lock()
	c.recoveryTimeout = 100 * time.Millisecond
	for i := 0; i < 8; i++ {
		cp := *r
		cp.absPlanFile = writePlan(t, r.repo, fmt.Sprintf("capacity-%d.yaml", i), "capacity")
		cp.planFile = cp.absPlanFile
		cp.runID = RunID(r.repo, cp.absPlanFile)
		cp.slotScope = fmt.Sprintf("capacity-%d", i)
		c.runs[cp.runID] = &cp
	}
	// Reserve before goroutine creation; context-ignoring spawns retain capacity.
	admitted := []string{}
	for id, candidate := range c.runs {
		if candidate.state == StateDegraded && c.admitRecoveryLocked(candidate) {
			admitted = append(admitted, id)
		}
	}
	c.mu.Unlock()
	require.Len(t, admitted, cap(c.recoverySem))
	for _, id := range admitted {
		c.launchRecovery(id)
	}
	awaitSignal(t, rt.entered)
	require.Eventually(t, func() bool { return c.Status().Recovery.Timeouts == uint64(cap(c.recoverySem)) }, time.Second, time.Millisecond)
	assertRecoveryResponsive(t, c, "healthy")
	c.mu.Lock()
	require.False(t, c.admitRecoveryLocked(c.runs[admitted[0]]), "expired work still owns its run")
	c.mu.Unlock()
	require.Equal(t, cap(c.recoverySem), c.Status().Recovery.InFlight)
	require.Positive(t, c.Status().Recovery.CapacityRefusals)
	close(rt.release)
	waitRecoveryIdle(t, c)
	for _, id := range admitted {
		st, err := c.LookupRun(id)
		require.NoError(t, err)
		require.Nil(t, st.Brain, "late result after deadline must not apply")
	}
}

func TestRecoveryBootReconciliationDoesNotHoldController(t *testing.T) {
	repo := t.TempDir()
	path := writePlan(t, repo, "plan.yaml", "boot")
	store, err := NewRunStore(t.TempDir())
	require.NoError(t, err)
	id := RunID(repo, path)
	require.NoError(t, store.Create(RunRecord{RunID: id, Repo: repo, PlanFile: path, State: StateActive, SlotScope: "plan", IntegrationBranch: "autopilot/plan"}))
	c := NewController(ControllerConfig{RunStore: store, BaseDir: repo}, &fakeEnv{})
	rt := &gatedRecoveryRuntime{guardianFake: newGuardianFake(), gate: "reconcile", entered: make(chan struct{}), release: make(chan struct{}), handle: BrainHandle{AgentID: "plan-autopilot"}}
	c.SetRuntime(rt)
	awaitSignal(t, rt.entered)
	assertRecoveryResponsive(t, c, id)
	_, err = c.PauseRun(context.Background(), id)
	require.NoError(t, err)
	close(rt.release)
	awaitSignal(t, c.bootDone)
	waitRecoveryIdle(t, c)
	st, err := c.LookupRun(id)
	require.NoError(t, err)
	require.Equal(t, StatePaused, st.State)
	require.NoError(t, c.Close())
}

func TestRecoveryCanonicalReadStallAndOwnership(t *testing.T) {
	c, r, rt := recoveryFixture(t, "")
	c.mu.Lock()
	delete(c.runs, r.runID)
	r.planID = "plan-owned"
	r.projectID = r.repo
	r.runID = PlanBoundRunID(r.repo, r.planID)
	r.absPlanFile = ""
	c.runs[r.runID] = r
	p := dbPlan(r.planID)
	p.ProjectID = r.repo
	p.AutopilotRunID = r.runID
	src := &gatedPlanSource{plan: p, entered: make(chan struct{}), release: make(chan struct{})}
	c.planSource = src
	c.mu.Unlock()
	c.guardianTick(context.Background())
	awaitSignal(t, src.entered)
	assertRecoveryResponsive(t, c, "healthy")
	close(src.release)
	waitRecoveryIdle(t, c)
	_, err := c.LookupRun(r.runID)
	require.NoError(t, err)
	require.NotNil(t, c.Status().Runs[0].Brain)
	require.Empty(t, rt.cleaned)
	// Empty/invalid exports are recoverable only with the complete ownership chain.
	s := recoverySnapshot{runID: r.runID, planID: r.planID, projectID: r.repo, repo: r.repo}
	require.NoError(t, validateRecoveryIdentity(s, p))
	for _, mutate := range []func(*planstore.Plan){
		func(p *planstore.Plan) { p.ID = "other" },
		func(p *planstore.Plan) { p.ProjectID = t.TempDir() },
		func(p *planstore.Plan) { p.AutopilotRunID = "other" },
		func(p *planstore.Plan) { p.AutopilotRunID = "" },
		func(p *planstore.Plan) {
			p.ActiveExecution = &planstore.PlanExecution{PlanID: p.ID, ExecutorID: "other", ExecutionMode: planstore.PlanModeAutopilot}
		},
	} {
		cp := *p
		mutate(&cp)
		require.Error(t, validateRecoveryIdentity(s, &cp))
	}
}

func TestRecoveryPersistenceRejectsLateOldSnapshot(t *testing.T) {
	store, err := NewRunStore(t.TempDir())
	require.NoError(t, err)
	defer store.Close()
	old := RunRecord{RunID: "ordered", State: StateActive, PersistRevision: 1}
	require.NoError(t, persistRecoveryRecord(nil, store, old, "autopilot/plan"))
	newer := old
	newer.PersistRevision = 3
	newer.State = StateStopped
	require.NoError(t, persistRecoveryRecord(nil, store, newer, "autopilot/plan"))
	late := old
	late.PersistRevision = 2
	require.NoError(t, persistRecoveryRecord(nil, store, late, "autopilot/plan"))
	got, err := store.Get(old.RunID)
	require.NoError(t, err)
	require.Equal(t, StateStopped, got.State)
	require.EqualValues(t, 3, got.PersistRevision)
}

type gatedLedgerStore struct {
	CtxStore
	write            bool
	entered, release chan struct{}
	once             sync.Once
}

func (s *gatedLedgerStore) Get(key string) (string, error) {
	if !s.write && strings.HasSuffix(key, ".tasks") {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.CtxStore.Get(key)
}
func (s *gatedLedgerStore) Set(key, value, by string) error {
	if s.write && strings.HasSuffix(key, ".integration_branch") {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.CtxStore.Set(key, value, by)
}
func TestRecoveryWatcherDigestAndPersistenceStalls(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(fmt.Sprint("write=", write), func(t *testing.T) {
			c, r, rt := recoveryFixture(t, "")
			ledger := &gatedLedgerStore{CtxStore: newFakeStore(), write: write, entered: make(chan struct{}), release: make(chan struct{})}
			rt.ledger = ledger
			c.watchPlanTick(context.Background(), r.runID)
			awaitSignal(t, ledger.entered)
			assertRecoveryResponsive(t, c, "healthy")
			require.Equal(t, 1, c.Status().Recovery.InFlight)
			close(ledger.release)
			waitRecoveryIdle(t, c)
			require.NotNil(t, c.Status().Runs[0].Brain)
		})
	}
}

func TestRecoveryLatePersistenceCannotOverwritePause(t *testing.T) {
	c, r, rt := recoveryFixture(t, "")
	store, err := NewRunStore(t.TempDir())
	require.NoError(t, err)
	c.mu.Lock()
	c.store = store
	c.mu.Unlock()
	ledger := &gatedLedgerStore{CtxStore: newFakeStore(), write: true, entered: make(chan struct{}), release: make(chan struct{})}
	rt.ledger = ledger
	c.watchPlanTick(context.Background(), r.runID)
	awaitSignal(t, ledger.entered)
	paused := make(chan error, 1)
	go func() { _, err := c.PauseRun(context.Background(), r.runID); paused <- err }()
	require.Eventually(t, func() bool { return c.Status().Runs[0].State == StatePaused }, time.Second, time.Millisecond)
	assertRecoveryResponsive(t, c, "healthy")
	close(ledger.release)
	require.NoError(t, <-paused)
	waitRecoveryIdle(t, c)
	rec, err := store.Get(r.runID)
	require.NoError(t, err)
	require.Equal(t, StatePaused, rec.State)
}

type nilLedgerRuntime struct{ Runtime }

func (nilLedgerRuntime) NewLedger(string) *Ledger { return nil }
func TestRecoveryNilLedgerAndCorruptJournal(t *testing.T) {
	t.Run("nil ledger", func(t *testing.T) {
		c, r, rt := recoveryFixture(t, "")
		c.SetRuntime(nilLedgerRuntime{Runtime: rt})
		c.watchPlanTick(context.Background(), r.runID)
		waitRecoveryIdle(t, c)
		require.NotNil(t, c.Status().Runs[0].Brain)
	})
	t.Run("corrupt journal", func(t *testing.T) {
		c, r, rt := recoveryFixture(t, "")
		require.NoError(t, rt.store.Set(NewLedger(rt.store, r.runID).JournalKey(), "invalid JSON", ledgerWriter))
		c.watchPlanTick(context.Background(), r.runID)
		waitRecoveryIdle(t, c)
		// The supported repair API quarantines and rebuilds the journal, so
		// recovery proceeds (see recovery_observability_test.go for failure).
		require.NotNil(t, c.Status().Runs[0].Brain)
	})
}

func TestRecoveryCanonicalDefinitionRepairIsAutomatic(t *testing.T) {
	c, r, _ := recoveryFixture(t, "")
	c.mu.Lock()
	delete(c.runs, r.runID)
	r.planID = "plan-repair"
	r.projectID = r.repo
	r.runID = PlanBoundRunID(r.repo, r.planID)
	c.runs[r.runID] = r
	broken := dbPlan(r.planID)
	broken.ProjectID = r.repo
	broken.AutopilotRunID = "stale-owner"
	broken.Revision = 1
	broken.ContentHash = "broken"
	source := &fakePlanSource{plans: map[string]*planstore.Plan{r.planID: broken}}
	c.planSource = source
	c.mu.Unlock()
	guardianTickAndWait(t, c, context.Background())
	require.Contains(t, c.Status().Runs[0].NeedsAttention, "ownership")
	repaired := *broken
	repaired.AutopilotRunID = r.runID
	repaired.Revision = 2
	repaired.ContentHash = "repaired"
	source.plans[r.planID] = &repaired
	guardianTickAndWait(t, c, context.Background())
	require.Empty(t, c.Status().Runs[0].NeedsAttention)
	require.NotNil(t, c.Status().Runs[0].Brain)
}
