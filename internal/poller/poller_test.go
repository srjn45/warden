package poller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// allowAllPolicy is an enabled policy whose single empty allow rule matches every
// non-destructive recognized prompt — the "approve everything" posture used by the
// worker/end-to-end fixtures that predate the allow/deny engine.
func allowAllPolicy() approval.Policy {
	return approval.Policy{
		Enabled: true,
		Rules:   approval.Rules{Allow: []approval.Rule{{}}},
	}
}

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

	exitCodes   map[string]int          // id -> recorded exit code (presence = in map)
	finalized   map[string]store.Status // records FinalizeExit successful swaps
	finalCode   map[string]int          // records the code passed to FinalizeExit
	cleared     map[string]bool         // records ClearExit calls
	finalizeErr error                   // when set, FinalizeExit returns this error

	// SendKeys recording (guarded — the approval worker calls SendKeys from its
	// own goroutine while the test reads these after draining). The same mutex
	// guards events, which raiseAnomaly may write.
	sendMu    sync.Mutex
	sentKeys  map[string]string        // tmuxSession -> last keys sent
	sendKeysN int                      // total SendKeys calls
	events    map[string][]store.Event // id -> recorded anomaly events
}

func (d *stubDeps) SendKeys(_ context.Context, tmuxSession, keys string) error {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	if d.sentKeys == nil {
		d.sentKeys = map[string]string{}
	}
	d.sentKeys[tmuxSession] = keys
	d.sendKeysN++
	return nil
}

func (d *stubDeps) lastSentKey(tmuxSession string) string {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	return d.sentKeys[tmuxSession]
}

func (d *stubDeps) sendCount() int {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	return d.sendKeysN
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
func (d *stubDeps) ExitCode(_ context.Context, id string) (int, bool) {
	c, ok := d.exitCodes[id]
	return c, ok
}
func (d *stubDeps) FinalizeExit(_ context.Context, id string, expected, next store.Status, code int) (bool, error) {
	if d.finalizeErr != nil {
		return false, d.finalizeErr
	}
	if d.lastExpected == nil {
		d.lastExpected = map[string]store.Status{}
	}
	d.lastExpected[id] = expected
	if d.casFail[id] {
		return false, nil
	}
	if d.finalized == nil {
		d.finalized = map[string]store.Status{}
		d.finalCode = map[string]int{}
	}
	d.finalized[id] = next
	d.finalCode[id] = code
	return true, nil
}
func (d *stubDeps) ClearExit(_ context.Context, id string) {
	if d.cleared == nil {
		d.cleared = map[string]bool{}
	}
	d.cleared[id] = true
}
func (d *stubDeps) RecordEvent(_ context.Context, id string, ev store.Event) error {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	if d.events == nil {
		d.events = map[string][]store.Event{}
	}
	d.events[id] = append(d.events[id], ev)
	return nil
}
func (d *stubDeps) recordedEvents(id string) []store.Event {
	d.sendMu.Lock()
	defer d.sendMu.Unlock()
	return append([]store.Event(nil), d.events[id]...)
}
func (d *stubDeps) ContextTokens(_ context.Context, _ *store.Session) (int, bool) { return 0, false }
func (d *stubDeps) TranscriptUsage(_ context.Context, _ *store.Session) (int, int, bool) {
	return 0, 0, false
}
func (d *stubDeps) UpdateContext(_ context.Context, _ string, _ int, _ string) error { return nil }
func (d *stubDeps) Compact(_ context.Context, _ *store.Session) error                { return nil }
func (d *stubDeps) StampCompact(_ context.Context, _ string) error                   { return nil }
func (d *stubDeps) SessionAlive(_ context.Context, name string) bool                 { return d.alive[name] }
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

// TestTickErroredAgentReclassifiesWhenAlive covers the case where a transient
// error (e.g. rate-limit resume race) left the agent marked errored but its
// tmux session is still live — the poller should reclassify it normally.
func TestTickErroredAgentReclassifiesWhenAlive(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A", TmuxSession: "A", Status: store.StatusErrored}},
		alive:    map[string]bool{"A": true},
		panes:    map[string]string{"A": "esc to interrupt"},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	var gotTo store.Status
	p.OnTransition = func(_ *store.Session, _, to store.Status) { gotTo = to }
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, store.StatusWorking, gotTo, "errored+alive should reclassify to working")
}

