package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/srajanpathak/agentctl/internal/store"
)

var (
	stBusy      = lipgloss.NewStyle().Foreground(lipgloss.Color("2")) // green
	stAttention = lipgloss.NewStyle().Foreground(lipgloss.Color("3")) // amber
	stIdle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8")) // grey
	stError     = lipgloss.NewStyle().Foreground(lipgloss.Color("1")) // red
	stHeader    = lipgloss.NewStyle().Bold(true)
	stCursor    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	stMuted     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	stPaneTitle = lipgloss.NewStyle().Bold(true)
	stStatus    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// badge maps a status to a short label + style (mirrors the web status.ts mapping).
func badge(s store.Status) (string, lipgloss.Style) {
	switch s {
	case store.StatusSpawning:
		return "starting", stBusy
	case store.StatusWorking:
		return "busy", stBusy
	case store.StatusWaitingForInput:
		return "needs-input", stAttention
	case store.StatusIdle:
		return "idle", stIdle
	case store.StatusDone:
		return "done", stIdle
	case store.StatusErrored:
		return "error", stError
	case store.StatusOrphaned:
		return "orphaned", stError
	default:
		if s == "" {
			return "classifying", stMuted
		}
		return string(s), stIdle
	}
}
