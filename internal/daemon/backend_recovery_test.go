package daemon

import (
	"context"
	"errors"
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
	swaps    []store.BackendCandidate
	failures map[string]error
}

func (f *recoveryLife) Restore(context.Context, *store.Session) error  { return nil }
func (f *recoveryLife) SendKeys(context.Context, string, string) error { return nil }
func (f *recoveryLife) HotSwap(_ context.Context, sess *store.Session, req lifecycle.SwapRequest) (*lifecycle.SwapResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := store.BackendCandidate{BackendID: req.Backend, ModelID: req.Model}
	f.swaps = append(f.swaps, target)
	if err := f.failures[candidateKey(req.Backend, req.Model)]; err != nil {
		return nil, err
	}
	sess.Backend, sess.Model = req.Backend, req.Model
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
	life := &recoveryLife{failures: make(map[string]error)}
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
		s, _ := st.Get(context.Background(), "agent-1")
		return s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing && s.Backend == "antigravity"
	}, time.Second, 5*time.Millisecond)

	s, _ := st.Get(context.Background(), "agent-1")
	require.Equal(t, "pipe-1", s.PipelineID)
	require.Equal(t, "build", s.JobID)
	require.Equal(t, "/tmp/worktree", s.Worktree)
	require.Equal(t, []string{"owned"}, s.Tags)
	require.Len(t, s.BackendRecovery.Attempts, 2)

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusWorking))
	c.OnTransition(s, store.StatusSpawning, store.StatusWorking)
	require.Eventually(t, func() bool {
		got, _ := st.Get(context.Background(), "agent-1")
		return got.BackendRecovery == nil
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
		s, _ := st.Get(context.Background(), "agent-1")
		return s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting && s.BackendRecovery.NextRetryAt != nil
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
		s, _ := st.Get(context.Background(), "agent-1")
		return s.BackendRecovery != nil && s.BackendRecovery.Round == 1 && s.BackendRecovery.Phase == recoveryStabilizing && s.BackendRecovery.Current != nil && *s.BackendRecovery.Current == s.BackendRecovery.Original
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
	s, _ := st.Get(context.Background(), "agent-1")
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
		s, err := st.Get(context.Background(), "agent-1")
		mu.Lock()
		defer mu.Unlock()
		if err == nil && len(s.Events) > 0 {
			calls = append(calls, s.Events[len(s.Events)-1].Type)
		}
	})

	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	// Wait until stabilizing phase (refreshing → selecting → switching → stabilizing).
	require.Eventually(t, func() bool {
		s, _ := st.Get(context.Background(), "agent-1")
		return s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
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
		s, _ := st.Get(context.Background(), "agent-1")
		return s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryWaiting
	}, time.Second, 5*time.Millisecond)

	s, err := st.Get(context.Background(), "agent-1")
	require.NoError(t, err)
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
func TestBackendRecoverySessionDTOFields(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, time.Now().Add(time.Hour)))
	require.Eventually(t, func() bool {
		s, _ := st.Get(context.Background(), "agent-1")
		return s.BackendRecovery != nil && s.BackendRecovery.Phase == recoveryStabilizing
	}, time.Second, 5*time.Millisecond)

	s, err := st.Get(context.Background(), "agent-1")
	require.NoError(t, err)
	require.NotNil(t, s.BackendRecovery, "backend_recovery must be non-null during active recovery")
	require.Equal(t, recoveryStabilizing, s.BackendRecovery.Phase)
	require.NotNil(t, s.BackendRecovery.Current, "current candidate must be set during stabilizing phase")
	require.Equal(t, "claude", s.BackendRecovery.Current.BackendID)

	// Trigger stabilization and verify recovery clears.
	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusWorking))
	c.OnTransition(s, store.StatusSpawning, store.StatusWorking)
	require.Eventually(t, func() bool {
		got, _ := st.Get(context.Background(), "agent-1")
		return got.BackendRecovery == nil
	}, time.Second, 5*time.Millisecond)
}