// TestTickErroredAgentSkippedWhenDead verifies that an errored agent whose
// tmux session is gone is still treated as terminal (no reclassification).
func TestTickErroredAgentSkippedWhenDead(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A", TmuxSession: "A", Status: store.StatusErrored}},
		alive:    map[string]bool{"A": false},
		panes:    map[string]string{},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	fired := false
	p.OnTransition = func(*store.Session, store.Status, store.Status) { fired = true }
	require.NoError(t, p.tick(context.Background()))
	require.False(t, fired, "errored+dead session should be skipped with no transition")
}

func TestTickFinalizesFromExitFile(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		wantNext store.Status
	}{
		{"clean exit hook missed", 0, store.StatusDone},
		{"crash with code", 137, store.StatusErrored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &stubDeps{
				sessions:  []*store.Session{{ID: "A", Status: store.StatusWorking}},
				alive:     map[string]bool{"A": true},
				panes:     map[string]string{},
				updates:   map[string]store.Status{},
				exitCodes: map[string]int{"A": tc.code},
			}
			p := New(d, 5*time.Minute)
			var gotFrom, gotTo store.Status
			fired := false
			p.OnTransition = func(_ *store.Session, from, to store.Status) {
				fired = true
				gotFrom, gotTo = from, to
			}
			require.NoError(t, p.tick(context.Background()))
			require.Equal(t, tc.wantNext, d.finalized["A"])
			require.Equal(t, tc.code, d.finalCode["A"])
			require.True(t, d.cleared["A"]) // file consumed
			require.True(t, fired, "OnTransition must fire on a finalize swap")
			require.Equal(t, store.StatusWorking, gotFrom)
			require.Equal(t, tc.wantNext, gotTo)
		})
	}
}

func TestTickOrphanedOnlyWhenNoExitFile(t *testing.T) {
	// Window gone, no exit-file -> orphaned via the existing classify path.
	d := &stubDeps{
		sessions:  []*store.Session{{ID: "A", Status: store.StatusWorking}},
		alive:     map[string]bool{"A": false},
		panes:     map[string]string{},
		updates:   map[string]store.Status{},
		exitCodes: map[string]int{}, // empty -> not present
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, store.StatusOrphaned, d.updates["A"]) // via UpdateStatusIf
	require.Empty(t, d.finalized)                          // FinalizeExit not called
}

func TestTickExitFileCASLosesToHook(t *testing.T) {
	d := &stubDeps{
		sessions:  []*store.Session{{ID: "A", Status: store.StatusWorking}},
		alive:     map[string]bool{"A": true},
		panes:     map[string]string{},
		updates:   map[string]store.Status{},
		exitCodes: map[string]int{"A": 1},
		casFail:   map[string]bool{"A": true}, // hook already finalized it
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Empty(t, d.finalized["A"]) // swap lost
	require.True(t, d.cleared["A"])    // stale file still cleared
}

func TestTickExitFileErrorLeavesFile(t *testing.T) {
	d := &stubDeps{
		sessions:    []*store.Session{{ID: "A", Status: store.StatusWorking}},
		alive:       map[string]bool{"A": true},
		panes:       map[string]string{},
		updates:     map[string]store.Status{},
		exitCodes:   map[string]int{"A": 1},
		finalizeErr: errors.New("store boom"),
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background())) // tick swallows per-session errors
	require.Empty(t, d.finalized)                    // no swap recorded
	require.False(t, d.cleared["A"])                 // file LEFT for retry (not cleared on error)
}

