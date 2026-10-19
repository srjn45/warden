package terminalcontract

// Fixture is one deterministic test/benchmark that must exist before the stage
// it gates. Deterministic means: no wall-clock sleeps for correctness (injected
// clocks / event waits), fixed seeds, loopback only, no external CLI or network.
type Fixture struct {
	ID    string
	Class string // survival | auth | fence | kind | lifecycle | vt | attach | rollout | parity | resource | privacy
	// Kinds the fixture runs against.
	Kinds []string
	What  string
	// Budget names the Budgets field(s) it asserts ("" = pass/fail only).
	Budget string
	// Owner task builds it; Gate is the stage that may not open without it.
	Owner string
	Gate  string
	// Test is the Go test/benchmark anchor once it exists ("" = to be written).
	Test Anchor
}

var both = []string{"agent", "terminal"}

// Fixtures is the deterministic fixture matrix. Rows with a Test anchor exist
// now (sealed by t1); the rest are written by their Owner and are listed here so
// no task can claim the gap closed without them.
var Fixtures = []Fixture{
	// Survival (G1, G2): the process tree and PTY master outlive the daemon.
	{"F-SURV-01", "survival", []string{"terminal"}, "SIGKILL the daemon; shell pid, cwd, env and a running `sleep` child are alive; broker re-adopted; scrollback intact", "SurvivalLossPct,AdoptAfterRestoreP99", "t3", "S2", Anchor{}},
	{"F-SURV-02", "survival", []string{"terminal"}, "graceful daemon stop + binary swap + start (update path): same shell pid, no relaunch, no duplicate", "SurvivalLossPct,DuplicateProcesses", "t3", "S2", Anchor{}},
	{"F-SURV-03", "survival", both, "broker crash (not daemon): sessions are reported lost with an actionable error, never silently relaunched", "ActionableErrorPct", "t3", "S2", Anchor{}},
	{"F-SURV-04", "survival", []string{"agent"}, "agent process tree survives daemon SIGKILL under the same ownership model as shells (or documented tmux-compat holder)", "SurvivalLossPct", "t3", "S2", Anchor{}},
	// Authenticated adoption (G3).
	{"F-AUTH-01", "auth", both, "adoption presents the per-session credential; wrong/missing credential is refused and journaled", "", "t3", "S2", Anchor{}},
	{"F-AUTH-02", "auth", both, "pid reuse: a recycled pid with a different start-time/identity is NOT adopted", "DuplicateProcesses", "t3", "S2", Anchor{}},
	{"F-AUTH-03", "auth", both, "broker socket is 0600, owner-checked (SO_PEERCRED/LOCAL_PEERCRED); other uid refused", "", "t3", "S2", Anchor{}},
	// Input fencing / controller lease (G4).
	{"F-FENCE-01", "fence", both, "two interactive clients: one controller, the other demoted to observe; lease generation increments on takeover", "LeaseTakeoverP99", "t3", "S2", Anchor{}},
	{"F-FENCE-02", "fence", both, "stale controller (older generation) write after takeover/epoch reset is rejected, not delivered", "StaleControllerWrites", "t3", "S2", Anchor{}},
	{"F-FENCE-03", "fence", both, "reconnect with the same client_id replays no input twice (idempotent input ids)", "DuplicateInputBytes", "t3", "S2", Anchor{}},
	{"F-FENCE-04", "fence", both, "human always wins over operator/warden input (sealed by plan 01 Arbitrate)", "", "plan01", "S2", Anchor{"internal/ptyevent/input_test.go", "func Test"}},
	// Shell vs AI kind boundary (G5).
	{"F-KIND-01", "kind", []string{"terminal"}, "approval/trust/password-shaped screen on a shell session with NO agent record: no prompt event, no decision, no keystrokes", "ShellClassifierEvents,PromptFalseApprovals", "t1", "S2", Anchor{"internal/stateclassifier/terminal_boundary_test.go", "func TestShellSessionNeverReachesPromptEngine"}},
	{"F-KIND-02", "kind", []string{"terminal"}, "same screen on a shell whose NAME collides with an agent record: still no prompt (kind, not name, gates)", "ShellClassifierEvents", "t2", "S2", Anchor{}},
	{"F-KIND-03", "kind", []string{"terminal"}, "claude/codex launched manually inside a shell: no auto-approve, session stays kind=terminal", "PromptFalseApprovals", "t2", "S2", Anchor{}},
	// Lifecycle (G6, G7, G12).
	{"F-LIFE-01", "lifecycle", []string{"terminal"}, "concurrent restore x3 (poller, operator, startup): exactly one process", "DuplicateProcesses", "t2", "S2", Anchor{}},
	{"F-LIFE-02", "lifecycle", []string{"terminal"}, "spawn rollback: store/project failure leaves no process and no record", "DuplicateProcesses", "t2", "S2", Anchor{}},
	{"F-LIFE-03", "lifecycle", []string{"terminal"}, "hibernate -> reopen preserves id/name/project/workdir and is labelled a relaunch", "", "t2", "S2", Anchor{}},
	{"F-LIFE-04", "lifecycle", []string{"terminal"}, "exit code recorded for direct shells; `exit 3` -> ExitCode=3, status exited", "", "t2", "S2", Anchor{}},
	{"F-LIFE-05", "lifecycle", []string{"terminal"}, "identity: id, auto name terminal-<hex>, project membership, cwd, $SHELL, TERM, WARDEN_SESSION_ID identical for tmux and direct paths", "", "t2", "S2", Anchor{}},
	// Terminal emulation (G8).
	{"F-VT-01", "vt", both, "snapshot after vim/htop/less alt-screen traces round-trips to the same visible cells", "ParityByteExactPct", "t3", "S2", Anchor{}},
	{"F-VT-02", "vt", both, "wide (CJK), combining and emoji sequences occupy correct cells in a snapshot", "", "t3", "S2", Anchor{}},
	{"F-VT-03", "vt", both, "SGR attributes survive snapshot for the local authorized view; redacted views drop them", "", "t3", "S2", Anchor{}},
	// Attach (G9, G10).
	{"F-ATTACH-01", "attach", []string{"terminal"}, "cockpit/TUI shows and drives a direct terminal (raw mode restored on detach, Ctrl-C/EOF/job control)", "", "t3", "S2", Anchor{}},
	{"F-ATTACH-02", "attach", both, "replay-then-live ordering with a cursor across epoch reset; slow viewer bounded then ErrResync", "ReplayOrderViolations,SlowViewerMaxBytes,SlowViewerPTYStall", "t3", "S2", Anchor{}},
	{"F-ATTACH-03", "attach", both, "resize arbitration: last controller wins, SIGWINCH delivered, journaled with origin", "ResizeToSIGWINCHP99", "t3", "S2", Anchor{}},
	// Rollout / compatibility (G11).
	{"F-ROLL-01", "rollout", both, "per-session runtime pin: flipping runtime.pty affects future launches only; live sessions keep runtime and control", "", "t4", "S3", Anchor{}},
	{"F-ROLL-02", "rollout", both, "old-binary/new-broker and new-binary/old-broker protocol handshake: refuse with version error, never misparse", "", "t4", "S3", Anchor{}},
	{"F-ROLL-03", "rollout", both, "shadow never mutates authoritative status and never sends input", "", "t4", "S1", Anchor{}},
	{"F-ROLL-04", "rollout", both, "mixed legacy+PTY fleet: list/attach/send/approve/kill all work for both", "", "t5", "S3", Anchor{}},
	// Parity / resource / privacy.
	{"F-PAR-01", "parity", both, "golden byte stream: tmux path vs direct path produce identical visible output", "ParityByteExactPct", "t4", "S3", Anchor{}},
	{"F-PAR-02", "parity", []string{"agent"}, "labelled AI prompt corpus: state + permission accuracy vs legacy poller", "StateAccuracyPct,PromptFalseApprovals", "t4", "S3", Anchor{}},
	{"F-RES-01", "resource", both, "SoakSessions idle sessions: RSS/FD/goroutine per session within budget; no leak after teardown (-race)", "PerSessionRSSKiB,PerSessionFDs,PerSessionGoroutines", "t5", "S3", Anchor{}},
	{"F-RES-02", "resource", both, "journal retention holds at JournalMaxBytes under 10x output flood", "JournalMaxBytes", "t5", "S3", Anchor{}},
	{"F-PRIV-01", "privacy", both, "planted secrets (typed no-echo password, env token, API-key output) absent from journal, audit, diagnostics, SSE and remote-safe views", "SecretLeaksInDurable", "t5", "S3", Anchor{}},
}

// SecretDetectionLimits documents what redaction cannot promise (spec §9).
var SecretDetectionLimits = []string{
	"echoed secrets with no recognizable shape (a password the user types with echo ON, or a short random token) are not detectable",
	"secrets split across redraws or interleaved with control sequences can evade a pattern that needs contiguous bytes",
	"no-echo input (termios ECHO off) is dropped from the journal, but a program that echoes it back itself is only caught by shape",
	"the local authorized live view and the in-memory screen are faithful by design; redaction applies from the journal boundary outward",
}
