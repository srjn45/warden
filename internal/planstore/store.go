// Package planstore persists plan records as first-class entities in the daemon.
//
// Authority (docs/specs/2026-09-30-scrivadb-canonical-plans.md): the ScrivaDB
// Plan record is the sole canonical Plan. It carries the complete definition
// (name, goal, constraints, done_when, task DAG), lifecycle Status + timestamps,
// monotonic Revision + ContentHash, execution/audit fields, and optional
// RepoExport metadata. Repository YAML under plans/ is an optional inert
// replica export — not discovery or execution authority.
//
// Legacy YAML scan/write paths remain in this package for migration until later
// phases retire them; they must not be treated as the source of truth for new
// work.
package planstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

// PlanStatus is the Plan lifecycle field. After the ScrivaDB-canonical cutover
// it is never inferred from an export path; directory names in replica exports
// are descriptive conventions only.
type PlanStatus string

const (
	PlanStatusPending    PlanStatus = "pending"
	PlanStatusInProgress PlanStatus = "in_progress"
	PlanStatusCompleted  PlanStatus = "completed"
	PlanStatusArchived   PlanStatus = "archived"
)

// Valid reports whether s is a known plan status.
func (s PlanStatus) Valid() bool {
	switch s {
	case PlanStatusPending, PlanStatusInProgress, PlanStatusCompleted, PlanStatusArchived:
		return true
	}
	return false
}

// PlanExecutionMode describes how a plan was (or will be) executed.
type PlanExecutionMode string

const (
	PlanModeAutopilot          PlanExecutionMode = "autopilot"
	PlanModePipeline           PlanExecutionMode = "pipeline"
	PlanModeOrchestratorWorker PlanExecutionMode = "orchestrator_worker"
	PlanModeManual             PlanExecutionMode = "manual"
)

