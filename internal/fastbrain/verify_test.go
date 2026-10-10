package fastbrain

// Load, leak and integration verification for the Fast-Brain gateway (spec
// docs/specs/2026-10-10-fast-brain-decision-inventory-and-slos.md §10). Every
// test is deterministic: runners block on channels, never on wall-clock luck,
// and assertions are on bounds (goroutines, slots, queues), not timings.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// settleGoroutines waits for the goroutine count to fall back to base+slack.
func settleGoroutines(t *testing.T, base, slack int) {
	t.Helper()
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= base+slack },
		3*time.Second, 10*time.Millisecond, "goroutines: base=%d now=%d", base, runtime.NumGoroutine())
}

// A runner that never honours its context must not let goroutines grow without
// bound: past the wedged-runner cap new calls fail open without a runner call.
func TestVerifyContextIgnoringRunnerGoroutinesAreBounded(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	stuck := RunnerFunc(func(context.Context, string) (string, error) {
		started.Add(1)
		<-release
		return `{"a":1}`, nil
	})
	eng := NewEngineWithOptions(stuck, stuck, EngineOptions{
		FastTimeout: 20 * time.Millisecond, CancelGrace: 10 * time.Millisecond, MaxConcurrent: 2,
	})
	base := runtime.NumGoroutine()
	var deferred int
	for i := range 200 {
		r, err := eng.Decide(context.Background(), Request{
			Kind: KindCommitMessage, Tier: TierFast, Prompt: fmt.Sprintf("distinct-%d", i),
		})
		require.NoError(t, err)
		require.False(t, r.OK())
		if r.Status == StatusDeferred {
			deferred++
		}
	}
	cap := eng.(*engine).maxAbandonedLive()
	require.LessOrEqual(t, int(started.Load()), cap+eng.(*engine).opts.MaxConcurrent, "runner invocations are bounded")
	require.LessOrEqual(t, runtime.NumGoroutine(), base+cap+8, "goroutines bounded")
	require.Greater(t, deferred, 100, "calls past the cap fail open as deferred")
	require.LessOrEqual(t, eng.(*engine).CancelStats().Live, int64(cap))

	close(release)
	require.Eventually(t, func() bool { return eng.(*engine).CancelStats().Live == 0 }, 2*time.Second, 5*time.Millisecond)
	settleGoroutines(t, base, 3)

	// Recovered: the runner works again and calls are admitted.
	ok := NewEngineWithOptions(fixed(`{"ok":true}`), nil, EngineOptions{})
	r, _ := ok.Decide(context.Background(), req(TierFast))
	require.True(t, r.OK())
	r, _ = eng.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "after-recovery"})
	require.True(t, r.OK(), "engine admits again once wedged runners returned: %v", r.Status)
}

// Engine-level redaction and bound (spec §7): runners never see secrets or an
// oversized prompt regardless of the caller.
func TestVerifyEngineRedactsAndBoundsPrompt(t *testing.T) {
	var seen atomic.Value
	run := RunnerFunc(func(_ context.Context, p string) (string, error) {
		seen.Store(p)
		return `{"ok":true}`, nil
	})
	eng := NewEngine(run, run)
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	prompt := "Authorization: Bearer " + secret + "\nghp_" + strings.Repeat("a", 30) + "\n/home/alice/project\n" +
		strings.Repeat("x", 100<<10) + "\nTAIL-MARKER"
	r, err := eng.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: prompt})
	require.NoError(t, err)
	require.True(t, r.OK())
	got := seen.Load().(string)
	require.NotContains(t, got, secret)
	require.NotContains(t, got, "ghp_aaaa")
	require.NotContains(t, got, "/home/alice")
	require.LessOrEqual(t, len(got), MaxPromptBytes)
	require.Contains(t, got, "TAIL-MARKER", "truncation keeps the tail")
}

// Prompts that differ only in redactable secrets are the same decision.
func TestVerifyRedactedIdentityDedup(t *testing.T) {
	var calls atomic.Int32
	gate := make(chan struct{})
	run := RunnerFunc(func(ctx context.Context, p string) (string, error) {
		calls.Add(1)
		<-gate
		return `{"ok":true}`, nil
	})
	eng := NewEngine(run, run)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = eng.Decide(context.Background(), Request{
				Kind: KindClassifyTask, Tier: TierFast,
				Prompt: fmt.Sprintf("task Authorization: Bearer tok%016d", i),
			})
		}()
	}
	require.Eventually(t, func() bool { return snap(eng).Coalesced == 19 }, 2*time.Second, 5*time.Millisecond)
	close(gate)
	wg.Wait()
	require.EqualValues(t, 1, calls.Load(), "no duplicate decision for the same redacted prompt")
}

