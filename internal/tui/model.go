package tui

import (
	"context"
	"os"
	"time"

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
	Terminate(ctx context.Context, id string) error
	Delete(ctx context.Context, id string, hard bool) error
	Input(ctx context.Context, id, text string) error
	ListDirs(ctx context.Context, path string) (client.DirListing, error)
}

type mode int

const (
	modeNormal mode = iota
	modeNewAgent
	modeSendMsg
	modeConfirmKill
	modeHelp
	modeOpenDir     // path input for `o`
	modeNewAgentDir // dir-override sub-state of modeNewAgent
)

// Model is the Bubble Tea model. Update is a pure reducer over messages.
type Model struct {
	api           api
	sessions      []*store.Session
	cursor        int
	output        string
	vp            viewport.Model
	ta            textarea.Model
	ti            textinput.Model
	tp            textinput.Model
	openedDirs    map[string]time.Time
	dirCandidates []string
	targetDir     string
	mode          mode
	outputFocused bool
	pendingSelect string
	status        string
	connected     bool
	w, h          int
	ready         bool
}

// New builds an initial model bound to the given api client.
func New(a api) Model {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	tp := textinput.New()
	tp.Placeholder = "~/path/to/dir"
	return Model{api: a, ta: ta, ti: ti, tp: tp, openedDirs: map[string]time.Time{}, connected: true}
}

func (m Model) items() []item { return buildItems(m.sessions, m.openedDirs) }

func (m Model) selected() *store.Session { return itemAt(m.items(), m.cursor).session }

func (m Model) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

func (m Model) selectedKey() string { return itemKey(itemAt(m.items(), m.cursor)) }

func (m Model) fallbackDir() string {
	d, _ := os.Getwd()
	return d
}

func (m Model) activeDir() string {
	return activeDir(m.items(), m.cursor, m.fallbackDir())
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
		prevKey := m.selectedKey()
		m.sessions = groupSort(msg.sessions)
		m.repin(prevKey)
		return m, nil

	case dirListMsg:
		if msg.err == nil && (m.mode == modeOpenDir || m.mode == modeNewAgentDir) {
			completed, cands := completeDir(msg.listing, msg.typed)
			m.tp.SetValue(completed)
			m.tp.CursorEnd()
			m.dirCandidates = cands
		}
		return m, nil

	case openDirMsg:
		if msg.err != nil {
			m.status = "cannot open " + msg.dir + ": " + msg.err.Error()
			return m, nil
		}
		m.openedDirs[msg.dir] = time.Now()
		m.pendingSelect = dirKey(msg.dir)
		m.mode = modeNormal
		m.tp.Blur()
		m.dirCandidates = nil
		m.repin("")
		return m, nil

	case outputMsg:
		if msg.id == m.selectedID() {
			m.output = msg.text
			m.vp.SetContent(msg.text)
			m.vp.GotoBottom()
		}
		return m, nil

	case spawnDoneMsg:
		if msg.err != nil {
			m.status = "spawn failed: " + msg.err.Error()
		} else {
			m.status = "spawned " + msg.id
			m.pendingSelect = msg.id
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
		if msg.err != nil {
			m.status = "remove failed: " + msg.err.Error()
		} else {
			m.status = "removed " + msg.id
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
		if m.mode == modeHelp {
			m.mode = modeNormal
			return m, nil
		}
		if m.mode == modeNewAgent {
			return m.updateNewAgent(msg)
		}
		if m.mode == modeOpenDir {
			return m.updateOpenDir(msg)
		}
		if m.mode == modeNewAgentDir {
			return m.updateNewAgentDir(msg)
		}
		if m.mode == modeSendMsg {
			return m.updateSendMsg(msg)
		}
		if m.mode == modeConfirmKill {
			return m.updateConfirmKill(msg)
		}
		if m.mode == modeNormal && m.outputFocused {
			switch msg.String() {
			case "tab", "esc":
				m.outputFocused = false
				return m, nil
			case "q", "ctrl+c":
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg) // PgUp/PgDn/up/down scroll the output
			return m, cmd
		}
		return m.handleKey(msg)
	}
	return m, nil
}

// repin keeps the cursor on the item with prevKey if it still exists, else clamps.
func (m *Model) repin(prevKey string) {
	items := m.items()
	want := prevKey
	if m.pendingSelect != "" {
		want = m.pendingSelect
	}
	if want != "" {
		for i, it := range items {
			if itemKey(it) == want {
				m.cursor = i
				if want == m.pendingSelect {
					m.pendingSelect = ""
				}
				return
			}
		}
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}
