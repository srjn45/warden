package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/srajanpathak/agentctl/internal/pipeline"
)

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global quit (normal mode only; modes handle their own esc).
	if m.mode == modeNormal {
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "down", "j":
			if m.cursor < len(m.items())-1 {
				m.cursor++
			}
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "i":
			if m.approvalsOn {
				m.cursor = 0 // the inbox row is always index 0 when enabled
				m.apprFocused = true
			}
			return m, nil
		case "tab":
			if itemAt(m.items(), m.cursor).approvals {
				m.apprFocused = true
			} else if m.selected() != nil {
				m.outputFocused = true
			}
			return m, nil
		case "n":
			m.targetDir = m.activeDir()
			m.mode = modeNewAgent
			m.ta.Reset()
			m.ta.Focus()
			return m, nil
		case "o":
			m.mode = modeOpenDir
			m.tp.Reset()
			m.tp.Focus()
			m.dirCandidates = nil
			return m, nil
		case "s":
			if m.selected() != nil {
				m.mode = modeSendMsg
				m.ti.Reset()
				m.ti.Focus()
			}
			return m, nil
		case "x":
			it := itemAt(m.items(), m.cursor)
			switch {
			case it.pipeline != nil:
				m.status = "canceling " + it.pipeline.ID
				return m, cancelPipelineCmd(m.api, it.pipeline.ID)
			case it.session != nil:
				m.mode = modeConfirmKill
			case it.dir != "":
				delete(m.openedDirs, it.dir)
				m.status = "closed " + abbrevHome(it.dir)
			}
			return m, nil
		case "a":
			it := itemAt(m.items(), m.cursor)
			if it.session != nil {
				return m, attachCmd(it.session.ID)
			}
			if it.pjJob != nil && it.pjJob.SessionID != "" {
				return m, attachCmd(it.pjJob.SessionID)
			}
			return m, nil
		case "r":
			it := itemAt(m.items(), m.cursor)
			if it.pjJob != nil && (it.pjJob.Status == pipeline.JobFailed || it.pjJob.Status == pipeline.JobNeedsAttention) {
				m.status = "retrying " + it.pjPipe + "/" + it.pjJob.ID
				return m, retryJobCmd(m.api, it.pjPipe, it.pjJob.ID)
			}
			return m, nil
		case "?":
			m.mode = modeHelp
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
	case tea.KeyTab:
		m.mode = modeNewAgentDir
		m.ta.Blur()
		m.tp.SetValue(m.targetDir)
		m.tp.CursorEnd()
		m.tp.Focus()
		m.dirCandidates = nil
		return m, nil
	case tea.KeyCtrlS:
		prompt := strings.TrimSpace(m.ta.Value())
		m.mode = modeNormal
		m.ta.Blur()
		if prompt == "" {
			m.status = "prompt was empty"
			return m, nil
		}
		return m, spawnCmd(m.api, prompt, m.targetDir)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

func (m Model) updateOpenDir(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNormal
		m.tp.Blur()
		m.dirCandidates = nil
		return m, nil
	case tea.KeyTab:
		typed := expandPath(m.tp.Value(), homeDir())
		listDir, _ := dirCompletionTarget(typed)
		return m, listDirsCmd(m.api, typed, listDir)
	case tea.KeyEnter:
		return m, openDirCmd(m.api, expandPath(m.tp.Value(), homeDir()))
	}
	var cmd tea.Cmd
	m.tp, cmd = m.tp.Update(msg)
	return m, cmd
}

func (m Model) updateNewAgentDir(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNewAgent
		m.tp.Blur()
		m.dirCandidates = nil
		m.ta.Focus()
		return m, nil
	case tea.KeyTab:
		typed := expandPath(m.tp.Value(), homeDir())
		listDir, _ := dirCompletionTarget(typed)
		return m, listDirsCmd(m.api, typed, listDir)
	case tea.KeyEnter:
		m.targetDir = expandPath(m.tp.Value(), homeDir())
		m.mode = modeNewAgent
		m.tp.Blur()
		m.dirCandidates = nil
		m.ta.Focus()
		return m, nil
	}
	var cmd tea.Cmd
	m.tp, cmd = m.tp.Update(msg)
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
		m.status = ""
		return m, nil
	case "y", "Y":
		if id != "" {
			return m, killCmd(m.api, id)
		}
		return m, nil
	}
	return m, nil
}
