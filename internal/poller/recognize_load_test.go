package poller

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/stretchr/testify/require"
)

// Many sessions stalled on the same unrecognized menu, ticked repeatedly while
// the model is blocked: the tick never waits for the model, identical panes are
// one decision, and at most recognizeMaxAttempts calls are made per menu.
func TestRecognizeBurstConcurrentSessionsBoundedAndNonBlocking(t *testing.T) {
	oldAfter := recognizeAfter
	recognizeAfter = 0
	t.Cleanup(func() { recognizeAfter = oldAfter })

	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	gate := make(chan struct{})
	var calls, peak, cur atomic.Int32
	r := fastbrain.RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		n := cur.Add(1)
		defer cur.Add(-1)
		calls.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return unknownReply, nil
	})
	p := New(d, 30*time.Second)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return blindBackend{} }
	p.FastBrain = fastbrain.NewEngineWithOptions(r, r, fastbrain.EngineOptions{MaxConcurrent: 2})
	p.SetRecognizePrompts(true)

	base := runtime.NumGoroutine()
	const sessions = 40
	start := time.Now()
	for round := 0; round < 5; round++ {
		for i := 0; i < sessions; i++ {
			s := &agentstore.Agent{ID: fmt.Sprintf("agent-%d", i), TmuxSession: "tmux-1", AutoApprove: true}
			p.tryRecognize(context.Background(), s, unknownPane)
		}
	}
	require.Less(t, time.Since(start), 2*time.Second, "tick must not wait for the model")
	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 2*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.EqualValues(t, 1, calls.Load(), "identical panes across sessions are one decision")
	require.LessOrEqual(t, peak.Load(), int32(2))

	close(gate)
	p.recogWG.Wait()
	require.Eventually(t, func() bool { return runtime.NumGoroutine() <= base+2 }, 3*time.Second, 10*time.Millisecond)
}

// The pane changes while the model is thinking: the stale reading is dropped
// and never answers the new menu.
func TestRecognizePaneChangeDuringDecisionDiscardsReading(t *testing.T) {
	oldAfter := recognizeAfter
	recognizeAfter = 0
	t.Cleanup(func() { recognizeAfter = oldAfter })

	d := &menuDeps{&stubDeps{panes: map[string]string{"tmux-1": unknownPane}}}
	gate := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	r := fastbrain.RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		once.Do(func() { close(started) })
		<-gate
		return unknownReply, nil
	})
	p := New(d, 30*time.Second)
	p.Backend = func(*agentstore.Agent) agentbackend.Backend { return blindBackend{} }
	p.FastBrain = fastbrain.NewEngine(r, r)
	p.SetRecognizePrompts(true)
	s := &agentstore.Agent{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: true}

	p.tryRecognize(context.Background(), s, unknownPane)
	<-started
	changed := "● Shell(rm -rf build)\n\nDifferent tool\n\nProceed?\n› Yes\n  No\n"
	p.tryRecognize(context.Background(), s, changed) // pane moved on
	close(gate)
	p.recogWG.Wait()

	_, ok := p.ParseApproval(s, changed)
	require.False(t, ok, "reading for the old menu must not apply to the new one")
	require.Zero(t, d.sendCount())
}
