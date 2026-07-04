package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/store"
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
	tn            textinput.Model // agent name input (new-agent form + rename)
	openedDirs    map[string]time.Time
	dirCandidates []string
	targetDir     string
	roles         []role.Role // built-in role catalog for the new-agent picker
	roleIdx       int         // selected role in the new-agent form (0 ⇒ general)
	mode          mode
	status        string
	connected     bool
	pendingSelect string
	pipelines     []*pipeline.Pipeline
	collapsed     map[string]bool // pipeline id → jobs hidden in the list
	seen          map[string]bool // pipeline ids the default-collapse has been applied to
	pressure      client.PressureStatus
	pendingPrompt string
	pendingName   string // name typed in the new-agent form, held across the pressure confirm
	pendingDir    string
	pendingRole   string                // role chosen in the new-agent form, held across the pressure confirm
	renameID      string                // agent id being renamed (modeRename)
	spawnVerdict  string                // reason text for the confirm prompt; "" when not confirming
	pendingDelete string                // pid awaiting delete confirmation; "" when not confirming
	ctxEntries    []client.ContextEntry // inspector: shared-context snapshot
	messages      []client.Message      // inspector: recent message traffic
	vp            viewport.Model        // scroll viewport (modeInspector / modeDigest)
	approvals     []approval.View       // pending tool-permission prompts
	apprEnabled   bool                  // approvals config setting on
	apprCursor    int                   // focused recognized approval (modeApprovals)
	digest        *digest.Digest        // last fetched digest (modeDigest)
	digestID      string                // agent id the digest is for
	w, h          int
	ready         bool
	// killWindow scopes the `q`/`ctrl+c` teardown to the cockpit's tmux *window*
	// instead of the whole session. It is set in the tmux-native cockpit, where
	// the cockpit is a window inside the user's own session — killing the session
	// there would take the user's entire tmux session down with it.
	killWindow bool
}

// quitCmd is what `q`/`ctrl+c` runs: tear the whole cockpit down (killCockpitCmd
// kills the hosting tmux session, taking the master + detail panes with it) and
// quit. The same teardown serves both flavors. Locally the user lands back in
// their shell; on the web the daemon notices the session vanished and tells the
// browser to leave the full-screen TUI (→ home) — see daemon.bridgeTmux.
func (m listPaneModel) quitCmd() tea.Cmd {
	return tea.Sequence(killCockpitCmd(m.killWindow), tea.Quit)
}

func newListPane(a api, detailPane string) listPaneModel {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	tp := textinput.New()
	tp.Placeholder = "~/path/to/dir"
	tn := textinput.New()
	tn.Placeholder = "agent-name (optional; blank = auto)"
	tn.CharLimit = 32
	return listPaneModel{
		api: a, ta: ta, ti: ti, tp: tp, tn: tn, detailPane: detailPane,
		// roles is the fixed built-in catalog embedded in the binary (general
		// first), so the picker is populated synchronously — no daemon round-trip.
		roles:      role.All(),
		openedDirs: map[string]time.Time{}, collapsed: map[string]bool{}, seen: map[string]bool{}, connected: true,
		vp: viewport.New(0, 0),
	}
}

func (m listPaneModel) items() []item {
	var head []item
	// A pinned approvals row appears at the top when prompts are waiting to be
	// answered (recognized menus only — unrecognized ones must be attached to).
	if m.apprEnabled {
		if rec := recognizedApprovals(m.approvals); len(rec) > 0 {
			head = append(head, item{approvals: true, apprCount: len(rec)})
		}
	}
	// Pipeline-owned sessions are shown under their pipeline, not the flat list —
	// except orphans whose pipeline was deleted, which fall back to the flat list.
	head = append(head, pipelineItems(m.pipelines, m.sessions, m.collapsed)...)
	return append(head, buildItems(flatSessions(m.sessions, m.pipelines), m.openedDirs, m.collapsed)...)
}

