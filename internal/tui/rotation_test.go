package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// altKey builds the tea.KeyMsg an Alt-letter produces (String() == "alt+<r>"),
// exactly what the tmux M-t/M-a/M-p bindings forward into the control pane.
func altKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true} }

// liveAgent is a plain live (non-terminal) agent session.
func liveAgent(id, workdir string) *store.Session {
	return &store.Session{ID: id, Status: store.StatusWorking, Workdir: workdir, TmuxSession: id}
}

// M-t cycles the terminal pane through the live terminals in creation order,
// wrapping, and records the newly-shown terminal.
func TestRotateTerminalCycles(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	base := time.Now()
	m.sessions = []*store.Session{
		liveTerminal("t1", "/w1", base),
		liveTerminal("t2", "/w2", base.Add(time.Second)),
		liveTerminal("t3", "/w3", base.Add(2*time.Second)),
	}
	m.openedTerminal = "t1"

	nm, cmd := m.Update(altKey('t'))
	m = nm.(controlPaneModel)
	require.Equal(t, "t2", m.openedTerminal, "M-t advances to the next terminal")
	require.NotNil(t, cmd, "rotation respawns the terminal pane")

	m = lstep(m, altKey('t'))
	require.Equal(t, "t3", m.openedTerminal)
	m = lstep(m, altKey('t'))
	require.Equal(t, "t1", m.openedTerminal, "M-t wraps back to the first terminal")
}

// M-t with nothing yet open starts at the first terminal.
func TestRotateTerminalFromNoneStartsFirst(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m.sessions = []*store.Session{liveTerminal("t1", "/w", time.Now())}
	m.openedTerminal = ""
	m = lstep(m, altKey('t'))
	require.Equal(t, "t1", m.openedTerminal)
}

// M-t in the tmux-native cockpit (no terminal pane) is a no-op with a status hint.
func TestRotateTerminalNoPane(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "")
	m.sessions = []*store.Session{liveTerminal("t1", "/w", time.Now())}
	m.openedTerminal = "t1"
	nm, cmd := m.Update(altKey('t'))
	m = nm.(controlPaneModel)
	require.Nil(t, cmd)
	require.Equal(t, "t1", m.openedTerminal, "no terminal pane ⇒ nothing rotates")
	require.Contains(t, m.status, "terminal pane")
}

// M-t with a terminal pane but no live terminals flashes a status and does nothing.
func TestRotateTerminalEmpty(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	nm, cmd := m.Update(altKey('t'))
	m = nm.(controlPaneModel)
	require.Nil(t, cmd)
	require.Contains(t, m.status, "no terminals")
}

// M-a cycles the agent pane through all live agents (keeping control focus) and
// updates the opened-agent dir so `t` still targets the right place.
func TestRotateAgentCycles(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m.sessions = []*store.Session{liveAgent("a1", "/a1"), liveAgent("a2", "/a2")}
	m.openedAgent = "a1"

	nm, cmd := m.Update(altKey('a'))
	m = nm.(controlPaneModel)
	require.Equal(t, "a2", m.openedAgent)
	require.Equal(t, "/a2", m.openedAgentDir, "rotation tracks the opened agent's dir")
	require.NotNil(t, cmd, "rotation respawns the agent pane")

	m = lstep(m, altKey('a'))
	require.Equal(t, "a1", m.openedAgent, "M-a wraps")
}

// M-a with no live agents flashes a status and does nothing.
func TestRotateAgentEmpty(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	// A lone terminal must not count as an agent.
	m.sessions = []*store.Session{liveTerminal("t1", "/w", time.Now())}
	nm, cmd := m.Update(altKey('a'))
	m = nm.(controlPaneModel)
	require.Nil(t, cmd)
	require.Contains(t, m.status, "no agents")
}

