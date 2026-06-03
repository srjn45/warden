package tui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/srajanpathak/agentctl/internal/store"
)

// listPaneModel is the top-left cockpit pane: the agents list plus the
// new/send/terminate/attach actions. It owns selection: on Enter it opens the
// selected agent in the detail pane via respawn-pane.
type listPaneModel struct {
	api           api
	detailPane    string // tmux pane id of the detail pane this list drives
	sessions      []*store.Session
	cursor        int
	ta            textarea.Model
	ti            textinput.Model
	mode          mode
	status        string
	connected     bool
	pendingSelect string
	w, h          int
	ready         bool
}

func newListPane(a api, detailPane string) listPaneModel {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	return listPaneModel{api: a, ta: ta, ti: ti, detailPane: detailPane, connected: true}
}

func (m listPaneModel) selectedID() string {
	if m.cursor >= 0 && m.cursor < len(m.sessions) {
		return m.sessions[m.cursor].ID
	}
	return ""
}

func (m listPaneModel) selected() *store.Session {
	if m.cursor >= 0 && m.cursor < len(m.sessions) {
		return m.sessions[m.cursor]
	}
	return nil
}

func (m listPaneModel) Init() tea.Cmd { return tea.Batch(listCmd(m.api), tick()) }

func (m listPaneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ta.SetWidth(m.w - 2)
		m.ta.SetHeight(4)
		m.ti.Width = m.w - 20
		m.ready = true
		return m, nil
	case tickMsg:
		return m, tea.Batch(listCmd(m.api), tick())
	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		prev := m.selectedID()
		m.sessions = groupSort(msg.sessions)
		m.repin(prev)
		return m, nil
	case spawnDoneMsg:
		if msg.err != nil {
			m.status = "spawn failed: " + msg.err.Error()
		} else {
			m.status, m.pendingSelect = "spawned "+msg.id, msg.id
		}
		return m, nil
	case inputDoneMsg:
		if msg.err != nil {
			m.status = "send failed: " + msg.err.Error()
		} else {
			m.status = "sent"
		}
		return m, nil
	case cleanupDoneMsg:
		m.mode = modeNormal
		m.status = "removed " + msg.id
		if msg.err != nil {
			m.status = "remove failed: " + msg.err.Error()
		}
		return m, nil
	case attachDoneMsg:
		if msg.err != nil {
			m.status = "attach failed: " + msg.err.Error()
		} else {
			m.status = ""
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *listPaneModel) repin(prevID string) {
	want := prevID
	if m.pendingSelect != "" {
		want = m.pendingSelect
	}
	if want != "" {
		for i, s := range m.sessions {
			if s.ID == want {
				m.cursor = i
				if want == m.pendingSelect {
					m.pendingSelect = ""
				}
				return
			}
		}
	}
	if m.cursor >= len(m.sessions) {
		m.cursor = len(m.sessions) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m listPaneModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeNewAgent:
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
	case modeSendMsg:
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
	case modeConfirmKill:
		switch msg.String() {
		case "esc", "n", "N":
			m.mode = modeNormal
			m.status = ""
		case "y", "Y":
			if id := m.selectedID(); id != "" {
				return m, killCmd(m.api, id)
			}
		}
		return m, nil
	case modeHelp:
		m.mode = modeNormal
		return m, nil
	}
	// normal mode
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Sequence(killCockpitCmd(), tea.Quit)
	case "enter":
		if s := m.selected(); s != nil && m.detailPane != "" {
			return m, openInDetailCmd(m.detailPane, s.TmuxSession)
		}
	case "down", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "n":
		m.mode = modeNewAgent
		m.ta.Reset()
		m.ta.Focus()
	case "s":
		if m.selected() != nil {
			m.mode = modeSendMsg
			m.ti.Reset()
			m.ti.Focus()
		}
	case "x":
		if m.selected() != nil {
			m.mode = modeConfirmKill
		}
	case "a":
		if id := m.selectedID(); id != "" {
			return m, switchClientCmd(id)
		}
	case "?":
		m.mode = modeHelp
	}
	return m, nil
}

func (m listPaneModel) View() string {
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
	if m.mode == modeHelp {
		return header + "\n" + lipgloss.NewStyle().Width(m.w).Height(bodyH).Render(helpText())
	}
	title := fmt.Sprintf("Agents (%d)", len(m.sessions))
	body := titleBox(title, renderList(m.sessions, m.cursor, m.w-2, bodyH-2), m.w, bodyH)

	footer := stMuted.Render("enter open · n new · s send · a attach · x kill · ? help · q quit")
	if m.status != "" {
		footer = stStatus.Render(m.status)
	}
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent (ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		footer = stError.Render("Kill & remove " + m.selectedID() + "? y / N")
	}
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

// killCockpitCmd tears down the whole cockpit by killing the tmux session that
// hosts this pane. Run from inside the session, `tmux kill-session` (no target)
// kills the current session, taking the detail + master panes down with it.
// If we are not inside tmux (kill-session fails), the subsequent tea.Quit still
// exits cleanly.
func killCockpitCmd() tea.Cmd {
	return func() tea.Msg {
		// Drop the back-to-dashboard binding buildCockpit installed, then kill the
		// session. Both are best-effort (harmless if not inside tmux).
		_ = exec.Command("tmux", "unbind-key", "Enter").Run()
		_ = exec.Command("tmux", "kill-session").Run()
		return nil
	}
}

// switchClientCmd moves the cockpit's tmux client to the selected agent's
// session. The list pane runs inside the cockpit's tmux session, where
// `tmux attach` refuses to nest — `switch-client` is the correct primitive.
// buildCockpit binds <prefix> Enter to switch-client -l; we flash that hint so
// the user knows how to get back to the dashboard.
func switchClientCmd(id string) tea.Cmd {
	return func() tea.Msg {
		if err := exec.Command("tmux", "switch-client", "-t", id).Run(); err != nil {
			return attachDoneMsg{err: err}
		}
		_ = exec.Command("tmux", "display-message", "agentctl: press Ctrl-b Enter to return to the dashboard").Run()
		return attachDoneMsg{err: nil}
	}
}

// respawnDetailArgs builds the tmux args that replace the detail pane's process
// with a live (nested) attach to the given agent's tmux session. `env -u TMUX`
// lets tmux attach from inside tmux; `respawn-pane -k` kills the placeholder
// (or the previously-opened agent) first.
func respawnDetailArgs(detailPane, agentSession string) []string {
	return []string{"respawn-pane", "-k", "-t", detailPane,
		"env -u TMUX tmux attach -t " + agentSession}
}

// openInDetailCmd opens the given agent's live session in the detail pane.
func openInDetailCmd(detailPane, agentSession string) tea.Cmd {
	return func() tea.Msg {
		return attachDoneMsg{err: exec.Command("tmux", respawnDetailArgs(detailPane, agentSession)...).Run()}
	}
}

// RunListPane runs the top-left cockpit pane; detailPane is the tmux id of the
// detail pane it drives (opened on Enter).
func RunListPane(a api, detailPane string) error {
	p := tea.NewProgram(newListPane(a, detailPane), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
