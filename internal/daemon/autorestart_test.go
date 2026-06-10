package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/store"
)

func TestDecideRestart(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	const max = 3
	const reset = 5 * time.Minute

	// First-ever crash: no prior restart -> restart, count 1.
	act, next := decideRestart(0, time.Time{}, now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Recent restart, below cap -> restart, count+1.
	act, next = decideRestart(1, now.Add(-30*time.Second), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 2, next)

	// At cap, recent -> give up.
	act, _ = decideRestart(3, now.Add(-30*time.Second), now, max, reset)
	require.Equal(t, actionGiveUp, act)

	// At cap but sustained-healthy (>= reset since last) -> reset -> restart, count 1.
	act, next = decideRestart(3, now.Add(-6*time.Minute), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Boundary: exactly reset elapsed -> resets.
	act, next = decideRestart(3, now.Add(-5*time.Minute), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Boundary: just under reset, at cap -> give up.
	act, _ = decideRestart(3, now.Add(-(5*time.Minute - time.Second)), now, max, reset)
	require.Equal(t, actionGiveUp, act)
}

func tp(t time.Time) *time.Time { return &t }

// fakeRestartLife records Terminate/Restore; restoreErr forces a Restore failure.
type fakeRestartLife struct {
	Lifecycle  // embed: unused interface methods stay nil (never called here)
	terminated []string
	restored   []string
	restoreErr error
}

func (f *fakeRestartLife) Terminate(_ context.Context, tmux string) error {
	f.terminated = append(f.terminated, tmux)
	return nil
}
func (f *fakeRestartLife) Restore(_ context.Context, sess *store.Session) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.restored = append(f.restored, sess.ID)
	return nil
}

// restartStore records SetRestart/AppendEvent/UpdateStatus; minimal store fake.
type restartStore struct {
	store.Store
	count  int
	at     time.Time
	events []string
	status store.Status
}

func (s *restartStore) SetRestart(_ context.Context, _ string, count int, at time.Time) error {
	s.count, s.at = count, at
	return nil
}
func (s *restartStore) AppendEvent(_ context.Context, _ string, ev store.Event) error {
	s.events = append(s.events, ev.Detail)
	return nil
}
func (s *restartStore) UpdateStatusIf(_ context.Context, _ string, _, next store.Status) (bool, error) {
	s.status = next
	return true, nil
}

func newTestRestarter(life Lifecycle, st store.Store) *Restarter {
	return &Restarter{life: life, store: st, max: 3, reset: 5 * time.Minute}
}

func TestRestarterIgnoresNonQualifying(t *testing.T) {
	life, st := &fakeRestartLife{}, &restartStore{}
	r := newTestRestarter(life, st)
	now := time.Now()
	r.onTransitionAt(&store.Session{ID: "a", AutoRestart: true}, store.StatusWorking, store.StatusIdle, now)                      // not errored
	r.onTransitionAt(&store.Session{ID: "b"}, store.StatusWorking, store.StatusErrored, now)                                      // flag off
	r.onTransitionAt(&store.Session{ID: "c", AutoRestart: true, PipelineID: "p1"}, store.StatusWorking, store.StatusErrored, now) // pipeline job
	require.Empty(t, life.restored)
	require.Empty(t, life.terminated)
}

func TestRestarterRestartsQualifying(t *testing.T) {
	life, st := &fakeRestartLife{}, &restartStore{}
	r := newTestRestarter(life, st)
	now := time.Now()
	sess := &store.Session{ID: "x", TmuxSession: "x", AutoRestart: true, Status: store.StatusErrored, RestartCount: 0}
	r.onTransitionAt(sess, store.StatusWorking, store.StatusErrored, now)
	require.Equal(t, []string{"x"}, life.terminated)
	require.Equal(t, []string{"x"}, life.restored)
	require.Equal(t, 1, st.count)
	require.Equal(t, now, st.at)
	require.Equal(t, store.StatusSpawning, st.status)
	require.Len(t, st.events, 1)
	require.Contains(t, st.events[0], "attempt 1/3")
}

func TestRestarterGivesUpAtCap(t *testing.T) {
	life, st := &fakeRestartLife{}, &restartStore{}
	r := newTestRestarter(life, st)
	now := time.Now()
	sess := &store.Session{ID: "x", TmuxSession: "x", AutoRestart: true, Status: store.StatusErrored,
		RestartCount: 3, LastRestartAt: tp(now.Add(-30 * time.Second))}
	r.onTransitionAt(sess, store.StatusWorking, store.StatusErrored, now)
	require.Empty(t, life.restored)
	require.Equal(t, store.Status(""), st.status) // status untouched (stays errored)
	require.Len(t, st.events, 1)
	require.Contains(t, st.events[0], "giving up after 3")
}

func TestRestarterRestoreFailureLeavesErrored(t *testing.T) {
	life := &fakeRestartLife{restoreErr: errors.New("workdir gone")}
	st := &restartStore{}
	r := newTestRestarter(life, st)
	now := time.Now()
	sess := &store.Session{ID: "x", TmuxSession: "x", AutoRestart: true, Status: store.StatusErrored}
	r.onTransitionAt(sess, store.StatusWorking, store.StatusErrored, now)
	require.Equal(t, 1, st.count)                 // attempt counted
	require.Equal(t, store.Status(""), st.status) // NOT spawning (restore failed)
	require.Len(t, st.events, 2)                  // attempt + restore-failed
	require.Contains(t, st.events[1], "restore failed")
}

func TestRestarterResetsAfterSustainedHealth(t *testing.T) {
	life, st := &fakeRestartLife{}, &restartStore{}
	r := newTestRestarter(life, st)
	now := time.Now()
	// At the cap, but the last restart was long ago (sustained healthy) -> reset to attempt 1.
	sess := &store.Session{ID: "x", TmuxSession: "x", AutoRestart: true, Status: store.StatusErrored,
		RestartCount: 3, LastRestartAt: tp(now.Add(-6 * time.Minute))}
	r.onTransitionAt(sess, store.StatusWorking, store.StatusErrored, now)
	require.Equal(t, []string{"x"}, life.restored) // restarted, not gave up
	require.Equal(t, 1, st.count)                  // counter RESET to 1, not 4
	require.Contains(t, st.events[0], "attempt 1/3")
}
