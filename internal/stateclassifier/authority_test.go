package stateclassifier

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/store"
)

// screenBackend reads the rendered screen like a real adapter: a busy marker
// means working, a bare input prompt as the last line means idle.
type screenBackend struct{ mockBackend }

func (b *screenBackend) DetectState(screen string) agentbackend.State {
	if strings.Contains(screen, "esc to interrupt") {
		return agentbackend.StateWorking
	}
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	if strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "❯") {
		return agentbackend.StateIdle
	}
	return agentbackend.StateUnknown
}

func rig(t *testing.T, rec *agentstore.Agent, b agentbackend.Backend) (*Classifier, *fakeStore, *fakeStream) {
	t.Helper()
	return rigDwell(t, rec, b, time.Hour)
}

func rigDwell(t *testing.T, rec *agentstore.Agent, b agentbackend.Backend, grace time.Duration) (*Classifier, *fakeStore, *fakeStream) {
	t.Helper()
	mgr := newFakeManager(ptyhost.ModeOn)
	st := newFakeStore()
	st.agents["sess-1"] = rec
	stream := newFakeStream("sess-1", 1)
	mgr.SetTarget("sess-1", ptyhost.Target{Name: "sess-1", Stream: stream}, true)
	cfg := DefaultConfig()
	cfg.QuiescenceGrace = grace // investigations are driven by the test
	cfg.Hysteresis.Dwell[ptyevent.StateIdle] = 0
	c := New(cfg, mgr, st, func(*agentstore.Agent) agentbackend.Backend { return b }, &mockProcs{info: ProcessInfo{RootAlive: true}}, nil)
	t.Cleanup(c.Close)
	return c, st, stream
}

func output(seq uint64, at time.Time) ptyevent.Event {
	return ptyevent.Event{Kind: ptyevent.KindOutput, Wall: at, Seq: seq, Output: &ptyevent.Output{Data: []byte("x")}}
}

// Only a record that says it is a directly hosted agent is written for: a
// legacy tmux record, an unknown-runtime one, and a same-named record without
// runtime identity stay the poller's even when the manager reports authority.
func TestStatusWrittenOnlyForDirectRecords(t *testing.T) {
	for name, rt := range map[string]*store.SessionRuntime{
		"no-runtime": nil,
		"tmux":       {Kind: "tmux"},
		"unknown":    {Kind: "unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			c, st, _ := rig(t, &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusIdle, Runtime: rt}, &mockBackend{id: "mock"})
			now := time.Now()
			c.OnPTYEvent("sess-1", ptyevent.Event{Kind: ptyevent.KindInput, Wall: now, Seq: 1, Input: &ptyevent.Input{Origin: ptyevent.OriginHuman}})
			c.OnPTYEvent("sess-1", output(2, now.Add(time.Millisecond)))
			snap, _ := c.CurrentState("sess-1")
			require.Equal(t, ptyevent.StateWorking, snap.State, "the classifier still tracks state")
			require.Equal(t, store.StatusIdle, st.agents["sess-1"].Status, "but must not write a non-direct record")
			require.Empty(t, st.updates)
		})
	}
}

// A late event for an agent already finished by the lifecycle observer must not
// resurrect it, and the process end is never written by the classifier.
func TestNeverOverwritesFinalStatusOrWritesExit(t *testing.T) {
	for _, final := range []store.Status{store.StatusDone, store.StatusErrored, store.StatusOrphaned} {
		c, st, _ := rig(t, &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: final, Runtime: directRuntime()}, &mockBackend{id: "mock"})
		now := time.Now()
		c.OnPTYEvent("sess-1", ptyevent.Event{Kind: ptyevent.KindInput, Wall: now, Seq: 1, Input: &ptyevent.Input{Origin: ptyevent.OriginHuman}})
		c.OnPTYEvent("sess-1", output(2, now.Add(time.Millisecond)))
		require.Equal(t, final, st.agents["sess-1"].Status)
	}
	c, st, _ := rig(t, &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusWorking, Runtime: directRuntime()}, &mockBackend{id: "mock"})
	exit := 1
	c.OnPTYEvent("sess-1", ptyevent.Event{Kind: ptyevent.KindLifecycle, Lifecycle: &ptyevent.Lifecycle{Phase: ptyevent.PhaseCrashed, ExitCode: &exit}})
	snap, _ := c.CurrentState("sess-1")
	require.Equal(t, ptyevent.StateExited, snap.State)
	require.Equal(t, store.StatusWorking, st.agents["sess-1"].Status, "exit status is the lifecycle observer's (it knows the exit code)")
}

