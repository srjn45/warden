package tui

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/store"
)

// controlPaneModel is the top-left cockpit pane: the agents list plus the
// new/send/terminate/attach actions. It owns selection: on Enter it opens the
// selected agent in the agent pane via respawn-pane.
type controlPaneModel struct {
	api            api
	agentPane      string // tmux pane id of the agent pane this list drives
	terminalPane   string // tmux pane id of the terminal pane this list drives ("" in tmux-native, which has no terminal pane)
	sessions       []*store.Session
	cursor         int
	ta             textarea.Model
	ti             textinput.Model
	tp             textinput.Model
	tn             textinput.Model // agent name input (new-agent form + rename)
	openedDirs     map[string]time.Time
	dirCandidates  []string
	targetDir      string
	roles          []role.Role     // built-in role catalog for the new-agent picker
	roleIdx        int             // selected role in the new-agent form (0 ⇒ general)
	backends       []backendChoice // registered backend catalog for the new-agent picker
	backendIdx     int             // selected backend in the new-agent form (0 ⇒ claude default)
	mode           mode
	status         string
	connected      bool
	pendingSelect  string
	pipelines      []*pipeline.Pipeline
	collapsed      map[string]bool // pipeline id → jobs hidden in the list
	seen           map[string]bool // pipeline ids the default-collapse has been applied to
	pressure       client.PressureStatus
	pendingPrompt  string
	pendingName    string // name typed in the new-agent form, held across the pressure confirm
	pendingDir     string
	pendingRole    string                 // role chosen in the new-agent form, held across the pressure confirm
	pendingBackend string                 // backend chosen in the new-agent form, held across the pressure confirm
	renameID       string                 // agent id being renamed (modeRename)
	spawnVerdict   string                 // reason text for the confirm prompt; "" when not confirming
	pendingDelete  string                 // pid awaiting delete confirmation; "" when not confirming
	ctxEntries     []client.ContextEntry  // inspector: shared-context snapshot
	messages       []client.Message       // inspector: recent message traffic
	vp             viewport.Model         // scroll viewport (modeInspector / modeDigest)
	approvals      []approval.View        // pending tool-permission prompts
	apprEnabled    bool                   // approvals config setting on
	apprCursor     int                    // focused recognized approval (modeApprovals)
	digest         *digest.Digest         // last fetched digest (modeDigest)
	digestID       string                 // agent id the digest is for
	autopilot      client.AutopilotStatus // last fetched autopilot status
	backendsState  client.BackendsState   // agent-backend registry snapshot (modeBackends)
	backendCursor  int                    // focused row in the Backends page
	w, h           int
	ready          bool
	// focused becomes true once the cursor has landed on a real row. Until then
	// (a freshly-loaded cockpit) the cursor auto-snaps to the first entity rather
	// than sitting on the always-present Approvals section header — so opening the
	// cockpit lands you on the first agent/pipeline, matching pre-sections UX.
	focused bool
	// defaultTerminalReady guards the startup "ensure ≥1 terminal" step so it runs
	// once: on the first session list we either adopt an existing live terminal
	// into the terminal pane or spawn a default one in the launch cwd (§5).
	defaultTerminalReady bool
	// openedAgent is the id of the agent currently shown in the agent pane; it
	// anchors §8 M-a/M-p rotation (advance from here) and is set on every agent
	// open/rotate. Empty until the first agent is opened.
	openedAgent string
	// openedAgentDir is the source dir of the agent last opened into the agent pane.
	// `t` opens a terminal there (§6.1); empty ⇒ fall back to $HOME.
	openedAgentDir string
	// openedTerminal is the id of the terminal currently shown in the terminal pane
	// (anchors §8 rotation, added in stage 5; set on open/create here).
	openedTerminal string
	// termChoiceDir is the dir the modeTerminalChoice prompt (`t`) will create/focus
	// a terminal in.
	termChoiceDir string
	// termInfo holds each terminal's live cwd/branch (polled from its tmux pane on
	// the tick, §7), keyed by session id; feeds the Terminals-section names.
	termInfo map[string]terminalLiveInfo
	// killWindow scopes the `q`/`ctrl+c` teardown to the cockpit's tmux *window*
	// instead of the whole session. It is set in the tmux-native cockpit, where
	// the cockpit is a window inside the user's own session — killing the session
	// there would take the user's entire tmux session down with it.
	killWindow bool
}

// quitCmd is what `q`/`ctrl+c` runs: tear the whole cockpit down (killCockpitCmd
// kills the hosting tmux session, taking the terminal + agent panes with it) and
// quit. The same teardown serves both flavors. Locally the user lands back in
// their shell; on the web the daemon notices the session vanished and tells the
// browser to leave the full-screen TUI (→ home) — see daemon.bridgeTmux.
func (m controlPaneModel) quitCmd() tea.Cmd {
	return tea.Sequence(killCockpitCmd(m.killWindow), tea.Quit)
}

