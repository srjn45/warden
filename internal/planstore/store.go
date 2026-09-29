// Package planstore persists plan records as first-class entities in the daemon.
//
// Plans are YAML files the user authors in their repository under plans/. The
// daemon scans those files at startup and on demand, deriving plan status from
// the subdirectory (pending/in_progress/completed/archived). This store holds
// the execution state — execution mode, linked run/pipeline IDs, task progress,
// and timestamps — that the YAML file intentionally does not carry.
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

// PlanStatus encodes which directory the plan YAML lives in.
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

// Plan is a tracked execution record for one plan YAML file in the repository.
// The canonical definition (goal, tasks, constraints, done_when) lives in the
// YAML; this record owns execution state only.
type Plan struct {
	ID        string `json:"id"` // plan-<8hex>, stable across git-mv
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`      // from YAML name: field or filename stem
	FilePath  string `json:"file_path"` // current path relative to project root

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

	// Hub sync seam — reserved for future warden-hub sync; never set by this package.
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
}

var (
	ErrNotFound = errors.New("plan not found")
	ErrExists   = errors.New("plan already exists")
)

// PlanID derives the stable plan ID from a project ID and plan name.
// Moving the YAML between status directories does not change this ID.
//
//	ID = "plan-" + hex(sha256(projectID + "\x00" + planName))[:8]
func PlanID(projectID, planName string) string {
	h := sha256.New()
	h.Write([]byte(projectID))
	h.Write([]byte{0x00})
	h.Write([]byte(planName))
	return fmt.Sprintf("plan-%x", h.Sum(nil)[:4])
}

// Store owns the ScrivaDB "plans" collection at <data>/plans-db.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
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
	return &Store{db: db, col: col}, nil
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
// already exists.
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

func (s *Store) Close() error { return s.db.Close() }
