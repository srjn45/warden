package wardendb

// HistoricalRef records a reference from an archived or finished record to a
// parent that no longer exists. The schema-1 -> schema-2 cutover moves such a
// dangling pipeline_id/job_id out of the relationship fields (which the
// relationship validator requires to resolve) and keeps it here, so the history
// stays auditable without a fictional runtime parent.
type HistoricalRef struct {
	// Kind names the missing parent's type ("pipeline").
	Kind string `json:"kind"`
	ID   string `json:"id"`
	// JobID is the job binding the record held inside the missing pipeline.
	JobID string `json:"job_id,omitempty"`
	// Reason is HistoricalDeleted (proven deleted) or HistoricalUnrecorded (no
	// record of the parent exists in any source).
	Reason string `json:"reason"`
	// Evidence says how Reason was established, without copying source content.
	Evidence string `json:"evidence,omitempty"`
	// RecordedBy is the migration that moved the reference.
	RecordedBy string `json:"recorded_by"`
}

const (
	HistoricalKindPipeline = "pipeline"
	HistoricalDeleted      = "deleted"
	HistoricalUnrecorded   = "unrecorded"
)
