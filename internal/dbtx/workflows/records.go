package workflows

// Minimal typed records. Field names follow the schema-2 contract; optional
// references are omitted when empty so sparse indexes never see "".

// Project (contract §3.3). Version is the source revision projections track;
// LiveAgents/ArchivedAgents are derived (D) counters maintained in the same
// transaction as the agents they count.
type Project struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"` // open | closed
	Version        uint64 `json:"version"`
	LiveAgents     int    `json:"live_agents"`
	ArchivedAgents int    `json:"archived_agents"`
}

// Agent (contract §3.1). The same shape is used in agents_archive, where Status
// is "archived" and ArchivedFrom holds the prior live status.
type Agent struct {
	ID           string `json:"id"`
	ProjectID    string `json:"project_id"`
	Status       string `json:"status"` // starting | running | stopped | archived
	Role         string `json:"role,omitempty"`
	RunID        string `json:"autopilot_run_id,omitempty"`
	ArchivedFrom string `json:"archived_from,omitempty"`
}

// Plan (contract §3.5). Revision counts authoritative changes; every change has
// exactly one Event row.
type Plan struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	Status     string `json:"status"` // draft | ready | running
	Revision   uint64 `json:"revision"`
	RunID      string `json:"autopilot_run_id,omitempty"`
	PipelineID string `json:"pipeline_id,omitempty"`
}

// Pipeline (contract §3.7). Jobs is the derived job count.
type Pipeline struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	PlanID    string `json:"plan_id,omitempty"`
	Status    string `json:"status"` // running | canceled
	Jobs      int    `json:"jobs"`
}

// PipelineJob (contract §3.8); id is "<pipeline>:<job>".
type PipelineJob struct {
	ID         string `json:"id"`
	PipelineID string `json:"pipeline_id"`
	JobID      string `json:"job_id"`
	Status     string `json:"status"`
}

// Run (contract §3.9). LivePlan is set only while the run is non-terminal.
type Run struct {
	ID             string `json:"id"`
	ProjectID      string `json:"project_id"`
	PlanID         string `json:"plan_id"`
	ManagerAgentID string `json:"manager_agent_id"`
	Status         string `json:"status"` // running | stopped
	LivePlan       string `json:"live_plan,omitempty"`
}

// View is the persisted project projection. It is derived: SourceVersion must
// equal the project's Version, and its copied fields must match.
type View struct {
	ID             string `json:"id"` // project id
	Name           string `json:"name"`
	Status         string `json:"status"`
	LiveAgents     int    `json:"live_agents"`
	ArchivedAgents int    `json:"archived_agents"`
	SourceVersion  uint64 `json:"source_version"`
}

// Event is the append-only audit row for one plan revision; id is
// "<plan>@<revision>" so a duplicate append is rejected by the unique index.
type Event struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Subject    string `json:"subject"` // plan id
	SubjectRev uint64 `json:"subject_rev"`
	To         string `json:"to"` // plan status after the change
}

// Operation is an operation-journal row: intent written by the transaction,
// outcome by a later transaction. The external effect itself is never run here.
type Operation struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	State       string `json:"state"` // pending | succeeded
}