func TestTickClearsExitFileForTerminalSession(t *testing.T) {
	// A clean exit sets done via the SessionEnd hook; the shell still wrote
	// exits/<id>=0. The poller must reap that leftover file even though it
	// skips the terminal session for classification.
	d := &stubDeps{
		sessions:  []*store.Session{{ID: "A", Status: store.StatusDone}},
		alive:     map[string]bool{},
		panes:     map[string]string{},
		updates:   map[string]store.Status{},
		exitCodes: map[string]int{},
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.True(t, d.cleared["A"]) // leftover exit-file reaped
	require.Empty(t, d.finalized)   // no finalize for an already-terminal session
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

func TestClassify_RateLimited(t *testing.T) {
	sess := &store.Session{
		ID:     "test",
		Status: store.StatusWorking,
	}

	pane := sampleLimitBanner

	got := classify(sess, pane, true, 0, 0)

	if got != store.StatusRateLimited {
		t.Errorf("classify() = %v, want %v", got, store.StatusRateLimited)
	}
}

func TestClassify_RateLimitPriority(t *testing.T) {
	// A real limit banner should take priority over prompt detection.
	sess := &store.Session{
		ID:     "test",
		Status: store.StatusWorking,
	}

	pane := sampleLimitBanner + "\n❯ Continue?"

	got := classify(sess, pane, true, 0, 0)

	if got != store.StatusRateLimited {
		t.Errorf("classify() = %v, want %v (rate limit should take priority)",
			got, store.StatusRateLimited)
	}
}

func TestClassify_WorkingVetoesStrayLimitKeyword(t *testing.T) {
	s := &store.Session{ID: "t", Status: store.StatusWorking}
	// Both a limit-ish line and the active-streaming marker present.
	pane := "discussing rate limit handling...\nesc to interrupt"
	got := classify(s, pane, true, 0, 0)
	require.Equal(t, store.StatusWorking, got, "esc to interrupt must win")
}

func TestClassify_RealLimitWhenNotStreaming(t *testing.T) {
	s := &store.Session{ID: "t", Status: store.StatusWorking}
	got := classify(s, sampleLimitBanner, true, 0, 0)
	require.Equal(t, store.StatusRateLimited, got)
}

func TestClassify_NoRateLimit(t *testing.T) {
	sess := &store.Session{
		ID:     "test",
		Status: store.StatusWorking,
	}

	pane := "Working on your request..."

	got := classify(sess, pane, true, 0, 0)

	if got == store.StatusRateLimited {
		t.Error("classify() should not return rate_limited for normal output")
	}
}

func TestPollerApprovalEventChannelInitialized(t *testing.T) {
	p := New(&stubDeps{}, 30*time.Second)
	require.NotNil(t, p.ApprovalEvents, "ApprovalEvents channel should be initialized")

	// Verify channel is buffered with capacity 100
	require.Equal(t, 100, cap(p.ApprovalEvents), "ApprovalEvents should have buffer capacity of 100")
}

func TestPublishApprovalEventNonBlocking(t *testing.T) {
	p := New(&stubDeps{}, 30*time.Second)
	sess := &store.Session{ID: "agent-123", Status: store.StatusWaitingForInput}
	pane := "some pane content"

	// Publish should succeed
	p.publishApprovalEvent(sess, pane)

	// Verify event was queued
	select {
	case event := <-p.ApprovalEvents:
		require.Equal(t, "agent-123", event.Session.ID)
		require.Equal(t, pane, event.Pane)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected event to be published")
	}
}

func TestPublishApprovalEventDropsWhenFull(t *testing.T) {
	p := New(&stubDeps{}, 30*time.Second)
	sess := &store.Session{ID: "agent-123", Status: store.StatusWaitingForInput}

	// Fill the channel to capacity
	for i := 0; i < 100; i++ {
		p.ApprovalEvents <- ApprovalEvent{Session: sess, Pane: "fill"}
	}

	// Capture log output to verify drop message
	buf := captureSlog(t)

	// Publish should drop (not block)
	done := make(chan struct{})
	go func() {
		p.publishApprovalEvent(sess, "should drop")
		close(done)
	}()

	select {
	case <-done:
		// Success - didn't block
		require.Contains(t, buf.String(), "agent=agent-123")
		require.Contains(t, buf.String(), "channel full")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("publishApprovalEvent blocked when channel was full")
	}
}

func TestApprovalWorkerConsumesEvents(t *testing.T) {
	// Capture log output to verify the approval was logged
	buf := captureSlog(t)

	d := &stubDeps{}
	p := New(d, 30*time.Second)
	p.AutoApprovePolicy = allowAllPolicy() // worker should actually approve, not just log

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start worker
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.runApprovalWorker(ctx)
	}()

	// Publish an event
	sess := &store.Session{ID: "agent-123", Status: store.StatusWaitingForInput, TmuxSession: "tmux-123"}
	pane := "Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	p.ApprovalEvents <- ApprovalEvent{Session: sess, Pane: pane}

	// Give worker time to process
	time.Sleep(50 * time.Millisecond)

	// Stop worker before reading shared state to avoid race
	cancel()
	wg.Wait()

	// Verify the worker actually auto-approved by sending option 1 to the pane.
	require.Equal(t, 1, d.sendCount(), "worker should send exactly one key")
	require.Equal(t, "1", d.lastSentKey("tmux-123"), "worker should send option 1")
	require.Contains(t, buf.String(), "auto-approved")
	require.Contains(t, buf.String(), "agent=agent-123")
}

// captureSlog redirects the slog default logger to an in-memory buffer for the
// duration of the test, restoring the previous default on cleanup. Replaces the
// old log.SetOutput capture now that the poller logs through slog.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestApprovalWorkerStopsOnContextCancel(t *testing.T) {
	p := New(&stubDeps{}, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.runApprovalWorker(ctx)
		close(done)
	}()

	// Cancel context
	cancel()

	// Verify worker stops
	select {
	case <-done:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("worker did not stop after context cancellation")
	}
}

