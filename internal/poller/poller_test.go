package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
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
	updates      map[string]store.Status // records successful status swaps
	lastExpected map[string]store.Status // records the CAS "expected" arg
	casFail      map[string]bool         // when true for an id, the CAS misses (lost race)
	paneUpdates  map[string]string       // records UpdatePane calls when non-nil
	subjects     map[string]string       // records UpdateSubject calls
	summary      string                  // canned Summarize result
	summarizeErr error
	summarizeN   int   // count of Summarize calls
	captureErr   error // when set, CapturePane fails for every session
	// summarizeFn, when set, overrides the canned result — lets a test inspect
	// the context or block, e.g. to exercise the per-call timeout.
	summarizeFn func(context.Context, *store.Session) (string, error)
}

func (d *stubDeps) List(_ context.Context) ([]*store.Session, error) { return d.sessions, nil }
func (d *stubDeps) UpdateStatusIf(_ context.Context, id string, expected, next store.Status) (bool, error) {
	if d.lastExpected == nil {
		d.lastExpected = map[string]store.Status{}
	}
	d.lastExpected[id] = expected
	if d.casFail[id] {
		return false, nil // simulate a hook having changed status since the snapshot
	}
	d.updates[id] = next
	return true, nil
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
func (d *stubDeps) Summarize(ctx context.Context, s *store.Session) (string, error) {
	d.summarizeN++
	if d.summarizeFn != nil {
		return d.summarizeFn(ctx, s)
	}
	return d.summary, d.summarizeErr
}

func TestRunSummaryAppliesPerCallTimeout(t *testing.T) {
	// A hung `claude -p` must not latch the inflight flag forever (which would
	// permanently suppress that session's subject refreshes). runSummary must
	// bound the call with a per-call timeout that clears inflight on expiry.
	orig := summaryTimeout
	summaryTimeout = 20 * time.Millisecond
	defer func() { summaryTimeout = orig }()

	deadlineSeen := make(chan bool, 1)
	d := &stubDeps{
		updates: map[string]store.Status{},
		summarizeFn: func(ctx context.Context, s *store.Session) (string, error) {
			_, ok := ctx.Deadline()
			deadlineSeen <- ok
			<-ctx.Done() // hang until the per-call timeout fires
			return "", ctx.Err()
		},
	}
	p := New(d, 0)
	p.mu.Lock()
	p.inflight["A-1"] = struct{}{} // dispatchSummary would have set this
	p.mu.Unlock()
	p.wg.Add(1)

	done := make(chan struct{})
	go func() {
		p.runSummary(context.Background(), &store.Session{ID: "A-1"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runSummary did not return — a hung Summarize must be bounded by a per-call timeout")
	}
	require.True(t, <-deadlineSeen, "Summarize must receive a context with a deadline")

	p.mu.Lock()
	_, busy := p.inflight["A-1"]
	p.mu.Unlock()
	require.False(t, busy, "inflight must be cleared after the summary times out")
}
func (d *stubDeps) SessionAlive(_ context.Context, name string) bool { return d.alive[name] }
func (d *stubDeps) CapturePane(_ context.Context, name string) (string, error) {
	if d.captureErr != nil {
		return "", d.captureErr
	}
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

func TestTickSkipsClassifyWhenCaptureFails(t *testing.T) {
	// Alive session but pane capture errors transiently: the poller must not
	// record an empty excerpt nor downgrade a stale "working" session to idle.
	d := &stubDeps{
		sessions: []*store.Session{{
			ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking,
			UpdatedAt:       time.Now().Add(-10 * time.Minute), // would be "stuck" if classified
			LastPaneExcerpt: "prior pane",
		}},
		alive:       map[string]bool{"A-1": true},
		panes:       map[string]string{},
		updates:     map[string]store.Status{},
		paneUpdates: map[string]string{},
		captureErr:  errors.New("tmux capture failed"),
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	_, statusChanged := d.updates["A-1"]
	require.False(t, statusChanged, "no fresh pane signal → status must be left untouched")
	_, paneWritten := d.paneUpdates["A-1"]
	require.False(t, paneWritten, "a failed capture must not overwrite the excerpt with empty")
}

func TestTickStillMarksOrphanedWhenSessionDead(t *testing.T) {
	// captureErr is irrelevant when the session is dead — capture isn't attempted
	// and orphan detection (pane-independent) must still fire.
	d := &stubDeps{
		sessions:   []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
		alive:      map[string]bool{"A-1": false},
		panes:      map[string]string{},
		updates:    map[string]store.Status{},
		captureErr: errors.New("should not be called"),
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, store.StatusOrphaned, d.updates["A-1"])
}

func TestTickSkipsStatusWriteWhenHookRaced(t *testing.T) {
	// classify would downgrade this stale "working" session to idle, but a hook
	// changed the status since the snapshot — the CAS misses and the poller must
	// neither record a change nor fire OnChange.
	d := &stubDeps{
		sessions: []*store.Session{{
			ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking,
			UpdatedAt: time.Now().Add(-10 * time.Minute), LastPaneExcerpt: "quiet",
		}},
		alive:   map[string]bool{"A-1": true},
		panes:   map[string]string{"A-1": "quiet"}, // unchanged pane
		updates: map[string]store.Status{},
		casFail: map[string]bool{"A-1": true},
	}
	p := New(d, 5*time.Minute)
	called := 0
	p.OnChange = func() { called++ }
	require.NoError(t, p.tick(context.Background()))
	_, wrote := d.updates["A-1"]
	require.False(t, wrote, "a lost CAS must not be treated as a status change")
	require.Equal(t, store.StatusWorking, d.lastExpected["A-1"], "CAS expected arg is the snapshot status")
	require.Equal(t, 0, called, "no OnChange when nothing actually changed")
}

func TestTickPrunesDepartedSessionsFromSummaryState(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking, LastPaneExcerpt: "old"}},
		alive:    map[string]bool{"A-1": true},
		panes:    map[string]string{"A-1": "changed"},
		updates:  map[string]store.Status{},
		summary:  "x",
	}
	p := New(d, 5*time.Minute)
	p.SummarizeAfter = 0
	require.NoError(t, p.tick(context.Background()))
	p.wg.Wait()

	// A session that has since been archived leaves a stale throttle entry.
	p.lastSummary["GONE-9"] = time.Now()
	require.NoError(t, p.tick(context.Background()))
	p.wg.Wait()

	_, gone := p.lastSummary["GONE-9"]
	require.False(t, gone, "throttle entry for a departed session must be pruned")
	_, live := p.lastSummary["A-1"]
	require.True(t, live, "throttle entry for a live session is retained")
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
	p.wg.Wait() // summarization runs in a background worker
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
	p.wg.Wait()
	require.Equal(t, 0, d.summarizeN, "no summary when pane didn't change")
}

func TestTickFiresOnTransition(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
		alive:    map[string]bool{"A-1": false}, // dead → orphaned
		panes:    map[string]string{},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	var gotFrom, gotTo store.Status
	var gotID string
	n := 0
	p.OnTransition = func(s *store.Session, from, to store.Status) {
		gotID, gotFrom, gotTo, n = s.ID, from, to, n+1
	}
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 1, n, "fired once on the transition")
	require.Equal(t, "A-1", gotID)
	require.Equal(t, store.StatusWorking, gotFrom)
	require.Equal(t, store.StatusOrphaned, gotTo)
}

func TestTickNoTransitionForTerminalStatus(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusDone}},
		alive:    map[string]bool{"A-1": false},
		panes:    map[string]string{},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	fired := false
	p.OnTransition = func(*store.Session, store.Status, store.Status) { fired = true }
	require.NoError(t, p.tick(context.Background()))
	require.False(t, fired, "terminal status is skipped → no transition")
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
	p.SummarizeAfter = time.Hour                     // not due (lastSummary set on first call)
	require.NoError(t, p.tick(context.Background())) // first: due (zero time) → summarizes
	p.wg.Wait()                                      // let the first worker finish before re-ticking
	d.panes["A-1"] = "changed again"
	require.NoError(t, p.tick(context.Background())) // second: within the hour → throttled
	p.wg.Wait()
	require.Equal(t, 1, d.summarizeN, "throttled to one within the interval")
}