func newListPane(a api, agentPane, terminalPane string) controlPaneModel {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	tp := textinput.New()
	tp.Placeholder = "~/path/to/dir"
	tn := textinput.New()
	tn.Placeholder = "agent-name (optional; blank = auto)"
	tn.CharLimit = 32
	return controlPaneModel{
		api: a, ta: ta, ti: ti, tp: tp, tn: tn, agentPane: agentPane, terminalPane: terminalPane,
		// roles is the fixed built-in catalog embedded in the binary (general
		// first), so the picker is populated synchronously — no daemon round-trip.
		roles: role.All(),
		// backends is the registered backend catalog (claude/default first), read
		// straight from the in-process registry — same synchronous, no-round-trip
		// pattern as roles.
		backends:   backendCatalog(),
		openedDirs: map[string]time.Time{}, collapsed: map[string]bool{}, seen: map[string]bool{}, connected: true,
		termInfo: map[string]terminalLiveInfo{},
		vp:       viewport.New(0, 0),
	}
}

// items assembles the control-pane navigator as four fixed, collapsible
// top-level sections (spec §4), in order: Approvals · Pipelines · Agents ·
// Terminals. Each section header is always present; collapsing one (its secKey in
// m.collapsed) folds away its whole sub-tree. Terminals are split out of the
// Agents tree entirely (§3) and rendered with their §7 names.
func (m controlPaneModel) items() []item {
	agents, terminals := splitByKind(flatSessions(m.sessions, m.pipelines))
	var out []item

	// ── Approvals: recognized menus only; unrecognized prompts must be attached to.
	rec := recognizedApprovals(m.approvals)
	apprCollapsed := m.collapsed[secKey(secApprovals)]
	out = append(out, item{section: secApprovals, secCount: len(rec), collapsed: apprCollapsed})
	if m.apprEnabled && !apprCollapsed {
		for i := range rec {
			v := rec[i] // fresh var → distinct pointer per row
			out = append(out, item{apprView: &v, apprIdx: i})
		}
	}

	// ── Pipelines: pipeline-owned sessions live here, under their pipeline.
	pipeCollapsed := m.collapsed[secKey(secPipelines)]
	out = append(out, item{section: secPipelines, secCount: len(m.pipelines), collapsed: pipeCollapsed})
	if !pipeCollapsed {
		out = append(out, pipelineItems(m.pipelines, m.sessions, m.collapsed)...)
	}

	// ── Agents: dir-grouped, subagents nested per the §4.1 render rule.
	agentCollapsed := m.collapsed[secKey(secAgents)]
	out = append(out, item{section: secAgents, secCount: len(agents), collapsed: agentCollapsed})
	if !agentCollapsed {
		out = append(out, buildItems(agents, m.openedDirs, m.collapsed)...)
	}

	// ── Terminals: plain shells, named per §7 (live cwd/branch from termInfo).
	termCollapsed := m.collapsed[secKey(secTerminals)]
	out = append(out, item{section: secTerminals, secCount: len(terminals), collapsed: termCollapsed})
	if !termCollapsed {
		out = append(out, terminalItems(terminals, m.termInfo)...)
	}
	markOpened(out, m.openedAgent, m.openedTerminal)
	return out
}

// markOpened flags the rows currently shown in the cockpit panes so they render
// with the stOpened marker: the openedAgent (an Agents-section agent row, or a
// Pipelines job row whose live session is that agent) and the openedTerminal (a
// Terminals-section row). Because it keys off m.openedAgent/m.openedTerminal —
// which both Enter-open and §8 Alt+a/p/t rotation set — the highlight tracks the
// panes without any extra plumbing. Empty ids match nothing.
func markOpened(items []item, openedAgent, openedTerminal string) {
	for i := range items {
		switch {
		case items[i].session != nil && items[i].session.IsTerminal():
			items[i].opened = openedTerminal != "" && items[i].session.ID == openedTerminal
		case items[i].session != nil:
			items[i].opened = openedAgent != "" && items[i].session.ID == openedAgent
		case items[i].pjSess != nil:
			items[i].opened = openedAgent != "" && items[i].pjSess.ID == openedAgent
		}
	}
}

func (m controlPaneModel) selected() *store.Session { return itemAt(m.items(), m.cursor).session }

func (m controlPaneModel) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

// selectedKey is the cursor's re-pin anchor across refreshes. It reports "" while
// the cockpit is unfocused so repin auto-snaps to the first entity on first load
// (rather than pinning the always-present Approvals header).
func (m controlPaneModel) selectedKey() string {
	if !m.focused {
		return ""
	}
	return itemKey(itemAt(m.items(), m.cursor))
}

// backendRow returns the backend under the Backends-page cursor, or false when the
// registry is empty / the cursor is out of range.
func (m controlPaneModel) backendRow() (client.Backend, bool) {
	if m.backendCursor >= 0 && m.backendCursor < len(m.backendsState.Backends) {
		return m.backendsState.Backends[m.backendCursor], true
	}
	return client.Backend{}, false
}

// detailTitle is the label for the modeDetails overlay: the agent id, or
// "pipeline/job" for a pipeline job row. The cursor cannot move while the overlay
// is open (keys scroll), so the item under it is stable across the overlay's life.
func (m controlPaneModel) detailTitle() string {
	if it := itemAt(m.items(), m.cursor); it.pjJob != nil {
		return it.pjPipe + "/" + it.pjJob.ID
	}
	return m.selectedID()
}

