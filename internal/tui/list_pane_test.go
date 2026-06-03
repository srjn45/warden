package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func lstep(m listPaneModel, msg tea.Msg) listPaneModel {
	nm, _ := m.Update(msg)
	return nm.(listPaneModel)
}

func TestListPaneWritesSelectionOnLoad(t *testing.T) {
	dir := t.TempDir()
	m := newListPane(&fakeAPI{}, dir)
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a"}, {ID: "b"}}})
	require.Equal(t, "a", readSelection(dir), "first session selected and written")
}

func TestListPaneWritesSelectionOnMove(t *testing.T) {
	dir := t.TempDir()
	m := newListPane(&fakeAPI{}, dir)
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a"}, {ID: "b"}}})
	m = lstep(m, key("down")) // move cursor to "b"
	require.Equal(t, "b", readSelection(dir))
}

func TestListPaneGroupsBySourceDir(t *testing.T) {
	now := time.Now()
	m := newListPane(&fakeAPI{}, t.TempDir())
	m = lstep(m, sessionsMsg{sessions: []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
	}})
	ids := []string{m.sessions[0].ID, m.sessions[1].ID, m.sessions[2].ID}
	require.Equal(t, []string{"b1", "b2", "a1"}, ids, "cockpit list pane stores grouped order")
}

func TestListPaneSpawnModal(t *testing.T) {
	m := newListPane(&fakeAPI{}, t.TempDir())
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}
