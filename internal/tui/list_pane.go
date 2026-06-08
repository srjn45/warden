package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/pipeline"
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
	tp            textinput.Model
	openedDirs    map[string]time.Time
	dirCandidates []string
	targetDir     string
	mode          mode
	status        string
	connected     bool
	pendingSelect string
	pipelines     []*pipeline.Pipeline
	pressure      client.PressureStatus
	pendingPrompt string
	pendingDir    string
	spawnVerdict  string // reason text for the confirm prompt; "" when not confirming
	pendingDelete string // pid awaiting delete confirmation; "" when not confirming
	w, h          int
	ready         bool
}

func newListPane(a api, detailPane string) listPaneModel {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	tp := textinput.New()
	tp.Placeholder = "~/path/to/dir"
	return listPaneModel{
		api: a, ta: ta, ti: ti, tp: tp, detailPane: detailPane,
		openedDirs: map[string]time.Time{}, connected: true,
	}
}

func (m listPaneModel) items() []item {
	// Pipeline-owned sessions are shown under their pipeline, not the flat list —
	// except orphans whose pipeline was deleted, which fall back to the flat list.
	return append(pipelineItems(m.pipelines), buildItems(flatSessions(m.sessions, m.pipelines), m.openedDirs)...)
}

func (m listPaneModel) selected() *store.Session { return itemAt(m.items(), m.cursor).session }

func (m listPaneModel) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

func (m listPaneModel) selectedKey() string { return itemKey(itemAt(m.items(), m.cursor)) }

func (m listPaneModel) fallbackDir() string {
	d, _ := os.Getwd()
	return d
}

func (m listPaneModel) activeDir() string {
	return activeDir(m.items(), m.cursor, m.fallbackDir())
}

