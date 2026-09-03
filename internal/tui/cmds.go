package tui

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

type sessionsMsg struct {
	sessions []*store.Session
	err      error
}
type spawnDoneMsg struct {
	id      string
	err     error
	confirm *client.ErrConfirmationRequired // non-nil ⇒ daemon's memory-pressure gate warned (HTTP 428)
}
type cleanupDoneMsg struct {
	id  string
	err error
}
type inputDoneMsg struct{ err error }
type attachDoneMsg struct{ err error }
type tickMsg time.Time

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// bgLong is for synchronous daemon operations that legitimately take longer than
// a read — spawn runs git worktree add + tmux setup on the daemon. A short
// deadline here would abort a slow-but-successful spawn (the daemon keeps going),
// leaving an orphaned session the TUI thinks failed.
func bgLong() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

func listCmd(a api, includeSystem bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		var ss []*store.Session
		var err error
		// Always retain the complete snapshot: guardian sessions are hidden from the
		// ordinary fleet but must be available beneath Autopilot Run nodes. The
		// compositor applies includeSystem to ordinary project rows.
		_ = includeSystem
		ss, err = a.ListAll(ctx)
		return sessionsMsg{sessions: ss, err: err}
	}
}

func spawnCmd(a api, prompt, name, cwd, role, backend string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		s, err := a.Spawn(ctx, client.SpawnParams{Prompt: prompt, Name: name, Cwd: cwd, Role: role, Backend: backend, Force: force})
		if err != nil {
			var cre *client.ErrConfirmationRequired
			if errors.As(err, &cre) {
				return spawnDoneMsg{confirm: cre}
			}
			return spawnDoneMsg{err: err}
		}
		return spawnDoneMsg{id: s.ID}
	}
}

// terminalSpawnedMsg reports the outcome of spawning a Kind=terminal session.
// focus carries whether the caller wants the terminal pane focused once opened
// (true for an explicit `t`/create, false for the default terminal at startup).
type terminalSpawnedMsg struct {
	id    string
	focus bool
	err   error
}

// spawnTerminalCmd creates a plain-shell terminal session in cwd via the daemon.
// It spawns with kind=terminal — the explicit session-kind create field (stage 6);
// the daemon launches a ${SHELL:-bash} pane, not an AI agent, ignoring
// backend/model/role/prompt. focus is echoed back on the result so the caller can
// decide whether to move focus onto the terminal pane after opening it.
func spawnTerminalCmd(a api, cwd string, focus bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		s, err := a.Spawn(ctx, client.SpawnParams{Cwd: cwd, Kind: terminalKind})
		if err != nil {
			return terminalSpawnedMsg{focus: focus, err: err}
		}
		return terminalSpawnedMsg{id: s.ID, focus: focus}
	}
}

// terminalKind is the session-kind create value that yields a Kind=terminal
// session (mirrors store.KindTerminal). Terminals are no longer a backend.
const terminalKind = "terminal"

// terminalLiveInfo is a terminal's live working-directory context, polled from
// its running tmux pane (§7) rather than its stored fields, so its Terminals-
// section name tracks the shell as it `cd`s and checks out branches.
type terminalLiveInfo struct {
	cwd      string
	repoRoot string
	branch   string
}

// terminalInfoMsg carries the freshly-polled live cwd/branch for each terminal,
// keyed by session id.
type terminalInfoMsg struct {
	info map[string]terminalLiveInfo
}

// terminalInfoCmd polls each terminal's live pane path (tmux #{pane_current_path})
// and the git repo-root/branch of that path, off the UI goroutine, on the refresh
// tick (§7). Terminals are few, so the per-tick tmux+git calls are cheap; a
// terminal whose pane can't be read is simply omitted (its row falls back to the
// stored name).
func terminalInfoCmd(terminals []*store.Session) tea.Cmd {
	type ent struct{ id, tmux string }
	list := make([]ent, 0, len(terminals))
	for _, s := range terminals {
		list = append(list, ent{id: s.ID, tmux: s.TmuxSession})
	}
	return func() tea.Msg {
		info := make(map[string]terminalLiveInfo, len(list))
		for _, e := range list {
			cwd := tmuxPanePath(e.tmux)
			if cwd == "" {
				continue
			}
			root, branch := gitRootBranch(cwd)
			info[e.id] = terminalLiveInfo{cwd: cwd, repoRoot: root, branch: branch}
		}
		return terminalInfoMsg{info: info}
	}
}

