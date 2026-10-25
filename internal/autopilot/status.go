package autopilot

// RunState is the run state machine (autopilot.md §2.1). S1 only ever produces
// the disabled → starting → active path (no brain, so "brain healthy" is
// immediate); the degraded/healing/complete states are reserved for S3+.
type RunState string

const (
	StateDisabled   RunState = "disabled"
	StateRegistered RunState = "registered"
	StateStarting   RunState = "starting"
	StateActive     RunState = "active"
	StatePaused     RunState = "paused"
	StateStopped    RunState = "stopped"
	StateDegraded   RunState = "degraded"
	StateHealing    RunState = "healing"
	StateComplete   RunState = "complete"
)

// Status is the AutopilotStatus wire shape (autopilot.md §5). It is mapped into
// the OpenAPI schema via x-go-type, so this struct's JSON tags ARE the API
// contract — keep them in sync with the spec.
type Status struct {
	Recovery RecoveryStats `json:"recovery"`
	Runs     []RunStatus   `json:"runs"`
}

// RunStatus is one run's slice of AutopilotStatus. In the S1 inert core Brain is
// always nil, WorkersInFlight/Tasks/LandedTotal are zero, and Backoff is nil —
// those go live in S3/S4/S5.
type RunStatus struct {
	RecoveryDiagnostic string       `json:"recovery_diagnostic,omitempty"`
	RunID              string       `json:"run_id"`
	Name               string       `json:"name"`
	PlanFile           string       `json:"plan_file"`
	Repo               string       `json:"repo"`
	PlanID             string       `json:"plan_id,omitempty"`
	ProjectID          string       `json:"project_id,omitempty"`
	State              RunState     `json:"state"`
	Gate               string       `json:"gate"`
	Brain              *BrainStatus `json:"brain"`
	WorkersInFlight    int          `json:"workers_in_flight"`
	Tasks              TaskCounts   `json:"tasks"`
	Backoff            *Backoff     `json:"backoff"`
	// NeedsAttention is set when the guardian stopped retrying a failure that
	// cannot succeed on retry; it carries the reason and how to clear it.
	NeedsAttention string `json:"needs_attention,omitempty"`
	// FinalPR is the single final PR (integration → default) once opened.
	FinalPR *FinalPR `json:"final_pr,omitempty"`
	// AwaitingMerge is set while the green final PR waits for its merge.
	AwaitingMerge *AwaitingMerge `json:"awaiting_merge,omitempty"`
	// ResolverAttempts counts resolver agents spawned per PR branch (capped at 3).
	ResolverAttempts map[string]int `json:"resolver_attempts,omitempty"`
	// LastProgressAt is the last observed run progress (ledger task change,
	// landing, plan task status change, worker spawn); Watchdog is the progress
	// watchdog state: disabled | idle | armed | escalating | parked.
	LastProgressAt string `json:"last_progress_at,omitempty"`
	Watchdog       string `json:"watchdog,omitempty"`
	// Supervisor says what supervises the run, whether it is addressable, and
	// its in-memory health (truthful-supervisor-status contract §3.2).
	Supervisor Supervisor `json:"supervisor"`
	// NextStep is the daemon supervisor's scheduled next action; RestingUntil is the
	// earliest instant a rate-limited agent is resumed or re-switched.
	NextStep     *NextStep `json:"next_step,omitempty"`
	RestingUntil string    `json:"resting_until,omitempty"`
	// LastDiagnosis is the guardian's last triage decision; Fix is the per-task
	// gate/fix-loop state; Resolver summarises resolver-agent activity. All are
	// persisted across daemon restarts (surface.go).
	LastDiagnosis *Diagnosis      `json:"last_diagnosis,omitempty"`
	Fix           []FixStatus     `json:"fix,omitempty"`
	Resolver      *ResolverStatus `json:"resolver,omitempty"`
	LandedTotal   int             `json:"landed_total"`
	PlanTasks     []PlanTask      `json:"plan_tasks"`
	SlotScope     string          `json:"slot_scope,omitempty"`
	ManagerSlotID string          `json:"manager_slot_id,omitempty"`
	// GuardianSlotID is a reserved slot name only (claims.go): no session with
	// that id exists and it must never be addressed.
	GuardianSlotID    string              `json:"guardian_slot_id,omitempty"`
	IntegrationBranch string              `json:"integration_branch,omitempty"`
	GateWarning       string              `json:"gate_warning,omitempty"`
	Workers           map[string][]string `json:"workers,omitempty"` // task_id -> worker session ids
	LedgerTasks       []LedgerTask        `json:"ledger_tasks,omitempty"`
	PreflightWarnings []string            `json:"preflight_warnings,omitempty"`
}

