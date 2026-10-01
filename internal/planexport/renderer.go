// Package planexport renders canonical ScrivaDB Plans as optional repository
// replicas and records per-repository export outcomes.
//
// Authority (docs/specs/2026-09-30-scrivadb-canonical-plans.md): replicas are
// inert. The conventional path plans/{lifecycle}/<slug>.yaml is descriptive
// only and must never feed canonical lifecycle. This package does not write
// repository files — Phase 7 (sync_to_repo) owns Git/PR mutation.
//
// The Renderer interface is format-neutral: YAML (Default) and JSON exporters
// share the same envelope without changing planstore.Plan storage.
package planexport

import (
	"fmt"
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

// Format names a replica encoding. Default() remains YAML; JSON is opt-in.
type Format string

const (
	FormatYAML Format = "yaml"
	FormatJSON Format = "json"
)

// SchemaVersion is the v1 export envelope schema_version.
const SchemaVersion = 1

// Options controls a single Render call. ExportedAt must be set so repeat
// renders of the same Plan revision are byte-identical when callers reuse it.
type Options struct {
	// ExportedAt is written into the envelope as RFC3339 UTC (…Z). Required.
	ExportedAt time.Time
}

// Envelope is the frozen export metadata projected into a replica (§8).
type Envelope struct {
	SchemaVersion       int                            `json:"schema_version"`
	PlanID              string                         `json:"plan_id"`
	Revision            int64                          `json:"revision"`
	ContentHash         string                         `json:"content_hash"`
	ExportedAt          time.Time                      `json:"exported_at"`
	Lifecycle           planstore.PlanStatus           `json:"lifecycle"`
	ExecutionSummaryRef *planstore.ExecutionSummaryRef `json:"execution_summary_ref,omitempty"`
}

// Result is the bytes of one rendered replica plus envelope and path metadata.
type Result struct {
	Format   Format
	Bytes    []byte
	Path     string // conventional plans/{lifecycle}/<slug>.{yaml|json} — descriptive only
	Envelope Envelope
}

// Renderer converts one canonical Plan revision into a replica encoding.
// Implementations must not consult the filesystem or mutate the Plan.
type Renderer interface {
	Format() Format
	Render(plan *planstore.Plan, opts Options) (*Result, error)
}

// Default returns the v1 YAML renderer.
func Default() Renderer {
	return YAMLRenderer{}
}

// BuildEnvelope derives the frozen envelope from a Plan. ContentHash is
// refreshed from definition fields when empty. ExecutionSummaryRef is set only
// when Plan.ExecutionSummary is present (reference only — no live query).
func BuildEnvelope(plan *planstore.Plan, exportedAt time.Time) (Envelope, error) {
	if plan == nil {
		return Envelope{}, fmt.Errorf("planexport: plan is nil")
	}
	if exportedAt.IsZero() {
		return Envelope{}, fmt.Errorf("planexport: exported_at is required")
	}
	hash := plan.ContentHash
	if hash == "" {
		hash = planstore.ComputeContentHash(plan)
	}
	env := Envelope{
		SchemaVersion: SchemaVersion,
		PlanID:        plan.ID,
		Revision:      plan.Revision,
		ContentHash:   hash,
		ExportedAt:    exportedAt.UTC().Truncate(time.Second),
		Lifecycle:     plan.Status,
	}
	if plan.ExecutionSummary != nil {
		env.ExecutionSummaryRef = &planstore.ExecutionSummaryRef{
			PlanID:      firstNonEmpty(plan.ExecutionSummary.PlanID, plan.ID),
			ExecutionID: summaryExecutionID(plan),
			ContentHash: hash,
		}
	}
	return env, nil
}

// RepoExportMeta builds the typed last-export metadata for Plan.RepoExport.
func RepoExportMeta(plan *planstore.Plan, exportedAt time.Time) (*planstore.RepoExportMeta, error) {
	env, err := BuildEnvelope(plan, exportedAt)
	if err != nil {
		return nil, err
	}
	at := env.ExportedAt
	return &planstore.RepoExportMeta{
		SchemaVersion:       env.SchemaVersion,
		Revision:            env.Revision,
		ContentHash:         env.ContentHash,
		ExportedAt:          &at,
		Lifecycle:           env.Lifecycle,
		FilePath:            ExportPath(env.Lifecycle, plan.Name),
		ExecutionSummaryRef: env.ExecutionSummaryRef,
	}, nil
}

func summaryExecutionID(plan *planstore.Plan) string {
	if plan.ActiveExecution != nil && plan.ActiveExecution.ID != "" {
		return plan.ActiveExecution.ID
	}
	if n := len(plan.ExecutionHistory); n > 0 {
		return plan.ExecutionHistory[n-1].ID
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
