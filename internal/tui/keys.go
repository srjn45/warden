package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

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
		case "tab":
			if m.selected() != nil {
				m.outputFocused = true
			}
			return m, nil
		case "n":
			m.mode = modeNewAgent
			m.ta.Reset()
			m.ta.Focus()
			return m, nil
		}
	}
	return m, nil
}

func (m Model) updateNewAgent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNormal
		m.ta.Blur()
		return m, nil
	case tea.KeyCtrlS:
		prompt := strings.TrimSpace(m.ta.Value())
		m.mode = modeNormal
		m.ta.Blur()
		if prompt == "" {
			m.status = "prompt was empty"
			return m, nil
		}
		return m, spawnCmd(m.api, prompt)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}