// isTmuxPaneDead reports whether pane is showing [exited] (remain-on-exit) rather
// than running a live attach/shell. Overridden in tests.
var isTmuxPaneDead = tmuxPaneDead

// tmuxPaneDead returns true when the tmux pane has exited and is being kept alive
// by remain-on-exit (the nested attach died, e.g. after a daemon restart).
func tmuxPaneDead(pane string) bool {
	if pane == "" {
		return false
	}
	out, err := exec.Command("tmux", "display-message", "-p", "-t", pane, "#{pane_dead}").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "1"
}

// tmuxPanePath returns the current working directory of the (single) pane in the
// given tmux session — how a terminal's live cwd is read without shell hooks. An
// empty result (no such session, tmux gone) signals the caller to keep the
// stored name.
func tmuxPanePath(session string) string {
	if session == "" {
		return ""
	}
	out, err := exec.Command("tmux", "display-message", "-p", "-t", session, "#{pane_current_path}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRootBranch resolves the git repo root and current branch of dir (worktree-
// aware). Outside a repo both are empty and the terminal name falls back to an
// abbreviated path (§7).
func gitRootBranch(dir string) (root, branch string) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", ""
	}
	root = strings.TrimSpace(string(out))
	if root == "" {
		return "", ""
	}
	if b, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch = strings.TrimSpace(string(b))
	}
	return root, branch
}

// renameDoneMsg reports the outcome of a SetName call.
type renameDoneMsg struct {
	id  string
	err error
}

// renameCmd renames an agent (blank name clears it) via the daemon.
func renameCmd(a api, id, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return renameDoneMsg{id: id, err: a.SetName(ctx, id, name)}
	}
}

// overrideDoneMsg reports the outcome of a per-agent override edit (auto-approve
// toggle or force-compact cycle) made from the detail view. note is the status
// line to show on success.
type overrideDoneMsg struct {
	note string
	err  error
}

// setAutoApproveCmd toggles an agent's auto-approve override via the daemon.
func setAutoApproveCmd(a api, id string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		note := "auto-approve " + boolOnOff(enabled) + " · " + id
		return overrideDoneMsg{note: note, err: a.SetAutoApprove(ctx, id, enabled)}
	}
}

// setForceCompactCmd sets an agent's force-compact override (on/off/inherit) via
// the daemon.
func setForceCompactCmd(a api, id, state string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return overrideDoneMsg{note: "force-compact " + state + " · " + id, err: a.SetForceCompact(ctx, id, state)}
	}
}

type pressureMsg struct {
	status client.PressureStatus
	err    error
}

func pressureCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		ps, err := a.Pressure(ctx)
		return pressureMsg{status: ps, err: err}
	}
}

func inputCmd(a api, id, text string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return inputDoneMsg{err: a.Input(ctx, id, text)}
	}
}

// killCmd kills the agent and removes it from the list in one action: it
// terminates the tmux+claude session, then soft-deletes (archives) the record
// so it drops off the list while staying recoverable in closed/. Mirrors
// `warden done`. Terminate is best-effort — an already-dead agent (status
// done/orphaned) errors there, which we ignore so its lingering record can
// still be removed; only a delete failure is surfaced.
func killCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		_ = a.Terminate(ctx, id)
		return cleanupDoneMsg{id: id, err: a.Delete(ctx, id, false)}
	}
}

type restoreDoneMsg struct {
	id  string
	err error
}

func restoreCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return restoreDoneMsg{id: id, err: a.Restore(ctx, id)}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// dirListMsg carries a /fs/dirs listing back for tab-completion. typed is the
// expanded path the user had typed when completion was requested.
type dirListMsg struct {
	typed   string
	listing client.DirListing
	err     error
}

// listDirsCmd fetches listDir's subdirectories for completing `typed`.
func listDirsCmd(a api, typed, listDir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		l, err := a.ListDirs(ctx, listDir)
		return dirListMsg{typed: typed, listing: l, err: err}
	}
}

