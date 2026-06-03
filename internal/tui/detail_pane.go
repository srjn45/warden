package tui

import (
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/srajanpathak/agentctl/internal/store"
)

// detailPaneModel is the full-height right cockpit pane: a read-only viewport of
// the selected agent's output. Selection comes from the shared state file
// (written by the list pane), re-read on every tick.
type detailPaneModel struct {
	api       api
	stateDir  string
	selID     string
	sess      *store.Session
	sessions  []*store.Session
	output    string
	vp        viewport.Model
	connected bool
	w, h      int
	ready     bool
}

func newDetailPane(a api, stateDir string) detailPaneModel {
	return detailPaneModel{api: a, stateDir: stateDir, connected: true}
}

func (m detailPaneModel) Init() tea.Cmd { return tick() }

func (m *detailPaneModel) findSelected() {
	m.sess = nil
	for _, s := range m.sessions {
		if s.ID == m.selID {
			m.sess = s
			return
		}
	}
}

func (m detailPaneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width = m.w - 2
		vpH := (m.h - 2) - 2 - detailChrome
		if vpH < 1 {
			vpH = 1
		}
		m.vp.Height = vpH
		m.ready = true
		return m, nil
	case tickMsg:
		m.selID = readSelection(m.stateDir)
		m.findSelected()
		return m, tea.Batch(listCmd(m.api), outputCmd(m.api, m.selID), tick())
	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		m.sessions = msg.sessions
		m.findSelected()
		return m, nil
	case outputMsg:
		if msg.id == m.selID {
			m.output = msg.text
			m.vp.SetContent(msg.text)
			m.vp.GotoBottom()
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // PgUp/PgDn/arrows scroll the output
		return m, cmd
	}
	return m, nil
}

func (m detailPaneModel) View() string {
	if !m.ready {
		return "loading…"
	}
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	title := m.selID
	if title == "" {
		title = "—"
	}
	box := titleBox(title, renderDetail(m.sess, m.vp, false, m.w-2), m.w, bodyH)
	if !m.connected {
		return stError.Render("reconnecting…") + "\n" + box
	}
	return box
}

// RunDetailPane runs the full-height right cockpit pane against the daemon client.
func RunDetailPane(a api, stateDir string) error {
	p := tea.NewProgram(newDetailPane(a, stateDir), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
