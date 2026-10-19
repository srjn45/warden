package terminalcontract

// Op is what a path does to a terminal.
type Op string

const (
	OpCreate  Op = "create"
	OpRestore Op = "restore"
	OpAttach  Op = "attach"
	OpList    Op = "list"
	OpStop    Op = "stop"
	OpAuto    Op = "auto" // a path that must NOT exist (negative requirement)
)

// Anchor pins a path to code: File (repo-relative) must contain Contains.
type Anchor struct {
	File     string
	Contains string
}

// Path is one row of the plain-terminal coverage matrix.
type Path struct {
	ID      string
	Surface string // cli | api | mcp | tui | web | daemon | tree
	Op      Op
	// Runtime is how the path reaches a shell today: "host" = through
	// tmuxproc.Host (routed to the direct PTY when runtime.pty=on, else tmux),
	// "tmux" = tmux-only, "n/a" = metadata only.
	Runtime string
	// Preserves lists what the path keeps across the operation.
	Preserves string
	Anchor    Anchor
}

// Paths is the coverage matrix. Every managed plain-terminal creation,
// restore, attach, list and stop path found in the t1 audit is here.
var Paths = []Path{
	{"P01", "cli", OpCreate, "host", "cwd(--dir), kind", Anchor{"internal/cli/lifecycle.go", `"session kind: empty/agent (default) spawns an AI agent`}},
	{"P02", "api", OpCreate, "host", "id, auto name, project, cwd, $SHELL", Anchor{"internal/daemon/strict_lifecycle.go", "return s.spawnTerminal(ctx, sr)"}},
	{"P03", "api", OpCreate, "host", "legacy backend=terminal / ai_cli=terminal alias", Anchor{"internal/daemon/strict_lifecycle.go", `sr.Backend == "terminal" || sr.AiCli == "terminal"`}},
	{"P04", "mcp", OpCreate, "host", "same SpawnRequest as P02", Anchor{"internal/mcp/server.go", "terminal opens a plain interactive shell"}},
	{"P05", "tui", OpCreate, "host", "project of cursor row, cwd of row", Anchor{"internal/tui/cmds.go", "func spawnTerminalCmd"}},
	{"P06", "web", OpCreate, "host", "cwd only (project by path-match)", Anchor{"web/src/lib/api.ts", "kind: 'terminal'"}},
	{"P07", "daemon", OpAuto, "n/a", "NO automatic terminal spawn exists", Anchor{"internal/tui/control_pane.go", "Terminals are never auto-spawned"}},
	{"P08", "daemon", OpAuto, "n/a", "project open/clone/scaffold spawns no terminal", Anchor{"internal/daemon/strict_projects.go", "do not auto-spawn"}},
	{"P09", "daemon", OpStop, "host", "hibernate: record kept, shell killed", Anchor{"internal/daemon/terminal_lifecycle.go", "func (s *Server) hibernateProjectTerminals"}},
	{"P10", "daemon", OpRestore, "host", "project reopen: fresh shell, same id/workdir", Anchor{"internal/daemon/terminal_lifecycle.go", "func (s *Server) restoreHibernatedTerminals"}},
	{"P11", "api", OpRestore, "host", "operator restore, orphaned or exited (409 while running)", Anchor{"internal/daemon/strict_lifecycle.go", "s.restoreTerminalRecord(ctx, t)"}},
	{"P12", "daemon", OpRestore, "host", "daemon start: running direct terminal re-spawned onto its journal", Anchor{"internal/daemon/pty_runtime.go", "recovered direct terminal onto its journal"}},
	{"P13", "daemon", OpRestore, "host", "terminal watcher: lost pane restored", Anchor{"internal/daemon/terminal_poller_deps.go", "func (d *terminalPollerDeps) Restore"}},
	{"P14", "api", OpAttach, "host", "legacy raw WS /attach; direct sessions bridged via BridgeLegacy (quiescent cut, resync on lag, takes the controller lease)", Anchor{"internal/daemon/attach.go", "ptyhost.BridgeLegacy"}},
	{"P15", "api", OpAttach, "host", "GET /sessions/{id}/pty §6 framed transport (interactive|observe)", Anchor{"internal/ptyhost/transport.go", "ModeObserve"}},
	{"P21", "cli", OpAttach, "host", "`wd attach <id>`: native §6 attach for direct sessions, tmux attach otherwise", Anchor{"internal/cli/lifecycle.go", "func attachDirect"}},
	{"P16", "tui", OpAttach, "host", "cockpit terminal pane = `warden attach` (native §6 attach for direct sessions, tmux attach for tmux-hosted ones)", Anchor{"internal/tui/control_pane.go", "func respawnTerminalArgs"}},
	{"P17", "api", OpList, "n/a", "kind filter; terminalstore projected to Session DTO", Anchor{"internal/daemon/terminal_lifecycle.go", "func (s *Server) listTerminalSessions"}},
	{"P18", "tree", OpList, "n/a", "terminal node in project tree", Anchor{"internal/tree/service.go", "func buildTerminalNode"}},
	{"P19", "api", OpCreate, "n/a", "project membership edge Project.Terminals[] (rolled back on failure)", Anchor{"internal/daemon/terminal_lifecycle.go", "AddTerminalToProject"}},
	{"P20", "api", OpStop, "host", "terminate: kill + delete record + drop membership", Anchor{"internal/daemon/terminal_lifecycle.go", "func (s *Server) stopTerminal"}},
}

