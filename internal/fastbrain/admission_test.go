package fastbrain

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func snap(e Engine) AdmissionSnapshot {
	return e.(interface{ AdmissionSnapshot() AdmissionSnapshot }).AdmissionSnapshot()
}

func decideAsync(e Engine, ctx context.Context, r Request) <-chan Response {
	ch := make(chan Response, 1)
	go func() {
		resp, _ := e.Decide(ctx, r)
		ch <- resp
	}()
	return ch
}

func TestClassOfMatchesInventory(t *testing.T) {
	for k, v := range decisionInventory {
		require.Equal(t, Class(v.Priority), ClassOf(k), "kind %s", k)
	}
}

// Backpressure: a full class queue sheds the newcomer with StatusDeferred and
// never starts a runner for it.
func TestQueueFullShedsExplicitly(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{
		MaxConcurrent: 1, Admission: AdmissionOptions{QueueDepth: 2, QueueWait: map[Class]time.Duration{ClassOperational: time.Minute}},
	})
	hold := decideAsync(eng, context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "hold"})
	waitStarted(t, rec, 1)
	var queued []<-chan Response
	for i := range 2 {
		queued = append(queued, decideAsync(eng, context.Background(),
			Request{Kind: KindSummarizeCheck, Tier: TierFast, Prompt: fmt.Sprintf("q%d", i)}))
	}
	require.Eventually(t, func() bool { return snap(eng).Queued[ClassOperational] == 2 }, time.Second, time.Millisecond)
	r, _ := eng.Decide(context.Background(), Request{Kind: KindSummarizeCheck, Tier: TierFast, Prompt: "overflow"})
	require.Equal(t, StatusDeferred, r.Status)
	require.Equal(t, ShedQueueFull, r.Admission.Reason)
	require.Zero(t, rec.count("overflow"))
	require.Equal(t, uint64(1), snap(eng).Shed[ShedQueueFull])
	close(rec.release)
	<-hold
	for _, q := range queued {
		require.True(t, (<-q).OK())
	}
}

// Best-effort work is never queued indefinitely; operational work is deferred
// after its bounded wait, not at the caller's whim.
func TestQueueWaitIsBounded(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{
		MaxConcurrent: 2, FastTimeout: 5 * time.Second,
		Admission: AdmissionOptions{QueueWait: map[Class]time.Duration{ClassOperational: 50 * time.Millisecond}},
	})
	hold := decideAsync(eng, context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "hold"})
	waitStarted(t, rec, 1)
	start := time.Now()
	r, _ := eng.Decide(context.Background(), Request{Kind: KindPRSummary, Tier: TierFast, Prompt: "waiter"})
	require.Equal(t, StatusDeferred, r.Status)
	require.Equal(t, ShedQueueWait, r.Admission.Reason)
	require.Less(t, time.Since(start), time.Second)
	require.Zero(t, snap(eng).Queued[ClassOperational], "timed-out waiter leaves the queue")
	close(rec.release)
	<-hold
}

// Priority: when the slot frees, the queued P1 is admitted before an earlier
// queued P3/P2.
func TestQueuedPriorityOrder(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{
		MaxConcurrent: 1,
		Admission: AdmissionOptions{QueueWait: map[Class]time.Duration{
			ClassOperational: time.Minute, ClassInteractive: time.Minute}},
	})
	hold := decideAsync(eng, context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "hold"})
	waitStarted(t, rec, 1)
	p3 := decideAsync(eng, context.Background(), Request{Kind: KindSummarizeCheck, Tier: TierFast, Prompt: "p3"})
	require.Eventually(t, func() bool { return snap(eng).Queued[ClassOperational] == 1 }, time.Second, time.Millisecond)
	p2 := decideAsync(eng, context.Background(), Request{Kind: KindReplTurn, Tier: TierFast, Prompt: "p2"})
	require.Eventually(t, func() bool { return snap(eng).Queued[ClassInteractive] == 1 }, time.Second, time.Millisecond)
	p1 := decideAsync(eng, context.Background(), Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: "p1"})
	require.Eventually(t, func() bool { return snap(eng).Queued[ClassSafety] == 1 }, time.Second, time.Millisecond)

	rec.release <- struct{}{} // finish "hold"
	<-hold
	var order []string
	for range 3 {
		order = append(order, <-rec.started)
		rec.release <- struct{}{}
	}
	require.Equal(t, []string{"p1", "p2", "p3"}, order)
	<-p1
	<-p2
	<-p3
}