func TestPublishEventOnStatusTransitionToWaitingForInput(t *testing.T) {
	pane := "Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	d := &stubDeps{
		sessions: []*store.Session{
			{ID: "agent-123", Status: store.StatusWorking, TmuxSession: "tmux-123", UpdatedAt: time.Now(), LastPaneExcerpt: "old"},
		},
		alive:   map[string]bool{"tmux-123": true},
		panes:   map[string]string{"tmux-123": pane},
		updates: map[string]store.Status{},
	}

	p := New(d, 30*time.Second)

	ctx := context.Background()

	// Tick should detect transition and publish event
	err := p.tick(ctx)
	require.NoError(t, err)

	// Verify event was published
	select {
	case event := <-p.ApprovalEvents:
		require.Equal(t, "agent-123", event.Session.ID)
		require.Contains(t, event.Pane, "Do you want to proceed")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected approval event to be published on status transition")
	}
}

func TestPublishEventOnPaneChangeWhileWaiting(t *testing.T) {
	firstPrompt := "Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	secondPrompt := "Another prompt?\n ❯ 1. Yes\n   2. No"

	d := &stubDeps{
		sessions: []*store.Session{
			{
				ID:              "agent-123",
				Status:          store.StatusWaitingForInput,
				TmuxSession:     "tmux-123",
				UpdatedAt:       time.Now(),
				LastPaneExcerpt: lastLines(firstPrompt, 20),
			},
		},
		alive:       map[string]bool{"tmux-123": true},
		panes:       map[string]string{"tmux-123": firstPrompt},
		paneUpdates: map[string]string{},
	}

	p := New(d, 30*time.Second)

	ctx := context.Background()

	// First tick: already waiting with first prompt, no pane change
	err := p.tick(ctx)
	require.NoError(t, err)

	// Drain any initial event from the channel
	select {
	case <-p.ApprovalEvents:
	default:
	}

	// Simulate pane changes to show second prompt (still waiting_for_input)
	d.panes["tmux-123"] = secondPrompt

	// Tick should detect pane change and publish event
	err = p.tick(ctx)
	require.NoError(t, err)

	// Verify event was published
	select {
	case event := <-p.ApprovalEvents:
		require.Equal(t, "agent-123", event.Session.ID)
		require.Contains(t, event.Pane, "Another prompt")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected approval event on pane change while waiting_for_input")
	}
}

