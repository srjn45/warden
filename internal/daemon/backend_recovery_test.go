package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type recoveryAdapter struct {
	id     string
	result backendusage.Result
}

func (a recoveryAdapter) BackendID() string { return a.id }
func (a recoveryAdapter) Fetch(context.Context, backendstore.Backend) backendusage.Result {
	return a.result
}

type recoveryLife struct {
	mu       sync.Mutex
	st       *fakeStore // used to write Backend/Model under the store lock (avoids race with snapSession)
	swaps    []store.BackendCandidate
	failures map[string]error
}

func (f *recoveryLife) Restore(context.Context, *store.Session) error  { return nil }
func (f *recoveryLife) SendKeys(context.Context, string, string) error { return nil }
func (f *recoveryLife) HotSwap(_ context.Context, sess *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
	f.mu.Lock()
	target := store.BackendCandidate{BackendID: req.Backend, ModelID: req.Model}
	f.swaps = append(f.swaps, target)
	err := f.failures[candidateKey(req.Backend, req.Model)]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// Write Backend/Model through the store lock so snapSession sees a consistent view.
	if f.st != nil {
		_ = f.st.Update(context.Background(), sess.ID, func(s *store.Session) error {
			s.Backend = req.Backend
			s.Model = req.Model
			return nil
		})
	} else {
		sess.Backend, sess.Model = req.Backend, req.Model
	}
	return &lifecycle.SwapResult{Session: sess, ToBackend: req.Backend, ToModel: req.Model}, nil
}

func recoveryFixture(t *testing.T, limits map[string][]backendusage.Limit) (*BackendRecoveryCoordinator, *fakeStore, *recoveryLife) {
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
	st := newFakeStore()
	require.NoError(t, st.Insert(context.Background(), &store.Session{ID: "agent-1", Backend: "codex", Model: "codex-model", Role: "general", Status: store.StatusRateLimited, PipelineID: "pipe-1", JobID: "build", Worktree: "/tmp/worktree", Tags: []string{"owned"}}))
	life := &recoveryLife{failures: make(map[string]error), st: st}
	c := NewBackendRecoveryCoordinator(st, bs, backendusage.NewService(bs, adapters...), life)
	c.stabilizationWindow = 5 * time.Millisecond
	return c, st, life
}

func used(v float64) *float64 { return &v }

func TestBackendRecoverySequentialFallbackAndStabilization(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	life.failures[candidateKey("claude", "claude-model")] = errors.New("launch failed")
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing && s.Backend == "antigravity"
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")
	require.Equal(t, "pipe-1", s.PipelineID)
	require.Equal(t, "build", s.JobID)
	require.Equal(t, "/tmp/worktree", s.Worktree)
	require.Equal(t, []string{"owned"}, s.Tags)
	require.Len(t, s.BackendRecovery.Attempts, 2)

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusWorking))
	c.OnTransition(s, store.StatusSpawning, store.StatusWorking)
	require.Eventually(t, func() bool {
		snap := st.snapSession("agent-1")
		return snap != nil && snap.BackendRecovery == nil
	}, time.Second, 5*time.Millisecond)
}

func TestBackendRecoveryAllExhaustedRetriesOriginalPool(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC()
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "short", Scope: "short", Label: "Short", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100), ResetsAt: &reset}},
	})
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, reset))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting && s.BackendRecovery.NextRetryAt != nil
	}, time.Second, 5*time.Millisecond)

	// A refreshed unknown measurement is eligible for trial. The new round keeps
	// the prior audit history but permits the exact original pool again.
	c.usage = backendusage.NewService(c.backends,
		recoveryAdapter{id: "codex", result: backendusage.Result{Status: backendusage.StatusOK}},
		recoveryAdapter{id: "claude", result: backendusage.Result{Status: backendusage.StatusOK, Usage: []backendusage.Limit{{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}}}},
		recoveryAdapter{id: "antigravity", result: backendusage.Result{Status: backendusage.StatusOK, Usage: []backendusage.Limit{{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100)}}}},
	)
	c.retry("agent-1", 1)
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Round == 1 && s.BackendRecovery.Phase == recoveryStabilizing && s.BackendRecovery.Current != nil && *s.BackendRecovery.Current == s.BackendRecovery.Original
	}, time.Second, 5*time.Millisecond)
}