func (m listPaneModel) selected() *store.Session { return itemAt(m.items(), m.cursor).session }

func (m listPaneModel) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

func (m listPaneModel) selectedKey() string { return itemKey(itemAt(m.items(), m.cursor)) }

// detailTitle is the label for the modeDetails overlay: the agent id, or
// "pipeline/job" for a pipeline job row. The cursor cannot move while the overlay
// is open (keys scroll), so the item under it is stable across the overlay's life.
func (m listPaneModel) detailTitle() string {
	if it := itemAt(m.items(), m.cursor); it.pjJob != nil {
		return it.pjPipe + "/" + it.pjJob.ID
	}
	return m.selectedID()
}

func (m listPaneModel) fallbackDir() string {
	d, _ := os.Getwd()
	return d
}

func (m listPaneModel) activeDir() string {
	return activeDir(m.items(), m.cursor, m.fallbackDir())
}

// bodyH is the height of the framed pane body, shared by View and the inspector
// viewport sizing so the two never disagree.
func (m listPaneModel) bodyH() int {
	if h := m.h - 2; h >= 3 {
		return h
	}
	return 3
}

// setInspectorContent re-renders the inspector body into the viewport, preserving
// the current scroll offset (SetContent/SetYOffset clamp it), so refresh ticks and
// resizes do not snap the view back to the top.
func (m *listPaneModel) setInspectorContent() {
	off := m.vp.YOffset
	m.vp.SetContent(inspectorBody(m.ctxEntries, m.messages, m.vp.Width))
	m.vp.SetYOffset(off)
}

// applyDefaultCollapse folds away each newly-seen completed pipeline once, so the
// list opens with finished work collapsed. Pipelines are auto-collapsed only on
// first sighting (tracked in `seen`), so a manual expand survives later refreshes.
func (m *listPaneModel) applyDefaultCollapse() {
	for _, p := range m.pipelines {
		if m.seen[p.ID] {
			continue
		}
		m.seen[p.ID] = true
		if pipelineIsCompleted(p.Status) {
			m.collapsed[p.ID] = true
		}
	}
}

func (m listPaneModel) Init() tea.Cmd {
	return tea.Batch(listCmd(m.api), pipelinesCmd(m.api), approvalsCmd(m.api), tick())
}

