package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// bulkRecoveryFixture wires a full coordinated-bulk-recovery stack: a
// fakeStore + backendstore + recoveryLife (same fixtures backend_recovery_test.go
// uses for the coordinator alone), plus a durable usage SnapshotStore and
// capacity FenceStore so Server.calculateBucketImpact / startBulkRecovery can
// run end to end exactly as the daemon wires them in internal/cli/daemon.go.
func bulkRecoveryFixture(t *testing.T, limits map[string][]backendusage.Limit) (*Server, *BackendRecoveryCoordinator, *fakeStore, *recoveryLife, *backendusage.SnapshotStore) {
	t.Helper()
	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	seeded, err := bs.ListModels("")
	require.NoError(t, err)
	for _, model := range seeded {
		require.NoError(t, bs.SetModelEnabled(model.BackendID, model.ModelID, false))
	}
	var adapters []backendusage.Adapter
	for _, id := range []string{"codex", "claude", "antigravity"} {
		require.NoError(t, bs.Upsert(backendstore.Backend{ID: id, Installed: true, Enabled: true, Tier: backendstore.TierSubscription}))
		model := id + "-model"
		require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: id, ModelID: model, Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))
		adapters = append(adapters, recoveryAdapter{id: id, result: backendusage.Result{Status: backendusage.StatusOK, Usage: limits[id]}})
	}

	snapStore, err := backendusage.NewSnapshotStore(t.TempDir())
	require.NoError(t, err)
	usageService := backendusage.NewService(bs, adapters...)
	usageService.SetSnapshotStore(snapStore)

	st := newFakeStore()
	life := &recoveryLife{failures: make(map[string]error), st: st}
	coord := NewBackendRecoveryCoordinator(st, bs, usageService, life)
	coord.stabilizationWindow = 5 * time.Millisecond

	fences, err := capacity.NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)

	s := NewServer(st, nil, nil, 0, false, nil, nil, nil)
	s.SetUsageService(usageService)
	s.SetUsageReconciliation(true, time.Minute, 15*time.Minute)
	s.SetImpactFences(fences)
	s.SetBackendRecovery(coord)

	return s, coord, st, life, snapStore
}

// boundAgent inserts a live agent bound (daemon-owned QuotaBinding) to the
// given provider/account-fingerprint/route/bucket so bucket-impact
// reconciliation can select it as affected.
func boundAgent(t *testing.T, st *fakeStore, id, aiCli, model, fingerprint, bucket string) {
	t.Helper()
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: id, AiCli: aiCli, Model: model, Role: "general", Status: store.StatusWorking,
		QuotaBinding: &capacity.QuotaBinding{
			Domain: capacity.CapacityDomain{
				Provider: aiCli, AiCli: aiCli, AccountFingerprint: fingerprint, Route: model, BucketKeys: []string{bucket},
			},
			MandatoryBuckets: []string{bucket},
		},
	}))
}

// recordExhaustedBucket records one fresh, authoritative exhausted-bucket usage
// snapshot — the same shape the usage reconciliation poller persists.
func recordExhaustedBucket(t *testing.T, snapStore *backendusage.SnapshotStore, provider, fingerprint, bucket string, resetsAt *time.Time) {
	t.Helper()
	now := time.Now().UTC()
	_, err := snapStore.Record(backendusage.UsageSnapshot{
		Domain:        backendusage.CapacityDomain{Provider: provider, ProfileFingerprint: fingerprint},
		ObservedAt:    now,
		RecordedAt:    now,
		SourceStatus:  backendusage.StatusOK,
		Authoritative: true,
		Freshness:     backendusage.FreshnessFresh,
		Buckets:       []backendusage.CapacityBucket{{Key: bucket, State: backendusage.BucketExhausted, ResetsAt: resetsAt}},
	})
	require.NoError(t, err)
}