// Output right after a warden/operator write is corroboration of THAT writer,
// never a phantom human at the keyboard.
func TestInputEvidenceKeepsItsOrigin(t *testing.T) {
	for _, org := range []ptyevent.Origin{ptyevent.OriginWarden, ptyevent.OriginOperator, ptyevent.OriginHuman} {
		c, _, stream := rig(t, &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusIdle, Runtime: directRuntime()}, &mockBackend{id: "mock"})
		now := time.Now()
		c.OnPTYEvent("sess-1", ptyevent.Event{Kind: ptyevent.KindInput, Wall: now, Seq: 1, Input: &ptyevent.Input{Origin: org}})
		c.OnPTYEvent("sess-1", output(2, now.Add(5*time.Millisecond)))
		c.mu.Lock()
		s := c.sessions["sess-1"]
		c.mu.Unlock()
		s.mu.Lock()
		got := s.lastInputOrg
		s.mu.Unlock()
		require.Equal(t, org, got)
		// The first-state evidence names the writer, not a phantom human.
		require.Contains(t, stream.Emits()[0].State.Evidence, ptyevent.EvInput)
	}
}

// Buffered output followed by silence is not completion: while the screen still
// shows the busy marker the agent stays working however quiet the stream got,
// and only the redrawn prompt (ANSI clear-line + prompt) completes the turn.
func TestBufferedOutputAndANSIPromptRedraw(t *testing.T) {
	b := &screenBackend{mockBackend{id: "mock"}}
	c, st, stream := rig(t, &agentstore.Agent{ID: "sess-1", TmuxSession: "sess-1", Status: store.StatusIdle, Runtime: directRuntime()}, b)
	scr := ptyruntime.NewScreen(80, 10)
	render := func(s string) { _, _ = scr.Write([]byte(s)); stream.SetScreen(scr.String()) }

	now := time.Now()
	c.OnPTYEvent("sess-1", ptyevent.Event{Kind: ptyevent.KindInput, Wall: now, Seq: 1, Input: &ptyevent.Input{Origin: ptyevent.OriginWarden}})
	// A burst arrives in pieces (buffered output): the screen shows the spinner.
	render("\x1b[2Kthinking... esc to interrupt")
	for i := uint64(2); i < 8; i++ {
		c.OnPTYEvent("sess-1", output(i, now.Add(time.Duration(i)*time.Millisecond)))
	}
	for i := 0; i < 4; i++ {
		c.Investigate(context.Background(), "sess-1") // quiet, repeatedly
	}
	snap, _ := c.CurrentState("sess-1")
	require.Equal(t, ptyevent.StateWorking, snap.State, "silence with a busy marker on screen is not completion")
	require.Equal(t, store.StatusWorking, st.agents["sess-1"].Status)

	// The TUI redraws in place: carriage return, erase line, bare prompt.
	render("\r\x1b[2K❯ ")
	for i := 0; i < 4; i++ {
		c.Investigate(context.Background(), "sess-1")
	}
	snap, _ = c.CurrentState("sess-1")
	require.Equal(t, ptyevent.StateIdle, snap.State, "the redrawn prompt is positive idle evidence")
	require.Equal(t, store.StatusIdle, st.agents["sess-1"].Status)
}
