package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a test double for the tui api interface.
type fakeAPI struct {
	sessions   []*store.Session
	listErr    error
	output     string
	spawned    *client.SpawnParams
	terminated string
	termErr    error
	deleted    string
	deleteErr  error
	sentTo     string
	sentText   string
	listedDir  string
	dirListing client.DirListing
	dirListErr error
}

func (f *fakeAPI) List(context.Context) ([]*store.Session, error) { return f.sessions, f.listErr }
func (f *fakeAPI) Output(_ context.Context, _ string, _ int) (string, error) {
	return f.output, nil
}
func (f *fakeAPI) Spawn(_ context.Context, p client.SpawnParams) (*store.Session, error) {
	f.spawned = &p
	return &store.Session{ID: "agent-new"}, nil
}
func (f *fakeAPI) Terminate(_ context.Context, id string) error {
	f.terminated = id
	return f.termErr
}
func (f *fakeAPI) Delete(_ context.Context, id string, _ bool) error {
	f.deleted = id
	return f.deleteErr
}
func (f *fakeAPI) Input(_ context.Context, id, text string) error {
	f.sentTo, f.sentText = id, text
	return nil
}
func (f *fakeAPI) ListDirs(_ context.Context, path string) (client.DirListing, error) {
	f.listedDir = path
	return f.dirListing, f.dirListErr
}

// step applies a msg and returns the updated concrete Model.
func step(m Model, msg tea.Msg) Model {
	nm, _ := m.Update(msg)
	return nm.(Model)
}

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "\t":
		return tea.KeyMsg{Type: tea.KeyTab}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func threeSessions() []*store.Session {
	return []*store.Session{
		{ID: "a", Status: store.StatusWorking},
		{ID: "b", Status: store.StatusIdle},
		{ID: "c", Status: store.StatusWaitingForInput},
	}
}

func TestCursorMovesAndClamps(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.Equal(t, 0, m.cursor)
	m = step(m, key("down"))
	m = step(m, key("down"))
	require.Equal(t, 2, m.cursor)
	m = step(m, key("down")) // clamp at last
	require.Equal(t, 2, m.cursor)
	m = step(m, key("up"))
	require.Equal(t, 1, m.cursor)
	require.Equal(t, "b", m.selectedID())
}

func TestSessionsMsgRepinsByID(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	m = step(m, key("down")) // cursor=1 → "b"
	// New snapshot reorders: b is now first. Selection should follow id "b".
	m = step(m, sessionsMsg{sessions: []*store.Session{
		{ID: "b", Status: store.StatusIdle},
		{ID: "a", Status: store.StatusWorking},
	}})
	require.Equal(t, "b", m.selectedID())
}

func TestListErrorSetsDisconnected(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{err: client.ErrDaemonDown})
	require.False(t, m.connected)
}

func TestQuitKey(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(key("q"))
	require.NotNil(t, cmd, "q should return a command (tea.Quit)")
}

func TestViewDoesNotPanic(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.NotEmpty(t, m.View())
}

func TestOutputMsgFillsSelected(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // cursor=0 → "a"
	m = step(m, outputMsg{id: "a", text: "hello output"})
	require.Equal(t, "hello output", m.output)
}

func TestOutputMsgIgnoresStaleID(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, outputMsg{id: "a", text: "for a"})
	m = step(m, outputMsg{id: "c", text: "stale"}) // not selected
	require.Equal(t, "for a", m.output)
}

func TestTabFocusesOutput(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.False(t, m.outputFocused)
	m = step(m, key("\t")) // tab
	require.True(t, m.outputFocused)
}

func TestNewAgentModeFlow(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	// type a prompt
	m = step(m, key("research SSE"))
	// submit with ctrl+s
	m, _ = submit(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, f.spawned)
	require.Equal(t, "research SSE", f.spawned.Prompt)
}

func TestNewAgentEscCancels(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	m = step(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestSpawnDoneSelectsNewAgent(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "agent-new"}, {ID: "x"}}})
	m = step(m, spawnDoneMsg{id: "agent-new"})
	require.Equal(t, "agent-new", m.pendingSelect)
	// next list refresh pins it
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "x"}, {ID: "agent-new"}}})
	require.Equal(t, "agent-new", m.selectedID())
}

func TestSessionsMsgGroupsBySourceDir(t *testing.T) {
	now := time.Now()
	m := step(New(&fakeAPI{}), sessionsMsg{sessions: []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
	}})
	ids := []string{m.sessions[0].ID, m.sessions[1].ID, m.sessions[2].ID}
	require.Equal(t, []string{"b1", "b2", "a1"}, ids, "classic model stores grouped order")
}

// submit is a test helper: applies one key, runs the resulting command (so
// fake-api side effects like Spawn are recorded), and returns model + cmd.
func submit(m Model, msg tea.Msg) (Model, tea.Cmd) {
	nm, cmd := m.Update(msg)
	if cmd != nil {
		cmd()
	}
	return nm.(Model), cmd
}

func TestSendMessageFlow(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("s"))
	require.Equal(t, modeSendMsg, m.mode)
	m = step(m, key("hello"))
	m, _ = submit(m, key("enter"))
	require.Equal(t, "a", f.sentTo)
	require.Equal(t, "hello", f.sentText)
	require.Equal(t, modeNormal, m.mode)
}

func TestKillConfirmTerminatesAndDeletes(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("x"))
	require.Equal(t, modeConfirmKill, m.mode)
	m, _ = submit(m, key("y")) // confirm → kill & remove
	require.Equal(t, "a", f.terminated)
	require.Equal(t, "a", f.deleted, "kill also removes the record from the list")
}

// An already-dead agent (terminate errors) must still be removed: the terminate
// step is best-effort, so the record is deleted regardless.
func TestKillRemovesEvenWhenTerminateFails(t *testing.T) {
	f := &fakeAPI{termErr: context.DeadlineExceeded}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("x"))
	m, _ = submit(m, key("y"))
	require.Equal(t, "a", f.deleted, "delete runs even though terminate failed")
}

func TestKillEscCancels(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	m = step(m, key("x"))
	m = step(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestHelpToggle(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("?"))
	require.Equal(t, modeHelp, m.mode)
	m = step(m, key("?")) // any key closes
	require.Equal(t, modeNormal, m.mode)
}

func TestAttachDoneShowsError(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, attachDoneMsg{err: context.DeadlineExceeded})
	require.Contains(t, m.status, "attach")
}

func TestAttachNoOpWhenNoSelection(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(key("a"))
	require.Nil(t, cmd, "attach with no selection does nothing")
}

func TestViewDoesNotOverflowHeight(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	for i := 0; i < 40; i++ {
		m.sessions = append(m.sessions, &store.Session{ID: fmt.Sprintf("a-%02d", i), Status: store.StatusWorking})
	}
	out := m.View()
	require.LessOrEqual(t, strings.Count(out, "\n")+1, 30, "View must not exceed terminal height")
}