// TestBulkRecoverySeveralAffectedAgentsAdvanceIndependently covers the "several
// affected agents" scenario from plan-69eb481d Phase 6: a single shared bucket
// exhaustion must deterministically start recovery, through
// BackendRecoveryCoordinator.OnHardLimit, for every eligible bound agent — never
// through a second direct hot-swap path.
func TestBulkRecoverySeveralAffectedAgentsAdvanceIndependently(t *testing.T) {
	fp := "sha256:shared-account"
	s, _, st, life, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	boundAgent(t, st, "agent-2", "codex", "codex-model", fp, "weekly")
	boundAgent(t, st, "agent-3", "codex", "codex-model", fp, "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"agent-1", "agent-2", "agent-3"}, impactIDs(result))

	s.startBulkRecovery(context.Background(), result.AffectedAgents)

	for _, id := range []string{"agent-1", "agent-2", "agent-3"} {
		require.Eventually(t, func() bool {
			sess := st.snapSession(id)
			return sess != nil && sess.BackendRecovery != nil &&
				sess.BackendRecovery.Phase == recoveryStabilizing &&
				sess.BackendRecovery.Current != nil && sess.BackendRecovery.Current.BackendID == "claude"
		}, time.Second, 5*time.Millisecond, "agent %s must independently reach stabilizing on claude", id)
		sess := st.snapSession(id)
		require.Equal(t, uint64(1), sess.BackendRecovery.Generation, "exactly one generation per affected agent")
	}
	life.mu.Lock()
	defer life.mu.Unlock()
	require.Len(t, life.swaps, 3, "each affected agent must get its own swap, no direct hot-swap path reused across agents")
}

// TestBulkRecoveryLimitedAlternativeCapacityAllLandOnSoleCandidate covers the
// "limited alternative capacity" scenario: only one candidate has usable
// headroom (claude at 90% used); every affected agent must still land on it
// rather than getting stuck, and the sole eligible candidate is never skipped
// just because more than one agent needs it.
func TestBulkRecoveryLimitedAlternativeCapacityAllLandOnSoleCandidate(t *testing.T) {
	fp := "sha256:limited-account"
	s, _, st, life, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(90)}}, // headroom=10, eligible
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100)}},   // headroom=0, excluded
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	boundAgent(t, st, "agent-2", "codex", "codex-model", fp, "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Len(t, result.AffectedAgents, 2)

	s.startBulkRecovery(context.Background(), result.AffectedAgents)

	for _, id := range []string{"agent-1", "agent-2"} {
		require.Eventually(t, func() bool {
			sess := st.snapSession(id)
			return sess != nil && sess.BackendRecovery != nil &&
				sess.BackendRecovery.Phase == recoveryStabilizing &&
				sess.BackendRecovery.Current != nil && sess.BackendRecovery.Current.BackendID == "claude"
		}, time.Second, 5*time.Millisecond, "agent %s must land on the sole eligible candidate", id)
	}
	life.mu.Lock()
	defer life.mu.Unlock()
	for _, sw := range life.swaps {
		require.Equal(t, "claude", sw.BackendID, "antigravity has no headroom and must never be selected")
	}
}

// TestBulkRecoveryAllCandidatesExhausted covers the "all candidates exhausted"
// scenario: when every policy-eligible candidate is also exhausted, affected
// agents must reach recoveryWaiting (never a direct swap, never stuck
// unscheduled) exactly like a pane-triggered hard limit would.
func TestBulkRecoveryAllCandidatesExhausted(t *testing.T) {
	fp := "sha256:all-exhausted"
	reset := time.Now().Add(time.Hour).UTC()
	s, _, st, _, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100), ResetsAt: &reset}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	boundAgent(t, st, "agent-2", "codex", "codex-model", fp, "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", &reset)

	result, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Len(t, result.AffectedAgents, 2)

	s.startBulkRecovery(context.Background(), result.AffectedAgents)

	for _, id := range []string{"agent-1", "agent-2"} {
		require.Eventually(t, func() bool {
			sess := st.snapSession(id)
			return sess != nil && sess.BackendRecovery != nil &&
				sess.BackendRecovery.Phase == recoveryWaiting && sess.BackendRecovery.NextRetryAt != nil
		}, time.Second, 5*time.Millisecond, "agent %s must reach recoveryWaiting when every candidate is exhausted", id)
	}
}

// TestBulkRecoveryCandidateLaunchFailure covers the "candidate launch failure"
// scenario: the top-ranked candidate's HotSwap fails, and the coordinator must
// fall through to the next eligible candidate for the bulk-triggered agent,
// recording the failed attempt — identical to the pane-triggered path.
func TestBulkRecoveryCandidateLaunchFailure(t *testing.T) {
	fp := "sha256:launch-failure"
	s, _, st, life, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}}, // top ranked, will fail to launch
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(40)}},
	})
	life.failures[candidateKey("claude", "claude-model")] = errors.New("launch failed")
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Len(t, result.AffectedAgents, 1)

	s.startBulkRecovery(context.Background(), result.AffectedAgents)

	require.Eventually(t, func() bool {
		sess := st.snapSession("agent-1")
		return sess != nil && sess.BackendRecovery != nil &&
			sess.BackendRecovery.Phase == recoveryStabilizing &&
			sess.BackendRecovery.Current != nil && sess.BackendRecovery.Current.BackendID == "antigravity"
	}, time.Second, 5*time.Millisecond)

	sess := st.snapSession("agent-1")
	var sawLaunchFailure bool
	for _, a := range sess.BackendRecovery.Attempts {
		if a.Outcome == "launch_failed" && a.Candidate.BackendID == "claude" {
			sawLaunchFailure = true
		}
	}
	require.True(t, sawLaunchFailure, "the failed claude attempt must be recorded")
}

