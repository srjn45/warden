// Package planbackup defines the versioned Plan backup bundle for local backup
// and machine transfer (docs/specs/2026-09-30-scrivadb-canonical-plans.md Phase 9).
//
// A bundle carries canonical Plan definitions, revision/execution audit needed
// for recovery, execution events/summaries, and integrity hashes. It excludes
// credentials and disposable worktrees. Restore reads only the bundle — never
// Git or a repository replica under plans/.
package planbackup

import (
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// SchemaVersion is the bundle envelope version. Bump when the on-disk shape
// changes in a way restore must detect to read older dumps safely.
const SchemaVersion = 1

// ConflictPolicy controls restore when a stable Plan ID already exists.
type ConflictPolicy string

const (
	// ConflictSkip leaves the existing Plan untouched when the ID matches and
	// content hashes agree; reports conflict when hashes differ (default).
	ConflictSkip ConflictPolicy = "skip"
	// ConflictFail aborts the whole restore on any stable-ID collision whose
	// content hash differs (or any collision when StrictIdentical is false).
	ConflictFail ConflictPolicy = "fail"
	// ConflictOverwrite replaces the existing Plan (and re-applies events/notes)
	// when IDs collide. Use deliberately — it clobbers local audit state.
	ConflictOverwrite ConflictPolicy = "overwrite"
)

// EntryOutcome names one plan's fate during restore.
type EntryOutcome string

const (
	OutcomeRestored    EntryOutcome = "restored"
	OutcomeSkipped     EntryOutcome = "skipped"
	OutcomeConflicted  EntryOutcome = "conflicted"
	OutcomeOverwritten EntryOutcome = "overwritten"
	OutcomeValidated   EntryOutcome = "validated" // dry-run only
)

// Bundle is the on-the-wire / on-disk form of one or more Plans plus audit
// evidence. Written by Export, consumed by Restore.
type Bundle struct {
	SchemaVersion int       `json:"schema_version"`
	ExportedAt    time.Time `json:"exported_at"`
	// BundleHash is sha256:… over the deterministic hash payload of Entries
	// (excluding BundleHash itself). Empty until Seal.
	BundleHash string  `json:"bundle_hash,omitempty"`
	Entries    []Entry `json:"entries"`
}

// Entry is one Plan plus its audit children and per-entry integrity hashes.
type Entry struct {
	Plan   *planstore.Plan                 `json:"plan"`
	Events []*planstore.PlanExecutionEvent `json:"events,omitempty"`
	Notes  []*planstore.ExecutionNote      `json:"notes,omitempty"`

	// PlanContentHash mirrors plan.ContentHash (canonical definition digest).
	PlanContentHash string `json:"plan_content_hash"`
	// EventsHash / NotesHash cover the sorted child collections.
	EventsHash string `json:"events_hash"`
	NotesHash  string `json:"notes_hash"`
	// EntryHash covers plan id + revision + the three digests above.
	EntryHash string `json:"entry_hash"`
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// DryRun validates integrity and conflict policy without mutating the store.
	DryRun bool
	// OnConflict is skip (default), fail, or overwrite.
	OnConflict ConflictPolicy
}

// EntryResult is the per-plan outcome of a restore (or dry-run).
type EntryResult struct {
	PlanID      string       `json:"plan_id"`
	Outcome     EntryOutcome `json:"outcome"`
	ContentHash string       `json:"content_hash,omitempty"`
	Revision    int64        `json:"revision,omitempty"`
	Reason      string       `json:"reason,omitempty"`
}

// RestoreResult summarizes one Restore call.
type RestoreResult struct {
	DryRun  bool          `json:"dry_run"`
	Entries []EntryResult `json:"entries"`
}
