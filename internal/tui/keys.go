package tui

import tea "github.com/charmbracelet/bubbletea"

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global quit (normal mode only; modes handle their own esc).
	if m.mode == modeNormal {
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "down", "j":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		}
	}
	return m, nil
}
