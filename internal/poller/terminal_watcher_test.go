package poller

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// twDeps is a minimal TerminalDeps mock for TerminalWatcher unit tests.
// No real tmux is required.
type twDeps struct {
	sessions  []*store.Session
	alive     map[string]bool
	panes     map[string]string
	exitCodes map[string]int

	paneUpdates  map[string]string
	statusSwaps  []twStatusSwap
	clearedExits []string
}

type twStatusSwap struct {
	id       string
	from, to store.Status
	accepted bool
}

func newTWDeps() *twDeps {
	return &twDeps{
		alive:       make(map[string]bool),
		panes:       make(map[string]string),
		exitCodes:   make(map[string]int),
		paneUpdates: make(map[string]string),
	}
}

func (d *twDeps) List(_ context.Context) ([]*store.Session, error) { return d.sessions, nil }

func (d *twDeps) SessionAlive(_ context.Context, name string) bool { return d.alive[name] }

func (d *twDeps) CapturePane(_ context.Context, name string) (string, error) {
	return d.panes[name], nil
}

func (d *twDeps) UpdateStatusIf(_ context.Context, id string, from, to store.Status) (bool, error) {
	for _, s := range d.sessions {
		if s.ID == id && s.Status == from {
			s.Status = to
			d.statusSwaps = append(d.statusSwaps, twStatusSwap{id: id, from: from, to: to, accepted: true})
			return true, nil
		}
	}
	d.statusSwaps = append(d.statusSwaps, twStatusSwap{id: id, from: from, to: to, accepted: false})
	return false, nil
}

func (d *twDeps) UpdatePane(_ context.Context, id, excerpt string) error {
	d.paneUpdates[id] = excerpt
	return nil
}

func (d *twDeps) ExitCode(_ context.Context, id string) (int, bool) {
	if code, ok := d.exitCodes[id]; ok {
		return code, true
	}
	return 0, false
}

func (d *twDeps) ClearExit(_ context.Context, id string) {
	d.clearedExits = append(d.clearedExits, id)
}

func (d *twDeps) Restore(_ context.Context, _ *store.Session) error { return nil }

// TestTerminalWatcherTick verifies the three core TerminalWatcher tick behaviours.
func TestTerminalWatcherTick(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			name: "alive terminal: pane updated on change",
			run: func(t *testing.T) {
				d := newTWDeps()
				sess := &store.Session{
					ID:              "T-1",
					TmuxSession:     "t-1",
					Kind:            store.KindTerminal,
					Status:          store.StatusWorking,
					LastPaneExcerpt: "old output",
				}
				d.sessions = []*store.Session{sess}
				d.alive["t-1"] = true
				d.panes["t-1"] = "new output"

				changes := 0
				var transitions []struct{ from, to store.Status }
				w := NewTerminalWatcher(d)
				w.OnChange = func() { changes++ }
				w.OnTransition = func(_ *store.Session, from, to store.Status) {
					transitions = append(transitions, struct{ from, to store.Status }{from, to})
				}

				w.tick(ctx)

				require.Equal(t, "new output", d.paneUpdates["T-1"], "pane excerpt updated")
				require.Equal(t, 1, changes, "OnChange fires on pane update")
				require.Empty(t, transitions, "no status transition for alive terminal")
				require.Equal(t, store.StatusWorking, sess.Status, "status unchanged")
			},
		},
		{
			name: "dead terminal: transitions to orphaned",
			run: func(t *testing.T) {
				d := newTWDeps()
				sess := &store.Session{
					ID:          "T-2",
					TmuxSession: "t-2",
					Kind:        store.KindTerminal,
					Status:      store.StatusWorking,
				}
				d.sessions = []*store.Session{sess}
				d.alive["t-2"] = false

				changes := 0
				type transition struct {
					sess     *store.Session
					from, to store.Status
				}
				var transitions []transition
				w := NewTerminalWatcher(d)
				w.OnChange = func() { changes++ }
				w.OnTransition = func(s *store.Session, from, to store.Status) {
					transitions = append(transitions, transition{s, from, to})
				}

				w.tick(ctx)

				require.Equal(t, 1, changes, "OnChange fires on orphan transition")
				require.Len(t, transitions, 1, "exactly one transition fired")
				require.Equal(t, store.StatusWorking, transitions[0].from)
				require.Equal(t, store.StatusOrphaned, transitions[0].to)
				require.Same(t, sess, transitions[0].sess, "OnTransition receives the session pointer")
				require.Equal(t, store.StatusOrphaned, sess.Status, "session status updated in store")
			},
		},
		{
			name: "already-orphaned terminal: no double-transition",
			run: func(t *testing.T) {
				d := newTWDeps()
				sess := &store.Session{
					ID:          "T-3",
					TmuxSession: "t-3",
					Kind:        store.KindTerminal,
					Status:      store.StatusOrphaned,
				}
				d.sessions = []*store.Session{sess}
				d.alive["t-3"] = false

				changes := 0
				transitions := 0
				w := NewTerminalWatcher(d)
				w.OnChange = func() { changes++ }
				w.OnTransition = func(_ *store.Session, _, _ store.Status) { transitions++ }

				w.tick(ctx)

				require.Equal(t, 0, changes, "OnChange must not fire for already-orphaned terminal")
				require.Equal(t, 0, transitions, "OnTransition must not fire for already-orphaned terminal")
				require.Empty(t, d.statusSwaps, "UpdateStatusIf must not be called")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, tc.run)
	}
}
