package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

func (m *Model) layout() {
	// detail viewport: right ~60% width, body height minus header/footer.
	rw := m.w * 6 / 10
	if rw < 20 {
		rw = m.w
	}
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	m.vp.Width = rw - 2
	m.vp.Height = bodyH - 6
	if m.vp.Height < 1 {
		m.vp.Height = 1
	}
	m.ta.SetWidth(m.w - 2)
	m.ta.SetHeight(4)
	m.ti.Width = m.w - 20
}

func (m Model) View() string {
	if !m.ready {
		return "loading…"
	}
	conn := stStatus.Render("live ●")
	if !m.connected {
		conn = stError.Render("reconnecting…")
	}
	header := stHeader.Render("agentctl") + "  " + conn

	leftW := m.w * 4 / 10
	rightW := m.w - leftW - 1
	left := lipgloss.NewStyle().Width(leftW).Render(m.renderList(leftW))
	right := lipgloss.NewStyle().Width(rightW).Render(m.renderDetail(rightW))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)

	footer := m.footer()
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — describe the task (ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		if m.killForce {
			footer = stError.Render("uncommitted/unpushed — press X to FORCE terminate, esc to cancel")
		} else {
			footer = stError.Render("Terminate " + m.selectedID() + "? y / N")
		}
	}
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

func (m Model) footer() string {
	if m.status != "" {
		return stStatus.Render(m.status)
	}
	return stMuted.Render("n new · s send · a attach · x kill · tab focus · ? help · q quit")
}
