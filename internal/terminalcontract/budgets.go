package terminalcontract

import "time"

// Budgets are the numeric acceptance thresholds for the staged rollout (plan 02
// t4 applies them; t5 gates on them). Every value is a hard pass/fail bound,
// measured on the Fixtures below. Observed production numbers are recorded in
// the t5 runbook separately and labelled as observed — a unit test passing is
// never reported as deployment evidence.
type Budgets struct {
	// --- Parity (new runtime vs legacy tmux path, same fixture) ---
	// ParityByteExact: bytes a human sees in a live attach must equal the bytes
	// the process wrote (UTF-8 boundary-split aside). 100% = no diff allowed.
	ParityByteExactPct float64
	// ReplayOrderViolations: seq gaps/dups/reorders across attach/replay/live.
	ReplayOrderViolations int
	// StateAccuracyPct: AI-session activity/permission state vs labelled fixture.
	StateAccuracyPct float64
	// PromptFalseApprovals: auto-approved prompts that a human did not allow.
	// Zero tolerance; includes ANY decision on a shell session.
	PromptFalseApprovals int
	// ShellClassifierEvents: prompt/decision/classification events produced for
	// a shell-kind session. Zero tolerance.
	ShellClassifierEvents int

	// --- Latency (p99 unless noted; loopback, idle daemon, 80x24) ---
	KeystrokeEchoP99     time.Duration // human byte in -> echoed byte at a live subscriber
	AttachToFirstByteP99 time.Duration // hello -> first replay/snapshot frame
	ResizeToSIGWINCHP99  time.Duration // resize frame -> process observes new size
	AdoptAfterRestoreP99 time.Duration // daemon up -> broker session adopted & attachable
	LeaseTakeoverP99     time.Duration // controller request -> previous holder fenced

	// --- Resource ---
	// Per live session, steady state, idle shell.
	PerSessionRSSKiB     int
	PerSessionFDs        int
	PerSessionGoroutines int
	// Journal retention bound per session (bytes on disk).
	JournalMaxBytes int64
	// Concurrent sessions the budgets above must hold at.
	SoakSessions int
	// Slow-viewer bound: memory a stalled subscriber may pin before ErrResync.
	SlowViewerMaxBytes int
	// A stalled viewer must never delay the PTY read loop beyond this.
	SlowViewerPTYStall time.Duration

	// --- Error / survival ---
	// SurvivalLossPct: shells+agents lost across daemon restart/update/crash in
	// the survival fixtures (G1-G3). Zero tolerance.
	SurvivalLossPct float64
	// DuplicateProcesses: second process for one session id after any recovery.
	DuplicateProcesses int
	// DuplicateInputBytes: any input byte delivered twice (reconnect/fencing).
	DuplicateInputBytes int
	// StaleControllerWrites accepted after a lease generation advanced.
	StaleControllerWrites int
	// SecretLeaksInDurable: planted secrets found in journal/audit/diagnostics/
	// remote-safe views by the redaction fixture.
	SecretLeaksInDurable int
	// ActionableErrorPct: failure paths whose error names cause + next step.
	ActionableErrorPct float64
	// RecoveryBound: max time a recovery attempt may run before it must fail
	// with an actionable error rather than hang.
	RecoveryBound time.Duration

	// --- Rollout gates ---
	// Minimum observed sample before a stage may advance (per cohort).
	MinSessionsPerStage  int
	MinSoakHoursPerStage int
	// Rollback trigger: any one of these in a stage aborts the stage.
	MaxStageErrorRatePct float64
	MaxStageFallbackPct  float64
}

// Default is the contract. Values are deliberately round and generous (a
// frictionless-safeguards rule): they fire on genuine regressions, not noise.
var Default = Budgets{
	ParityByteExactPct:    100,
	ReplayOrderViolations: 0,
	StateAccuracyPct:      98,
	PromptFalseApprovals:  0,
	ShellClassifierEvents: 0,

	KeystrokeEchoP99:     50 * time.Millisecond,
	AttachToFirstByteP99: 250 * time.Millisecond,
	ResizeToSIGWINCHP99:  100 * time.Millisecond,
	AdoptAfterRestoreP99: 2 * time.Second,
	LeaseTakeoverP99:     200 * time.Millisecond,

	PerSessionRSSKiB:     16 * 1024,
	PerSessionFDs:        6,
	PerSessionGoroutines: 6,
	JournalMaxBytes:      64 << 20,
	SoakSessions:         50,
	SlowViewerMaxBytes:   4 << 20,
	SlowViewerPTYStall:   10 * time.Millisecond,

	SurvivalLossPct:       0,
	DuplicateProcesses:    0,
	DuplicateInputBytes:   0,
	StaleControllerWrites: 0,
	SecretLeaksInDurable:  0,
	ActionableErrorPct:    100,
	RecoveryBound:         30 * time.Second,

	MinSessionsPerStage:  20,
	MinSoakHoursPerStage: 24,
	MaxStageErrorRatePct: 1,
	MaxStageFallbackPct:  5,
}

// Stage is one rung of the rollout ladder. A session's runtime is pinned at
// launch; moving a cohort back affects FUTURE launches only.
type Stage struct {
	ID   string
	Name string
	// Applies to new sessions of these kinds.
	Kinds []string
	// Authoritative reports whether the new runtime drives status/input.
	Authoritative bool
	Entry         string
}

// Stages is the rollout ladder (t4). Shadow is observe-only and only valid for
// agents (a tmux-hosted process already exists to mirror); plain terminals have
// no valid shadow — a second owner of one PTY master is impossible.
var Stages = []Stage{
	{"S0", "off", []string{"agent", "terminal"}, false, "default today (runtime.pty=off)"},
	{"S1", "shadow", []string{"agent"}, false, "observe-only mirror; never mutates status or writes input"},
	{"S2", "opt-in", []string{"agent", "terminal"}, true, "runtime.pty=on per cohort; survival gate (G1-G4) green"},
	{"S3", "default-new", []string{"agent", "terminal"}, true, "every Budgets gate met over MinSessionsPerStage x MinSoakHoursPerStage"},
	{"S4", "legacy-drain", []string{"agent", "terminal"}, true, "legacy sessions finish naturally; plan 03 entry gate"},
}