// A burst of identical decisions from many sessions (unchanged panes) costs one
// runner call per identity, never exceeds the slot bound and leaks nothing.
func TestVerifyUnchangedPaneBurstIsOneCall(t *testing.T) {
	base := runtime.NumGoroutine()
	var calls atomic.Int32
	gate := make(chan struct{})
	run := RunnerFunc(func(ctx context.Context, p string) (string, error) {
		calls.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return `{"is_prompt":false,"confidence":0.9}`, nil
	})
	eng := NewEngineWithOptions(run, run, EngineOptions{MaxConcurrent: 2})
	var wg sync.WaitGroup
	for i := range 400 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = eng.Decide(context.Background(), NewRequest(KindRecognizePrompt, TierThinking, "same pane", fmt.Sprintf("agent-%d", i%25)))
		}()
	}
	require.Eventually(t, func() bool { return snap(eng).Coalesced == 399 }, 3*time.Second, 5*time.Millisecond)
	require.EqualValues(t, 1, calls.Load())
	close(gate)
	wg.Wait()
	require.EqualValues(t, 1, calls.Load())
	require.Zero(t, snap(eng).Active)
	settleGoroutines(t, base, 2)
}

// Prompts that change while a decision is in flight are independent decisions:
// every caller receives the answer to its own prompt.
func TestVerifyPromptChangeDuringDecisionNoCrossTalk(t *testing.T) {
	run := RunnerFunc(func(ctx context.Context, p string) (string, error) {
		select {
		case <-time.After(2 * time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return fmt.Sprintf(`{"echo":%q}`, p), nil
	})
	eng := NewEngineWithOptions(run, run, EngineOptions{MaxConcurrent: 3})
	var wg sync.WaitGroup
	var wrong atomic.Int32
	for i := range 120 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := fmt.Sprintf("pane-version-%d", i%6)
			r, _ := eng.Decide(context.Background(), NewRequest(KindClassifyCIFailure, TierFast, p, "a"))
			if r.OK() && !strings.Contains(string(r.Output.Parsed), p+`"`) {
				wrong.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Zero(t, wrong.Load())
}

// A follower whose leader walked away still gets the result; cancelling one
// follower never cancels another. Run under -race.
func TestVerifyFollowersCancelIndependently(t *testing.T) {
	gate := make(chan struct{})
	var ran atomic.Int32
	run := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		ran.Add(1)
		select {
		case <-gate:
			return `{"v":1}`, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	eng := NewEngine(run, run)
	const n = 10
	ctxs := make([]context.Context, n)
	cancels := make([]context.CancelFunc, n)
	res := make([]<-chan Response, n)
	for i := range n {
		ctxs[i], cancels[i] = context.WithCancel(context.Background())
		res[i] = decideAsync(eng, ctxs[i], Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "shared"})
	}
	require.Eventually(t, func() bool { return snap(eng).Coalesced == n-1 }, 2*time.Second, 5*time.Millisecond)
	for i := 0; i < n-1; i++ { // leader first, then all but the last follower
		cancels[i]()
		r := <-res[i]
		require.Equal(t, StatusCanceled, r.Status)
	}
	require.EqualValues(t, 1, ran.Load())
	close(gate)
	r := <-res[n-1]
	require.True(t, r.OK(), "remaining follower is unaffected: %s %s", r.Status, r.Error)
	cancels[n-1]()
}

// Saturated lower-priority queues never delay a P1 decision beyond the slot
// being freed, and the P1 reserve keeps working with every other slot wedged.
func TestVerifySaturatedQueuesDoNotStarveCritical(t *testing.T) {
	gate := make(chan struct{})
	var p1Ran atomic.Int32
	run := RunnerFunc(func(ctx context.Context, p string) (string, error) {
		if strings.HasPrefix(p, "safety") {
			p1Ran.Add(1)
			return `{"ok":true}`, nil
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return `{"ok":true}`, nil
	})
	eng := NewEngineWithOptions(run, run, EngineOptions{MaxConcurrent: 3})
	var wg sync.WaitGroup
	kinds := []DecisionKind{KindCommitMessage, KindSummarizeCheck, KindReplTurn, KindClassifyTask, KindCurateExtract}
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = eng.Decide(ctx, Request{Kind: kinds[i%len(kinds)], Tier: TierFast, Prompt: fmt.Sprintf("bulk-%d", i),
				Metadata: map[string]string{"agent_id": fmt.Sprintf("a%d", i%9)}})
		}()
	}
	require.Eventually(t, func() bool { return snap(eng).Active >= 2 }, 2*time.Second, 5*time.Millisecond)
	for i := range 10 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		r, _ := eng.Decide(ctx, Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: fmt.Sprintf("safety-%d", i)})
		cancel()
		require.True(t, r.OK(), "P1 admitted promptly despite saturation: %s %s", r.Status, r.Error)
	}
	s := snap(eng)
	for c, q := range s.Queued {
		require.LessOrEqual(t, q, 64, "queue %d bounded", c)
	}
	close(gate)
	wg.Wait()
	require.EqualValues(t, 10, p1Ran.Load())
	require.Zero(t, snap(eng).Active)
}