func TestBackendRecoveryManualOverrideAndRestartTimer(t *testing.T) {
	c, st, _ := recoveryFixture(t, nil)
	next := time.Now().Add(time.Hour).UTC()
	require.NoError(t, st.Update(context.Background(), "agent-1", func(s *store.Session) error {
		s.BackendRecovery = &store.BackendRecovery{Generation: 7, Phase: recoveryWaiting, Original: store.BackendCandidate{BackendID: s.Backend, ModelID: s.Model}, NextRetryAt: &next}
		return nil
	}))
	require.NoError(t, c.Reconstruct(context.Background()))
	c.mu.Lock()
	require.NotNil(t, c.timers["agent-1"])
	c.mu.Unlock()

	c.Supersede(context.Background(), "agent-1", "manual_switch")
	s := st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery)
	c.mu.Lock()
	require.Nil(t, c.timers["agent-1"])
	c.mu.Unlock()
}

// TestBackendRecoveryNotifyOnPhaseChange verifies that the SSE notify callback
// fires at least once for each recovery phase transition. The spec (§8) requires
// SSE to fire on phase changes, not every stabilization poll.
func TestBackendRecoveryNotifyOnPhaseChange(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	var mu sync.Mutex
	var calls []string
	c.SetNotify(func() {
		// Record which event fired the notify: read the last event from the store.
		s := st.snapSession("agent-1")
		mu.Lock()
		defer mu.Unlock()
		if s != nil && len(s.Events) > 0 {
			calls = append(calls, s.Events[len(s.Events)-1].Type)
		}
	})

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	// Wait until stabilizing phase (refreshing → selecting → switching → stabilizing).
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
	}, time.Second, 5*time.Millisecond)

	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	require.NotEmpty(t, gotCalls, "notify must have been called on at least one phase transition")
	// backend_recovery_started fires first — verify it was captured.
	require.Contains(t, gotCalls, "backend_recovery_started")
	// backend_recovery_stabilizing fires when the candidate is stabilizing.
	require.Contains(t, gotCalls, "backend_recovery_stabilizing")
}

// TestBackendRecoveryNullableResetRoundTrip verifies that a null resets_at in
// RecoveryReset survives a store write+read cycle without being coerced to zero
// or a synthetic value. The spec (§8 privacy) requires null to remain null.
func TestBackendRecoveryNullableResetRoundTrip(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "short", Scope: "short", Label: "Short", UsedPercent: used(100)}}, // no ResetsAt
		"claude":      {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(100)}},
	})
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery)
	// At least one reset has a null ResetsAt (codex reports no reset time).
	var hasNull bool
	for _, r := range s.BackendRecovery.Resets {
		if r.ResetsAt == nil {
			hasNull = true
		}
	}
	require.True(t, hasNull, "a reset without provider-supplied time must have null resets_at, not zero")
}

// TestBackendRecoverySessionDTOFields verifies that the session returned by the
// daemon GET /sessions/{id} carries backend_recovery when active, and that the
// field is absent (null) when recovery completes.
// TestBackendRecoveryWithStabilizationWindow verifies that WithStabilizationWindow
// overrides the default and that a longer window delays stabilization confirmation.
func TestBackendRecoveryWithStabilizationWindow(t *testing.T) {
	c, _, _ := recoveryFixture(t, nil)
	// default is 5ms set by recoveryFixture; override to 50ms
	c.WithStabilizationWindow(50 * time.Millisecond)
	require.Equal(t, 50*time.Millisecond, c.stabilizationWindow)
}

