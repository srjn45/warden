package fastbrain

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func okRunner(counter *atomic.Int32) Runner {
	return RunnerFunc(func(context.Context, string) (string, error) {
		counter.Add(1)
		return `{"ok":true}`, nil
	})
}

func failRunner(counter *atomic.Int32) Runner {
	return RunnerFunc(func(context.Context, string) (string, error) {
		counter.Add(1)
		return "", errors.New("boom")
	})
}

func TestHealthOpensOnConsecutiveFailuresAndProbes(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h := NewHealth(HealthOptions{Now: clk.now})
	for range 2 {
		h.Failure("a", FailRunnerError)
	}
	require.Equal(t, CircuitClosed, h.State("a"))
	h.Failure("a", FailTimeout)
	require.Equal(t, CircuitOpen, h.State("a"))
	require.False(t, h.Allow("a"))

	clk.t = clk.t.Add(31 * time.Second)
	require.True(t, h.Allow("a"), "one probe after the first cooldown")
	require.False(t, h.Allow("a"), "only one probe at a time")
	h.Failure("a", FailTimeout) // failed probe -> longer cooldown (2m)
	clk.t = clk.t.Add(31 * time.Second)
	require.False(t, h.Allow("a"))
	clk.t = clk.t.Add(90 * time.Second)
	require.True(t, h.Allow("a"))
	h.Success("a")
	require.Equal(t, CircuitClosed, h.State("a"))
}

func TestHealthOpensOnFailureRatio(t *testing.T) {
	h := NewHealth(HealthOptions{})
	// Alternating results never reach 3 consecutive but hit 50 % over 10.
	for i := range 10 {
		if i%2 == 1 {
			h.Failure("a", FailRunnerError)
		} else {
			h.Success("a")
		}
	}
	require.Equal(t, CircuitOpen, h.State("a"))
}

func TestHealthReleaseFreesProbeSlot(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h := NewHealth(HealthOptions{Now: clk.now})
	for range 3 {
		h.Failure("a", FailRunnerError)
	}
	clk.t = clk.t.Add(time.Minute)
	require.True(t, h.Allow("a"))
	h.Release("a")
	require.True(t, h.Allow("a"))
}

func TestPoolFallsBackOnceAndRecordsTrail(t *testing.T) {
	var bad, good atomic.Int32
	src := func(Tier) []Candidate {
		return []Candidate{
			{ID: "paid", Ineligible: "paid_tier"},
			{ID: "first", Runner: failRunner(&bad)},
			{ID: "second", Runner: okRunner(&good)},
		}
	}
	p := NewPool(TierFast, src, nil, PoolOptions{})
	out, sel, err := p.RunDetailed(context.Background(), "x")
	require.NoError(t, err)
	require.Contains(t, out, "ok")
	require.Equal(t, "second", sel.Runner)
	require.True(t, sel.Fallback)
	require.Equal(t, 2, sel.Attempts)
	require.Equal(t, int32(1), bad.Load(), "never retried within one decision")
	require.Equal(t, []CandidateOutcome{
		{ID: "paid", Verdict: VerdictRejected, Reason: "paid_tier"},
		{ID: "first", Verdict: VerdictFailed, Reason: "runner_error"},
		{ID: "second", Verdict: VerdictSelected, Reason: "ok"},
	}, sel.Trail)
}

func TestPoolSkipsOpenCircuitAndFailsOpenWithNoCandidate(t *testing.T) {
	var bad atomic.Int32
	h := NewHealth(HealthOptions{})
	src := func(Tier) []Candidate { return []Candidate{{ID: "only", Runner: failRunner(&bad)}} }
	p := NewPool(TierFast, src, h, PoolOptions{})
	for range 3 {
		_, _, err := p.RunDetailed(context.Background(), "x")
		require.Error(t, err)
	}
	_, sel, err := p.RunDetailed(context.Background(), "x")
	require.ErrorIs(t, err, ErrNoCandidate)
	require.Equal(t, int32(3), bad.Load(), "open circuit means zero further calls")
	require.Equal(t, "circuit_open", sel.Trail[0].Reason)
}