// Operator surfaces (poller/TUI/API) must stay responsive while every slot is
// held by a blocked runner.
func TestVerifyTelemetryResponsiveUnderSaturation(t *testing.T) {
	gate := make(chan struct{})
	run := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return `{"ok":true}`, nil
	})
	eng := NewEngineWithOptions(run, run, EngineOptions{MaxConcurrent: 2})
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = eng.Decide(ctx, Request{Kind: KindReplTurn, Tier: TierFast, Prompt: fmt.Sprintf("p%d", i)})
		}()
	}
	require.Eventually(t, func() bool { return snap(eng).Active >= 1 }, 2*time.Second, 5*time.Millisecond)
	insp := eng.(Inspector)
	done := make(chan struct{})
	go func() {
		for range 200 {
			_ = insp.Telemetry()
			_ = insp.Decisions(50)
			_ = snap(eng)
			require.NoError(t, insp.SetKindPaused(KindClassifyTask, true, time.Minute))
			require.NoError(t, insp.SetKindPaused(KindClassifyTask, false, 0))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("telemetry/controls blocked behind saturated runners")
	}
	// Paused kinds fail open without touching a runner, even when saturated.
	require.NoError(t, insp.SetKindPaused(KindClassifyTask, true, time.Minute))
	r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "x"})
	require.Equal(t, StatusDeferred, r.Status)
	close(gate)
	wg.Wait()
}

// Shutdown: cancelling every caller (daemon stop) stops all runners, empties
// every queue and slot, and leaves no goroutines behind.
func TestVerifyShutdownDrainsEverything(t *testing.T) {
	base := runtime.NumGoroutine()
	var live atomic.Int32
	run := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		live.Add(1)
		defer live.Add(-1)
		<-ctx.Done()
		return "", ctx.Err()
	})
	eng := NewEngineWithOptions(run, run, EngineOptions{MaxConcurrent: 3})
	root, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	kinds := []DecisionKind{KindArbitrateApproval, KindReplTurn, KindCommitMessage, KindClassifyTask, KindDiagnoseStall}
	for i := range 150 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = eng.Decide(root, Request{Kind: kinds[i%len(kinds)], Tier: TierFast, Prompt: fmt.Sprintf("s-%d", i),
				Metadata: map[string]string{"agent_id": fmt.Sprintf("a%d", i%8)}})
		}()
	}
	require.Eventually(t, func() bool { return snap(eng).Active == 3 }, 2*time.Second, 5*time.Millisecond)
	stop()
	wg.Wait()
	require.Eventually(t, func() bool { return live.Load() == 0 }, 2*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		s := snap(eng)
		q := s.Active
		for _, v := range s.Queued {
			q += v
		}
		return q == 0
	}, 2*time.Second, 5*time.Millisecond, "slots and queues drain after shutdown")
	settleGoroutines(t, base, 2)
}

