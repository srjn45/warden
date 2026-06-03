package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func dstep(m detailPaneModel, msg tea.Msg) detailPaneModel {
	nm, _ := m.Update(msg)
	return nm.(detailPaneModel)
}

func TestDetailPaneReadsSelectionOnTick(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeSelection(dir, "agent-x", 1))
	m := newDetailPane(&fakeAPI{}, dir)
	m = dstep(m, tickMsg{}) // tick re-reads the selection file
	require.Equal(t, "agent-x", m.selID)
}

func TestDetailPaneShowsOutputForSelection(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeSelection(dir, "agent-x", 1))
	m := newDetailPane(&fakeAPI{}, dir)
	m = dstep(m, tickMsg{})
	m = dstep(m, sessionsMsg{sessions: []*store.Session{{ID: "agent-x", Subject: "doing things"}}})
	m = dstep(m, outputMsg{id: "agent-x", text: "hello from agent"})
	require.Contains(t, m.output, "hello from agent")
	require.NotNil(t, m.sess)
	require.Equal(t, "agent-x", m.sess.ID)
}