func TestNoEventOnPaneChangeWhenNotWaiting(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{
			{
				ID:              "agent-123",
				Status:          store.StatusWorking, // NOT waiting
				TmuxSession:     "tmux-123",
				UpdatedAt:       time.Now(),
				LastPaneExcerpt: "old output",
			},
		},
		alive: map[string]bool{"tmux-123": true},
		panes: map[string]string{"tmux-123": "old output"},
	}

	p := New(d, 30*time.Second)
	ctx := context.Background()

	// First tick
	err := p.tick(ctx)
	require.NoError(t, err)

	// Change pane content
	d.panes["tmux-123"] = "new output"

	// Second tick
	err = p.tick(ctx)
	require.NoError(t, err)

	// Verify NO event published (not waiting_for_input)
	select {
	case <-p.ApprovalEvents:
		t.Fatal("should not publish event when status is not waiting_for_input")
	case <-time.After(50 * time.Millisecond):
		// Success - no event
	}
}

func TestTryAutoApproveAffirmativeSelection(t *testing.T) {
	const stickyFirst = "Do you want to proceed?\n" +
		" ❯ 1. Yes, and don't ask again for Bash commands\n" +
		"   2. Yes\n" +
		"   3. No"
	const destructive = "Bash(rm -rf build)\n" +
		"Do you want to proceed?\n" +
		" ❯ 1. Yes\n" +
		"   2. No"
	const stickyOnly = "Do you want to proceed?\n" +
		" ❯ 1. Yes, allow always\n" +
		"   2. No, keep asking"
	const noAffirmative = "Do you want to proceed?\n" +
		" ❯ 1. No\n" +
		"   2. Cancel"
	const plainYesNo = "Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	const destructiveAllowMatch = "Bash(git push --force origin main)\n" +
		"Do you want to proceed?\n" +
		" ❯ 1. Yes\n" +
		"   2. No"

	// allow is the allow-all posture; deny adds a rule that matches the benign
	// "Do you want to proceed?" question (deny wins over allow).
	withSticky := func(p approval.Policy, sticky bool) approval.Policy {
		p.AllowSticky = sticky
		return p
	}

	cases := []struct {
		name       string
		pane       string
		policy     approval.Policy
		perSession bool
		wantSends  int
		wantKey    string // checked only when wantSends > 0
	}{
		// allow + non-destructive + non-sticky affirmative -> press least-privilege yes.
		{name: "sticky-first picks least-privilege yes", pane: stickyFirst, policy: allowAllPolicy(), wantSends: 1, wantKey: "2"},
		// Destructive guard runs before Decide: an allow rule that WOULD match can't un-block it.
		{name: "destructive is blocked", pane: destructive, policy: allowAllPolicy(), wantSends: 0},
		{name: "destructive still wins over a matching allow rule", pane: destructiveAllowMatch,
			policy: approval.Policy{Enabled: true, Rules: approval.Rules{Allow: []approval.Rule{{Tool: "Bash"}}}}, wantSends: 0},
		// Policy deny match -> nothing, even with an allow rule present.
		{name: "policy deny match abstains", pane: plainYesNo,
			policy: approval.Policy{Enabled: true, Rules: approval.Rules{
				Allow: []approval.Rule{{}},
				Deny:  []approval.Rule{{Pattern: "proceed"}},
			}}, wantSends: 0},
		// Enabled but empty allow -> approve nothing (fail-safe).
		{name: "empty allow approves nothing", pane: plainYesNo, policy: approval.Policy{Enabled: true}, wantSends: 0},
		// Per-session opt-in: a disabled policy still supplies the allow rules.
		{name: "per-session opt-in participates in global rules", pane: plainYesNo,
			policy:     approval.Policy{Enabled: false, Rules: approval.Rules{Allow: []approval.Rule{{}}}},
			perSession: true, wantSends: 1, wantKey: "1"},
		{name: "sticky-only abstains when allow_sticky off", pane: stickyOnly, policy: withSticky(allowAllPolicy(), false), wantSends: 0},
		{name: "sticky-only accepted when allow_sticky on", pane: stickyOnly, policy: withSticky(allowAllPolicy(), true), wantSends: 1, wantKey: "1"},
		{name: "no affirmative abstains", pane: noAffirmative, policy: allowAllPolicy(), wantSends: 0},
		{name: "gate off sends nothing", pane: plainYesNo, policy: approval.Policy{Enabled: false}, perSession: false, wantSends: 0},
		{name: "unrecognized prompt sends nothing", pane: "just some neutral text", policy: allowAllPolicy(), wantSends: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &stubDeps{}
			p := New(d, 30*time.Second)
			p.AutoApprovePolicy = tc.policy
			s := &store.Session{ID: "agent-1", TmuxSession: "tmux-1", AutoApprove: tc.perSession}

			p.tryAutoApprove(context.Background(), s, tc.pane)

			require.Equal(t, tc.wantSends, d.sendCount())
			if tc.wantSends > 0 {
				require.Equal(t, tc.wantKey, d.lastSentKey("tmux-1"))
			}
		})
	}
}