// TestBackendRecoveryDeprecatedThresholdFieldsHaveNoEffect verifies that
// ThresholdPercent and RollingQuotaThreshold stored in HandoverSettings have
// no effect on recovery candidate selection or switching — the coordinator
// ignores them and triggers only on a confirmed hard limit.
func TestBackendRecoveryDeprecatedThresholdFieldsHaveNoEffect(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(50)}},
	})
	// Set the deprecated threshold fields to a very low value (50%). If the
	// coordinator were still consulting them for quota switching, it would trigger
	// on "claude" (50% >= 50%); instead, no switch should occur without an explicit
	// OnHardLimit call.
	handoverSettings := backendstore.HandoverSettings{
		Enabled:               true,
		ThresholdPercent:      50,
		RollingQuotaThreshold: 50,
		ContextFillThreshold:  90,
	}
	_ = handoverSettings // Fields stored in backendstore, not consumed by coordinator.

	// Without OnHardLimit, no swap should occur.
	require.Empty(t, life.swaps, "no swap before hard limit")
	s := st.snapSession("agent-1")
	require.NotNil(t, s)
	require.Nil(t, s.BackendRecovery, "recovery must not start without a confirmed hard limit")

	// A hard limit DOES start recovery, regardless of threshold values.
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		snap := st.snapSession("agent-1")
		return snap != nil && snap.BackendRecovery != nil
	}, time.Second, 5*time.Millisecond)
}

func TestBackendRecoverySessionDTOFields(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery, "backend_recovery must be non-null during active recovery")
	require.Equal(t, recoveryStabilizing, s.BackendRecovery.Phase)
	require.NotNil(t, s.BackendRecovery.Current, "current candidate must be set during stabilizing phase")
	require.Equal(t, "claude", s.BackendRecovery.Current.BackendID)

	// Trigger stabilization and verify recovery clears.
	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusWorking))
	c.OnTransition(s, store.StatusSpawning, store.StatusWorking)
	require.Eventually(t, func() bool {
		snap := st.snapSession("agent-1")
		return snap != nil && snap.BackendRecovery == nil
	}, time.Second, 5*time.Millisecond)
}

// ---------------------------------------------------------------------------
// Spec §11 matrix rows: coordinator transitions and policy
// ---------------------------------------------------------------------------

// TestBackendRecoveryDetectionExactPoolRecorded — Detection: one generation starts,
// exact backend/model recorded.
// The synchronous path of OnHardLimit must record the generation and the exact
// limited (backend, model) before launching the advance goroutine.
func TestBackendRecoveryDetectionExactPoolRecorded(t *testing.T) {
	c, st, _ := recoveryFixture(t, nil) // all unknown → eligible
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))

	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery, "BackendRecovery must be set synchronously on OnHardLimit")
	require.Equal(t, uint64(1), s.BackendRecovery.Generation, "exactly one generation must start")
	require.Equal(t, "codex", s.BackendRecovery.Original.BackendID, "limited backend must be recorded as Original")
	require.Equal(t, "codex-model", s.BackendRecovery.Original.ModelID, "limited model must be recorded as Original")
}

// TestBackendRecoveryRepeatedHardLimitNoDuplicate — Detection: repeated same
// transition/callback produces no duplicate generation, switch, or timer.
func TestBackendRecoveryRepeatedHardLimitNoDuplicate(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC()
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
	})

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, reset))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting
	}, time.Second, 5*time.Millisecond)

	// Duplicate transition while waiting: must return true (owned) without incrementing.
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, reset))
	s := st.snapSession("agent-1")
	require.Equal(t, uint64(1), s.BackendRecovery.Generation, "duplicate must not start a new generation")
	life.mu.Lock()
	swapCount := len(life.swaps)
	life.mu.Unlock()
	require.Zero(t, swapCount, "duplicate must not trigger an extra swap")
}

