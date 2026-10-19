package terminalcontract

// Severity ranks a gap for the default-rollout decision.
type Severity string

const (
	// ReleaseCritical gaps must be closed (or the feature kept off by default)
	// before `runtime.pty: on` becomes the default for any session kind.
	ReleaseCritical Severity = "release-critical"
	// Hardening gaps degrade fidelity or operability but cannot lose a process
	// or send wrong input.
	Hardening Severity = "hardening"
)

// Gap is a verified shortfall of the merged plan 01 runtime (PR #979 at
// dae3cbd4) against the plan 02 contract. Evidence is a code anchor proving the
// shortfall exists TODAY; when a task fixes the gap it removes or rewrites the
// anchor and flips Closed in the same change.
type Gap struct {
	ID       string
	Severity Severity
	Title    string
	Evidence Anchor
	// Owner is the plan 02 task that closes it (never "plan01": plan 01 is
	// frozen and not modified by plan 02).
	Owner string
	// Fixtures are the fixture IDs (see Fixtures) that prove the gap closed.
	Fixtures []string
	Closed   bool
}

// Gaps is the register. Plan 01 shipped the event/journal/arbitration layers;
// it did not ship an independent PTY owner.
var Gaps = []Gap{
	{"G1", ReleaseCritical, "No broker: the daemon holds the PTY master, so a direct shell dies with the daemon (restart/update/crash); recovery relaunches a FRESH shell",
		Anchor{"internal/ptyruntime/session.go", "master: master, pid: cmd.Process.Pid"}, "t3", []string{"F-SURV-01", "F-SURV-02", "F-SURV-03"}, false},
	{"G2", ReleaseCritical, "Agents stay in tmux in `on` mode; mirrors observe via pipe-pane only, so an agent has NO warden-owned PTY and nothing to adopt",
		Anchor{"internal/ptyhost/manager.go", "if r.terminal && mode == ModeOn"}, "t3", []string{"F-SURV-04"}, false},
	{"G3", ReleaseCritical, "No authenticated adoption: nothing identifies/authorizes a process re-claimed after restart (pid reuse, foreign process, other user)",
		Anchor{"internal/daemon/pty_runtime.go", "func (s *Server) recoverPTY"}, "t3", []string{"F-AUTH-01", "F-AUTH-02", "F-AUTH-03"}, false},
	{"G4", ReleaseCritical, "No input fencing: any number of interactive humans write concurrently; there is no controller lease generation, so a stale/reconnected client can type after takeover or after an epoch reset",
		Anchor{"internal/ptyhost/transport.go", "Origin: ptyevent.OriginHuman, ClientID: clientID"}, "t3", []string{"F-FENCE-01", "F-FENCE-02", "F-FENCE-03", "F-ATTACH-03"}, false},
	{"G5", ReleaseCritical, "Shell/AI boundary is incidental: the state classifier observes EVERY runtime session; shells are shielded only because no agent record exists (backendFor==nil). A future shell-kind record or agent-name collision would route shell output to the prompt engine",
		Anchor{"internal/stateclassifier/classifier.go", "mgr.Observe(c.OnPTYEvent)"}, "t2", []string{"F-KIND-01", "F-KIND-02", "F-KIND-03"}, false},
	{"G6", ReleaseCritical, "Hibernate/stop semantic is kill-and-relaunch: project close terminates shells; reopen starts a new shell (cwd preserved, process state lost) — must stay explicit, never be used for daemon restart",
		Anchor{"internal/daemon/terminal_lifecycle.go", "s.life.Terminate(ctx, t.TmuxSession)"}, "t2", []string{"F-LIFE-03"}, false},
	{"G7", ReleaseCritical, "Launch/recovery is not idempotent across races: RestoreTerminal checks HasSession then NewSession (TOCTOU); poller restore, operator restore and startup recovery can all fire",
		Anchor{"internal/lifecycle/lifecycle.go", "if l.Proc().HasSession(ctx, id) {"}, "t2", []string{"F-LIFE-01", "F-LIFE-02"}, false},
	{"G8", Hardening, "Screen model is a minimal VT: one cell per rune (wide/combining wrong), colors/attributes dropped, simplified scroll regions — snapshots are lossy for full-screen TUIs; live bytes are unaffected (raw frames), only resync snapshots",
		Anchor{"internal/ptyruntime/screen.go", "Every rune is one"}, "t3", []string{"F-VT-01", "F-VT-02", "F-VT-03"}, false},
	{"G9", Hardening, "Cockpit/TUI attach is `tmux attach`; a direct terminal has no tmux session so the cockpit pane cannot show it in `on` mode",
		Anchor{"internal/tui/control_pane.go", "terminals need the cockpit terminal pane"}, "t3", []string{"F-ATTACH-01"}, false},
	{"G10", Hardening, "Legacy /attach on a direct session loses structured resync (raw bridge); only /pty speaks the §6 protocol",
		Anchor{"internal/daemon/attach.go", "ptyhost.BridgeLegacy"}, "t3", []string{"F-ATTACH-02"}, false},
	{"G11", Hardening, "Mode is a global hot-reloaded switch applying to sessions created afterwards; there is no per-session/cohort runtime pin or rollback record, and no old-binary/broker protocol version",
		Anchor{"internal/ptyhost/mode.go", "ModeOn"}, "t4", []string{"F-ROLL-01", "F-ROLL-02"}, false},
	{"G12", Hardening, "Exit status of a direct terminal is journaled but terminalstore.ExitCode is only set by the tmux exit-file watcher path",
		Anchor{"internal/daemon/pty_runtime.go", "func (s *Server) onPTYEvent"}, "t2", []string{"F-LIFE-04"}, false},
}
