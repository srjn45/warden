package tui

import (
	"context"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
)

// api is the subset of *client.Client the TUI needs (fakeable in tests).
type api interface {
	List(ctx context.Context) ([]*store.Session, error)
	Output(ctx context.Context, id string, lines int) (string, error)
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	Cleanup(ctx context.Context, id string, force, hard bool) error
	Input(ctx context.Context, id, text string) error
}

type mode int

const (
	modeNormal mode = iota
	modeNewAgent
	modeSendMsg
	modeConfirmKill
	modeHelp
)

// Model is the Bubble Tea model. Update is a pure reducer over messages.
type Model struct {
	api       api
	sessions  []*store.Session
	cursor    int
	output    string
	vp        viewport.Model
	ta        textarea.Model
	ti        textinput.Model
	mode      mode
	killForce bool
	status    string
	connected bool
	w, h      int
	ready     bool
}

// New builds an initial model bound to the given api client.
func New(a api) Model {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	return Model{api: a, ta: ta, ti: ti, connected: true}
}

func (m Model) selected() *store.Session {
	if m.cursor >= 0 && m.cursor < len(m.sessions) {
		return m.sessions[m.cursor]
	}
	return nil
}

func (m Model) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(listCmd(m.api), tick())
}

// Update is the pure reducer.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
		m.ready = true
		return m, nil

	case tickMsg:
		return m, tea.Batch(listCmd(m.api), outputCmd(m.api, m.selectedID()), tick())

	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		prevID := m.selectedID()
		m.sessions = msg.sessions
		m.repin(prevID)
		return m, nil

	case outputMsg:
		if msg.id == m.selectedID() {
			m.output = msg.text
			m.vp.SetContent(msg.text)
			m.vp.GotoBottom()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// repin keeps the cursor on the session with prevID if it still exists, else clamps.
func (m *Model) repin(prevID string) {
	if prevID != "" {
		for i, s := range m.sessions {
			if s.ID == prevID {
				m.cursor = i
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
