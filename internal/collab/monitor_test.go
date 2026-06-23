package collab

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

// fakeLister returns a fixed session set.
type fakeLister struct{ sessions []*store.Session }

func (f fakeLister) List(context.Context) ([]*store.Session, error) { return f.sessions, nil }

// newTestMonitor wires a monitor over a fixed session set and a diff map keyed
// by worktree path.
func newTestMonitor(t *testing.T, sessions []*store.Session, diffs map[string][]string) *Monitor {
	t.Helper()
	mbox, err := mailbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailbox.New: %v", err)
	}
	m := NewMonitor(fakeLister{sessions: sessions}, mbox)
	m.diff = func(_ context.Context, worktree string) []string { return diffs[worktree] }
	return m
}

func TestConflictsFiltersAndDetects(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Name: "alpha", Worktree: "/wt/a", Status: store.StatusWorking},
		{ID: "b", Name: "beta", Worktree: "/wt/b", Status: store.StatusWaitingForInput}, // paused but still holds edits
		{ID: "c", Worktree: "", Status: store.StatusWorking},                            // no worktree → skipped
		{ID: "d", Worktree: "/wt/d", Status: store.StatusDone},                          // terminal → skipped
	}
	diffs := map[string][]string{
		"/wt/a": {"internal/auth.go", "internal/only-a.go"},
		"/wt/b": {"internal/auth.go"},
		"/wt/d": {"internal/auth.go"}, // would collide, but d is terminal
	}
	m := newTestMonitor(t, sessions, diffs)

	conflicts, err := m.Conflicts(context.Background())
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d: %+v", len(conflicts), conflicts)
	}
	c := conflicts[0]
	if c.File != "internal/auth.go" {
		t.Fatalf("want conflict on auth.go, got %q", c.File)
	}
	if len(c.Agents) != 2 {
		t.Fatalf("want 2 agents (a,b — not terminal d), got %+v", c.Agents)
	}
}

func TestConflictsNoneWhenDistinctFiles(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Worktree: "/wt/a", Status: store.StatusWorking},
		{ID: "b", Worktree: "/wt/b", Status: store.StatusWorking},
	}
	diffs := map[string][]string{"/wt/a": {"a.go"}, "/wt/b": {"b.go"}}
	m := newTestMonitor(t, sessions, diffs)

	conflicts, err := m.Conflicts(context.Background())
	if err != nil {
		t.Fatalf("Conflicts: %v", err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("want no conflicts, got %+v", conflicts)
	}
}

func TestTickWarnsBothAgentsOnceWithinWindow(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Name: "alpha", Worktree: "/wt/a", Status: store.StatusWorking},
		{ID: "b", Name: "beta", Worktree: "/wt/b", Status: store.StatusWorking},
	}
	diffs := map[string][]string{"/wt/a": {"auth.go"}, "/wt/b": {"auth.go"}}
	m := newTestMonitor(t, sessions, diffs)

	m.tick(context.Background())
	m.tick(context.Background()) // within dedup window → no new warnings

	for _, id := range []string{"a", "b"} {
		msgs, err := m.mbox.Messages(id)
		if err != nil {
			t.Fatalf("Messages(%s): %v", id, err)
		}
		if len(msgs) != 1 {
			t.Fatalf("agent %s: want exactly 1 warning after two ticks, got %d", id, len(msgs))
		}
		if msgs[0].From != daemonSender {
			t.Fatalf("agent %s: warning From = %q, want %q", id, msgs[0].From, daemonSender)
		}
	}
}

func TestTickReWarnsAfterDedupExpiry(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Worktree: "/wt/a", Status: store.StatusWorking},
		{ID: "b", Worktree: "/wt/b", Status: store.StatusWorking},
	}
	diffs := map[string][]string{"/wt/a": {"auth.go"}, "/wt/b": {"auth.go"}}
	m := newTestMonitor(t, sessions, diffs)

	m.tick(context.Background())
	// Expire the dedup window by backdating recorded warnings.
	m.mu.Lock()
	for k := range m.dedup {
		m.dedup[k] = m.dedup[k].Add(-2 * dedupWindow)
	}
	m.mu.Unlock()
	m.tick(context.Background())

	msgs, err := m.mbox.Messages("a")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 warnings after dedup expiry, got %d", len(msgs))
	}
}

func TestPruneDedupDropsStaleEntries(t *testing.T) {
	m := newTestMonitor(t, nil, nil)
	m.shouldWarn("a", "f.go")
	m.mu.Lock()
	m.dedup["a\x00f.go"] = m.dedup["a\x00f.go"].Add(-2 * dedupWindow)
	m.mu.Unlock()

	m.pruneDedup()

	m.mu.Lock()
	n := len(m.dedup)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("want stale dedup entry pruned, got %d entries", n)
	}
}
