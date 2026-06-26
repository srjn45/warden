package branchtrack

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

// fakeLister returns a fixed session set.
type fakeLister struct{ sessions []*store.Session }

func (f fakeLister) List(context.Context) ([]*store.Session, error) { return f.sessions, nil }

// recordingNotifier captures desktop notifications.
type recordingNotifier struct{ calls []string }

func (r *recordingNotifier) Notify(title, body string) { r.calls = append(r.calls, title) }

// branchState is the per-branch git answer the test feeds the tracker.
type branchState struct {
	behind, ahead int
	merged        bool
}

// newTestTracker wires a tracker over fixed sessions and per-branch CI/git maps
// keyed by branch.
func newTestTracker(t *testing.T, sessions []*store.Session, ci map[string]CIStatus, git map[string]branchState) (*Tracker, *recordingNotifier) {
	t.Helper()
	mbox, err := mailbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailbox.New: %v", err)
	}
	n := &recordingNotifier{}
	tr := NewTracker(fakeLister{sessions: sessions}, mbox, n)
	tr.ci = func(_ context.Context, _, branch string) CIStatus { return ci[branch] }
	tr.branchCmp = func(_ context.Context, worktree string) (int, int, bool) {
		// map worktree → branch via the session set so the test keys by branch.
		for _, s := range sessions {
			if s.Worktree == worktree {
				bs := git[s.Branch]
				return bs.behind, bs.ahead, bs.merged
			}
		}
		return 0, 0, false
	}
	return tr, n
}

func TestStatusesFiltersAndDedupsByBranch(t *testing.T) {
	calls := 0
	sessions := []*store.Session{
		{ID: "a", Name: "alpha", Worktree: "/wt/a", Branch: "feat-a", Status: store.StatusWorking},
		{ID: "b", Worktree: "/wt/b", Branch: "feat-a", Status: store.StatusWaitingForInput}, // shares branch
		{ID: "c", Worktree: "/wt/c", Branch: "", Status: store.StatusWorking},               // no branch → skipped
		{ID: "d", Worktree: "", Branch: "feat-d", Status: store.StatusWorking},              // no worktree → skipped
		{ID: "e", Worktree: "/wt/e", Branch: "feat-e", Status: store.StatusDone},            // terminal → skipped
	}
	tr, _ := newTestTracker(t, sessions, map[string]CIStatus{"feat-a": {State: ciSuccess}, "feat-e": {State: ciFailure}}, nil)
	tr.ci = func(_ context.Context, _, branch string) CIStatus {
		calls++
		return CIStatus{State: ciSuccess}
	}

	statuses, err := tr.Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("want 2 statuses (a,b — not c/d/e), got %d: %+v", len(statuses), statuses)
	}
	if calls != 1 {
		t.Fatalf("want CI queried once for the shared branch, got %d calls", calls)
	}
}

func TestCIFailureAlertsInboxAndNotifier(t *testing.T) {
	sessions := []*store.Session{{ID: "a", Name: "alpha", Worktree: "/wt/a", Branch: "feat-a", Status: store.StatusWorking}}
	ci := map[string]CIStatus{"feat-a": {State: ciFailure, Workflow: "build", URL: "http://ci/1"}}
	tr, n := newTestTracker(t, sessions, ci, nil)

	tr.tick(context.Background())

	msgs, err := tr.mbox.Messages("a")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 inbox alert, got %d", len(msgs))
	}
	if msgs[0].From != daemonSender {
		t.Fatalf("alert From = %q, want %q", msgs[0].From, daemonSender)
	}
	if len(n.calls) != 1 {
		t.Fatalf("want 1 desktop notification, got %d", len(n.calls))
	}
}

func TestCISuccessIsSilent(t *testing.T) {
	sessions := []*store.Session{{ID: "a", Worktree: "/wt/a", Branch: "feat-a", Status: store.StatusWorking}}
	ci := map[string]CIStatus{"feat-a": {State: ciSuccess, Workflow: "build"}}
	tr, n := newTestTracker(t, sessions, ci, nil)

	tr.tick(context.Background())

	msgs, _ := tr.mbox.Messages("a")
	if len(msgs) != 0 {
		t.Fatalf("success should be silent, got %d inbox messages", len(msgs))
	}
	if len(n.calls) != 0 {
		t.Fatalf("success should not notify, got %d", len(n.calls))
	}
}

func TestDedupSuppressesReAlertWithinWindow(t *testing.T) {
	sessions := []*store.Session{{ID: "a", Worktree: "/wt/a", Branch: "feat-a", Status: store.StatusWorking}}
	ci := map[string]CIStatus{"feat-a": {State: ciFailure, Workflow: "build"}}
	tr, n := newTestTracker(t, sessions, ci, nil)

	tr.tick(context.Background())
	tr.tick(context.Background()) // within window → no new alert

	msgs, _ := tr.mbox.Messages("a")
	if len(msgs) != 1 {
		t.Fatalf("want exactly 1 alert after two ticks, got %d", len(msgs))
	}
	if len(n.calls) != 1 {
		t.Fatalf("want exactly 1 notification after two ticks, got %d", len(n.calls))
	}
}