// TestBulkRecoveryImmediateCandidateHardLimit covers the "immediate candidate
// hard limit" scenario: recovery started by the bulk/usage-API path reaches
// stabilizing on a candidate that then itself reports an immediate confirmed
// hard limit (e.g. a later pane/menu observation for that same agent). The
// coordinator must advance within the SAME recovery generation — no duplicate
// generation, no second independent swap path.
func TestBulkRecoveryImmediateCandidateHardLimit(t *testing.T) {
	fp := "sha256:immediate-hard-limit"
	s, coord, st, _, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(40)}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Len(t, result.AffectedAgents, 1)
	s.startBulkRecovery(context.Background(), result.AffectedAgents)

	require.Eventually(t, func() bool {
		sess := st.snapSession("agent-1")
		return sess != nil && sess.BackendRecovery != nil &&
			sess.BackendRecovery.Phase == recoveryStabilizing &&
			sess.BackendRecovery.Current != nil && sess.BackendRecovery.Current.BackendID == "claude"
	}, time.Second, 5*time.Millisecond)
	gen := st.snapSession("agent-1").BackendRecovery.Generation

	// A later confirmed signal (e.g. a Claude pane/menu hard-limit) fires the
	// same coordinator entry point again, immediately, while stabilizing.
	current := st.snapSession("agent-1")
	require.True(t, coord.OnHardLimit(current, time.Now().Add(time.Hour)),
		"OnHardLimit during stabilizing must still return true (owned)")

	require.Eventually(t, func() bool {
		sess := st.snapSession("agent-1")
		return sess != nil && sess.BackendRecovery != nil &&
			sess.BackendRecovery.Phase == recoveryStabilizing &&
			sess.BackendRecovery.Current != nil && sess.BackendRecovery.Current.BackendID == "antigravity"
	}, time.Second, 5*time.Millisecond)

	sess := st.snapSession("agent-1")
	require.Equal(t, gen, sess.BackendRecovery.Generation, "immediate hard limit must not start a new generation")
	var sawImmediate bool
	for _, a := range sess.BackendRecovery.Attempts {
		if a.Outcome == "immediate_hard_limit" && a.Candidate.BackendID == "claude" {
			sawImmediate = true
		}
	}
	require.True(t, sawImmediate, "immediate_hard_limit must be recorded for the just-swapped candidate")
}

// TestBulkRecoveryCancelledByManualSwitch covers "cancellation by manual
// switch/stop": an operator action (Supersede) during a bulk-triggered recovery
// must clear state and cancel timers immediately, and a later reconciliation
// pass for the same still-exhausted bucket must not resurrect it.
func TestBulkRecoveryCancelledByManualSwitch(t *testing.T) {
	fp := "sha256:cancel-by-manual"
	reset := time.Now().Add(time.Hour).UTC()
	s, coord, st, _, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100), ResetsAt: &reset}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", &reset)

	result, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Len(t, result.AffectedAgents, 1)
	s.startBulkRecovery(context.Background(), result.AffectedAgents)

	require.Eventually(t, func() bool {
		sess := st.snapSession("agent-1")
		return sess != nil && sess.BackendRecovery != nil && sess.BackendRecovery.Phase == recoveryWaiting
	}, time.Second, 5*time.Millisecond)

	coord.Supersede(context.Background(), "agent-1", "manual_switch")
	sess := st.snapSession("agent-1")
	require.Nil(t, sess.BackendRecovery, "manual switch must clear bulk-triggered recovery state")
	coord.mu.Lock()
	_, hasTimer := coord.timers["agent-1"]
	coord.mu.Unlock()
	require.False(t, hasTimer, "manual switch must cancel the retry timer")

	// A later reconciliation pass with a NEW snapshot revision for the same
	// still-exhausted bucket must skip the superseded agent, not re-trigger it.
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", &reset)
	again, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Empty(t, again.AffectedAgents, "a manually superseded agent must not be re-selected")
	require.Equal(t, capacity.SkipSuperseded, skipReason(again, "agent-1"))
}