func (m controlPaneModel) fallbackDir() string {
	d, _ := os.Getwd()
	return d
}

func (m controlPaneModel) activeDir() string {
	return activeDir(m.items(), m.cursor, m.fallbackDir())
}

// liveTerminals returns the live Kind=terminal sessions, ordered by CreatedAt so
// the first is the stable "terminal 1" (matches the Terminals-section ordinal).
func (m controlPaneModel) liveTerminals() []*store.Session {
	var out []*store.Session
	for _, s := range m.sessions {
		if s.IsTerminal() && liveStatus(s.Status) {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// liveAgents returns every live agent (non-terminal) session ordered by CreatedAt
// — the M-a rotation set (§8). CreatedAt order is stable across refreshes so the
// cycle stays predictable as the list re-sorts.
func (m controlPaneModel) liveAgents() []*store.Session {
	var out []*store.Session
	for _, s := range m.sessions {
		if !s.IsTerminal() && liveStatus(s.Status) {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// pipelineAgents returns the live agents that belong to a pipeline, ordered
// pipeline-by-pipeline then job order (the M-p rotation set, §8: "pipeline >
// agents"). A job with no live session (pending/reaped) is skipped.
func (m controlPaneModel) pipelineAgents() []*store.Session {
	byID := make(map[string]*store.Session, len(m.sessions))
	for _, s := range m.sessions {
		byID[s.ID] = s
	}
	var out []*store.Session
	for _, p := range m.pipelines {
		for i := range p.Jobs {
			sid := p.Jobs[i].SessionID
			if sid == "" {
				continue
			}
			if s := byID[sid]; s != nil && !s.IsTerminal() && liveStatus(s.Status) {
				out = append(out, s)
			}
		}
	}
	return out
}

// stepInCycle returns the entity `step` positions from the one with id `current`
// in set (step +1 = forward / the next, -1 = reverse / the previous), wrapping in
// both directions. When current is absent (nothing open yet, or it just exited) a
// forward step starts at the first entity and a reverse step at the last, so the
// very first rotation lands predictably. nil only for an empty set.
func stepInCycle(set []*store.Session, current string, step int) *store.Session {
	n := len(set)
	if n == 0 {
		return nil
	}
	idx := -1
	for i, s := range set {
		if s.ID == current {
			idx = i
			break
		}
	}
	if idx == -1 {
		if step < 0 {
			return set[n-1]
		}
		return set[0]
	}
	// Go's % keeps the sign of the dividend, so bias by +n before the mod to make
	// a negative step wrap correctly.
	return set[((idx+step)%n+n)%n]
}

// nextInCycle advances forward one position (§8 forward rotation).
func nextInCycle(set []*store.Session, current string) *store.Session {
	return stepInCycle(set, current, 1)
}

// rotateTerminal advances the terminal pane by step over the live terminals (§8
// M-t forward / M-T reverse), grabbing focus since terminals are interactive. A
// no-op with a status hint when there is no terminal pane or no terminals.
func (m controlPaneModel) rotateTerminal(step int) (tea.Model, tea.Cmd) {
	if m.terminalPane == "" {
		m.status = "no terminal pane in this cockpit"
		return m, nil
	}
	next := stepInCycle(m.liveTerminals(), m.openedTerminal, step)
	if next == nil {
		m.status = "no terminals"
		return m, nil
	}
	m.openedTerminal = next.ID
	m.status = ""
	return m, openInTerminalCmd(m.terminalPane, next.TmuxSession, true)
}

// rotateAgent advances the agent pane by step over the entities in set (§8
// M-a/M-p forward / M-A/M-P reverse), keeping focus in the control pane
// (watch-mode, §6). Rotation traverses live agents only, so it always attaches
// directly. emptyMsg is flashed when the set is empty.
func (m controlPaneModel) rotateAgent(set []*store.Session, emptyMsg string, step int) (tea.Model, tea.Cmd) {
	if m.agentPane == "" {
		return m, nil
	}
	next := stepInCycle(set, m.openedAgent, step)
	if next == nil {
		m.status = emptyMsg
		return m, nil
	}
	m.openedAgent = next.ID
	m.openedAgentDir = sourceDir(next)
	m.status = ""
	// Grab focus on the agent pane so the rotation drops you into the session, the
	// same way rotateTerminal focuses the terminal pane (§8).
	return m, openInDetailCmd(m.agentPane, next.TmuxSession, true)
}

// termDir is a terminal's directory for §6.1 matching: its live pane cwd when
// polled (termInfo), else its stored cwd.
func (m controlPaneModel) termDir(t *store.Session) string {
	if li, ok := m.termInfo[t.ID]; ok && li.cwd != "" {
		return li.cwd
	}
	return terminalCwd(t)
}

// liveTerminalInDir returns the first live terminal whose dir matches dir, or nil.
func (m controlPaneModel) liveTerminalInDir(dir string) *store.Session {
	for _, t := range m.liveTerminals() {
		if m.termDir(t) == dir {
			return t
		}
	}
	return nil
}

// ensureDefaultTerminalCmd guarantees the cockpit shows a terminal in its terminal
// pane at startup (§5): it adopts the first existing live terminal, or spawns a
// default one in the launch cwd when none exists. It runs once (guarded by
// defaultTerminalReady) and is a no-op in the tmux-native cockpit (no terminal
// pane). The startup open does not steal focus — the control pane stays focused.
func (m *controlPaneModel) ensureDefaultTerminalCmd() tea.Cmd {
	if m.terminalPane == "" || m.defaultTerminalReady {
		return nil
	}
	m.defaultTerminalReady = true
	if live := m.liveTerminals(); len(live) > 0 {
		m.openedTerminal = live[0].ID
		return openInTerminalCmd(m.terminalPane, live[0].TmuxSession, false)
	}
	return spawnTerminalCmd(m.api, m.fallbackDir(), false)
}

// bodyH is the height of the framed pane body, shared by View and the inspector
// viewport sizing so the two never disagree.
func (m controlPaneModel) bodyH() int {
	if h := m.h - 2; h >= 3 {
		return h
	}
	return 3
}

// setInspectorContent re-renders the inspector body into the viewport, preserving
// the current scroll offset (SetContent/SetYOffset clamp it), so refresh ticks and
// resizes do not snap the view back to the top.
func (m *controlPaneModel) setInspectorContent() {
	off := m.vp.YOffset
	m.vp.SetContent(inspectorBody(m.ctxEntries, m.messages, m.vp.Width))
	m.vp.SetYOffset(off)
}

// applyDefaultCollapse folds away each newly-seen completed pipeline once, so the
// list opens with finished work collapsed. Pipelines are auto-collapsed only on
// first sighting (tracked in `seen`), so a manual expand survives later refreshes.
func (m *controlPaneModel) applyDefaultCollapse() {
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

func (m controlPaneModel) Init() tea.Cmd {
	return tea.Batch(listCmd(m.api), pipelinesCmd(m.api), approvalsCmd(m.api), autopilotCmd(m.api), tick())
}

func (m controlPaneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		cmds := []tea.Cmd{listCmd(m.api), pipelinesCmd(m.api), approvalsCmd(m.api), pressureCmd(m.api), autopilotCmd(m.api), tick()}
		if m.mode == modeInspector {
			cmds = append(cmds, contextCmd(m.api), messagesCmd(m.api))
		}
		if m.mode == modeBackends {
			cmds = append(cmds, backendsCmd(m.api)) // keep the table + limited-until countdown fresh
		}
		// Poll live cwd/branch for terminal names (§7) — only when a terminal pane
		// exists and at least one terminal is live to read.
		if m.terminalPane != "" {
			if terms := m.liveTerminals(); len(terms) > 0 {
				cmds = append(cmds, terminalInfoCmd(terms))
			}
		}
		return m, tea.Batch(cmds...)
	case pressureMsg:
		if msg.err == nil {
			m.pressure = msg.status
		}
		return m, nil
	case autopilotMsg:
		if msg.err == nil {
			m.autopilot = msg.status
		}
		return m, nil
	case autopilotToggleDoneMsg:
		if msg.err == nil {
			m.autopilot = msg.status
		} else {
			m.status = "autopilot: " + msg.err.Error()
		}
		return m, nil
	case backendsMsg:
		if msg.err != nil {
			// Surface an action's failure (a rejected default, a bad tier); on a
			// passive refresh blip keep the last good table unless it is still empty.
			if msg.action || len(m.backendsState.Backends) == 0 {
				m.status = "backends: " + msg.err.Error()
			}
			return m, nil
		}
		m.backendsState = sortBackendsState(msg.state)
		if m.backendCursor >= len(m.backendsState.Backends) {
			m.backendCursor = len(m.backendsState.Backends) - 1
		}
		if m.backendCursor < 0 {
			m.backendCursor = 0
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
		// Once we have a session list, make sure a default terminal exists and is
		// shown in the terminal pane (§5). Runs once (defaultTerminalReady guard).
		return m, m.ensureDefaultTerminalCmd()
	case terminalSpawnedMsg:
		if msg.err != nil {
			m.status = "terminal failed: " + msg.err.Error()
			return m, nil
		}
		m.openedTerminal = msg.id
		m.status = ""
		// The spawned terminal's tmux session is its id (lifecycle sets
		// TmuxSession=id). Refresh the list so it appears under Terminals and open
		// it in the terminal pane (focusing it only on an explicit create/`t`).
		cmds := []tea.Cmd{listCmd(m.api)}
		if m.terminalPane != "" {
			cmds = append(cmds, openInTerminalCmd(m.terminalPane, msg.id, msg.focus))
		}
		return m, tea.Batch(cmds...)
	case terminalInfoMsg:
		if msg.info != nil {
			m.termInfo = msg.info
		}
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

func (m *controlPaneModel) repin(prevKey string) {
	items := m.items()
	want := prevKey
	if m.pendingSelect != "" {
		want = m.pendingSelect
	}
	if want != "" {
		for i, it := range items {
			if itemKey(it) == want {
				m.cursor = i
				m.focused = true
				if want == m.pendingSelect {
					m.pendingSelect = ""
				}
				return
			}
		}
	}
	// Unfocused (fresh cockpit): snap to the first real entity, skipping the
	// always-present section headers, so the cursor opens on the first agent /
	// pipeline / approval rather than the Approvals header.
	if !m.focused {
		if idx := firstEntityCursor(items); idx >= 0 {
			m.cursor = idx
			m.focused = true
			return
		}
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// firstEntityCursor returns the index of the first non-section row (an approval,
// pipeline, agent, terminal, or dir placeholder), or -1 when the list holds only
// section headers.
func firstEntityCursor(items []item) int {
	for i, it := range items {
		if it.section == "" {
			return i
		}
	}
	return -1
}

func (m controlPaneModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
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
		case tea.KeyCtrlT:
			// Switch to the backend picker. Defaults to claude. (ctrl+b is the tmux
			// prefix, so the backend picker binds to ctrl+t instead.)
			m.mode = modeNewAgentBackend
			m.ta.Blur()
			return m, nil
		case tea.KeyCtrlS:
			// An empty prompt is intentional: it opens the agent in the target dir
			// and waits for the user to type instructions into it directly (for the
			// terminal backend, that is just a plain shell).
			prompt := strings.TrimSpace(m.ta.Value())
			name := strings.TrimSpace(m.tn.Value())
			role := m.selectedRole()
			backend := m.selectedBackend()
			m.mode = modeNormal
			m.ta.Blur()
			m.pendingPrompt, m.pendingName, m.pendingDir, m.pendingRole, m.pendingBackend = prompt, name, m.targetDir, role, backend
			return m, spawnCmd(m.api, prompt, name, m.targetDir, role, backend, false)
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
	case modeNewAgentBackend:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyEnter:
			m.mode = modeNewAgent
			m.ta.Focus()
			return m, nil
		case tea.KeyUp, tea.KeyLeft:
			if len(m.backends) > 0 {
				m.backendIdx = (m.backendIdx - 1 + len(m.backends)) % len(m.backends)
			}
			return m, nil
		case tea.KeyDown, tea.KeyRight, tea.KeyTab:
			if len(m.backends) > 0 {
				m.backendIdx = (m.backendIdx + 1) % len(m.backends)
			}
			return m, nil
		}
		// j/k also cycle, matching the list's vim-style navigation.
		switch msg.String() {
		case "k":
			if len(m.backends) > 0 {
				m.backendIdx = (m.backendIdx - 1 + len(m.backends)) % len(m.backends)
			}
		case "j":
			if len(m.backends) > 0 {
				m.backendIdx = (m.backendIdx + 1) % len(m.backends)
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
			prompt, name, dir, role, backend := m.pendingPrompt, m.pendingName, m.pendingDir, m.pendingRole, m.pendingBackend
			m.spawnVerdict = ""
			m.status = "spawning (forced)…"
			return m, spawnCmd(m.api, prompt, name, dir, role, backend, true)
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
	case modeBackends:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, m.quitCmd()
		case "esc", "b":
			m.mode = modeNormal
			m.status = ""
			return m, nil
		case "down", "j":
			if m.backendCursor < len(m.backendsState.Backends)-1 {
				m.backendCursor++
			}
			return m, nil
		case "up", "k":
			if m.backendCursor > 0 {
				m.backendCursor--
			}
			return m, nil
		case "r":
			m.status = "rescanning backends…"
			return m, rescanBackendsCmd(m.api)
		case "m":
			next := nextThinkingMode(thinkingModeOf(m.backendsState))
			m.status = "thinking mode → " + next
			return m, setThinkingModeCmd(m.api, next)
		case "t":
			b, ok := m.backendRow()
			if !ok {
				return m, nil
			}
			if b.IsLocal {
				m.status = "local tier is system-set"
				return m, nil
			}
			next := nextTier(b.Tier)
			m.status = b.ID + " tier → " + next
			return m, setBackendTierCmd(m.api, b.ID, next)
		case "d", "enter":
			b, ok := m.backendRow()
			if !ok {
				return m, nil
			}
			if b.IsLocal {
				m.status = "local cannot be the default"
				return m, nil
			}
			m.status = "default → " + b.ID
			return m, setDefaultBackendCmd(m.api, b.ID)
		case "e", " ":
			b, ok := m.backendRow()
			if !ok {
				return m, nil
			}
			m.status = b.ID + " → " + enabledWord(!b.Enabled)
			return m, setBackendEnabledCmd(m.api, b.ID, !b.Enabled)
		}
		return m, nil
	case modeTerminalChoice:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, m.quitCmd()
		case "esc", "n", "N":
			m.mode = modeNormal
			m.status = ""
			return m, nil
		case "c", "C":
			// Create a fresh terminal in the chosen dir and open it (focused).
			dir := m.termChoiceDir
			m.mode = modeNormal
			m.status = "opening terminal in " + abbrevHome(dir)
			return m, spawnTerminalCmd(m.api, dir, true)
		case "f", "F":
			// Focus an existing live terminal in that dir, else fall back to create.
			dir := m.termChoiceDir
			m.mode = modeNormal
			if t := m.liveTerminalInDir(dir); t != nil {
				m.openedTerminal = t.ID
				m.status = ""
				return m, openInTerminalCmd(m.terminalPane, t.TmuxSession, true)
			}
			m.status = "no terminal in " + abbrevHome(dir) + " — creating one"
			return m, spawnTerminalCmd(m.api, dir, true)
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
	case "ctrl+a":
		// Toggle autopilot on/off. The result message updates m.autopilot.
		return m, autopilotToggleCmd(m.api, !m.autopilot.Enabled)
	case "alt+t":
		// §8 global rotation: advance the terminal pane to the next live terminal
		// (delivered here by the tmux M-t root binding, from any pane).
		return m.rotateTerminal(1)
	case "alt+T":
		// §8 reverse: Alt+Shift+t (tmux M-T) steps the terminal pane backward.
		return m.rotateTerminal(-1)
	case "alt+a":
		// §8: advance the agent pane to the next live agent (all agents).
		return m.rotateAgent(m.liveAgents(), "no agents", 1)
	case "alt+A":
		// §8 reverse: Alt+Shift+a (tmux M-A) steps the agent pane backward.
		return m.rotateAgent(m.liveAgents(), "no agents", -1)
	case "alt+p":
		// §8: advance the agent pane to the next pipeline agent (pipeline > agents order).
		return m.rotateAgent(m.pipelineAgents(), "no pipeline agents", 1)
	case "alt+P":
		// §8 reverse: Alt+Shift+p (tmux M-P) steps the pipeline-agent rotation backward.
		return m.rotateAgent(m.pipelineAgents(), "no pipeline agents", -1)
	case "c":
		// Open the read-only shared-context + message-traffic inspector and
		// kick off an immediate fetch (the tick keeps it fresh while open).
		m.mode = modeInspector
		m.setInspectorContent()
		m.vp.GotoTop() // a freshly opened inspector starts at the top
		return m, tea.Batch(contextCmd(m.api), messagesCmd(m.api))
	case "enter":
		it := itemAt(m.items(), m.cursor)
		if it.section != "" {
			// A section header toggles its own collapse; re-pin so the cursor rides
			// the header rather than a row that just appeared/vanished beneath it.
			key := secKey(it.section)
			m.collapsed[key] = !m.collapsed[key]
			m.repin(key)
			return m, nil
		}
		if it.apprView != nil {
			if m.apprEnabled && len(recognizedApprovals(m.approvals)) > 0 {
				m.mode = modeApprovals
				m.apprCursor = it.apprIdx // open the overlay focused on this prompt
			}
			return m, nil
		}
		// A terminal opens in the terminal pane (and grabs focus — terminals are
		// interactive, §6). It never routes to the agent pane.
		if it.session != nil && it.session.IsTerminal() {
			if !liveStatus(it.session.Status) {
				return m, nil // a dead terminal is removed from the list (§11); nothing to open
			}
			if m.terminalPane == "" {
				m.status = "no terminal pane in this cockpit (press a to attach full-screen)"
				return m, nil
			}
			m.openedTerminal = it.session.ID
			return m, openInTerminalCmd(m.terminalPane, it.session.TmuxSession, true)
		}
		if m.agentPane != "" {
			attach, jobPipe, jobID, agentDetail := cockpitDetailCmd(it)
			switch {
			case attach != "":
				if it.session != nil {
					m.openedAgent = it.session.ID            // anchors §8 M-a/M-p rotation
					m.openedAgentDir = sourceDir(it.session) // `t` opens a terminal here (§6.1)
				}
				return m, openInDetailCmd(m.agentPane, attach, false)
			case jobID != "":
				// A terminal job's agent tmux is gone — render its stored detail
				// instead of attaching to a dead session (which leaves a blank pane).
				return m, openJobDetailCmd(m.agentPane, jobPipe, jobID)
			case agentDetail != "":
				// A terminal agent (tombstone or finished) has no live tmux — render
				// its stored detail rather than attaching to a dead session.
				return m, openAgentDetailCmd(m.agentPane, agentDetail)
			}
		}
	case "down", "j":
		m.focused = true
		if m.cursor < len(m.items())-1 {
			m.cursor++
		}
	case "up", "k":
		m.focused = true
		if m.cursor > 0 {
			m.cursor--
		}
	case "right", "l":
		it := itemAt(m.items(), m.cursor)
		switch {
		case it.section != "":
			m.collapsed[secKey(it.section)] = false
		case it.pipeline != nil:
			m.collapsed[it.pipeline.ID] = false
		case it.session != nil && it.hasKids:
			m.collapsed[it.session.ID] = false
		}
	case "left", "h":
		it := itemAt(m.items(), m.cursor)
		switch {
		case it.section != "":
			// Fold the whole section away; re-pin to its header so the cursor never
			// lands on a now-hidden child.
			m.collapsed[secKey(it.section)] = true
			m.repin(secKey(it.section))
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
		m.roleIdx = 0    // reset the role picker to general on every fresh form
		m.backendIdx = 0 // reset the backend picker to claude on every fresh form
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
	case "b":
		// Open the agent-backend registry page and kick off an immediate load (the
		// tick keeps it fresh — including the limited-until countdown — while open).
		m.mode = modeBackends
		m.backendCursor = 0
		m.status = "loading backends…"
		return m, backendsCmd(m.api)
	case "t":
		// Open a terminal in the currently-opened agent's dir (§6.1). Not available
		// in the tmux-native cockpit, which has no terminal pane.
		if m.terminalPane == "" {
			m.status = "terminals need the cockpit terminal pane (unavailable in the tmux-native cockpit)"
			return m, nil
		}
		m.termChoiceDir = m.openedAgentDir
		if m.termChoiceDir == "" {
			m.termChoiceDir = homeDir()
		}
		m.mode = modeTerminalChoice
	case "?":
		m.mode = modeHelp
	}
	return m, nil
}

func (m controlPaneModel) View() string {
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
	if badge := autopilotBadge(m.autopilot); badge != "" {
		header += "  " + badge
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
	if m.mode == modeBackends {
		body := titleBox("Backends", backendsBody(m.backendsState, m.backendCursor), m.w, bodyH)
		footer := stMuted.Render("↑/↓ move · t tier · d default · e enable · r rescan · m mode · b/esc back")
		if m.status != "" {
			footer = stStatus.Render(m.status)
		}
		return header + "\n" + body + "\n" + footer
	}
	body := titleBox("Control", renderList(m.items(), m.cursor, m.w-2, bodyH-2), m.w, bodyH)

	// Lean teaser — the full keymap (o/d/i/c/r/x/←→/D…) lives in the ? overlay, so
	// this stays short enough to fit the narrow control pane and always show `? help`.
	footer := stMuted.Render("enter open · n new · t term · o dir · s send · a attach · x kill · ? help · q quit")
	if m.status != "" {
		footer = stStatus.Render(m.status)
	}
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — "+abbrevHome(m.targetDir)+"  (tab: dir · ctrl+n: name · ctrl+r: role · ctrl+t: backend · ctrl+s submit (blank = just open & wait) · esc cancel)") +
			"\n" + m.ta.View() +
			"\n" + stMuted.Render("name: ") + newAgentNameLabel(m.tn.Value()) +
			stMuted.Render("  ·  role: ") + m.selectedRoleName() +
			stMuted.Render("  ·  backend: ") + m.selectedBackendName()
	case modeNewAgentName:
		footer = stPaneTitle.Render("Agent name (enter/esc back to prompt · blank = auto-name):") + " " + m.tn.View()
	case modeNewAgentRole:
		footer = stPaneTitle.Render("Role (↑/↓ or j/k select · enter/esc back to prompt):") + "\n" + m.rolePickerView()
	case modeNewAgentBackend:
		footer = stPaneTitle.Render("Backend (↑/↓ or j/k select · enter/esc back to prompt):") + "\n" + m.backendPickerView()
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
	case modeTerminalChoice:
		footer = stPaneTitle.Render("Terminal in " + abbrevHome(m.termChoiceDir) + ":  (c)reate new  ·  (f)ocus existing  ·  esc cancel")
	}
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

// selectedRole returns the role name chosen in the new-agent form. The general
// role (index 0 / no persona) canonicalizes to "" so a plain spawn stays
// byte-identical to today.
func (m controlPaneModel) selectedRole() string {
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
func (m controlPaneModel) selectedRoleName() string {
	if m.roleIdx >= 0 && m.roleIdx < len(m.roles) {
		return m.roles[m.roleIdx].Name
	}
	return role.Default
}

// rolePickerView renders the built-in role catalog with the selected role
// marked and its one-line description shown beneath.
func (m controlPaneModel) rolePickerView() string {
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

// backendChoice is one entry in the new-agent backend picker: the registered id
// warden spawns with and its human-readable display name.
type backendChoice struct {
	id   string
	name string
}

// backendCatalog reads the registered agent backends from the in-process
// registry and returns them ordered for the picker: the default backend (claude)
// first, the rest alphabetical. The registry is populated at import time (the
// warden binary pulls in internal/agentbackend/backends via the CLI), so this is
// synchronous with no daemon round-trip — the same pattern as role.All().
func backendCatalog() []backendChoice {
	ids := agentbackend.IDs()
	sort.Slice(ids, func(i, j int) bool {
		// DefaultID sorts first; everything else alphabetically.
		if ids[i] == agentbackend.DefaultID {
			return true
		}
		if ids[j] == agentbackend.DefaultID {
			return false
		}
		return ids[i] < ids[j]
	})
	out := make([]backendChoice, 0, len(ids))
	for _, id := range ids {
		name := id
		if b, err := agentbackend.Get(id); err == nil {
			name = b.DisplayName()
		}
		out = append(out, backendChoice{id: id, name: name})
	}
	return out
}

// selectedBackend returns the backend id chosen in the new-agent form. The
// default (claude, index 0) canonicalizes to "" so a plain spawn stays
// byte-identical to today (the daemon resolves an empty backend to claude).
func (m controlPaneModel) selectedBackend() string {
	if m.backendIdx <= 0 || m.backendIdx >= len(m.backends) {
		return ""
	}
	id := m.backends[m.backendIdx].id
	if id == agentbackend.DefaultID {
		return ""
	}
	return id
}

// selectedBackendName is the display label for the chosen backend (never blank).
func (m controlPaneModel) selectedBackendName() string {
	if m.backendIdx >= 0 && m.backendIdx < len(m.backends) {
		return m.backends[m.backendIdx].name
	}
	return agentbackend.DefaultID
}

// backendPickerView renders the registered backend catalog with the selected
// backend marked and its id shown beneath.
func (m controlPaneModel) backendPickerView() string {
	if len(m.backends) == 0 {
		return stMuted.Render("(no backends)")
	}
	var b strings.Builder
	for i, c := range m.backends {
		if i == m.backendIdx {
			b.WriteString(stCursor.Render("› " + c.name))
		} else {
			b.WriteString(stMuted.Render("  " + c.name))
		}
		if i < len(m.backends)-1 {
			b.WriteString("  ")
		}
	}
	return b.String() + "\n" + stMuted.Render(m.backends[m.backendIdx].id)
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
// cockpit down, run from inside the control pane. In the classic cockpit the
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
// session. The control pane runs inside a tmux session, where `tmux attach` refuses
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

// respawnDetailArgs builds the tmux args that replace the agent pane's process
// with a live (nested) attach to the given agent's tmux session. `env -u TMUX`
// lets tmux attach from inside tmux; `respawn-pane -k` kills the placeholder
// (or the previously-opened agent) first.
func respawnDetailArgs(agentPane, agentSession string) []string {
	return []string{"respawn-pane", "-k", "-t", agentPane,
		"env -u TMUX tmux attach -t " + agentSession}
}

// openInDetailCmd opens the given agent's live session in the agent pane. When
// focus is set it then selects the agent pane so the user lands in it — the Alt
// rotation (§8) passes focus=true so cycling agents drops you straight into the
// session, mirroring the terminal rotation. Enter-open passes focus=false to keep
// the control pane focused for continued browsing (watch-mode, §6).
func openInDetailCmd(agentPane, agentSession string, focus bool) tea.Cmd {
	return func() tea.Msg {
		if err := exec.Command("tmux", respawnDetailArgs(agentPane, agentSession)...).Run(); err != nil {
			return attachDoneMsg{err: err}
		}
		if focus {
			_ = exec.Command("tmux", "select-pane", "-t", agentPane).Run()
		}
		return attachDoneMsg{err: nil}
	}
}

// openInTerminalCmd opens the given terminal's live session in the terminal pane
// (same respawn-pane attach the agent pane uses). When focus is set it then
// selects the terminal pane so the user can type immediately — terminals are
// interactive (§6). The default terminal opened at startup passes focus=false so
// the control pane keeps focus.
func openInTerminalCmd(terminalPane, tmuxSession string, focus bool) tea.Cmd {
	return func() tea.Msg {
		if err := exec.Command("tmux", respawnDetailArgs(terminalPane, tmuxSession)...).Run(); err != nil {
			return attachDoneMsg{err: err}
		}
		if focus {
			_ = exec.Command("tmux", "select-pane", "-t", terminalPane).Run()
		}
		return attachDoneMsg{err: nil}
	}
}

// cockpitDetailCmd decides what the cockpit shows in its agent pane for the item
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

// respawnJobDetailArgs builds the tmux command that replaces the agent pane with
// a render of one terminal job's stored detail (self re-invoked as a hidden pane).
func respawnJobDetailArgs(agentPane, self, pid, jobID string) []string {
	return []string{"respawn-pane", "-k", "-t", agentPane,
		self + " tui --pane=jobdetail --pipeline=" + pid + " --job=" + jobID}
}

// openJobDetailCmd renders a terminal job's stored detail into the agent pane.
func openJobDetailCmd(agentPane, pid, jobID string) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{err: exec.Command("tmux", respawnJobDetailArgs(agentPane, self, pid, jobID)...).Run()}
	}
}

// respawnAgentDetailArgs builds the tmux command that replaces the agent pane
// with a render of one terminal agent's stored detail (self re-invoked as a
// hidden pane) — the agent parallel to respawnJobDetailArgs.
func respawnAgentDetailArgs(agentPane, self, agentID string) []string {
	return []string{"respawn-pane", "-k", "-t", agentPane,
		self + " tui --pane=agentdetail --agent=" + agentID}
}

// openAgentDetailCmd renders a terminal agent's stored detail into the agent pane.
func openAgentDetailCmd(agentPane, agentID string) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{err: exec.Command("tmux", respawnAgentDetailArgs(agentPane, self, agentID)...).Run()}
	}
}

// RunControlPane runs the top-left cockpit pane; agentPane and terminalPane are
// the tmux ids of the two viewport panes it drives (agents open in the former on
// Enter, terminals in the latter). terminalPane is "" in the tmux-native cockpit,
// which has no terminal pane. killWindow scopes the `q` teardown to the cockpit
// window instead of the whole session — set in the tmux-native cockpit, where the
// cockpit lives inside the user's own tmux session.
func RunControlPane(a api, agentPane, terminalPane string, killWindow bool) error {
	m := newListPane(a, agentPane, terminalPane)
	m.killWindow = killWindow
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}
