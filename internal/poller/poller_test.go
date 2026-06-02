package poller

import (
	"context"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestHeuristicWorkingWhenInterruptVisible(t *testing.T) {
	s := &store.Session{Status: store.StatusIdle}
	got := classify(s, "Thinking… (esc to interrupt)", true, 0, 5*time.Minute)
	require.Equal(t, store.StatusWorking, got)
}

func TestHeuristicOrphanedWhenSessionGone(t *testing.T) {
	s := &store.Session{Status: store.StatusWorking}
	got := classify(s, "", false, 0, 5*time.Minute) // sessionAlive=false
	require.Equal(t, store.StatusOrphaned, got)
}

func TestHeuristicConfirmsWaitingAfterThreshold(t *testing.T) {
	s := &store.Session{
		Status:    store.StatusWaitingForInput,
		UpdatedAt: time.Now().Add(-10 * time.Minute),
		Events:    []store.Event{{Type: "Notification", TS: time.Now().Add(-10 * time.Minute)}},
	}
	got := classify(s, "Do you want to proceed? ❯ 1. Yes", true, 10*time.Minute, 5*time.Minute)
	require.Equal(t, store.StatusWaitingForInput, got)
}

func TestHeuristicKeepsHookStatusWhenNothingConclusive(t *testing.T) {
	s := &store.Session{Status: store.StatusIdle}
	got := classify(s, "some neutral pane text", true, 0, 5*time.Minute)
	require.Equal(t, store.StatusIdle, got) // unchanged
}

// --- stuck detection (time-based) ---

func TestHeuristicStuckWorkingBecomesIdle(t *testing.T) {
	// Claims "working" but no pane churn and no "esc to interrupt" for >= stuckAfter.
	s := &store.Session{Status: store.StatusWorking}
	got := classify(s, "no activity here", true, 10*time.Minute, 5*time.Minute)
	require.Equal(t, store.StatusIdle, got)
}

func TestHeuristicWorkingNotYetStuck(t *testing.T) {
	// Below the threshold → still working.
	s := &store.Session{Status: store.StatusWorking}
	got := classify(s, "no activity here", true, 1*time.Minute, 5*time.Minute)
	require.Equal(t, store.StatusWorking, got)
}

func TestHeuristicStuckIgnoredWhenInterruptVisible(t *testing.T) {
	// "esc to interrupt" means genuinely churning — never stuck.
	s := &store.Session{Status: store.StatusWorking}
	got := classify(s, "Thinking… (esc to interrupt)", true, 30*time.Minute, 5*time.Minute)
	require.Equal(t, store.StatusWorking, got)
}

func TestHeuristicStuckOnlyDowngradesWorking(t *testing.T) {
	// A long-idle waiting_for_input session is genuinely waiting, not "stuck".
	s := &store.Session{Status: store.StatusWaitingForInput}
	got := classify(s, "neutral", true, 30*time.Minute, 5*time.Minute)
	require.Equal(t, store.StatusWaitingForInput, got)
}

func TestHeuristicStuckDisabledWhenThresholdZero(t *testing.T) {
	s := &store.Session{Status: store.StatusWorking}
	got := classify(s, "neutral", true, 99*time.Hour, 0)
	require.Equal(t, store.StatusWorking, got)
}

// stubDeps fakes everything the poller touches.
type stubDeps struct {
	sessions     []*store.Session
	alive        map[string]bool
	panes        map[string]string
	updates      map[string]store.Status
	paneUpdates  map[string]string // records UpdatePane calls when non-nil
	subjects     map[string]string // records UpdateSubject calls
	summary      string            // canned Summarize result
	summarizeErr error
	summarizeN   int // count of Summarize calls
}

func (d *stubDeps) List(_ context.Context) ([]*store.Session, error) { return d.sessions, nil }
func (d *stubDeps) UpdateStatus(_ context.Context, id string, st store.Status) error {
	d.updates[id] = st
	return nil
}
func (d *stubDeps) UpdatePane(_ context.Context, id, ex string) error {
	if d.paneUpdates != nil {
		d.paneUpdates[id] = ex
	}
	return nil
}
func (d *stubDeps) UpdateSubject(_ context.Context, id, subject string) error {
	if d.subjects == nil {
		d.subjects = map[string]string{}
	}
	d.subjects[id] = subject
	return nil
}
func (d *stubDeps) Summarize(_ context.Context, s *store.Session) (string, error) {
	d.summarizeN++
	return d.summary, d.summarizeErr
}
func (d *stubDeps) SessionAlive(_ context.Context, name string) bool { return d.alive[name] }
func (d *stubDeps) CapturePane(_ context.Context, name string) (string, error) {
	return d.panes[name], nil
}

func TestTickMarksOrphaned(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
		alive:    map[string]bool{"A-1": false},
		panes:    map[string]string{},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, store.StatusOrphaned, d.updates["A-1"])
}