// M-p cycles the agent pane through pipeline agents only, in pipeline > job order,
// skipping standalone agents.
func TestRotatePipelineAgents(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m.sessions = []*store.Session{
		liveAgent("pa1", "/p/a1"),
		liveAgent("pa2", "/p/a2"),
		liveAgent("solo", "/solo"), // not in any pipeline → never selected by M-p
	}
	m.pipelines = []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{
		{ID: "j1", SessionID: "pa1", Status: pipeline.JobRunning},
		{ID: "j2", SessionID: "pa2", Status: pipeline.JobRunning},
	}}}

	m = lstep(m, altKey('p')) // from nothing open → first pipeline agent
	require.Equal(t, "pa1", m.openedAgent)
	m = lstep(m, altKey('p'))
	require.Equal(t, "pa2", m.openedAgent)
	m = lstep(m, altKey('p'))
	require.Equal(t, "pa1", m.openedAgent, "M-p wraps within the pipeline set, skipping the standalone agent")
}

// nextInCycle wraps and treats an unknown/absent current as "start at the first".
func TestNextInCycleWrap(t *testing.T) {
	set := []*store.Session{{ID: "x"}, {ID: "y"}, {ID: "z"}}
	require.Equal(t, "y", nextInCycle(set, "x").ID)
	require.Equal(t, "x", nextInCycle(set, "z").ID, "wraps past the end")
	require.Equal(t, "x", nextInCycle(set, "gone").ID, "absent current ⇒ first")
	require.Nil(t, nextInCycle(nil, "x"), "empty set ⇒ nil")
}

// stepInCycle walks forward (+1) and backward (-1), wrapping in both directions,
// and lands predictably when nothing is open yet (forward → first, reverse → last).
func TestStepInCycleDirection(t *testing.T) {
	set := []*store.Session{{ID: "x"}, {ID: "y"}, {ID: "z"}}
	require.Equal(t, "z", stepInCycle(set, "x", -1).ID, "reverse wraps past the start")
	require.Equal(t, "x", stepInCycle(set, "y", -1).ID, "reverse steps back one")
	require.Equal(t, "y", stepInCycle(set, "z", -1).ID)
	require.Equal(t, "z", stepInCycle(set, "gone", -1).ID, "absent current, reverse ⇒ last")
	require.Equal(t, "x", stepInCycle(set, "gone", 1).ID, "absent current, forward ⇒ first")
	require.Nil(t, stepInCycle(nil, "x", -1), "empty set ⇒ nil")
}

// Alt+Shift+t (M-T) rotates the terminal pane in reverse, wrapping past the start.
func TestRotateTerminalReverse(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	base := time.Now()
	m.sessions = []*store.Session{
		liveTerminal("t1", "/w1", base),
		liveTerminal("t2", "/w2", base.Add(time.Second)),
		liveTerminal("t3", "/w3", base.Add(2*time.Second)),
	}
	m.openedTerminal = "t1"

	m = lstep(m, altKey('T'))
	require.Equal(t, "t3", m.openedTerminal, "M-T wraps back to the last terminal")
	m = lstep(m, altKey('T'))
	require.Equal(t, "t2", m.openedTerminal, "M-T steps backward")
}

// Alt+Shift+a (M-A) rotates the agent pane in reverse.
func TestRotateAgentReverse(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m.sessions = []*store.Session{liveAgent("a1", "/a1"), liveAgent("a2", "/a2"), liveAgent("a3", "/a3")}
	m.openedAgent = "a1"

	m = lstep(m, altKey('A'))
	require.Equal(t, "a3", m.openedAgent, "M-A wraps back to the last agent")
	require.Equal(t, "/a3", m.openedAgentDir, "reverse rotation tracks the opened agent's dir")
	m = lstep(m, altKey('A'))
	require.Equal(t, "a2", m.openedAgent, "M-A steps backward")
}

// Alt+Shift+p (M-P) rotates the pipeline-agent set in reverse.
func TestRotatePipelineAgentsReverse(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9", "%1")
	m.defaultTerminalReady = true
	m.sessions = []*store.Session{liveAgent("pa1", "/p/a1"), liveAgent("pa2", "/p/a2")}
	m.pipelines = []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{
		{ID: "j1", SessionID: "pa1", Status: pipeline.JobRunning},
		{ID: "j2", SessionID: "pa2", Status: pipeline.JobRunning},
	}}}
	m.openedAgent = "pa1"

	m = lstep(m, altKey('P'))
	require.Equal(t, "pa2", m.openedAgent, "M-P wraps back within the pipeline set")
}