// Fairness: one noisy agent cannot take more than ceil(max/2) slots in P2–P4;
// another agent's request proceeds while the noisy one waits.
func TestPerAgentFairness(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{
		MaxConcurrent: 4,
		Admission:     AdmissionOptions{QueueWait: map[Class]time.Duration{ClassOperational: time.Minute}},
	})
	md := func(a string) map[string]string { return map[string]string{"agent_id": a} }
	var noisy []<-chan Response
	for i := range 4 {
		noisy = append(noisy, decideAsync(eng, context.Background(), Request{
			Kind: KindSummarizeCheck, Tier: TierFast, Prompt: fmt.Sprintf("noisy-%d", i), Metadata: md("a1")}))
	}
	// cap = ceil(4/2)=2 and non-P1 cap = 3 -> a1 gets 2 slots.
	waitStarted(t, rec, 2)
	require.Eventually(t, func() bool { return snap(eng).Queued[ClassOperational] == 2 }, time.Second, time.Millisecond)
	other := decideAsync(eng, context.Background(), Request{
		Kind: KindSummarizeCheck, Tier: TierFast, Prompt: "other", Metadata: md("a2")})
	waitStarted(t, rec, 1)
	require.Equal(t, 1, rec.count("other"))
	close(rec.release)
	<-other
	for _, n := range noisy {
		require.True(t, (<-n).OK())
	}
}

// A running P4 call is preempted when a P2 call is blocked behind it; the P4
// caller sees StatusDeferred (fail open) and the P2 call is served.
func TestInteractivePreemptsBestEffort(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{MaxConcurrent: 2, FastTimeout: 5 * time.Second})
	p4 := decideAsync(eng, context.Background(), Request{Kind: KindSummarizeActivity, Tier: TierFast, Prompt: "badge"})
	waitStarted(t, rec, 1)
	p2 := decideAsync(eng, context.Background(), Request{Kind: KindReplTurn, Tier: TierFast, Prompt: "turn"})
	r4 := <-p4
	require.Equal(t, StatusDeferred, r4.Status)
	require.Equal(t, ShedPreempted, r4.Admission.Reason)
	waitStarted(t, rec, 1)
	close(rec.release)
	require.True(t, (<-p2).OK())
	require.Equal(t, uint64(1), snap(eng).Preempted)
}

// A caller leaving a queued call detaches; when all callers leave, the queued
// call is removed and no runner is ever started for it.
func TestQueuedCancelNeverStartsRunner(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{
		MaxConcurrent: 1, Admission: AdmissionOptions{QueueWait: map[Class]time.Duration{ClassOperational: time.Minute}},
	})
	hold := decideAsync(eng, context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: "hold"})
	waitStarted(t, rec, 1)
	ctx, cancel := context.WithCancel(context.Background())
	q := decideAsync(eng, ctx, Request{Kind: KindSummarizeCheck, Tier: TierFast, Prompt: "queued"})
	require.Eventually(t, func() bool { return snap(eng).Queued[ClassOperational] == 1 }, time.Second, time.Millisecond)
	cancel()
	require.Equal(t, StatusCanceled, (<-q).Status)
	require.Eventually(t, func() bool { return snap(eng).Queued[ClassOperational] == 0 }, time.Second, time.Millisecond)
	close(rec.release)
	<-hold
	require.Zero(t, rec.count("queued"))
}

// All callers leaving a running call cancels the runner context, and a new
// identical caller afterwards starts a fresh call rather than inheriting it.
func TestLastCallerLeavingCancelsRunnerAndFreshCallStartsNew(t *testing.T) {
	var starts, canceled atomic.Int32
	started := make(chan struct{}, 4)
	eng := NewEngineWithOptions(RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		starts.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		canceled.Add(1)
		return "", ctx.Err()
	}), nil, EngineOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	a := decideAsync(eng, ctx, req(TierFast))
	<-started
	cancel()
	require.Equal(t, StatusCanceled, (<-a).Status)
	require.Eventually(t, func() bool { return canceled.Load() == 1 }, time.Second, time.Millisecond)
	ctx2, cancel2 := context.WithCancel(context.Background())
	b := decideAsync(eng, ctx2, req(TierFast))
	<-started
	require.Equal(t, int32(2), starts.Load())
	cancel2()
	<-b
}