// Attribute is one preserved-identity attribute of a terminal.
type Attribute struct {
	Name  string
	Rule  string
	Where Anchor
}

// Attributes are the identity rules t2 must preserve bit-for-bit.
var Attributes = []Attribute{
	{"id", "resolveID(req); tmux session name == terminal id; stable across restore", Anchor{"internal/lifecycle/lifecycle.go", "TmuxSession: id, ProjectID: req.ProjectID"}},
	{"name", `caller name, else "terminal-"+shortID(); store.ValidateName; never model-backed`, Anchor{"internal/lifecycle/lifecycle.go", `req.Name = "terminal-" + suffix`}},
	{"project", "explicit project_id (must exist) else path-match/register from cwd else 400; never project-less", Anchor{"internal/daemon/terminal_lifecycle.go", "a terminal must belong to a project"}},
	{"cwd", "Terminal.Workdir; restore refuses a missing dir with ErrWorkdirMissing", Anchor{"internal/lifecycle/lifecycle.go", "return ErrWorkdirMissing"}},
	{"shell", "Terminal.Shell=$SHELL at spawn; direct host: $SHELL else /bin/sh; tmux path: agentbackend.TerminalBackend LaunchCmd (nested login shell + exit-code capture)", Anchor{"internal/ptyruntime/host.go", `return []string{"/bin/sh"}`}},
	{"environment", "direct: daemon os.Environ + TERM=xterm-256color + WARDEN_SESSION_ID/AGENTCTL_SESSION_ID; tmux: server environment", Anchor{"internal/ptyruntime/host.go", "WARDEN_SESSION_ID="}},
	{"persistence", "terminalstore (ScrivaDB terminals-db) + per-session journal under <data>/pty/direct/<id>", Anchor{"internal/terminalstore/store.go", `Collection("terminals")`}},
}

// Platform is a supported-platform row.
type Platform struct {
	GOOS      string
	Supported bool
	Note      string
}

// Platforms: the PTY runtime builds for the two release targets only.
var Platforms = []Platform{
	{"linux", true, "release target; direct PTY + tmux"},
	{"darwin", true, "release target; direct PTY + tmux; no pipe-pane FIFO path change needed (fifo_darwin.go)"},
	{"windows", false, "no PTY runtime; ptyruntime is //go:build linux||darwin; terminals unsupported"},
}
