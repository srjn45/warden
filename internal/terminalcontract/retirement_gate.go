package terminalcontract

// EvidenceKind says what may satisfy a retirement-gate item.
type EvidenceKind string

const (
	// EvidenceObserved is data recorded from a running, operator-authorised
	// deployment (wd version, wd inspect pty runtime, warden_pty_* metrics,
	// filled rollout templates). Only this kind can satisfy a gate item.
	EvidenceObserved EvidenceKind = "observed"
	// EvidenceOperatorDecision is a recorded human ratification.
	EvidenceOperatorDecision EvidenceKind = "operator-decision"
)

// NotEvidence lists what must never satisfy any Plan03EntryGate item.
// Completed plan status and green CI are claims about code, not about a
// deployment, a cohort, a soak or a legacy drain.
var NotEvidence = []string{
	"plan status (completed/done task flags)",
	"merged PR or green CI",
	"fixture or unit-test results (docs/rollout t1 matrix)",
	"runtime.pty requested mode (requested_mode is informational)",
	"a tmux mirror of an agent (actual_kind=tmux, authoritative=false)",
}

// GateItem is one entry-gate requirement of plan-5cd3c58b (legacy retirement).
type GateItem struct {
	ID       string
	Need     string
	Evidence EvidenceKind
}

// Plan03EntryGate mirrors docs/RUNBOOK-pty-rollout.md §11. Plan 03 may not
// start until every item is satisfied by its Evidence kind; none is satisfiable
// by a NotEvidence source. Today none is satisfied and the agent item is
// structurally BLOCKED (no adapter is direct-capable).
var Plan03EntryGate = []GateItem{
	{"G03-DEPLOYED", "plans 01, 02 and 02b-01..05 merged, released AND deployed; running daemon SHA recorded from wd version", EvidenceObserved},
	{"G03-DIRECT", "supported agents and shells show actual_kind=direct per session (agents are BLOCKED today)", EvidenceObserved},
	{"G03-SOAK", "per cohort: >= MinSessionsPerStage sessions, >= MinSoakHoursPerStage hours, error/fallback under budget, filed templates", EvidenceObserved},
	{"G03-METRICS", "warden_pty_* healthy and zero-tolerance counters at 0", EvidenceObserved},
	{"G03-NO-TMUX-OWNER", "no supported workflow still needs tmux as process owner", EvidenceObserved},
	{"G03-DRAIN", "legacy inventory drained naturally to 0 with no forced conversion or termination", EvidenceObserved},
	{"G03-COMPAT", "unsupported compatibility (Windows, unmanaged terminals, non-direct adapters) retained and ratified", EvidenceOperatorDecision},
	{"G03-ROLLBACK", "rollback/downgrade/control recovery verified on a live cohort", EvidenceObserved},
	{"G03-ISSUES", "#918/#919 resolved or affected scenarios explicitly accepted", EvidenceOperatorDecision},
	{"G03-PLAN-TEXT", "plan 03 text names the corrective plan IDs and requires separately authorised deployment", EvidenceOperatorDecision},
}