func TestTickSkipsTerminalStatuses(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusDone}},
		alive:    map[string]bool{"A-1": false},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	_, changed := d.updates["A-1"]
	require.False(t, changed, "done sessions must not be re-classified")
}

func TestTickFlagsStuckWorkingAsIdle(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{
			ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking,
			UpdatedAt:       time.Now().Add(-10 * time.Minute),
			LastPaneExcerpt: "quiet pane",
		}},
		alive:   map[string]bool{"A-1": true},
		panes:   map[string]string{"A-1": "quiet pane"}, // unchanged → updated_at stays stale
		updates: map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, store.StatusIdle, d.updates["A-1"])
}

func TestTickFreshWorkingStaysWorking(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{
			ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking,
			UpdatedAt:       time.Now().Add(-30 * time.Second),
			LastPaneExcerpt: "quiet pane",
		}},
		alive:   map[string]bool{"A-1": true},
		panes:   map[string]string{"A-1": "quiet pane"},
		updates: map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	_, changed := d.updates["A-1"]
	require.False(t, changed, "30s < 5m threshold → still working")
}

func TestTickUpdatesPaneOnlyWhenChanged(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{
			{ID: "same", TmuxSession: "same", Status: store.StatusWorking, LastPaneExcerpt: "hello", UpdatedAt: time.Now()},
			{ID: "diff", TmuxSession: "diff", Status: store.StatusWorking, LastPaneExcerpt: "old", UpdatedAt: time.Now()},
		},
		alive:       map[string]bool{"same": true, "diff": true},
		panes:       map[string]string{"same": "hello", "diff": "new output"},
		updates:     map[string]store.Status{},
		paneUpdates: map[string]string{},
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	_, sameUpdated := d.paneUpdates["same"]
	require.False(t, sameUpdated, "unchanged pane must not be re-written (keeps updated_at stable for staleness)")
	require.Equal(t, "new output", d.paneUpdates["diff"], "changed pane is persisted")
}

func TestTickCallsOnChangeWhenStatusChanges(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
		alive:    map[string]bool{"A-1": false}, // → orphaned (a change)
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	called := 0
	p.OnChange = func() { called++ }
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 1, called, "OnChange fires once when a status changed")
}

func TestTickNoOnChangeWhenNothingChanges(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{
			ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking,
			UpdatedAt: time.Now(), LastPaneExcerpt: "x",
		}},
		alive:   map[string]bool{"A-1": true},
		panes:   map[string]string{"A-1": "x"}, // unchanged pane, fresh → no change
		updates: map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	called := 0
	p.OnChange = func() { called++ }
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 0, called)
}

func TestTickRefreshesSubjectWhenPaneChangedAndDue(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking, LastPaneExcerpt: "old"}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": "new pane text"}, // changed
		updates:  map[string]store.Status{},
		summary:  "doing the thing",
	}
	p := New(d, 5*time.Minute)
	p.SummarizeAfter = 0 // always due
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 1, d.summarizeN)
	require.Equal(t, "doing the thing", d.subjects["A-1"])
}

func TestTickSkipsSummaryWhenPaneUnchanged(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking, LastPaneExcerpt: "same"}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": "same"}, // unchanged
		updates:  map[string]store.Status{},
		summary:  "x",
	}
	p := New(d, 5*time.Minute)
	p.SummarizeAfter = 0
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 0, d.summarizeN, "no summary when pane didn't change")
}

func TestTickThrottlesSummary(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking, LastPaneExcerpt: "old"}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": "changed"},
		updates:  map[string]store.Status{},
		summary:  "y",
	}
	p := New(d, 5*time.Minute)
	p.SummarizeAfter = time.Hour // not due (lastSummary set on first call)
	require.NoError(t, p.tick(context.Background()))           // first: due (zero time) → summarizes
	d.panes["A-1"] = "changed again"
	require.NoError(t, p.tick(context.Background()))           // second: within the hour → throttled
	require.Equal(t, 1, d.summarizeN, "throttled to one within the interval")
}