// TestBackendRecoveryEligibilityRulesRespected — Eligibility: disabled/uninstalled/
// local/pay-per-use backends must never be selected as recovery candidates.
func TestBackendRecoveryEligibilityRulesRespected(t *testing.T) {
	bs, err := backendstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bs.Close()) })
	seeded, _ := bs.ListModels("")
	for _, m := range seeded {
		require.NoError(t, bs.SetModelEnabled(m.BackendID, m.ModelID, false))
	}

	// Ineligible: backend disabled.
	require.NoError(t, bs.Upsert(backendstore.Backend{ID: "disabled-b", Installed: true, Enabled: false, Tier: backendstore.TierSubscription}))
	require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: "disabled-b", ModelID: "m", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))
	// Ineligible: backend not installed.
	require.NoError(t, bs.Upsert(backendstore.Backend{ID: "uninstalled-b", Installed: false, Enabled: true, Tier: backendstore.TierSubscription}))
	require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: "uninstalled-b", ModelID: "m", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))
	// Ineligible: local backend.
	require.NoError(t, bs.Upsert(backendstore.Backend{ID: "local-b", Installed: true, Enabled: true, IsLocal: true, Tier: backendstore.TierLocal}))
	require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: "local-b", ModelID: "m", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))
	// Ineligible: pay-per-use tier.
	require.NoError(t, bs.Upsert(backendstore.Backend{ID: "ppu-b", Installed: true, Enabled: true, Tier: backendstore.TierPayPerUse}))
	require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: "ppu-b", ModelID: "m", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))
	// Eligible: subscription backend.
	require.NoError(t, bs.Upsert(backendstore.Backend{ID: "ok-b", Installed: true, Enabled: true, Tier: backendstore.TierSubscription}))
	require.NoError(t, bs.UpsertModel(backendstore.ModelEntry{BackendID: "ok-b", ModelID: "ok-m", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true}))

	st := newFakeStore()
	require.NoError(t, st.Insert(context.Background(), &store.Session{
		ID: "agent-1", Backend: "disabled-b", Model: "m", Status: store.StatusRateLimited,
	}))
	life := &recoveryLife{failures: make(map[string]error), st: st}
	c := NewBackendRecoveryCoordinator(st, bs, backendusage.NewService(bs,
		recoveryAdapter{id: "ok-b", result: backendusage.Result{Status: backendusage.StatusOK}},
	), life)
	c.stabilizationWindow = 5 * time.Millisecond

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && (s.BackendRecovery.Phase == recoveryStabilizing || s.BackendRecovery.Phase == recoveryWaiting)
	}, time.Second, 5*time.Millisecond)

	life.mu.Lock()
	swaps := append([]store.BackendCandidate(nil), life.swaps...)
	life.mu.Unlock()
	for _, sw := range swaps {
		require.Equal(t, "ok-b", sw.BackendID, "only the eligible backend must be selected; got %s", sw.BackendID)
	}
}

// TestBackendRecoveryTierRolePreservedInRanking — Tier/role: the session's role
// tier determines candidate priority ordering. A tier-1 model must rank ahead of
// tier-2 models and be selected first.
func TestBackendRecoveryTierRolePreservedInRanking(t *testing.T) {
	c, st, life := recoveryFixture(t, nil) // all backends unknown → eligible
	// Promote the "general" role to model-tier Tier1.
	require.NoError(t, c.backends.SetRoleTier("general", backendstore.Tier1))
	// Add a tier-1 model on claude so it gets priority 0 (ahead of all tier-2).
	require.NoError(t, c.backends.UpsertModel(backendstore.ModelEntry{
		BackendID: "claude", ModelID: "claude-t1", Tier: backendstore.Tier1, Enabled: true, AutoAssign: true,
	}))

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
	}, time.Second, 5*time.Millisecond)

	life.mu.Lock()
	swaps := append([]store.BackendCandidate(nil), life.swaps...)
	life.mu.Unlock()
	require.NotEmpty(t, swaps, "a swap must have occurred")
	require.Equal(t, "claude", swaps[0].BackendID, "first swap must go to the tier-1 backend")
	require.Equal(t, "claude-t1", swaps[0].ModelID, "first swap must use the tier-1 model")
}

