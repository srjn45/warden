package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestContextAlertMessage(t *testing.T) {
	s := &store.Session{ID: "agent-x", Subject: "refactor auth"}
	title, body := ContextAlertMessage(s, ctxtokens.StateWarning, 210000)
	if title == "" || body == "" {
		t.Fatal("empty message")
	}
	if !strings.Contains(body, "agent-x") || !strings.Contains(body, "210k") {
		t.Fatalf("body missing id/size: %q", body)
	}
	tCrit, bCrit := ContextAlertMessage(s, ctxtokens.StateCritical, 410000)
	if !strings.Contains(strings.ToLower(tCrit+bCrit), "critical") {
		t.Fatalf("critical message should say critical: %q / %q", tCrit, bCrit)
	}
}

func TestNotifyMessageActionable(t *testing.T) {
	s := &store.Session{ID: "agent-x", Subject: "review auth"}
	cases := []struct {
		to         store.Status
		wantTitle  string
		wantInBody string
	}{
		{store.StatusWaitingForInput, "warden — needs input", "review auth"},
		{store.StatusIdle, "warden — possibly-stuck", "went idle"},
		{store.StatusOrphaned, "warden — agent lost", "tmux gone"},
		{store.StatusErrored, "warden — errored", "agent-x"},
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