// openProjectMsg is the unified result of opening/cloning/creating a project
// through the daemon's Phase 2 project APIs. The project is registered in the
// project store and survives daemon restart.
type openProjectMsg struct {
	proj projectstore.Project
	err  error
}

// openLocalProjectCmd opens an existing local directory as a project via the
// daemon's POST /projects/local. The daemon validates, normalizes, and persists.
func openLocalProjectCmd(a api, path string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		p, err := a.OpenLocalProject(ctx, path, "")
		return openProjectMsg{proj: p, err: err}
	}
}

// openRemoteProjectCmd clones a remote URL via POST /projects/remote and
// registers it as a project. Uses bgLong — clone is a network round-trip.
func openRemoteProjectCmd(a api, url string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		p, err := a.OpenRemoteProject(ctx, url, "")
		return openProjectMsg{proj: p, err: err}
	}
}

// createProjectCmd scaffolds a new project via POST /projects/new (git init +
// README + commit in the daemon's workspace) and registers it. Uses bgLong —
// git init + commit can be slow on some filesystems.
func createProjectCmd(a api, name string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		p, err := a.CreateProject(ctx, name)
		return openProjectMsg{proj: p, err: err}
	}
}

// contextMsg / messagesMsg carry the inspector's read-only fetches. On error we
// keep the last good data (like outputMsg) so a transient blip doesn't blank the
// view the user is reading.
type contextMsg struct {
	entries []client.ContextEntry
	err     error
}
type messagesMsg struct {
	messages []client.Message
	err      error
}

func contextCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		es, err := a.CtxList(ctx, "")
		return contextMsg{entries: es, err: err}
	}
}

// inspectorMsgLimit bounds the recent-message fetch behind the inspector — a
// full pane's worth without unbounded reads.
const inspectorMsgLimit = 100

func messagesCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		ms, err := a.MsgRecent(ctx, inspectorMsgLimit)
		return messagesMsg{messages: ms, err: err}
	}
}

type pipelinesMsg struct {
	pipelines []*pipeline.Pipeline
	err       error
}
type pipelineActionMsg struct{ err error }

func pipelinesCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ps, err := a.PipelineList(context.Background())
		return pipelinesMsg{pipelines: ps, err: err}
	}
}

// projectsMsg carries the persisted project list for the §4 project-grouped
// navigator. A transient error keeps the last good list (handled in Update).
type projectsMsg struct {
	projects []projectstore.Project
	err      error
}

func projectsCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ps, err := a.ListProjects(context.Background())
		return projectsMsg{projects: ps, err: err}
	}
}

// projectGroupsMsg carries the project groups for the §4 navigator's per-project
// group label. A transient error keeps the last good list (handled in Update).
type projectGroupsMsg struct {
	groups []projectstore.ProjectGroup
	err    error
}

func projectGroupsCmd(a api) tea.Cmd {
	return func() tea.Msg {
		gs, err := a.ListProjectGroups(context.Background())
		return projectGroupsMsg{groups: gs, err: err}
	}
}

// closeProjectMsg reports the result of hibernating a project (§4 close). The
// daemon terminates the project's live agents and flips its status; the poller
// then reflects both.
type closeProjectMsg struct {
	id  string
	err error
}

func closeProjectCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		_, err := a.CloseProject(context.Background(), id)
		return closeProjectMsg{id: id, err: err}
	}
}

func cancelPipelineCmd(a api, pid string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineCancel(context.Background(), pid)}
	}
}

func pausePipelineCmd(a api, pid string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelinePause(context.Background(), pid)}
	}
}

func resumePipelineCmd(a api, pid string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineResume(context.Background(), pid)}
	}
}

func deletePipelineCmd(a api, pid string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineDelete(context.Background(), pid)}
	}
}

func retryJobCmd(a api, pid, job string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineRetry(context.Background(), pid, job)}
	}
}

// digestMsg carries a fetched completion digest (or an error) back to the model.
type digestMsg struct {
	id  string
	d   *digest.Digest
	err error
}

// digestCmd fetches the agent's completion digest. It uses the long deadline
// because the daemon may shell `claude -p` to write the narrative.
func digestCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		d, err := a.Digest(ctx, id)
		return digestMsg{id: id, d: d, err: err}
	}
}