func TestAutoApprovalEndToEnd(t *testing.T) {
	// Scenario: Agent shows first prompt (status transition), gets auto-approved,
	// then shows second prompt (pane change, no status transition), gets auto-approved.
	// This test validates the bug fix where the second prompt would NOT trigger
	// auto-approval in the old implementation.

	firstPrompt := "First prompt\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	secondPrompt := "Second prompt\nDo you want to continue?\n ❯ 1. Yes\n   2. No"

	d := &stubDeps{
		sessions: []*store.Session{
			{
				ID:          "agent-123",
				Status:      store.StatusWorking,
				TmuxSession: "tmux-123",
				UpdatedAt:   time.Now(),
			},
		},
		alive:       map[string]bool{"tmux-123": true},
		panes:       map[string]string{"tmux-123": "working..."},
		updates:     map[string]store.Status{},
		paneUpdates: map[string]string{},
	}

	p := New(d, 30*time.Second)
	p.AutoApprovePolicy = allowAllPolicy() // both prompts should be auto-approved end-to-end

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track events consumed by the worker
	eventsConsumed := make(chan string, 10)

	// Start custom worker that tracks events
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-p.ApprovalEvents:
				// Record which prompt triggered the event
				eventsConsumed <- event.Session.ID
				// Simulate auto-approval processing
				p.tryAutoApprove(ctx, event.Session, event.Pane)
			}
		}
	}()

	// === First prompt: status transition to waiting_for_input ===

	// Simulate first prompt appearing
	d.panes["tmux-123"] = firstPrompt

	// Tick should transition to waiting_for_input and publish event (Task 4)
	err := p.tick(ctx)
	require.NoError(t, err)

	// Check that status transition happened
	require.Equal(t, store.StatusWaitingForInput, d.updates["agent-123"],
		"status should have transitioned to waiting_for_input")

	// Verify first event was consumed
	select {
	case id := <-eventsConsumed:
		require.Equal(t, "agent-123", id, "first prompt should trigger event")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first prompt event was not consumed")
	}

	// Update session to reflect the transition (simulate next tick's snapshot)
	d.sessions[0].Status = store.StatusWaitingForInput
	d.sessions[0].LastPaneExcerpt = lastLines(firstPrompt, 20)

	// === Second prompt: pane change while already waiting_for_input ===

	// Pane changes to show second prompt (THIS IS THE BUG FIX VALIDATION)
	d.panes["tmux-123"] = secondPrompt

	// Tick should detect pane change and publish event (Task 5 fix)
	// NO status change - status stays waiting_for_input
	err = p.tick(ctx)
	require.NoError(t, err)

	// Verify second event was consumed (validates the bug fix)
	select {
	case id := <-eventsConsumed:
		require.Equal(t, "agent-123", id, "second prompt should trigger event (bug fix validation)")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second prompt event was not consumed - THIS IS THE BUG being tested")
	}

	// Verify status did NOT change (still waiting_for_input)
	require.Equal(t, store.StatusWaitingForInput, d.sessions[0].Status,
		"status should remain waiting_for_input on second prompt")

	// Verify no more events are pending
	select {
	case <-eventsConsumed:
		t.Fatal("unexpected extra event")
	case <-time.After(50 * time.Millisecond):
		// Expected - no more events
	}

	// Stop worker
	cancel()
	p.wg.Wait()

	// Both prompts must have been auto-approved (option 1 sent each time). This
	// is the real bug-fix validation: the second prompt (pane change, no status
	// transition) was previously never approved.
	require.Equal(t, 2, d.sendCount(), "both prompts should be auto-approved")
	require.Equal(t, "1", d.lastSentKey("tmux-123"), "auto-approval should send option 1")
}
