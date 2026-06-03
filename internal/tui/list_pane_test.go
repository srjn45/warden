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

func TestListPaneGroupsBySourceDir(t *testing.T) {
	now := time.Now()
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
	}})
	ids := []string{m.sessions[0].ID, m.sessions[1].ID, m.sessions[2].ID}
	require.Equal(t, []string{"b1", "b2", "a1"}, ids, "cockpit list pane stores grouped order")
}

func TestListPaneSpawnModal(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestListPaneEnterOpensDetail(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a", TmuxSession: "a"}}})
	_, cmd := m.Update(key("enter"))
	require.NotNil(t, cmd, "Enter on a selected agent opens it in the detail pane")
}

func TestListPaneEnterNoopWithoutSelection(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	_, cmd := m.Update(key("enter")) // no sessions loaded
	require.Nil(t, cmd, "Enter with no selection does nothing")
}

func TestRespawnDetailArgs(t *testing.T) {
	require.Equal(t,
		[]string{"respawn-pane", "-k", "-t", "%9", "env -u TMUX tmux attach -t agent-4f98"},
		respawnDetailArgs("%9", "agent-4f98"))
}