// approvalsMsg carries the pending tool-permission queue (enabled flag + views).
type approvalsMsg struct {
	enabled bool
	views   []approval.View
	err     error
}

func approvalsCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		enabled, views, err := a.Approvals(ctx)
		return approvalsMsg{enabled: enabled, views: views, err: err}
	}
}

type approveDoneMsg struct{ err error }

// approveCmd answers one prompt by 1-based option, echoing the fingerprint so the
// daemon can prove the menu hasn't changed underneath it.
func approveCmd(a api, id string, option int, fingerprint string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return approveDoneMsg{err: a.Approve(ctx, id, option, fingerprint)}
	}
}

// autopilotMsg carries the autopilot status fetched on a periodic poll.
type autopilotMsg struct {
	status client.AutopilotStatus
	err    error
}

// autopilotToggleDoneMsg carries the result of toggling autopilot on or off.
type autopilotToggleDoneMsg struct {
	status client.AutopilotStatus
	err    error
}

type autopilotRunActionMsg struct {
	runID, action string
	err           error
}

func autopilotRunActionCmd(a api, runID, action string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		_, err := a.ControlAutopilotRun(ctx, runID, action)
		return autopilotRunActionMsg{runID: runID, action: action, err: err}
	}
}

// autopilotCmd fetches the current autopilot status from the daemon.
func autopilotCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		st, err := a.GetAutopilot(ctx)
		return autopilotMsg{status: st, err: err}
	}
}

// autopilotToggleCmd flips the autopilot switch for the daemon's working-directory
// repo (empty repo ⇒ that default — the switch is per-repo).
func autopilotToggleCmd(a api, enable bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		st, err := a.SetAutopilot(ctx, enable, "")
		return autopilotToggleDoneMsg{status: st, err: err}
	}
}

// backendsMsg carries a backend-registry snapshot to the Backends page. action is
// set for the result of a user action (rescan / tier / default / enable / thinking
// mode) so its errors surface in the status line; a passive load/refresh (open or
// tick) leaves it false and keeps the last good table on a transient blip.
type backendsMsg struct {
	state  client.BackendsState
	err    error
	action bool
}

// backendsCmd loads the backend registry for the Backends page (passive refresh).
func backendsCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		st, err := a.ListBackends(ctx)
		return backendsMsg{state: st, err: err}
	}
}

// rescanBackendsCmd re-detects installed backend CLIs and returns the refreshed
// registry.
func rescanBackendsCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		st, err := a.RescanBackends(ctx)
		return backendsMsg{state: st, err: err, action: true}
	}
}

// setDefaultBackendCmd makes id the single default backend and returns the updated
// registry (the daemon rejects unknown/uninstalled/disabled/reserved targets).
func setDefaultBackendCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		st, err := a.SetDefaultBackend(ctx, id)
		return backendsMsg{state: st, err: err, action: true}
	}
}

// setBackendTierCmd sets a backend's tier, then re-lists so the whole table (and
// any derived state) stays coherent — SetBackendTier alone returns just the one row.
func setBackendTierCmd(a api, id, tier string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		if _, err := a.SetBackendTier(ctx, id, tier); err != nil {
			return backendsMsg{err: err, action: true}
		}
		st, err := a.ListBackends(ctx)
		return backendsMsg{state: st, err: err, action: true}
	}
}

// setBackendEnabledCmd toggles a backend's enabled flag, then re-lists (as
// setBackendTierCmd) so the full table reflects the change.
func setBackendEnabledCmd(a api, id string, enabled bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		if _, err := a.SetBackendEnabled(ctx, id, enabled); err != nil {
			return backendsMsg{err: err, action: true}
		}
		st, err := a.ListBackends(ctx)
		return backendsMsg{state: st, err: err, action: true}
	}
}

// setThinkingModeCmd sets the internal-thinking routing mode, then re-lists so the
// header control and the settings footer reflect it (SetThinkingMode returns only
// the settings singleton).
func setThinkingModeCmd(a api, mode string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		if _, err := a.SetThinkingMode(ctx, mode); err != nil {
			return backendsMsg{err: err, action: true}
		}
		st, err := a.ListBackends(ctx)
		return backendsMsg{state: st, err: err, action: true}
	}
}
