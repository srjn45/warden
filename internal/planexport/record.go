package planexport

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// Outcome is the terminal result of one repository export attempt.
type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeConflict Outcome = "conflict"
	OutcomeFailed   Outcome = "failed"
	OutcomeSkipped  Outcome = "skipped" // same rev/hash already exported
)

// Valid reports whether o is a known export outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeSuccess, OutcomeConflict, OutcomeFailed, OutcomeSkipped:
		return true
	}
	return false
}

// Record is one per-repository export of a Plan revision. Records are keyed by
// PlanID + repository identity + target ref + output path so Phase 7 can
// return a prior sync result without opening a duplicate PR.
type Record struct {
	ID           string    `json:"id"` // RecordID(...)
	PlanID       string    `json:"plan_id"`
	Repository   string    `json:"repository"` // remote URL or project identity
	TargetRef    string    `json:"target_ref"` // base branch / ref the PR targets
	OutputPath   string    `json:"output_path"`
	Revision     int64     `json:"revision"`
	ContentHash  string    `json:"content_hash"`
	PRURL        string    `json:"pr_url,omitempty"`
	CommitSHA    string    `json:"commit_sha,omitempty"`
	Outcome      Outcome   `json:"outcome"`
	ErrorMessage string    `json:"error_message,omitempty"`
	ExportedAt   time.Time `json:"exported_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// RecordID derives the stable store key for a (plan, repo, ref, path) tuple.
func RecordID(planID, repository, targetRef, outputPath string) string {
	h := sha256.New()
	h.Write([]byte(planID))
	h.Write([]byte{0x00})
	h.Write([]byte(repository))
	h.Write([]byte{0x00})
	h.Write([]byte(targetRef))
	h.Write([]byte{0x00})
	h.Write([]byte(outputPath))
	return fmt.Sprintf("pex-%x", h.Sum(nil)[:8])
}

// SameExport reports whether r already covers the given revision and content
// hash (Phase 7 idempotency check).
func (r *Record) SameExport(revision int64, contentHash string) bool {
	if r == nil {
		return false
	}
	return r.Revision == revision && r.ContentHash == contentHash && r.Outcome == OutcomeSuccess
}
