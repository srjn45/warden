package planstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrRevisionConflict is returned when an UpdateIf expected revision does not
// match the stored Plan.Revision.
var ErrRevisionConflict = errors.New("plan revision conflict")

// RevisionConflictError is the structured conflict returned by UpdateIf when
// concurrent editors race. Callers should surface Expected vs Actual rather
// than silently retrying with stale state.
type RevisionConflictError struct {
	PlanID   string
	Expected int64
	Actual   int64
}

func (e *RevisionConflictError) Error() string {
	if e == nil {
		return ErrRevisionConflict.Error()
	}
	return fmt.Sprintf("plan %s revision conflict: expected %d, actual %d", e.PlanID, e.Expected, e.Actual)
}

func (e *RevisionConflictError) Unwrap() error { return ErrRevisionConflict }

// PlanTask is one node in the canonical task DAG stored on the Plan record.
// Only id / prompt / after participate in the definition (and thus content_hash).
type PlanTask struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	After  []string `json:"after,omitempty"`
}

// ExecutionSummaryRef is an immutable pointer to a persisted ExecutionSummary
// carried on a repository export envelope. It is a reference only — not a live
// query handle.
type ExecutionSummaryRef struct {
	PlanID      string `json:"plan_id"`
	ExecutionID string `json:"execution_id,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

// RepoExportMeta is typed metadata about the last repository replica export of
// a Plan. It is intentionally separate from execution history: exports are
// inert replicas (see docs/specs/2026-09-30-scrivadb-canonical-plans.md D2).
//
// Nil / absent on Plans that have never been exported. Phase 6/7 populate this
// when sync_to_repo lands; Phase 2 only defines the schema.
type RepoExportMeta struct {
	SchemaVersion       int                  `json:"schema_version,omitempty"`
	Revision            int64                `json:"revision,omitempty"`
	ContentHash         string               `json:"content_hash,omitempty"`
	ExportedAt          *time.Time           `json:"exported_at,omitempty"`
	Lifecycle           PlanStatus           `json:"lifecycle,omitempty"`
	FilePath            string               `json:"file_path,omitempty"` // last export path relative to project root
	ExecutionSummaryRef *ExecutionSummaryRef `json:"execution_summary_ref,omitempty"`
}

// contentHashPolicy (Phase 2 lock): ContentHash covers ONLY the canonical
// definition fields — Name, Goal, Constraints, DoneWhen, and Tasks
// (id/prompt/after). Lifecycle (Status), timestamps, FilePath, RepoExport,
// revision itself, and all execution/audit fields are excluded.
//
// Revision increments on every successful UpdateIf that changes either the
// definition hash or Status. Pure execution mutations (TaskProgress,
// ActiveExecution, events, …) that go through UpdateIf without touching
// definition or Status do not bump Revision. The unconstrained Update path
// (legacy scan / finalize) does not bump Revision either — optimistic
// concurrency is opt-in via UpdateIf.

// canonicalDef is the stable JSON shape hashed for ContentHash.
type canonicalDef struct {
	Name        string          `json:"name"`
	Goal        string          `json:"goal"`
	Constraints []string        `json:"constraints"`
	DoneWhen    []string        `json:"done_when"`
	Tasks       []canonicalTask `json:"tasks"`
}

type canonicalTask struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	After  []string `json:"after"`
}

// ComputeContentHash returns the deterministic sha256:… digest of p's canonical
// definition fields. Nil and empty slices normalize to []. Task order is
// declaration order (DAG order is significant).
func ComputeContentHash(p *Plan) string {
	if p == nil {
		return hashCanonical(canonicalDef{
			Constraints: []string{},
			DoneWhen:    []string{},
			Tasks:       []canonicalTask{},
		})
	}
	def := canonicalDef{
		Name:        p.Name,
		Goal:        p.Goal,
		Constraints: normalizeStringSlice(p.Constraints),
		DoneWhen:    normalizeStringSlice(p.DoneWhen),
		Tasks:       make([]canonicalTask, 0, len(p.Tasks)),
	}
	for _, t := range p.Tasks {
		def.Tasks = append(def.Tasks, canonicalTask{
			ID:     t.ID,
			Prompt: t.Prompt,
			After:  normalizeStringSlice(t.After),
		})
	}
	return hashCanonical(def)
}

func hashCanonical(def canonicalDef) string {
	b, err := json.Marshal(def)
	if err != nil {
		// Def is composed only of strings/slices; marshal cannot fail in practice.
		panic("planstore: content hash marshal: " + err.Error())
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("sha256:%x", sum)
}

func normalizeStringSlice(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// RefreshContentHash sets p.ContentHash from the current definition fields.
func RefreshContentHash(p *Plan) {
	if p == nil {
		return
	}
	p.ContentHash = ComputeContentHash(p)
}

// definitionOrStatusChanged reports whether fn mutated fields that participate
// in the revision-bump policy.
func definitionOrStatusChanged(beforeHash string, beforeStatus PlanStatus, after *Plan) bool {
	if after == nil {
		return false
	}
	if after.Status != beforeStatus {
		return true
	}
	return ComputeContentHash(after) != beforeHash
}
