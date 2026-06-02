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
		case "s":
			if m.selected() != nil {
				m.mode = modeSendMsg
				m.ti.Reset()
				m.ti.Focus()
			}
			return m, nil
		case "x":
			if m.selected() != nil {
				m.mode = modeConfirmKill
				m.killForce = false
			}
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

func (m Model) updateSendMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNormal
		m.ti.Blur()
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.ti.Value())
		id := m.selectedID()
		m.mode = modeNormal
		m.ti.Blur()
		if text == "" || id == "" {
			return m, nil
		}
		return m, inputCmd(m.api, id, text)
	}
	var cmd tea.Cmd
	m.ti, cmd = m.ti.Update(msg)
	return m, cmd
}

func (m Model) updateConfirmKill(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	id := m.selectedID()
	switch msg.String() {
	case "esc", "n", "N":
		m.mode = modeNormal
		m.killForce = false
		m.status = ""
		return m, nil
	case "y", "Y":
		if !m.killForce && id != "" {
			return m, cleanupCmd(m.api, id, false)
		}
		return m, nil
	case "X":
		if m.killForce && id != "" {
			return m, cleanupCmd(m.api, id, true)
		}
		return m, nil
	}
	return m, nil
}