// Runner recovery: a failing provider opens its circuit (P4 stops calling it),
// then a healthy probe closes it and decisions flow again; callers see only
// fail-open responses during the outage.
func TestVerifyRunnerRecoveryThroughPool(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	var healthy atomic.Bool
	var calls atomic.Int32
	h := NewHealth(HealthOptions{Now: clk.now})
	cand := Candidate{ID: "p1", Runner: RunnerFunc(func(context.Context, string) (string, error) {
		calls.Add(1)
		if !healthy.Load() {
			return "", errors.New("provider down")
		}
		return `{"ok":true}`, nil
	})}
	src := CandidateSource(func(Tier) []Candidate { return []Candidate{cand} })
	pool := NewPool(TierFast, src, h, PoolOptions{})
	eng := NewEngine(pool, nil)
	do := func() Response {
		r, err := eng.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: fmt.Sprintf("c-%d", clk.t.UnixNano())})
		require.NoError(t, err)
		clk.t = clk.t.Add(time.Millisecond)
		return r
	}
	for range 3 {
		require.False(t, do().OK())
	}
	require.Equal(t, CircuitOpen, h.State("p1"))
	before := calls.Load()
	for range 20 {
		require.False(t, do().OK())
	}
	require.Equal(t, before, calls.Load(), "open circuit makes no provider calls")

	healthy.Store(true)
	clk.t = clk.t.Add(time.Minute) // past the first cooldown: half-open probe
	require.True(t, do().OK())
	require.Equal(t, CircuitClosed, h.State("p1"))
	require.True(t, do().OK())
}

// Race hammer: mixed kinds, tiers, cancels and pauses all at once. Meaningful
// under -race; asserts the controller's accounting returns to zero.
func TestVerifyRaceHammer(t *testing.T) {
	base := runtime.NumGoroutine()
	var n atomic.Int64
	run := RunnerFunc(func(ctx context.Context, p string) (string, error) {
		switch n.Add(1) % 7 {
		case 0:
			return "", errors.New("flaky")
		case 1:
			return "not json", nil
		case 2:
			<-ctx.Done()
			return "", ctx.Err()
		}
		return `{"ok":true}`, nil
	})
	eng := NewEngineWithOptions(run, run, EngineOptions{
		MaxConcurrent: 3, FastTimeout: 15 * time.Millisecond, ThinkingTimeout: 15 * time.Millisecond,
		CancelGrace: 5 * time.Millisecond,
	})
	insp := eng.(Inspector)
	kinds := AllKinds()
	var wg sync.WaitGroup
	for i := range 600 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			if i%5 == 0 {
				time.AfterFunc(time.Duration(i%7)*time.Millisecond, cancel)
			}
			defer cancel()
			tier := TierFast
			if i%3 == 0 {
				tier = TierThinking
			}
			_, _ = eng.Decide(ctx, Request{Kind: kinds[i%len(kinds)], Tier: tier, Prompt: fmt.Sprintf("h-%d", i%40),
				Metadata: map[string]string{"agent_id": fmt.Sprintf("a%d", i%11)}})
			if i%50 == 0 {
				_ = insp.SetKindPaused(kinds[i%len(kinds)], i%100 == 0, time.Second)
			}
			_ = insp.Telemetry()
		}()
	}
	wg.Wait()
	require.Eventually(t, func() bool {
		s := snap(eng)
		q := 0
		for _, v := range s.Queued {
			q += v
		}
		return s.Active == 0 && q == 0
	}, 3*time.Second, 5*time.Millisecond)
	require.Eventually(t, func() bool { return eng.(*engine).CancelStats().Live == 0 }, 3*time.Second, 5*time.Millisecond)
	settleGoroutines(t, base, 3)
}

// Daemon restart: the engine holds no durable state. Config-driven pauses come
// back; operator pauses are volatile by design (they never silently outlive a
// restart); no stale in-flight or cached state survives.
func TestVerifyRestartSemantics(t *testing.T) {
	var calls atomic.Int32
	run := RunnerFunc(func(context.Context, string) (string, error) {
		calls.Add(1)
		return `{"ok":true}`, nil
	})
	first := NewEngineWithOptions(run, run, EngineOptions{PausedKinds: []DecisionKind{KindClassifyTask}})
	require.NoError(t, first.(Inspector).SetKindPaused(KindCommitMessage, true, 0))
	r, _ := first.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "c"})
	require.Equal(t, StatusDeferred, r.Status)

	second := NewEngineWithOptions(run, run, EngineOptions{PausedKinds: []DecisionKind{KindClassifyTask}})
	r, _ = second.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "t"})
	require.Equal(t, StatusDeferred, r.Status, "config pause persists across restart")
	r, _ = second.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "c"})
	require.True(t, r.OK(), "operator pause does not survive a restart")
	require.False(t, r.Admission.Cached, "no cache carried over")
	require.EqualValues(t, 1, calls.Load())
}