func TestStateChangeReAlerts(t *testing.T) {
	sessions := []*store.Session{{ID: "a", Worktree: "/wt/a", Branch: "feat-a", Status: store.StatusWorking}}
	state := CIStatus{State: ciPending, Workflow: "build"}
	tr, n := newTestTracker(t, sessions, nil, nil)
	tr.ci = func(_ context.Context, _, _ string) CIStatus { return state }

	tr.tick(context.Background()) // pending → no alert
	state.State = ciFailure
	tr.tick(context.Background()) // pending→failure → fresh alert

	msgs, _ := tr.mbox.Messages("a")
	if len(msgs) != 1 {
		t.Fatalf("want 1 alert on the state change, got %d", len(msgs))
	}
	if len(n.calls) != 1 {
		t.Fatalf("want 1 notification on the state change, got %d", len(n.calls))
	}
}

func TestMergedAndBehindAlerts(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Worktree: "/wt/a", Branch: "merged-br", Status: store.StatusWorking},
		{ID: "b", Worktree: "/wt/b", Branch: "behind-br", Status: store.StatusWorking},
		{ID: "c", Worktree: "/wt/c", Branch: "fresh-br", Status: store.StatusWorking},
	}
	git := map[string]branchState{
		"merged-br": {behind: 3, merged: true},
		"behind-br": {behind: behindThreshold + 1},
		"fresh-br":  {behind: 0, ahead: 0}, // up to date → silent
	}
	tr, _ := newTestTracker(t, sessions, nil, git)

	tr.tick(context.Background())

	if msgs, _ := tr.mbox.Messages("a"); len(msgs) != 1 {
		t.Fatalf("merged branch: want 1 alert, got %d", len(msgs))
	}
	if msgs, _ := tr.mbox.Messages("b"); len(msgs) != 1 {
		t.Fatalf("behind branch: want 1 alert, got %d", len(msgs))
	}
	if msgs, _ := tr.mbox.Messages("c"); len(msgs) != 0 {
		t.Fatalf("up-to-date branch: want 0 alerts, got %d", len(msgs))
	}
}

func TestBehindAtThresholdIsSilent(t *testing.T) {
	sessions := []*store.Session{{ID: "a", Worktree: "/wt/a", Branch: "feat-a", Status: store.StatusWorking}}
	git := map[string]branchState{"feat-a": {behind: behindThreshold}} // not strictly greater
	tr, _ := newTestTracker(t, sessions, nil, git)

	tr.tick(context.Background())

	if msgs, _ := tr.mbox.Messages("a"); len(msgs) != 0 {
		t.Fatalf("behind == threshold should be silent, got %d", len(msgs))
	}
}

func TestParseCIRun(t *testing.T) {
	cases := []struct {
		status, conclusion string
		want               string
	}{
		{"completed", "success", ciSuccess},
		{"completed", "failure", ciFailure},
		{"completed", "timed_out", ciFailure},
		{"completed", "cancelled", ciNone},
		{"in_progress", "", ciPending},
		{"queued", "", ciPending},
	}
	for _, c := range cases {
		if got := parseCIRun(c.status, c.conclusion, "wf", "url").State; got != c.want {
			t.Fatalf("parseCIRun(%q,%q) = %q, want %q", c.status, c.conclusion, got, c.want)
		}
	}
}

// TestGhAbsentNoOp ensures the real subprocess path degrades to "none"/zeros
// and never panics when gh/git can't run against a bogus worktree.
func TestGhAbsentNoOp(t *testing.T) {
	sessions := []*store.Session{{ID: "a", Worktree: "/nonexistent/wt", Branch: "feat-a", Status: store.StatusWorking}}
	mbox, err := mailbox.New(t.TempDir())
	if err != nil {
		t.Fatalf("mailbox.New: %v", err)
	}
	tr := NewTracker(fakeLister{sessions: sessions}, mbox, &recordingNotifier{})

	statuses, err := tr.Statuses(context.Background())
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	if len(statuses) != 1 || statuses[0].CI.State != ciNone {
		t.Fatalf("want one status with CI none, got %+v", statuses)
	}
	tr.tick(context.Background()) // must not panic or alert
}

func TestPruneDedupDropsStaleEntries(t *testing.T) {
	tr, _ := newTestTracker(t, nil, nil, nil)
	tr.shouldAlert("feat-a", "merged")
	tr.mu.Lock()
	tr.dedup["feat-a\x00merged"] = tr.dedup["feat-a\x00merged"].Add(-2 * dedupWindow)
	tr.mu.Unlock()

	tr.pruneDedup()

	tr.mu.Lock()
	n := len(tr.dedup)
	tr.mu.Unlock()
	if n != 0 {
		t.Fatalf("want stale dedup entry pruned, got %d", n)
	}
}