// Plan is the canonical ScrivaDB Plan record: definition, lifecycle, revision,
// content hash, optional repo-export metadata, and execution/audit state.
//
// New fields added after the YAML-authority era decode as zero/nil when absent
// from older stored documents (backward-compatible).
type Plan struct {
	ID        string `json:"id"` // plan-<8hex>, stable across renames/imports
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`

	// FilePath is legacy / last-known relative path metadata. After cutover it
	// may mirror RepoExport.FilePath but is never an execution input. Kept for
	// decode compatibility with pre-canonical records.
	FilePath string `json:"file_path,omitempty"`

	// Canonical definition fields (authority lives here, not in repo YAML).
	Goal        string     `json:"goal,omitempty"`
	Constraints []string   `json:"constraints,omitempty"`
	DoneWhen    []string   `json:"done_when,omitempty"`
	Tasks       []PlanTask `json:"tasks,omitempty"`

	Status        PlanStatus        `json:"status"`
	ExecutionMode PlanExecutionMode `json:"execution_mode,omitempty"`

	// At most one execution link is set at a time.
	AutopilotRunID string `json:"autopilot_run_id,omitempty"`
	PipelineID     string `json:"pipeline_id,omitempty"`
	OrchestratorID string `json:"orchestrator_id,omitempty"`

	// Per-task progress: task id → "pending"|"in_progress"|"done"|"skipped".
	TaskProgress map[string]string `json:"task_progress,omitempty"`

	// Branches is the list of git branches associated with this plan's
	// execution. Populated by scan (heuristic match on plan name/id) and by
	// run. Checked before completion and used to clean up worktrees.
	// Serialized as plan_branches to match the OpenAPI contract. ScrivaDB
	// stores plans as JSON documents, so adding this field needs no migration.
	Branches []string `json:"plan_branches,omitempty"`

	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	ArchivedAt  *time.Time `json:"archived_at,omitempty"`

	// Revision is the optimistic-concurrency token. Starts at 1 on Create.
	// Bumps on UpdateIf when definition hash or Status changes (see
	// contentHashPolicy in canonical.go). Absent in old records → 0.
	Revision int64 `json:"revision,omitempty"`

	// ContentHash is sha256:… over canonical definition fields only.
	// Absent in old records → "" until refreshed.
	ContentHash string `json:"content_hash,omitempty"`

	// RepoExport is typed last-export metadata, separate from ExecutionHistory.
	// Absent / nil when never exported.
	RepoExport *RepoExportMeta `json:"repo_export,omitempty"`

	// Hub sync seam — reserved for future warden-hub sync via internal/plansync;
	// never set by this package. See docs/specs/2026-09-30-plan-hub-sync-boundary.md.
	SyncedAt *time.Time `json:"synced_at,omitempty"`
	RemoteID string     `json:"remote_id,omitempty"`

	// Execution model — flattened here for now; later tasks promote
	// ActiveExecution to a first-class ScrivaDB record.

	// ActiveExecution is the single currently-running execution of this plan.
	// At most one is active at a time; nil when the plan is not running.
	// New field: absent in old records → decodes as nil (backward-compatible).
	ActiveExecution *PlanExecution `json:"active_execution,omitempty"`

	// ExecutionHistory is the append-only list of all past executions.
	// New field: absent in old records → decodes as nil (backward-compatible).
	ExecutionHistory []PlanExecution `json:"execution_history,omitempty"`

	// TaskOutcomes records terminal evidence for each task: agent assignment,
	// verified checks, PRs opened, and final status. Lives on the Plan (not on
	// the worker Agent) so it survives Agent teardown.
	// New field: absent in old records → decodes as nil (backward-compatible).
	TaskOutcomes map[string]TaskOutcome `json:"task_outcomes,omitempty"`

	// BranchSummaries records each git branch opened during execution with its
	// linked task, agent, and PR evidence.
	// New field: absent in old records → decodes as nil (backward-compatible).
	BranchSummaries []BranchSummary `json:"branch_summaries,omitempty"`

	// ExecutionSummary is the immutable reduced report persisted by Finalize
	// before executor cleanup. Once set it is never overwritten (retry-safe).
	// New field: absent in old records → decodes as nil (backward-compatible).
	ExecutionSummary *ExecutionSummary `json:"execution_summary,omitempty"`

	// CleanupEvidence records a partial executor teardown so Finalize can be
	// retried without losing ExecutionSummary. Cleared on successful cleanup.
	// New field: absent in old records → decodes as nil (backward-compatible).
	CleanupEvidence *CleanupEvidence `json:"cleanup_evidence,omitempty"`
}

var (
	ErrNotFound = errors.New("plan not found")
	ErrExists   = errors.New("plan already exists")
)

// PlanID derives the stable plan ID from a project ID and plan name.
// Renames of replica export paths do not change this ID; imports preserve it
// so execution links survive.
//
//	ID = "plan-" + hex(sha256(projectID + "\x00" + planName))[:8]
func PlanID(projectID, planName string) string {
	h := sha256.New()
	h.Write([]byte(projectID))
	h.Write([]byte{0x00})
	h.Write([]byte(planName))
	return fmt.Sprintf("plan-%x", h.Sum(nil)[:4])
}

// Store owns the ScrivaDB "plans", "plan-events", and "plan-notes" collections
// at <data>/plans-db. Events and notes are child collections of Plan — they
// survive executor teardown because they are not stored per-executor.
type Store struct {
	mu         sync.Mutex
	db         *scriva.DB
	col        *engine.Collection // "plans"
	events     *engine.Collection // "plan-events"  (append-only audit log)
	notes      *engine.Collection // "plan-notes"   (attributed agent prose)
	seqCounter int64              // monotonic event sequence, guarded by mu
}

// New opens (creating if needed) the ScrivaDB-backed plan store at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbDir := filepath.Join(dir, "plans-db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("plans")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	evts, err := db.Collection("plan-events")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	nts, err := db.Collection("plan-notes")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	seq, err := initSeqCounter(evts)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, col: col, events: evts, notes: nts, seqCounter: seq}, nil
}

// initSeqCounter scans existing events to find the current high-water seq so
// that new events continue from where the previous store instance left off.
func initSeqCounter(col *engine.Collection) (int64, error) {
	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, row := range rows {
		b, err := json.Marshal(row.Data)
		if err != nil {
			continue
		}
		var ev struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(b, &ev); err == nil && ev.Seq > max {
			max = ev.Seq
		}
	}
	return max, nil
}

func encodeRecord(p *Plan) (map[string]any, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func decodeRecord(rec map[string]any) (*Plan, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) get(id string) (*Plan, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeRecord(r.Data)
}

// Create inserts a new plan. Returns ErrExists if a record with the same ID
// already exists. When Revision is 0 it is initialized to 1; ContentHash is
// always refreshed from the definition fields.
func (s *Store) Create(ctx context.Context, p *Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	if p.Revision == 0 {
		p.Revision = 1
	}
	RefreshContentHash(p)
	rec, err := encodeRecord(p)
	if err != nil {
		return err
	}
	_, _, err = s.col.InsertWithKey(p.ID, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return ErrExists
	}
	return err
}

// Get returns the plan with the given ID.
func (s *Store) Get(ctx context.Context, id string) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// List returns all plans, newest-updated first.
func (s *Store) List(ctx context.Context) ([]*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Plan, 0, len(rows))
	for _, row := range rows {
		p, err := decodeRecord(row.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// ListByProject returns all plans for the given project, newest-updated first.
func (s *Store) ListByProject(ctx context.Context, projectID string) ([]*Plan, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Plan
	for _, p := range all {
		if p.ProjectID == projectID {
			out = append(out, p)
		}
	}
	return out, nil
}

// ListByProjectAndStatus returns plans for the given project filtered by status.
func (s *Store) ListByProjectAndStatus(ctx context.Context, projectID string, status PlanStatus) ([]*Plan, error) {
	all, err := s.ListByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var filtered []*Plan
	for _, p := range all {
		if p.Status == status {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

// Update atomically applies fn to a plan and stamps UpdatedAt.
// It does not enforce optimistic concurrency and does not bump Revision —
// use UpdateIf for concurrent editors that must not silently overwrite.
func (s *Store) Update(ctx context.Context, id string, fn func(*Plan) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(id)
	if err != nil {
		return err
	}
	if err := fn(p); err != nil {
		return err
	}
	p.UpdatedAt = time.Now().UTC()
	rec, err := encodeRecord(p)
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(id, rec)
	return err
}

// UpdateIf applies fn only when the stored Plan.Revision equals expected.
// On mismatch it returns a *RevisionConflictError (errors.Is → ErrRevisionConflict)
// and leaves the record unchanged.
//
// After fn succeeds, if the definition content hash or Status changed relative
// to the pre-update snapshot, Revision is incremented and ContentHash is
// refreshed. Execution-only mutations do not bump Revision.
func (s *Store) UpdateIf(ctx context.Context, id string, expected int64, fn func(*Plan) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(id)
	if err != nil {
		return err
	}
	if p.Revision != expected {
		return &RevisionConflictError{PlanID: id, Expected: expected, Actual: p.Revision}
	}
	beforeHash := ComputeContentHash(p)
	beforeStatus := p.Status
	if err := fn(p); err != nil {
		return err
	}
	if definitionOrStatusChanged(beforeHash, beforeStatus, p) {
		p.Revision++
		RefreshContentHash(p)
	}
	p.UpdatedAt = time.Now().UTC()
	rec, err := encodeRecord(p)
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(id, rec)
	return err
}

// Delete permanently removes a plan record. A missing id returns ErrNotFound.
// The YAML file in the repository is never touched.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.col.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

// RestorePlan inserts a Plan exactly as provided for backup restore. Unlike
// Create it does not rewrite timestamps or Revision; ContentHash is verified
// against the canonical definition (and filled when empty). Returns ErrExists
// when the stable ID is already present.
//
// Restore must not consult Git or repository replicas — callers pass the
// canonical record from a Plan backup bundle only.
func (s *Store) RestorePlan(ctx context.Context, p *Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.ID == "" {
		return fmt.Errorf("planstore: restore requires plan id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	want := ComputeContentHash(p)
	if p.ContentHash == "" {
		p.ContentHash = want
	} else if p.ContentHash != want {
		return fmt.Errorf("planstore: restore content hash mismatch for %s: bundle %s != computed %s",
			p.ID, p.ContentHash, want)
	}
	if p.Revision == 0 {
		p.Revision = 1
	}
	rec, err := encodeRecord(p)
	if err != nil {
		return err
	}
	_, _, err = s.col.InsertWithKey(p.ID, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return ErrExists
	}
	return err
}

func (s *Store) Close() error { return s.db.Close() }
