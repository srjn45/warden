// Package migrate provides the data schema migration registry and runner
// (docs/specs/2026-10-09-update-process.md §4).
//
// Migrations are ordered, idempotent, journaled through the ledger in_progress
// field, and verified BEFORE the ledger advances and a history entry is appended.
// Migrations run only via `warden migrate` or `wd update`. The daemon never runs
// destructive migrations at boot; it refuses start if data needs migration or
// has an interrupted migration.
package migrate

import (
	"context"
	"log/slog"

	"github.com/srjn45/warden/internal/config"
)

// Kind classifies what a migration mutates.
type Kind string

const (
	KindSchema Kind = "schema"
	KindData   Kind = "data"
	KindConfig Kind = "config"
)

// Severity classifies preflight findings.
type Severity string

const (
	SeverityAuto       Severity = "auto"
	SeverityRepairable Severity = "repairable"
	SeverityBlocking   Severity = "blocking"
)

// Finding is a preflight inspection finding.
type Finding struct {
	Severity Severity `json:"severity"`
	// Class is the stable strict-preflight reason (schema-2 cutover only).
	Class   string `json:"class,omitempty"`
	Store   string `json:"store,omitempty"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
	Command string `json:"command,omitempty"`
}

// ClassSchema2CutoverPending is the Finding.Class a schema-2 target binary
// reports for the expected schema-1 -> schema-2 cutover marker. Value matches
// the target release's constant so its JSON report decodes here.
const ClassSchema2CutoverPending = "schema2_cutover_pending"

// Step is one named, journaled sub-step of a Migration.
type Step struct {
	Name string
	Run  func(Env) error
}

// Env provides the execution environment for Check, Run, and Verify.
type Env struct {
	DataDir       string
	BinaryVersion string
	Config        *config.Config
	Logger        *slog.Logger
	Context       context.Context
	StepFn        func(name string) error
	// SnapshotFn records the immutable rollback source in the schema journal.
	// A migration must call it immediately after creating a backup, before it
	// makes any live-state change.
	SnapshotFn func(path string) error
}

func (e Env) Snapshot(path string) error {
	if e.SnapshotFn != nil {
		return e.SnapshotFn(path)
	}
	return nil
}

// Step records step progress in the journal.
func (e Env) Step(name string) error {
	if e.StepFn != nil {
		return e.StepFn(name)
	}
	return nil
}

// Migration defines one schema format transition.
type Migration struct {
	ID     string              // e.g. "0001-legacy-import"
	From   int                 // schema version before
	To     int                 // schema version after
	Kind   Kind                // Schema | Data | Config
	Check  func(Env) []Finding // read-only preflight check
	Steps  []Step              // optional discrete journaled steps
	Run    func(Env) error     // idempotent migration logic (used if Steps is empty)
	Verify func(Env) error     // post-condition check run before commit
}