// concurrencyTrackingLife wraps recoveryLife to record the peak number of
// concurrent HotSwap calls, with an artificial delay long enough for an
// unbounded fan-out to overlap in practice.
type concurrencyTrackingLife struct {
	*recoveryLife
	delay time.Duration

	mu      sync.Mutex
	current int
	peak    int
}

func (f *concurrencyTrackingLife) HotSwap(ctx context.Context, sess *agentstore.Agent, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
	f.mu.Lock()
	f.current++
	if f.current > f.peak {
		f.peak = f.current
	}
	f.mu.Unlock()
	time.Sleep(f.delay)
	f.mu.Lock()
	f.current--
	f.mu.Unlock()
	return f.recoveryLife.HotSwap(ctx, sess, req)
}

// TestBulkRecoveryBoundedAdvanceLimitsStampede verifies the "bounded
// queue/max parallelism" requirement itself: WithMaxParallelAdvance must cap
// how many candidate-selection/launch passes run concurrently even when a
// single shared bucket loss marks many agents affected at once, so they
// cannot all stampede onto the same limited alternative simultaneously.
func TestBulkRecoveryBoundedAdvanceLimitsStampede(t *testing.T) {
	fp := "sha256:stampede"
	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	seeded, err := bs.ListModels("")
	require.NoError(t, err)
	for _, m := range seeded {
		require.NoError(t, bs.SetModelEnabled(m.BackendID, m.ModelID, false))
	}
	for _, id := range []string{"codex", "claude", "antigravity"} {
		require.NoError(t, bs.Upsert(backendstore.Backend{ID: id, Installed: true, Enabled: true, Tier: backendstore.TierSubscription}))
		require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: id, ModelID: id + "-model", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))
	}
	adapters := []backendusage.Adapter{
		recoveryAdapter{id: "codex", result: backendusage.Result{Status: backendusage.StatusOK, Usage: []backendusage.Limit{{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}}}},
		recoveryAdapter{id: "claude", result: backendusage.Result{Status: backendusage.StatusOK, Usage: []backendusage.Limit{{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}}}},
		recoveryAdapter{id: "antigravity", result: backendusage.Result{Status: backendusage.StatusOK, Usage: []backendusage.Limit{{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100)}}}},
	}
	snapStore, err := backendusage.NewSnapshotStore(t.TempDir())
	require.NoError(t, err)
	usageService := backendusage.NewService(bs, adapters...)
	usageService.SetSnapshotStore(snapStore)

	st := newFakeStore()
	life := &concurrencyTrackingLife{recoveryLife: &recoveryLife{failures: make(map[string]error), st: st}, delay: 30 * time.Millisecond}
	coord := NewBackendRecoveryCoordinator(st, bs, usageService, life).WithMaxParallelAdvance(2)
	coord.stabilizationWindow = 5 * time.Millisecond

	fences, err := capacity.NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)
	s := NewServer(st, nil, nil, 0, false, nil, nil, nil)
	s.SetUsageService(usageService)
	s.SetUsageReconciliation(true, time.Minute, 15*time.Minute)
	s.SetImpactFences(fences)
	s.SetBackendRecovery(coord)

	const n = 6
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agent-%d", i)
		ids = append(ids, id)
		boundAgent(t, st, id, "codex", "codex-model", fp, "weekly")
	}
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err := s.calculateBucketImpact(context.Background())
	require.NoError(t, err)
	require.Len(t, result.AffectedAgents, n)

	s.startBulkRecovery(context.Background(), result.AffectedAgents)

	for _, id := range ids {
		require.Eventually(t, func() bool {
			sess := st.snapSession(id)
			return sess != nil && sess.BackendRecovery != nil && sess.BackendRecovery.Phase == recoveryStabilizing
		}, 2*time.Second, 5*time.Millisecond, "agent %s must still reach stabilizing despite bounded concurrency", id)
	}

	life.mu.Lock()
	peak := life.peak
	life.mu.Unlock()
	require.GreaterOrEqual(t, peak, 1, "sanity: at least one HotSwap must have happened")
	require.LessOrEqual(t, peak, 2, "WithMaxParallelAdvance(2) must bound concurrent candidate-selection/launch passes")
}