// BrainStatus describes the run's brain agent (autopilot.md §5). Nil in S1.
type BrainStatus struct {
	AgentID       string `json:"agent_id"`
	Backend       string `json:"backend"`
	Tier          string `json:"tier"`
	LastHeartbeat string `json:"last_heartbeat"`
	ContextLevel  string `json:"context_level"`
}

// TaskCounts is the ledger task rollup shown in status. Zero-valued in S1.
type TaskCounts struct {
	Pending    int `json:"pending"`
	InProgress int `json:"in_progress"`
	Landed     int `json:"landed"`
	Failed     int `json:"failed"`
}

// Backoff describes the guardian's capped-exponential backoff (autopilot.md
// §2.3). Nil unless the run is degraded — always nil in S1.
type Backoff struct {
	Stage       int    `json:"stage"`
	NextRetryAt string `json:"next_retry_at"`
	LastError   string `json:"last_error"`
	// Kind classifies the cause: backend_unavailable | no_backend_selectable |
	// definition_error | spawn_error.
	Kind string `json:"kind,omitempty"`
}

func copyAttempts(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Supervisor kinds (contract §3.1). Only SupervisorDaemon is implemented;
// SupervisorSession is reserved for a future session-backed supervisor.
const (
	SupervisorDaemon  = "daemon"
	SupervisorSession = "session"
	SupervisorNone    = "none"
)

// Supervisor states, health values and heal stages (contract §3.2).
const (
	SupStateSupervising = "supervising"
	SupStateIdle        = "idle"
	SupStateSuspended   = "suspended"
	SupStateDisabled    = "disabled"

	HealthHealthy = "healthy"
	HealthHealing = "healing"
	HealthBackoff = "backoff"
	HealthParked  = "parked"
	HealthUnknown = "unknown"
)

// Supervisor describes what supervises a run. Invariant S1: Addressable is true
// iff Kind == "session". Nothing here is persisted; heal state is rebuilt after
// a daemon restart, so Health is "unknown" until the first guardian tick.
type Supervisor struct {
	Kind        string `json:"kind"`
	Addressable bool   `json:"addressable"`
	SessionID   string `json:"session_id,omitempty"`
	State       string `json:"state"`
	Health      string `json:"health"`
	HealStage   string `json:"heal_stage"`
	LastTickAt  string `json:"last_tick_at,omitempty"`
	Persisted   bool   `json:"persisted"`
}

func (h healStage) wire() string {
	switch h {
	case stageNudged:
		return "nudged"
	case stageRestarted:
		return "restarted"
	case stageRotated:
		return "rotated"
	case stageBackoff:
		return "backoff"
	}
	return "none"
}

// supervisorLocked derives r's supervisor view from in-memory state; the caller
// holds c.mu.
func (c *Controller) supervisorLocked(r *run) Supervisor {
	sup := Supervisor{Kind: SupervisorDaemon, State: SupStateSupervising, Health: HealthUnknown,
		HealStage: r.healStage.wire(), LastTickAt: rfc3339OrEmpty(r.lastTickAt)}
	switch r.state {
	case StateDisabled:
		sup.Kind, sup.State = SupervisorNone, SupStateDisabled
		return sup
	case StateRegistered, StatePaused, StateStopped, StateComplete:
		sup.Kind, sup.State = SupervisorNone, SupStateSuspended
		return sup
	}
	if r.state == StateActive && c.awaitingMergeLocked(r) {
		sup.State = SupStateIdle
	}
	switch {
	case r.needsAttention != "":
		sup.Health = HealthParked
	case r.lastTickAt.IsZero():
		sup.Health = HealthUnknown
	case r.healStage == stageBackoff:
		sup.Health = HealthBackoff
	case r.healStage != stageHealthy || r.state == StateHealing || r.state == StateDegraded:
		sup.Health = HealthHealing
	default:
		sup.Health = HealthHealthy
	}
	return sup
}