func TestPoolNeverDefaultsToUnlistedProvider(t *testing.T) {
	p := NewPool(TierThinking, func(Tier) []Candidate {
		return []Candidate{{ID: "claude", Ineligible: "rate_limited"}}
	}, nil, PoolOptions{})
	_, _, err := p.RunDetailed(context.Background(), "x")
	require.ErrorIs(t, err, ErrNoCandidate)
}

func TestPoolCallerCancelIsNotAHealthFailure(t *testing.T) {
	h := NewHealth(HealthOptions{})
	block := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	p := NewPool(TierFast, func(Tier) []Candidate { return []Candidate{{ID: "a", Runner: block}} }, h, PoolOptions{})
	for range 5 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, _, _ = p.RunDetailed(ctx, "x")
	}
	require.Equal(t, CircuitClosed, h.State("a"))
}

func TestPoolFirstAttemptLeavesTimeForFallback(t *testing.T) {
	var good atomic.Int32
	hang := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	src := func(Tier) []Candidate {
		return []Candidate{{ID: "slow", Runner: hang}, {ID: "fast", Runner: okRunner(&good)}}
	}
	eng := NewEngineWithOptions(NewPool(TierFast, src, nil, PoolOptions{}), nil, EngineOptions{FastTimeout: time.Second})
	r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "p"})
	require.True(t, r.OK(), r.Error)
	require.Equal(t, "fast", r.Selection.Runner)
	require.True(t, r.Selection.Fallback)
}

func TestEngineMapsNoCandidateToNoRunner(t *testing.T) {
	p := NewPool(TierFast, func(Tier) []Candidate { return nil }, nil, PoolOptions{})
	eng := NewEngine(p, nil)
	r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "p"})
	require.Equal(t, StatusNoRunner, r.Status)
}

func TestEngineAbandonsContextIgnoringRunnerWithinBound(t *testing.T) {
	release := make(chan struct{})
	stuck := RunnerFunc(func(context.Context, string) (string, error) {
		<-release
		return `{"a":1}`, nil
	})
	eng := NewEngineWithOptions(stuck, nil, EngineOptions{FastTimeout: 50 * time.Millisecond, CancelGrace: 50 * time.Millisecond, MaxConcurrent: 1})
	start := time.Now()
	r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "p"})
	require.Less(t, time.Since(start), time.Second)
	require.Equal(t, StatusTimeout, r.Status)
	require.Equal(t, CancelAbandoned, r.Cancel)
	cs := eng.(*engine).CancelStats()
	require.Equal(t, int64(1), cs.Abandoned)
	require.Equal(t, int64(1), cs.Live)

	close(release)
	require.Eventually(t, func() bool { return eng.(*engine).CancelStats().Live == 0 }, time.Second, 5*time.Millisecond)
}

func TestEngineRecordsAcknowledgedCancel(t *testing.T) {
	rr := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	eng := NewEngineWithOptions(rr, nil, EngineOptions{FastTimeout: 30 * time.Millisecond})
	r, _ := eng.Decide(context.Background(), Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "p"})
	require.Equal(t, StatusTimeout, r.Status)
	require.Equal(t, CancelAcknowledged, r.Cancel)
	require.Zero(t, eng.(*engine).CancelStats().Abandoned)
}

func TestLastWaiterCancelStopsRunnerAndNoGoroutineLeak(t *testing.T) {
	base := runtime.NumGoroutine()
	stopped := make(chan struct{})
	rr := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		close(stopped)
		return "", ctx.Err()
	})
	eng := NewEngine(rr, nil)
	ctx1, c1 := context.WithCancel(context.Background())
	ctx2, c2 := context.WithCancel(context.Background())
	done := make(chan struct{}, 2)
	for _, c := range []context.Context{ctx1, ctx2} {
		go func() {
			_, _ = eng.Decide(c, Request{Kind: KindClassifyTask, Tier: TierFast, Prompt: "p"})
			done <- struct{}{}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	c1()
	select {
	case <-stopped:
		t.Fatal("runner canceled while a follower is still interested")
	case <-time.After(50 * time.Millisecond):
	}
	c2()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("runner not canceled after last waiter left")
	}
	<-done
	<-done
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= base+1 }, 2*time.Second, 10*time.Millisecond)
}
