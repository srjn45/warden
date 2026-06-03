package tui

import (
	"testing"

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

func TestListPaneSpawnModal(t *testing.T) {
	m := newListPane(&fakeAPI{}, t.TempDir())
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestRespawnDetailArgs(t *testing.T) {
	require.Equal(t,
		[]string{"respawn-pane", "-k", "-t", "%9", "env -u TMUX tmux attach -t agent-4f98"},
		respawnDetailArgs("%9", "agent-4f98"))
}
