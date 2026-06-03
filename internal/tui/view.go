package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

// detailChrome is the number of non-viewport lines renderDetail emits
// (head, dir, subject, blank, output-title).
const detailChrome = 5

func (m *Model) layout() {
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	listOuter := m.listOuterW()
	detailInner := (m.w - listOuter) - 2 // detail box inner width
	if detailInner < 1 {
		detailInner = 1
	}
	m.vp.Width = detailInner
	vpH := (bodyH - 2) - detailChrome // box inner height minus detail chrome
	if vpH < 1 {
		vpH = 1
	}
	m.vp.Height = vpH
	m.ta.SetWidth(m.w - 2)
	m.ta.SetHeight(4)
	m.ti.Width = m.w - 20
}

// listOuterW is the list box outer width (~40%, clamped to leave room for detail).
func (m Model) listOuterW() int {
	w := m.w * 4 / 10
	if w < 24 {
		w = 24
	}
	if w > m.w-10 {
		w = m.w - 10
	}
	if w < 4 {
		w = 4
	}
	return w
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
	if !m.connected {
		header += "  " + stError.Render("daemon not running — start it with `agentctl daemon`")
	}

	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	var body string
	if m.mode == modeHelp {
		body = lipgloss.NewStyle().Width(m.w).Height(bodyH).Render(helpText())
	} else {
		listOuter := m.listOuterW()
		detailOuter := m.w - listOuter
		listTitle := fmt.Sprintf("Agents (%d)", len(m.sessions))
		detailTitle := m.selectedID()
		if detailTitle == "" {
			detailTitle = "—"
		}
		left := titleBox(listTitle, renderList(m.sessions, m.cursor, listOuter-2, bodyH-2), listOuter, bodyH)
		right := titleBox(detailTitle, renderDetail(m.selected(), m.vp, m.outputFocused, detailOuter-2), detailOuter, bodyH)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}

	footer := m.footer()
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — describe the task (ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		footer = stError.Render("Terminate " + m.selectedID() + "? y / N")
	}
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

func helpText() string {
	return stPaneTitle.Render("Keys") + "\n" +
		"  ↑/↓ or j/k   move selection\n" +
		"  tab          focus output (PgUp/PgDn scroll), tab/esc to leave\n" +
		"  n            new agent (prompt)\n" +
		"  s            send a message to the selected agent\n" +
		"  a            attach to its tmux session\n" +
		"  x            terminate agent\n" +
		"  ?            toggle this help\n" +
		"  q            quit\n"
}

func (m Model) footer() string {
	if m.status != "" {
		return stStatus.Render(m.status)
	}
	return stMuted.Render("n new · s send · a attach · x kill · tab focus · ? help · q quit")
}