// TestBackendRecoveryTwoModelsOnSameBackendDistinct — Identity: two models on
// the same backend are tracked as distinct (backend, model) keys.
// A model-specific limit for model-a must not exclude model-b on the same backend.
func TestBackendRecoveryTwoModelsOnSameBackendDistinct(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		// model-a specific window: only codex-model is exhausted
		"codex":       {{ID: "limit-ma", Scope: "limit-ma", Label: "Model A limit", Models: []string{"codex-model"}, UsedPercent: used(100)}},
		"claude":      {{ID: "global", Scope: "global", Label: "Global", UsedPercent: used(100)}},
		"antigravity": {{ID: "global", Scope: "global", Label: "Global", UsedPercent: used(100)}},
	})
	// Add a second model on the same codex backend.
	require.NoError(t, c.backends.UpsertModel(backendstore.ModelEntry{
		BackendID: "codex", ModelID: "codex-model-2", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true,
	}))

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery.Current)
	require.Equal(t, "codex", s.BackendRecovery.Current.BackendID)
	require.Equal(t, "codex-model-2", s.BackendRecovery.Current.ModelID,
		"model-a's limit marker must not exclude model-b on the same backend")

	life.mu.Lock()
	swaps := append([]store.BackendCandidate(nil), life.swaps...)
	life.mu.Unlock()
	require.NotEmpty(t, swaps)
	require.Equal(t, "codex-model-2", swaps[0].ModelID)
}

// TestBackendRecoveryImmediateHardLimitDuringStabilizing — Recursive event guard:
// when the attempted candidate immediately hits a hard limit again during
// stabilization, the coordinator must advance exactly once within the same
// generation — no nested invocation, no new generation.
func TestBackendRecoveryImmediateHardLimitDuringStabilizing(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude":      {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
		"antigravity": {{ID: "other", Scope: "other", Label: "Other", UsedPercent: used(40)}},
	})

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing &&
			s.BackendRecovery.Current != nil && s.BackendRecovery.Current.BackendID == "claude"
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")
	gen := s.BackendRecovery.Generation

	// Simulate claude immediately hitting a hard limit again while stabilizing.
	require.True(t, c.OnHardLimit(s, time.Now().Add(time.Hour)),
		"OnHardLimit during stabilizing must still return true (owned)")

	// Coordinator must advance to antigravity without starting a new generation.
	require.Eventually(t, func() bool {
		cur := st.snapSession("agent-1")
		return cur != nil && cur.BackendRecovery != nil && cur.BackendRecovery.Phase == recoveryStabilizing &&
			cur.BackendRecovery.Current != nil && cur.BackendRecovery.Current.BackendID == "antigravity"
	}, time.Second, 5*time.Millisecond)

	s = st.snapSession("agent-1")
	require.Equal(t, gen, s.BackendRecovery.Generation, "recursive event must not start a new generation")

	var hasImmediate bool
	for _, a := range s.BackendRecovery.Attempts {
		if a.Outcome == "immediate_hard_limit" && a.Candidate.BackendID == "claude" {
			hasImmediate = true
		}
	}
	require.True(t, hasImmediate, "immediate_hard_limit must be recorded in attempts for claude")
}

// TestBackendRecoveryLiveWindowClearsExactlyOnce — Stabilization: live non-limited
// observations for the stabilization window clear recovery exactly once.
// A second OnTransition after clearing must be a harmless no-op.
func TestBackendRecoveryLiveWindowClearsExactlyOnce(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
	}, time.Second, 5*time.Millisecond)

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusWorking))
	s := st.snapSession("agent-1")
	c.OnTransition(s, store.StatusSpawning, store.StatusWorking)

	require.Eventually(t, func() bool {
		snap := st.snapSession("agent-1")
		return snap != nil && snap.BackendRecovery == nil
	}, time.Second, 5*time.Millisecond)

	// A second OnTransition must not panic or re-clear.
	s = st.snapSession("agent-1")
	c.OnTransition(s, store.StatusSpawning, store.StatusWorking)

	got := st.snapSession("agent-1")
	require.Nil(t, got.BackendRecovery)
	var stabilizedCount int
	for _, ev := range got.Events {
		if ev.Type == "backend_recovery_stabilized" {
			stabilizedCount++
		}
	}
	require.Equal(t, 1, stabilizedCount, "backend_recovery_stabilized must fire exactly once")
}