func (m listPaneModel) Init() tea.Cmd { return tea.Batch(listCmd(m.api), pipelinesCmd(m.api), tick()) }

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
		return m, tea.Batch(listCmd(m.api), pipelinesCmd(m.api), pressureCmd(m.api), tick())
	case pressureMsg:
		if msg.err == nil {
			m.pressure = msg.status
		}
		return m, nil
	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		prev := m.selectedKey()
		m.sessions = groupSort(msg.sessions)
		m.repin(prev)
		return m, nil
	case pipelinesMsg:
		if msg.err == nil {
			prev := m.selectedKey()
			m.pipelines = msg.pipelines
			m.repin(prev)
		}
		return m, nil
	case pipelineActionMsg:
		if msg.err != nil {
			m.status = "pipeline action failed: " + msg.err.Error()
		} else {
			m.status = ""
		}
		return m, pipelinesCmd(m.api)
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
	case spawnDoneMsg:
		switch {
		case msg.confirm != nil:
			m.mode = modeConfirmSpawn
			m.spawnVerdict = msg.confirm.Verdict.Reason
			m.status = "memory pressure: " + msg.confirm.Verdict.Reason
		case msg.err != nil:
			m.status = "spawn failed: " + msg.err.Error()
		default:
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

func (m *listPaneModel) repin(prevKey string) {
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

func (m listPaneModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeNewAgent:
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
			m.pendingPrompt, m.pendingDir = prompt, m.targetDir
			return m, spawnCmd(m.api, prompt, m.targetDir, false)
		}
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	case modeOpenDir:
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
	case modeNewAgentDir:
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
	case modeConfirmSpawn:
		switch msg.String() {
		case "f", "F":
			m.mode = modeNormal
			prompt, dir := m.pendingPrompt, m.pendingDir
			m.spawnVerdict = ""
			m.status = "spawning (forced)…"
			return m, spawnCmd(m.api, prompt, dir, true)
		case "esc", "n", "N":
			m.mode = modeNormal
			m.spawnVerdict = ""
			m.status = "spawn cancelled"
		}
		return m, nil
	case modeConfirmDeletePipeline:
		switch msg.String() {
		case "esc", "n", "N":
			m.mode = modeNormal
			m.pendingDelete = ""
			m.status = ""
		case "y", "Y":
			pid := m.pendingDelete
			m.mode = modeNormal
			m.pendingDelete = ""
			if pid != "" {
				m.status = "deleting " + pid
				return m, deletePipelineCmd(m.api, pid)
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
		if m.detailPane != "" {
			attach, jobPipe, jobID := cockpitDetailCmd(itemAt(m.items(), m.cursor))
			switch {
			case attach != "":
				return m, openInDetailCmd(m.detailPane, attach)
			case jobID != "":
				// A terminal job's agent tmux is gone — render its stored detail
				// instead of attaching to a dead session (which leaves a blank pane).
				return m, openJobDetailCmd(m.detailPane, jobPipe, jobID)
			}
		}
	case "down", "j":
		if m.cursor < len(m.items())-1 {
			m.cursor++
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "n":
		m.targetDir = m.activeDir()
		m.mode = modeNewAgent
		m.ta.Reset()
		m.ta.Focus()
	case "o":
		m.mode = modeOpenDir
		m.tp.Reset()
		m.tp.Focus()
		m.dirCandidates = nil
	case "s":
		if m.selected() != nil {
			m.mode = modeSendMsg
			m.ti.Reset()
			m.ti.Focus()
		}
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
	case "D":
		it := itemAt(m.items(), m.cursor)
		if it.pipeline != nil {
			if pipelineHasLiveJobs(it.pipeline) {
				m.status = "cancel " + it.pipeline.ID + " first (it has live jobs)"
				return m, nil
			}
			m.pendingDelete = it.pipeline.ID
			m.mode = modeConfirmDeletePipeline
		}
	case "r":
		it := itemAt(m.items(), m.cursor)
		if it.pjJob != nil && (it.pjJob.Status == pipeline.JobFailed || it.pjJob.Status == pipeline.JobNeedsAttention) {
			m.status = "retrying " + it.pjPipe + "/" + it.pjJob.ID
			return m, retryJobCmd(m.api, it.pjPipe, it.pjJob.ID)
		}
	case "a":
		if id := m.selectedID(); id != "" {
			return m, switchClientCmd(id)
		}
		if it := itemAt(m.items(), m.cursor); it.pjJob != nil && it.pjJob.SessionID != "" {
			return m, switchClientCmd(it.pjJob.SessionID)
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
	if chip := pressureChip(m.pressure); chip != "" {
		header += "  " + chip
	}
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
	body := titleBox(title, renderList(m.items(), m.cursor, m.w-2, bodyH-2), m.w, bodyH)

	footer := stMuted.Render("enter open · n new · o open dir · s send · a attach · r retry · x kill/cancel · ? help · q quit")
	if m.status != "" {
		footer = stStatus.Render(m.status)
	}
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — "+abbrevHome(m.targetDir)+"  (tab: change dir · ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeNewAgentDir:
		footer = stPaneTitle.Render("Launch dir (tab complete · enter · esc)") + "\n" + m.tp.View() + "\n" + stMuted.Render(strings.Join(m.dirCandidates, "  "))
	case modeOpenDir:
		footer = stPaneTitle.Render("Open directory (tab complete · enter · esc)") + "\n" + m.tp.View() + "\n" + stMuted.Render(strings.Join(m.dirCandidates, "  "))
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		footer = stError.Render("Kill & remove " + m.selectedID() + "? y / N")
	case modeConfirmSpawn:
		footer = stAttention.Render("⚠ memory pressure: " + m.spawnVerdict + "  [f] spawn anyway  [esc] cancel")
	case modeConfirmDeletePipeline:
		footer = stError.Render("Delete pipeline " + m.pendingDelete + "? y / N")
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

// cockpitDetailCmd decides what the cockpit shows in its detail pane for the item
// under the cursor. A terminal pipeline job (done/failed/skipped) has no live tmux
// to attach to, so it returns the pipeline+job ids for a stored-detail render;
// anything else with a live session returns that session to attach. Returns all
// empty for rows with nothing to open (headers, placeholders, pending jobs).
func cockpitDetailCmd(it item) (attach, jobPipe, jobID string) {
	if it.pjJob != nil {
		if jobIsTerminal(it.pjJob.Status) {
			return "", it.pjPipe, it.pjJob.ID
		}
		return it.pjJob.SessionID, "", ""
	}
	if it.session != nil {
		return it.session.TmuxSession, "", ""
	}
	return "", "", ""
}

// respawnJobDetailArgs builds the tmux command that replaces the detail pane with
// a render of one terminal job's stored detail (self re-invoked as a hidden pane).
func respawnJobDetailArgs(detailPane, self, pid, jobID string) []string {
	return []string{"respawn-pane", "-k", "-t", detailPane,
		self + " tui --pane=jobdetail --pipeline=" + pid + " --job=" + jobID}
}

// openJobDetailCmd renders a terminal job's stored detail into the detail pane.
func openJobDetailCmd(detailPane, pid, jobID string) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{err: exec.Command("tmux", respawnJobDetailArgs(detailPane, self, pid, jobID)...).Run()}
	}
}

// RunListPane runs the top-left cockpit pane; detailPane is the tmux id of the
// detail pane it drives (opened on Enter).
func RunListPane(a api, detailPane string) error {
	p := tea.NewProgram(newListPane(a, detailPane), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
