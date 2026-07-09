package autopilot

// RunState is the run state machine (autopilot.md §2.1). S1 only ever produces
// the disabled → starting → active path (no brain, so "brain healthy" is
// immediate); the degraded/healing/complete states are reserved for S3+.
type RunState string

const (
	StateDisabled RunState = "disabled"
	StateStarting RunState = "starting"
	StateActive   RunState = "active"
	StateDegraded RunState = "degraded"
	StateHealing  RunState = "healing"
	StateComplete RunState = "complete"
)

// Status is the AutopilotStatus wire shape (autopilot.md §5). It is mapped into
// the OpenAPI schema via x-go-type, so this struct's JSON tags ARE the API
// contract — keep them in sync with the spec.
type Status struct {
	Enabled bool        `json:"enabled"`
	Runs    []RunStatus `json:"runs"`
}

// RunStatus is one run's slice of AutopilotStatus. In the S1 inert core Brain is
// always nil, WorkersInFlight/Tasks/LandedTotal are zero, and Backoff is nil —
// those go live in S3/S4/S5.
type RunStatus struct {
	RunID           string       `json:"run_id"`
	PlanFile        string       `json:"plan_file"`
	Repo            string       `json:"repo"`
	State           RunState     `json:"state"`
	Gate            string       `json:"gate"`
	Brain           *BrainStatus `json:"brain"`
	WorkersInFlight int          `json:"workers_in_flight"`
	Tasks           TaskCounts   `json:"tasks"`
	Backoff         *Backoff     `json:"backoff"`
	LandedTotal     int          `json:"landed_total"`
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
}

// Backoff describes the guardian's capped-exponential backoff (autopilot.md
// §2.3). Nil unless the run is degraded — always nil in S1.
type Backoff struct {
	Stage       int    `json:"stage"`
	NextRetryAt string `json:"next_retry_at"`
	LastError   string `json:"last_error"`
}