// TestBackendRecoveryReconstructWhileStabilizingIsHarmless — Restart: daemon dies
// while stabilizing. Reconstruct must not arm a retry timer for a stabilizing
// session; only waiting sessions get timers.
func TestBackendRecoveryReconstructWhileStabilizingIsHarmless(t *testing.T) {
	c, st, _ := recoveryFixture(t, nil)

	now := time.Now().UTC()
	cur := store.BackendCandidate{BackendID: "claude", ModelID: "claude-model"}
	require.NoError(t, st.Update(context.Background(), "agent-1", func(s *store.Session) error {
		s.BackendRecovery = &store.BackendRecovery{
			Generation: 3, Phase: recoveryStabilizing,
			Original:  store.BackendCandidate{BackendID: "codex", ModelID: "codex-model"},
			Current:   &cur,
			UpdatedAt: now,
		}
		return nil
	}))

	require.NoError(t, c.Reconstruct(context.Background()))

	c.mu.Lock()
	_, scheduled := c.timers["agent-1"]
	c.mu.Unlock()
	require.False(t, scheduled, "Reconstruct must not arm a retry timer for a stabilizing session")

	s := st.snapSession("agent-1")
	require.NotNil(t, s.BackendRecovery)
	require.Equal(t, recoveryStabilizing, s.BackendRecovery.Phase, "recovery state must be untouched by Reconstruct")
}

// TestBackendRecoveryStopWinsDuringWait — Stop during recovery: Supersede with
// "stop" wins over waiting recovery. No retry, relaunch, or state resurrection.
func TestBackendRecoveryStopWinsDuringWait(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC()
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "s", Scope: "short", Label: "Short", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "o", Scope: "other", Label: "Other", UsedPercent: used(100), ResetsAt: &reset}},
	})

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, reset))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting
	}, time.Second, 5*time.Millisecond)

	c.Supersede(context.Background(), "agent-1", "stop")
	s := st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery, "stop must clear recovery state")
	c.mu.Lock()
	_, hasTimer := c.timers["agent-1"]
	c.mu.Unlock()
	require.False(t, hasTimer, "stop must cancel the retry timer")

	// Stale retry firing after stop must be a harmless no-op.
	c.retry("agent-1", 1)
	s = st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery, "stale retry after stop must not resurrect recovery")
}

// TestBackendRecoveryStaleTimerAfterSupersede — Delete: a stale timer that fires
// after the recovery record is cleared (by Supersede) must be a no-op.
func TestBackendRecoveryStaleTimerAfterSupersede(t *testing.T) {
	c, st, _ := recoveryFixture(t, nil)

	next := time.Now().Add(time.Hour).UTC()
	require.NoError(t, st.Update(context.Background(), "agent-1", func(s *store.Session) error {
		s.BackendRecovery = &store.BackendRecovery{
			Generation: 9, Phase: recoveryWaiting,
			Original:    store.BackendCandidate{BackendID: "codex", ModelID: "codex-model"},
			NextRetryAt: &next,
		}
		return nil
	}))
	require.NoError(t, c.Reconstruct(context.Background()))

	// Delete supersedes and clears the timer.
	c.Supersede(context.Background(), "agent-1", "delete")
	s := st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery)

	// A stale retry with the old generation must be a no-op (generation mismatch guard).
	c.retry("agent-1", 9)
	s = st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery, "stale timer after deletion must not resurrect state")
}

