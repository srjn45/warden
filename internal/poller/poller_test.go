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
	got := classify(s, "Thinking… (esc to interrupt)", true, 5*time.Minute)
	require.Equal(t, store.StatusWorking, got)
}

func TestHeuristicOrphanedWhenSessionGone(t *testing.T) {
	s := &store.Session{Status: store.StatusWorking}
	got := classify(s, "", false, 5*time.Minute) // sessionAlive=false
	require.Equal(t, store.StatusOrphaned, got)
}

func TestHeuristicConfirmsWaitingAfterThreshold(t *testing.T) {
	s := &store.Session{
		Status:    store.StatusWaitingForInput,
		UpdatedAt: time.Now().Add(-10 * time.Minute),
		Events:    []store.Event{{Type: "Notification", TS: time.Now().Add(-10 * time.Minute)}},
	}
	got := classify(s, "Do you want to proceed? ❯ 1. Yes", true, 5*time.Minute)
	require.Equal(t, store.StatusWaitingForInput, got)
}

func TestHeuristicKeepsHookStatusWhenNothingConclusive(t *testing.T) {
	s := &store.Session{Status: store.StatusIdle}
	got := classify(s, "some neutral pane text", true, 5*time.Minute)
	require.Equal(t, store.StatusIdle, got) // unchanged
}

// stubDeps fakes everything the poller touches.
type stubDeps struct {
	sessions []*store.Session
	alive    map[string]bool
	panes    map[string]string
	updates  map[string]store.Status
}

func (d *stubDeps) List(_ context.Context) ([]*store.Session, error) { return d.sessions, nil }
func (d *stubDeps) UpdateStatus(_ context.Context, id string, st store.Status) error {
	d.updates[id] = st
	return nil
}
func (d *stubDeps) UpdatePane(_ context.Context, id, ex string) error { return nil }
func (d *stubDeps) SessionAlive(_ context.Context, name string) bool  { return d.alive[name] }
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