func (m listPaneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ta.SetWidth(m.w - 2)
		m.ta.SetHeight(4)
		m.ti.Width = m.w - 20
		// Size the inspector viewport to the titleBox interior (width w-4 leaves
		// the same right margin inspectorBody used; height bodyH-2 matches the box).
		m.vp.Width = max(1, m.w-4)
		m.vp.Height = max(1, m.bodyH()-2)
		if m.mode == modeInspector {
			m.setInspectorContent() // re-flow for the new width, keep scroll position
		}
		m.ready = true
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{listCmd(m.api), pipelinesCmd(m.api), approvalsCmd(m.api), pressureCmd(m.api), tick()}
		if m.mode == modeInspector {
			cmds = append(cmds, contextCmd(m.api), messagesCmd(m.api))
		}
		return m, tea.Batch(cmds...)
	case pressureMsg:
		if msg.err == nil {
			m.pressure = msg.status
		}
		return m, nil
	case contextMsg:
		if msg.err == nil { // keep last good snapshot on a transient blip
			m.ctxEntries = msg.entries
			if m.mode == modeInspector {
				m.setInspectorContent() // refresh without snapping back to the top
			}
		}
		return m, nil
	case messagesMsg:
		if msg.err == nil {
			m.messages = msg.messages
			if m.mode == modeInspector {
				m.setInspectorContent()
			}
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
			m.applyDefaultCollapse()
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
	case approvalsMsg:
		if msg.err == nil {
			prev := m.selectedKey()
			m.apprEnabled = msg.enabled
			m.approvals = msg.views
			if rc := len(recognizedApprovals(m.approvals)); m.apprCursor >= rc {
				m.apprCursor = 0
			}
			m.repin(prev) // the approvals row appearing/disappearing shifts indices
		}
		return m, nil
	case digestMsg:
		if msg.err != nil {
			m.status = "digest failed: " + msg.err.Error()
			return m, nil
		}
		m.digest, m.digestID = msg.d, msg.id
		m.mode = modeDigest
		m.vp.SetContent(digestBody(msg.d, m.vp.Width))
		m.vp.GotoTop()
		m.status = ""
		return m, nil
	case approveDoneMsg:
		if msg.err != nil {
			m.status = "approve failed: " + msg.err.Error()
		} else {
			m.status = "answered"
		}
		return m, approvalsCmd(m.api) // refresh the queue right away
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
	case renameDoneMsg:
		if msg.err != nil {
			m.status = "rename failed: " + msg.err.Error()
		} else {
			m.status = "renamed " + msg.id
		}
		return m, listCmd(m.api) // refresh so the new name shows immediately
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
		case tea.KeyCtrlN:
			// Switch focus to the name field. Blank means "let warden auto-name it".
			m.mode = modeNewAgentName
			m.ta.Blur()
			m.tn.CursorEnd()
			m.tn.Focus()
			return m, nil
		case tea.KeyCtrlR:
			// Switch to the role picker. Defaults to general (no persona).
			m.mode = modeNewAgentRole
			m.ta.Blur()
			return m, nil
		case tea.KeyCtrlS:
			// An empty prompt is intentional: it opens claude in the target dir
			// and waits for the user to type instructions into Claude directly.
			prompt := strings.TrimSpace(m.ta.Value())
			name := strings.TrimSpace(m.tn.Value())
			role := m.selectedRole()
			m.mode = modeNormal
			m.ta.Blur()
			m.pendingPrompt, m.pendingName, m.pendingDir, m.pendingRole = prompt, name, m.targetDir, role
			return m, spawnCmd(m.api, prompt, name, m.targetDir, role, false)
		}
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	case modeNewAgentName:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyEnter:
			m.mode = modeNewAgent
			m.tn.Blur()
			m.ta.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.tn, cmd = m.tn.Update(msg)
		return m, cmd
	case modeNewAgentRole:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyEnter:
			m.mode = modeNewAgent
			m.ta.Focus()
			return m, nil
		case tea.KeyUp, tea.KeyLeft:
			if len(m.roles) > 0 {
				m.roleIdx = (m.roleIdx - 1 + len(m.roles)) % len(m.roles)
			}
			return m, nil
		case tea.KeyDown, tea.KeyRight, tea.KeyTab:
			if len(m.roles) > 0 {
				m.roleIdx = (m.roleIdx + 1) % len(m.roles)
			}
			return m, nil
		}
		// j/k also cycle, matching the list's vim-style navigation.
		switch msg.String() {
		case "k":
			if len(m.roles) > 0 {
				m.roleIdx = (m.roleIdx - 1 + len(m.roles)) % len(m.roles)
			}
		case "j":
			if len(m.roles) > 0 {
				m.roleIdx = (m.roleIdx + 1) % len(m.roles)
			}
		}
		return m, nil
	case modeRename:
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeDetails
			m.tn.Blur()
			return m, nil
		case tea.KeyEnter:
			name := strings.TrimSpace(m.tn.Value())
			id := m.renameID
			m.mode = modeDetails
			m.tn.Blur()
			if id == "" {
				return m, nil
			}
			m.status = "renaming " + id
			return m, renameCmd(m.api, id, name)
		}
		var cmd tea.Cmd
		m.tn, cmd = m.tn.Update(msg)
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
			prompt, name, dir, role := m.pendingPrompt, m.pendingName, m.pendingDir, m.pendingRole
			m.spawnVerdict = ""
			m.status = "spawning (forced)…"
			return m, spawnCmd(m.api, prompt, name, dir, role, true)
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
	case modeInspector:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, m.quitCmd()
		case "esc", "c":
			m.mode = modeNormal
			return m, nil
		case "g":
			m.vp.GotoTop()
			return m, nil
		case "G":
			m.vp.GotoBottom()
			return m, nil
		}
		// Everything else (↑/↓, pgup/pgdn, and the viewport's own j/k/d/u bindings)
		// scrolls the content. No mouse capture — keyboard only.
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case modeDigest:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, m.quitCmd()
		case "esc", "d":
			m.mode = modeNormal
			return m, nil
		case "g":
			m.vp.GotoTop()
			return m, nil
		case "G":
			m.vp.GotoBottom()
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case modeDetails:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, m.quitCmd()
		case "esc", "i":
			m.mode = modeNormal
			return m, nil
		case "r":
			// Rename the agent this detail view is for: seed the name field with
			// its current name and focus it.
			if s := m.selected(); s != nil {
				m.mode = modeRename
				m.renameID = s.ID
				m.tn.SetValue(s.Name)
				m.tn.CursorEnd()
				m.tn.Focus()
			}
			return m, nil
		case "g":
			m.vp.GotoTop()
			return m, nil
		case "G":
			m.vp.GotoBottom()
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case modeApprovals:
		rec := recognizedApprovals(m.approvals)
		switch msg.String() {
		case "q", "ctrl+c":
			return m, m.quitCmd()
		case "esc", "p":
			m.mode = modeNormal
			return m, nil
		case "tab":
			if len(rec) > 0 {
				m.apprCursor = (m.apprCursor + 1) % len(rec)
			}
			return m, nil
		}
		// A digit 1..len(options) answers the focused prompt.
		if n, err := strconv.Atoi(msg.String()); err == nil && len(rec) > 0 && m.apprCursor < len(rec) {
			v := rec[m.apprCursor]
			if n >= 1 && n <= len(v.Options) {
				m.mode = modeNormal
				m.status = "answering " + v.ID + " → " + strconv.Itoa(n)
				return m, approveCmd(m.api, v.ID, n, v.Fingerprint)
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
		return m, m.quitCmd()
	case "c":
		// Open the read-only shared-context + message-traffic inspector and
		// kick off an immediate fetch (the tick keeps it fresh while open).
		m.mode = modeInspector
		m.setInspectorContent()
		m.vp.GotoTop() // a freshly opened inspector starts at the top
		return m, tea.Batch(contextCmd(m.api), messagesCmd(m.api))
	case "enter":
		if itemAt(m.items(), m.cursor).approvals {
			if len(recognizedApprovals(m.approvals)) > 0 {
				m.mode = modeApprovals
				m.apprCursor = 0
			}
			return m, nil
		}
		if m.detailPane != "" {
			attach, jobPipe, jobID, agentDetail := cockpitDetailCmd(itemAt(m.items(), m.cursor))
			switch {
			case attach != "":
				return m, openInDetailCmd(m.detailPane, attach)
			case jobID != "":
				// A terminal job's agent tmux is gone — render its stored detail
				// instead of attaching to a dead session (which leaves a blank pane).
				return m, openJobDetailCmd(m.detailPane, jobPipe, jobID)
			case agentDetail != "":
				// A terminal agent (tombstone or finished) has no live tmux — render
				// its stored detail rather than attaching to a dead session.
				return m, openAgentDetailCmd(m.detailPane, agentDetail)
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
	case "right", "l":
		it := itemAt(m.items(), m.cursor)
		switch {
		case it.pipeline != nil:
			m.collapsed[it.pipeline.ID] = false
		case it.session != nil && it.hasKids:
			m.collapsed[it.session.ID] = false
		}
	case "left", "h":
		it := itemAt(m.items(), m.cursor)
		switch {
		case it.pipeline != nil:
			m.collapsed[it.pipeline.ID] = true
		case it.pjJob != nil:
			// Collapsing hides the job under the cursor; re-pin to the parent
			// header so the cursor never lands on a now-hidden row.
			m.collapsed[it.pjPipe] = true
			m.repin(itemKey(item{pipeline: &pipeline.Pipeline{ID: it.pjPipe}}))
		case it.session != nil && it.hasKids:
			// Hide this agent's sub-tree; the header row stays, so re-pin to it (its
			// key is the session id) — the cursor never lands on a now-hidden child.
			m.collapsed[it.session.ID] = true
			m.repin(it.session.ID)
		}
	case "n":
		m.targetDir = m.activeDir()
		m.mode = modeNewAgent
		m.ta.Reset()
		m.ta.Focus()
		m.tn.Reset()
		m.roleIdx = 0 // reset the role picker to general on every fresh form
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
			return m, switchClientCmd(id, m.killWindow)
		}
		if it := itemAt(m.items(), m.cursor); it.pjJob != nil && it.pjJob.SessionID != "" {
			return m, switchClientCmd(it.pjJob.SessionID, m.killWindow)
		}
	case "d":
		if s := m.selected(); s != nil {
			m.status = "generating digest for " + s.ID + "…"
			return m, digestCmd(m.api, s.ID)
		}
	case "i":
		// Details overlay for an agent row, or a pipeline job row (its live agent
		// detail when running, else the job's stored detail).
		it := itemAt(m.items(), m.cursor)
		switch {
		case it.session != nil:
			m.mode = modeDetails
			m.vp.SetContent(detailBody(it.session, m.vp.Width))
			m.vp.GotoTop()
		case it.pjJob != nil:
			m.mode = modeDetails
			m.vp.SetContent(jobDetailBody(it.pjJob, it.pjSess, m.vp.Width))
			m.vp.GotoTop()
		}
	case "p":
		// On a pipeline row, p pauses a running pipeline / resumes a paused one.
		// Elsewhere it opens the pending-approvals view.
		if it := itemAt(m.items(), m.cursor); it.pipeline != nil {
			switch it.pipeline.Status {
			case pipeline.StatusRunning:
				m.status = "pausing " + it.pipeline.ID
				return m, pausePipelineCmd(m.api, it.pipeline.ID)
			case pipeline.StatusPaused:
				m.status = "resuming " + it.pipeline.ID
				return m, resumePipelineCmd(m.api, it.pipeline.ID)
			default:
				m.status = "can only pause a running pipeline"
			}
			return m, nil
		}
		if len(recognizedApprovals(m.approvals)) > 0 {
			m.mode = modeApprovals
			m.apprCursor = 0
		} else if !m.apprEnabled {
			m.status = "approvals disabled (enable approvals: true in config)"
		} else {
			m.status = "no approvals pending"
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
	header := stHeader.Render("warden") + "  " + conn
	if chip := pressureChip(m.pressure); chip != "" {
		header += "  " + chip
	}
	if !m.connected {
		header += "  " + stError.Render("daemon not running — start it with `warden daemon`")
	}
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	if m.mode == modeHelp {
		return header + "\n" + lipgloss.NewStyle().Width(m.w).Height(bodyH).Render(helpText())
	}
	if m.mode == modeInspector {
		body := titleBox("Context & Messages", m.vp.View(), m.w, bodyH)
		return header + "\n" + body + "\n" + stMuted.Render("read-only · ↑/↓ pgup/pgdn g/G scroll · c/esc back · q quit")
	}
	if m.mode == modeDigest {
		body := titleBox("Digest — "+m.digestID, m.vp.View(), m.w, bodyH)
		return header + "\n" + body + "\n" + stMuted.Render("↑/↓ pgup/pgdn g/G scroll · d/esc back · q quit")
	}
	if m.mode == modeDetails {
		body := titleBox("Details — "+m.detailTitle(), m.vp.View(), m.w, bodyH)
		keys := "↑/↓ pgup/pgdn g/G scroll · i/esc back · q quit"
		if m.selected() != nil { // rename applies to a standalone agent only
			keys = "↑/↓ pgup/pgdn g/G scroll · r rename · i/esc back · q quit"
		}
		return header + "\n" + body + "\n" + stMuted.Render(keys)
	}
	if m.mode == modeRename {
		body := titleBox("Details — "+m.selectedID(), m.vp.View(), m.w, bodyH)
		input := stPaneTitle.Render("Rename "+m.renameID+" (enter save · blank clears · esc cancel):") + " " + m.tn.View()
		return header + "\n" + body + "\n" + input
	}
	if m.mode == modeApprovals {
		body := titleBox("Approvals", approvalsBody(recognizedApprovals(m.approvals), m.apprCursor, m.w-2), m.w, bodyH)
		return header + "\n" + body + "\n" + stMuted.Render("1-9 answer · tab next · p/esc back · q quit")
	}
	title := fmt.Sprintf("Agents (%d)", len(m.sessions))
	body := titleBox(title, renderList(m.items(), m.cursor, m.w-2, bodyH-2), m.w, bodyH)

	// Lean teaser — the full keymap (o/d/i/c/r/x/←→/D…) lives in the ? overlay, so
	// this stays short enough to fit the narrow list pane and always show `? help`.
	footer := stMuted.Render("enter open · n new · o dir · s send · a attach · i info · x kill · ? help · q quit")
	if m.status != "" {
		footer = stStatus.Render(m.status)
	}
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — "+abbrevHome(m.targetDir)+"  (tab: dir · ctrl+n: name · ctrl+r: role · ctrl+s submit (blank = open Claude & wait) · esc cancel)") +
			"\n" + m.ta.View() +
			"\n" + stMuted.Render("name: ") + newAgentNameLabel(m.tn.Value()) +
			stMuted.Render("  ·  role: ") + m.selectedRoleName()
	case modeNewAgentName:
		footer = stPaneTitle.Render("Agent name (enter/esc back to prompt · blank = auto-name):") + " " + m.tn.View()
	case modeNewAgentRole:
		footer = stPaneTitle.Render("Role (↑/↓ or j/k select · enter/esc back to prompt):") + "\n" + m.rolePickerView()
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

// selectedRole returns the role name chosen in the new-agent form. The general
// role (index 0 / no persona) canonicalizes to "" so a plain spawn stays
// byte-identical to today.
func (m listPaneModel) selectedRole() string {
	if m.roleIdx <= 0 || m.roleIdx >= len(m.roles) {
		return ""
	}
	name := m.roles[m.roleIdx].Name
	if name == role.Default {
		return ""
	}
	return name
}

// selectedRoleName is the display label for the chosen role (never blank).
func (m listPaneModel) selectedRoleName() string {
	if m.roleIdx >= 0 && m.roleIdx < len(m.roles) {
		return m.roles[m.roleIdx].Name
	}
	return role.Default
}

// rolePickerView renders the built-in role catalog with the selected role
// marked and its one-line description shown beneath.
func (m listPaneModel) rolePickerView() string {
	if len(m.roles) == 0 {
		return stMuted.Render("(no roles)")
	}
	var b strings.Builder
	for i, r := range m.roles {
		if i == m.roleIdx {
			b.WriteString(stCursor.Render("› " + r.Name))
		} else {
			b.WriteString(stMuted.Render("  " + r.Name))
		}
		if i < len(m.roles)-1 {
			b.WriteString("  ")
		}
	}
	desc := m.roles[m.roleIdx].Description
	if desc == "" {
		desc = "no persona — behaves exactly like a plain agent"
	}
	return b.String() + "\n" + stMuted.Render(desc)
}

// newAgentNameLabel renders the name chosen in the new-agent form, or a muted
// "(auto)" hint when it is blank (warden will derive one after spawn).
func newAgentNameLabel(name string) string {
	if strings.TrimSpace(name) == "" {
		return stMuted.Render("(auto)")
	}
	return name
}

// killCockpitArgs returns the tmux command(s) `q`/`ctrl+c` runs to tear the
// cockpit down, run from inside the list pane. In the classic cockpit the
// cockpit owns its own session, so it drops the <prefix> Enter override
// buildCockpit installed and kills the whole session (no target = current).
// In the tmux-native cockpit (killWindow) the cockpit is just one window in the
// user's own session, so we kill only that window — killing the session would
// take the user's entire tmux session down — and we leave their key bindings
// untouched (the native build never rebinds Enter).
func killCockpitArgs(killWindow bool) [][]string {
	if killWindow {
		return [][]string{{"kill-window"}}
	}
	return [][]string{{"unbind-key", "Enter"}, {"kill-session"}}
}

// killCockpitCmd tears the cockpit down. All calls are best-effort (harmless if
// not inside tmux); the subsequent tea.Quit still exits cleanly either way.
func killCockpitCmd(killWindow bool) tea.Cmd {
	return func() tea.Msg {
		for _, argv := range killCockpitArgs(killWindow) {
			_ = exec.Command("tmux", argv...).Run()
		}
		return nil
	}
}

// switchClientCmd moves the cockpit's tmux client to the selected agent's
// session. The list pane runs inside a tmux session, where `tmux attach` refuses
// to nest — `switch-client` is the correct primitive. In the classic cockpit
// buildCockpit binds <prefix> Enter to switch-client -l; the native cockpit does
// not rebind anything, so it points the user at tmux's default <prefix> L
// (switch-client -l). We flash the right hint so the user can get back.
func switchClientCmd(id string, killWindow bool) tea.Cmd {
	hint := "warden: press Ctrl-b Enter to return to the dashboard"
	if killWindow {
		hint = "warden: press Ctrl-b L to return to the dashboard"
	}
	return func() tea.Msg {
		if err := exec.Command("tmux", "switch-client", "-t", id).Run(); err != nil {
			return attachDoneMsg{err: err}
		}
		_ = exec.Command("tmux", "display-message", hint).Run()
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
// a live agent returns its tmux session to attach; a terminal agent (incl. a
// tombstone parent) has no live tmux, so it returns the agent id for a stored-
// detail render. Returns all empty for rows with nothing to open (headers,
// placeholders, pending jobs).
func cockpitDetailCmd(it item) (attach, jobPipe, jobID, agentDetail string) {
	if it.pjJob != nil {
		if jobIsTerminal(it.pjJob.Status) {
			return "", it.pjPipe, it.pjJob.ID, ""
		}
		return it.pjJob.SessionID, "", "", ""
	}
	if it.session != nil {
		if liveStatus(it.session.Status) {
			return it.session.TmuxSession, "", "", ""
		}
		return "", "", "", it.session.ID
	}
	return "", "", "", ""
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

// respawnAgentDetailArgs builds the tmux command that replaces the detail pane
// with a render of one terminal agent's stored detail (self re-invoked as a
// hidden pane) — the agent parallel to respawnJobDetailArgs.
func respawnAgentDetailArgs(detailPane, self, agentID string) []string {
	return []string{"respawn-pane", "-k", "-t", detailPane,
		self + " tui --pane=agentdetail --agent=" + agentID}
}

// openAgentDetailCmd renders a terminal agent's stored detail into the detail pane.
func openAgentDetailCmd(detailPane, agentID string) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{err: exec.Command("tmux", respawnAgentDetailArgs(detailPane, self, agentID)...).Run()}
	}
}

// RunListPane runs the top-left cockpit pane; detailPane is the tmux id of the
// detail pane it drives (opened on Enter). killWindow scopes the `q` teardown to
// the cockpit window instead of the whole session — set in the tmux-native
// cockpit, where the cockpit lives inside the user's own tmux session.
func RunListPane(a api, detailPane string, killWindow bool) error {
	m := newListPane(a, detailPane)
	m.killWindow = killWindow
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
