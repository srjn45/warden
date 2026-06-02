package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a test double for the tui api interface.
type fakeAPI struct {
	sessions []*store.Session
	listErr  error
	output   string
	spawned  *client.SpawnParams
	cleaned  *struct {
		id          string
		force, hard bool
	}
	cleanErr error
	sentTo   string
	sentText string
}

func (f *fakeAPI) List(context.Context) ([]*store.Session, error) { return f.sessions, f.listErr }
func (f *fakeAPI) Output(_ context.Context, _ string, _ int) (string, error) {
	return f.output, nil
}
func (f *fakeAPI) Spawn(_ context.Context, p client.SpawnParams) (*store.Session, error) {
	f.spawned = &p
	return &store.Session{ID: "agent-new"}, nil
}
func (f *fakeAPI) Cleanup(_ context.Context, id string, force, hard bool) error {
	f.cleaned = &struct {
		id          string
		force, hard bool
	}{id, force, hard}
	return f.cleanErr
}
func (f *fakeAPI) Input(_ context.Context, id, text string) error {
	f.sentTo, f.sentText = id, text
	return nil
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