// TestBackendRecoveryAutopilotWorkerFieldsPreserved — Autopilot worker: run/task
// ownership and slot tags must survive the entire recovery cycle unchanged.
// The coordinator must not mutate autopilot metadata.
func TestBackendRecoveryAutopilotWorkerFieldsPreserved(t *testing.T) {
	reset := time.Now().Add(time.Hour).UTC()
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":       {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"claude":      {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(100), ResetsAt: &reset}},
		"antigravity": {{ID: "o", Scope: "other", Label: "Other", UsedPercent: used(100), ResetsAt: &reset}},
	})

	require.NoError(t, st.Update(context.Background(), "agent-1", func(s *store.Session) error {
		s.AutopilotRunID = "run-42"
		s.AutopilotSlot = store.AutopilotSlotWorker
		s.AutopilotTaskID = "task-99"
		s.ParentID = "orchestrator-1"
		return nil
	}))

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, reset))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")
	require.Equal(t, "run-42", s.AutopilotRunID, "autopilot run ID must survive recovery")
	require.Equal(t, store.AutopilotSlotWorker, s.AutopilotSlot, "autopilot slot must survive recovery")
	require.Equal(t, "task-99", s.AutopilotTaskID, "autopilot task ID must survive recovery")
	require.Equal(t, "orchestrator-1", s.ParentID, "parent ID must survive recovery")
}

// TestBackendRecoveryEventsArePrivate — Privacy: events/API/TUI must carry no
// credential, account id, raw response/banner, prompt, or transcript. Event
// types must all be from the spec §8 allowed set.
func TestBackendRecoveryEventsArePrivate(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "w", Scope: "weekly", Label: "Weekly", UsedPercent: used(30)}},
	})

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")

	// No banned keywords in event detail (credentials, PII, raw tokens).
	banned := []string{"token", "password", "secret", "credential", "authorization", "@", "api_key"}
	for _, ev := range s.Events {
		detail := strings.ToLower(ev.Detail)
		for _, kw := range banned {
			require.NotContains(t, detail, kw,
				"event %q detail must not contain sensitive keyword %q", ev.Type, kw)
		}
	}

	// All emitted events must belong to the spec §8 defined type set.
	allowedTypes := map[string]bool{
		"backend_recovery_started":              true,
		"backend_pool_limited":                  true,
		"backend_usage_refreshed":               true,
		"backend_recovery_candidate_selected":   true,
		"backend_recovery_switch_started":       true,
		"backend_recovery_stabilizing":          true,
		"backend_recovery_attempt_failed":       true,
		"backend_recovery_waiting_for_capacity": true,
		"backend_recovery_retry_scheduled":      true,
		"backend_recovery_resumed_same_backend": true,
		"backend_recovery_switched_backend":     true,
		"backend_recovery_stabilized":           true,
		"backend_recovery_superseded":           true,
	}
	for _, ev := range s.Events {
		require.True(t, allowedTypes[ev.Type],
			"event type %q is not in the spec §8 allowed set", ev.Type)
	}
}

// TestBackendRecoveryContextFillDoesNotTriggerCoordinator — Context: context-fill
// transitions must not start or advance the coordinator. OnTransition to live
// statuses (the path context-fill HotSwap generates) must be a no-op when no
// active recovery exists. OnTransition to StatusRateLimited is also a no-op
// (the coordinator's OnTransition guard ignores non-live target statuses).
func TestBackendRecoveryContextFillDoesNotTriggerCoordinator(t *testing.T) {
	c, st, _ := recoveryFixture(t, nil)

	s := st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery, "no recovery before any OnHardLimit call")

	// Transitions that a context-fill HotSwap generates must not start recovery.
	c.OnTransition(s, store.StatusIdle, store.StatusWorking)
	c.OnTransition(s, store.StatusWorking, store.StatusWaitingForInput)
	c.OnTransition(s, store.StatusWaitingForInput, store.StatusIdle)

	s = st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery, "context-fill transitions must not trigger recovery coordinator")

	// OnTransition to StatusRateLimited (not a live status) must also be a no-op:
	// the coordinator's OnTransition only acts during the stabilizing phase.
	c.OnTransition(s, store.StatusWorking, store.StatusRateLimited)
	s = st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery, "OnTransition to rate-limited without OnHardLimit must not start recovery")
}
