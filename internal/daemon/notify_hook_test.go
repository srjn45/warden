package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestNotifyMessageActionable(t *testing.T) {
	s := &store.Session{ID: "agent-x", Subject: "review auth"}
	cases := []struct {
		to         store.Status
		wantTitle  string
		wantInBody string
	}{
		{store.StatusWaitingForInput, "agentctl — needs input", "review auth"},
		{store.StatusIdle, "agentctl — stuck", "went idle"},
		{store.StatusOrphaned, "agentctl — agent lost", "tmux gone"},
		{store.StatusErrored, "agentctl — errored", "agent-x"},
	}
	for _, tc := range cases {
		title, body, ok := notifyMessage(s, tc.to)
		require.True(t, ok, tc.to)
		require.Equal(t, tc.wantTitle, title)
		require.Contains(t, body, tc.wantInBody)
	}
}

func TestNotifyMessageNonActionable(t *testing.T) {
	s := &store.Session{ID: "agent-x"}
	for _, st := range []store.Status{store.StatusWorking, store.StatusSpawning, store.StatusDone} {
		_, _, ok := notifyMessage(s, st)
		require.False(t, ok, st)
	}
}

func TestNotifyMessageSubjectFallsBackToID(t *testing.T) {
	_, body, ok := notifyMessage(&store.Session{ID: "agent-x"}, store.StatusWaitingForInput)
	require.True(t, ok)
	require.Contains(t, body, "agent-x")
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeNotifier) Notify(title, body string) { f.mu.Lock(); f.calls++; f.mu.Unlock() }
func (f *fakeNotifier) count() int                { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestNotifyOnTransitionFiresForActionableOnly(t *testing.T) {
	fn := &fakeNotifier{}
	hook := NotifyOnTransition(fn)
	hook(&store.Session{ID: "a"}, store.StatusWorking, store.StatusWaitingForInput) // actionable → fires
	hook(&store.Session{ID: "a"}, store.StatusWaitingForInput, store.StatusWorking) // not actionable
	require.Eventually(t, func() bool { return fn.count() == 1 }, time.Second, 5*time.Millisecond)
}