// Per-kind limit: at most one P4 call per kind runs; distinct-prompt siblings
// are shed rather than queued.
func TestBestEffortKindLimit(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{MaxConcurrent: 8})
	a := decideAsync(eng, context.Background(), Request{Kind: KindResolveAgentName, Tier: TierFast, Prompt: "n1"})
	waitStarted(t, rec, 1)
	r, _ := eng.Decide(context.Background(), Request{Kind: KindResolveAgentName, Tier: TierFast, Prompt: "n2"})
	require.Equal(t, StatusDeferred, r.Status)
	close(rec.release)
	<-a
}

// Dedup is by sanitized identity: prompts differing only in a secret coalesce.
func TestCoalescesOnSanitizedIdentity(t *testing.T) {
	rec := newPromptRecorder()
	eng := NewEngineWithOptions(rec, rec, EngineOptions{})
	k1 := "sk-" + "aaaaaaaaaaaaaaaaaaaa"
	k2 := "sk-" + "bbbbbbbbbbbbbbbbbbbb"
	a := decideAsync(eng, context.Background(), Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: "run with " + k1})
	waitStarted(t, rec, 1)
	b := decideAsync(eng, context.Background(), Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: "run with " + k2})
	require.Eventually(t, func() bool { return snap(eng).Coalesced == 1 }, time.Second, time.Millisecond)
	close(rec.release)
	require.True(t, (<-a).OK())
	require.True(t, (<-b).OK())
	require.Len(t, rec.prompts, 1)
}

// Approval decisions are never served from cache (freshness).
func TestSafetyKindsAreNotCached(t *testing.T) {
	var n atomic.Int32
	eng := NewEngineWithOptions(RunnerFunc(func(context.Context, string) (string, error) {
		n.Add(1)
		return `{"a":1}`, nil
	}), nil, EngineOptions{})
	for range 3 {
		_, _ = eng.Decide(context.Background(), Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: "same"})
	}
	require.Equal(t, int32(3), n.Load())
}

func TestCacheIsBounded(t *testing.T) {
	eng := NewEngineWithOptions(fixed(`{"a":1}`), nil, EngineOptions{Admission: AdmissionOptions{CacheEntries: 3}})
	for i := range 20 {
		_, _ = eng.Decide(context.Background(), Request{Kind: KindCommitMessage, Tier: TierFast, Prompt: fmt.Sprintf("p%d", i)})
	}
	require.LessOrEqual(t, snap(eng).CacheItems, 3)
}

// Race/stress: a saturating flood of mixed classes never starves safety
// decisions, never exceeds the global bound, and leaves no work behind.
func TestMixedFloodDoesNotStarveSafetyOrExceedBound(t *testing.T) {
	var cur, peak atomic.Int32
	run := RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer cur.Add(-1)
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return `{"ok":true}`, nil
	})
	eng := NewEngineWithOptions(run, run, EngineOptions{MaxConcurrent: 3})
	kinds := []DecisionKind{KindCommitMessage, KindClassifyTask, KindSummarizeActivity, KindSummarizeCheck, KindReplTurn, KindCurateExtract}
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = eng.Decide(context.Background(), Request{
				Kind: kinds[i%len(kinds)], Tier: TierFast, Prompt: fmt.Sprintf("flood-%d", i),
				Metadata: map[string]string{"agent_id": fmt.Sprintf("a%d", i%7)},
			})
		}()
	}
	var failed atomic.Int32
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			r, _ := eng.Decide(ctx, Request{Kind: KindArbitrateApproval, Tier: TierFast, Prompt: fmt.Sprintf("safety-%d", i)})
			if !r.OK() {
				failed.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Zero(t, failed.Load(), "no safety decision was shed or starved")
	require.LessOrEqual(t, peak.Load(), int32(3))
	s := snap(eng)
	require.Zero(t, s.Active)
	for c, n := range s.Queued {
		require.Zero(t, n, "class %d queue drained", c)
	}
}
