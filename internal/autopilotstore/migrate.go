package autopilotstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/planstore"
)

const (
	legacyRunsRelDir   = "autopilot/runs-db"
	legacyArchiveRel   = "autopilot/legacy-runs-db"
	importedMarkerName = ".autopilots-from-runs-imported"
	legacyCollection   = "autopilot_runs"
	archiveCollection  = "legacy_autopilot_runs"
)

// LegacyRunRecord mirrors the JSON shape of autopilot.RunRecord so migration
// can read the durable registration store without importing the autopilot
// package (avoids an import cycle once the controller adopts this store).
type LegacyRunRecord struct {
	RunID             string    `json:"run_id"`
	Name              string    `json:"name"`
	Repo              string    `json:"repo"`
	PlanFile          string    `json:"plan_file"`
	PlanID            string    `json:"plan_id,omitempty"`
	ProjectID         string    `json:"project_id,omitempty"`
	State             string    `json:"state"`
	IntegrationBranch string    `json:"integration_branch,omitempty"`
	Gate              string    `json:"gate,omitempty"`
	Strategy          string    `json:"strategy,omitempty"`
	DeleteBranch      bool      `json:"delete_branch,omitempty"`
	BrainID           string    `json:"brain_id,omitempty"`
	SlotScope         string    `json:"slot_scope,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// MigrateReport summarizes one legacy-run migration pass.
type MigrateReport struct {
	LiveCreated       int // live Autopilot entities created
	HistoryAttached   int // runs whose facts were written onto a Plan
	ArchivedUnmatched int // runs archived because no Plan could be resolved
	Skipped           int // already migrated / no-op rows
}

// Changed reports whether the pass wrote anything.
func (r MigrateReport) Changed() bool {
	return r.LiveCreated > 0 || r.HistoryAttached > 0 || r.ArchivedUnmatched > 0
}

// PlanResolver looks up plans during migration. *planstore.Store satisfies it.
type PlanResolver interface {
	Get(ctx context.Context, id string) (*planstore.Plan, error)
	List(ctx context.Context) ([]*planstore.Plan, error)
	Update(ctx context.Context, id string, fn func(*planstore.Plan) error) error
	AppendEvent(ctx context.Context, ev *planstore.PlanExecutionEvent) error
}

// MigrateLegacyRuns converts durable registered RunRecords into either:
//   - Plan execution history (+ optional live Autopilot when operationally live), or
//   - a legacy audit archive when no Plan can be resolved.
//
// It never deletes unmatched history. The source RunStore is left intact so the
// legacy controller keeps working until the autopilot-plan-controller cutover.
// Idempotent: a marker plus dedup keys make a second pass a no-op.
func MigrateLegacyRuns(ctx context.Context, dataDir string, live *Store, plans PlanResolver) (MigrateReport, error) {
	var rep MigrateReport
	if live == nil {
		return rep, fmt.Errorf("autopilotstore: live store is required")
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}

	marker := filepath.Join(dataDir, importedMarkerName)
	if _, err := os.Stat(marker); err == nil {
		// Marker present: still scan for any new legacy rows created after the
		// first pass (restart-safe incremental). Missing source is a no-op.
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return rep, err
	}

	legacyDir := filepath.Join(dataDir, filepath.FromSlash(legacyRunsRelDir))
	if _, err := os.Stat(legacyDir); errors.Is(err, os.ErrNotExist) {
		if werr := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); werr != nil {
			return rep, werr
		}
		return rep, nil
	} else if err != nil {
		return rep, err
	}

	runs, err := listLegacyRuns(legacyDir)
	if err != nil {
		return rep, err
	}
	if len(runs) == 0 {
		if werr := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); werr != nil {
			return rep, werr
		}
		return rep, nil
	}

	var planIndex map[string]*planstore.Plan
	if plans != nil {
		planIndex, err = buildPlanIndex(ctx, plans)
		if err != nil {
			return rep, err
		}
	}

	archive, err := openLegacyArchive(dataDir)
	if err != nil {
		return rep, err
	}
	defer archive.Close()

	for _, run := range runs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		plan := resolvePlan(run, planIndex)
		if plan == nil {
			created, aerr := archive.archive(run)
			if aerr != nil {
				return rep, aerr
			}
			if created {
				rep.ArchivedUnmatched++
			} else {
				rep.Skipped++
			}
			continue
		}

		attached, aerr := attachRunHistory(ctx, plans, plan, run)
		if aerr != nil {
			return rep, aerr
		}
		if attached {
			rep.HistoryAttached++
		}

		created := false
		if IsLiveState(run.State) {
			var cerr error
			created, cerr = ensureLiveAutopilot(ctx, live, plan, run)
			if cerr != nil {
				return rep, cerr
			}
			if created {
				rep.LiveCreated++
			}
		}
		if !attached && !created {
			rep.Skipped++
		}
	}

	if werr := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); werr != nil {
		return rep, werr
	}
	return rep, nil
}

func listLegacyRuns(legacyDir string) ([]LegacyRunRecord, error) {
	db, err := scriva.Open(legacyDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	col, err := db.Collection(legacyCollection)
	if err != nil {
		return nil, err
	}
	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]LegacyRunRecord, 0, len(rows))
	for _, row := range rows {
		b, err := json.Marshal(row.Data)
		if err != nil {
			return nil, err
		}
		var r LegacyRunRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		if r.RunID == "" {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

func buildPlanIndex(ctx context.Context, plans PlanResolver) (map[string]*planstore.Plan, error) {
	rows, err := plans.List(ctx)
	if err != nil {
		return nil, err
	}
	idx := make(map[string]*planstore.Plan, len(rows)*3)
	for _, p := range rows {
		if p == nil {
			continue
		}
		idx["id:"+p.ID] = p
		if p.Name != "" {
			idx["name:"+p.ProjectID+"\x00"+p.Name] = p
		}
		if p.FilePath != "" {
			idx["file:"+filepath.Clean(p.FilePath)] = p
			idx["base:"+filepath.Base(p.FilePath)] = p
		}
	}
	return idx, nil
}

func resolvePlan(run LegacyRunRecord, idx map[string]*planstore.Plan) *planstore.Plan {
	if idx == nil {
		return nil
	}
	if run.PlanID != "" {
		if p := idx["id:"+run.PlanID]; p != nil {
			return p
		}
	}
	if run.PlanFile != "" {
		clean := filepath.Clean(run.PlanFile)
		if p := idx["file:"+clean]; p != nil {
			return p
		}
		// Match on suffix (absolute plan_file vs relative FilePath).
		base := filepath.Base(clean)
		if p := idx["base:"+base]; p != nil {
			return p
		}
		for key, p := range idx {
			if !strings.HasPrefix(key, "file:") {
				continue
			}
			if strings.HasSuffix(clean, p.FilePath) || strings.HasSuffix(filepath.ToSlash(clean), filepath.ToSlash(p.FilePath)) {
				return p
			}
		}
	}
	if run.Name != "" && run.ProjectID != "" {
		if p := idx["name:"+run.ProjectID+"\x00"+run.Name]; p != nil {
			return p
		}
	}
	if run.Name != "" && run.Repo != "" {
		if p := idx["name:"+run.Repo+"\x00"+run.Name]; p != nil {
			return p
		}
	}
	return nil
}

// attachRunHistory writes typed PlanExecutionEvents (and a PlanExecution history
// entry) for a legacy run. Dedup keys make the write idempotent.
func attachRunHistory(ctx context.Context, plans PlanResolver, plan *planstore.Plan, run LegacyRunRecord) (bool, error) {
	if plans == nil || plan == nil {
		return false, nil
	}
	execID := legacyExecutionID(run.RunID)
	startedAt := run.CreatedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	updatedAt := run.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = startedAt
	}

	events := []*planstore.PlanExecutionEvent{
		{
			DedupKey:    "legacy-run:" + run.RunID + ":execution_started",
			PlanID:      plan.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutionStarted,
			OccurredAt:  startedAt,
			Payload: &planstore.EventPayload{
				ExecutorID:    run.RunID,
				ExecutionMode: string(planstore.PlanModeAutopilot),
				PlanName:      plan.Name,
				TasksTotal:    0,
			},
		},
		{
			DedupKey:    "legacy-run:" + run.RunID + ":executor_created",
			PlanID:      plan.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutorCreated,
			OccurredAt:  startedAt,
			Payload: &planstore.EventPayload{
				ExecutorID:    run.RunID,
				ExecutionMode: string(planstore.PlanModeAutopilot),
			},
		},
	}
	if run.BrainID != "" {
		events = append(events, &planstore.PlanExecutionEvent{
			DedupKey:    "legacy-run:" + run.RunID + ":agent_spawned",
			PlanID:      plan.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindAgentSpawned,
			OccurredAt:  startedAt,
			Payload: &planstore.EventPayload{
				AgentID:    run.BrainID,
				ExecutorID: run.RunID,
			},
		})
	}
	switch run.State {
	case "complete":
		events = append(events, &planstore.PlanExecutionEvent{
			DedupKey:    "legacy-run:" + run.RunID + ":completion_verified",
			PlanID:      plan.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindCompletionVerified,
			OccurredAt:  updatedAt,
			Payload:     &planstore.EventPayload{ExecutorID: run.RunID},
		})
	case "stopped", "disabled":
		events = append(events, &planstore.PlanExecutionEvent{
			DedupKey:    "legacy-run:" + run.RunID + ":execution_stopped",
			PlanID:      plan.ID,
			ExecutionID: execID,
			Kind:        planstore.EventKindExecutionStopped,
			OccurredAt:  updatedAt,
			Payload: &planstore.EventPayload{
				ExecutorID: run.RunID,
				StopReason: "legacy run state=" + run.State,
			},
		})
	}

	// Detect whether any event is new by listing before/after would be racy;
	// AppendEvent is idempotent, so we probe with ListEvents after writing and
	// treat "already had started event" as skip for HistoryAttached counting.
	before, err := listExecutionEvents(ctx, plans, plan.ID, execID)
	if err != nil {
		return false, err
	}
	hadStarted := false
	for _, ev := range before {
		if ev.Kind == planstore.EventKindExecutionStarted {
			hadStarted = true
			break
		}
	}

	for _, ev := range events {
		if err := plans.AppendEvent(ctx, ev); err != nil {
			return false, err
		}
	}

	termStatus := legacyTerminalStatus(run.State)
	pe := planstore.PlanExecution{
		ID:             execID,
		PlanID:         plan.ID,
		ExecutionMode:  planstore.PlanModeAutopilot,
		ExecutorID:     run.RunID,
		StartedAt:      startedAt,
		TerminalStatus: termStatus,
		PlanBranches:   nil,
	}
	if run.IntegrationBranch != "" {
		pe.PlanBranches = []string{run.IntegrationBranch}
	}
	if termStatus == planstore.ExecutionStatusCompleted ||
		termStatus == planstore.ExecutionStatusFailed ||
		termStatus == planstore.ExecutionStatusCancelled {
		t := updatedAt
		pe.CompletedAt = &t
	}

	if err := plans.Update(ctx, plan.ID, func(p *planstore.Plan) error {
		// Preserve an already-recorded history entry for this legacy execution.
		for _, h := range p.ExecutionHistory {
			if h.ID == execID || h.ExecutorID == run.RunID {
				return nil
			}
		}
		if p.ActiveExecution != nil && (p.ActiveExecution.ID == execID || p.ActiveExecution.ExecutorID == run.RunID) {
			return nil
		}
		if IsLiveState(run.State) {
			// Live legacy run: surface as ActiveExecution if none is set.
			if p.ActiveExecution == nil {
				cp := pe
				p.ActiveExecution = &cp
				if p.AutopilotRunID == "" {
					p.AutopilotRunID = run.RunID
				}
				if p.ExecutionMode == "" {
					p.ExecutionMode = planstore.PlanModeAutopilot
				}
			}
			return nil
		}
		p.ExecutionHistory = append(p.ExecutionHistory, pe)
		return nil
	}); err != nil {
		return false, err
	}

	return !hadStarted, nil
}

func listExecutionEvents(ctx context.Context, plans PlanResolver, planID, execID string) ([]*planstore.PlanExecutionEvent, error) {
	// PlanResolver does not expose ListEvents; use type assertion when available.
	type lister interface {
		ListEvents(ctx context.Context, planID, executionID string) ([]*planstore.PlanExecutionEvent, error)
	}
	if l, ok := plans.(lister); ok {
		return l.ListEvents(ctx, planID, execID)
	}
	return nil, nil
}

func ensureLiveAutopilot(ctx context.Context, live *Store, plan *planstore.Plan, run LegacyRunRecord) (bool, error) {
	if _, err := live.Get(ctx, run.RunID); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}

	projectID := run.ProjectID
	if projectID == "" {
		projectID = plan.ProjectID
	}
	if projectID == "" {
		projectID = run.Repo
	}

	a := &Autopilot{
		ID:             run.RunID,
		ProjectID:      projectID,
		PlanID:         plan.ID,
		Name:           DisplayName(plan.Name),
		ManagerAgentID: run.BrainID, // historical BrainID is the manager slot
		Diagnostics: Diagnostics{
			State:             run.State,
			IntegrationBranch: run.IntegrationBranch,
			Gate:              run.Gate,
			Strategy:          run.Strategy,
			SlotScope:         run.SlotScope,
			Repo:              run.Repo,
			PlanFile:          run.PlanFile,
		},
		CreatedAt: run.CreatedAt,
		UpdatedAt: run.UpdatedAt,
	}
	if err := live.Create(ctx, a); err != nil {
		if errors.Is(err, ErrExists) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func legacyExecutionID(runID string) string {
	// Deterministic pe- id derived from the run id so re-migration is stable.
	// pe- + 8 hex chars. Use a simple hex fold of the run id bytes.
	h := fmt.Sprintf("%x", []byte(runID))
	if len(h) < 8 {
		h = h + strings.Repeat("0", 8-len(h))
	}
	return "pe-" + h[:8]
}

func legacyTerminalStatus(state string) planstore.ExecutionStatus {
	switch state {
	case "complete":
		return planstore.ExecutionStatusCompleted
	case "stopped", "disabled":
		return planstore.ExecutionStatusCancelled
	case "starting", "active", "healing", "degraded", "paused":
		return planstore.ExecutionStatusRunning
	default:
		return planstore.ExecutionStatusCancelled
	}
}

type legacyArchive struct {
	db  *scriva.DB
	col *engine.Collection
}

func openLegacyArchive(dataDir string) (*legacyArchive, error) {
	dir := filepath.Join(dataDir, filepath.FromSlash(legacyArchiveRel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection(archiveCollection)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &legacyArchive{db: db, col: col}, nil
}

func (a *legacyArchive) Close() error {
	if a == nil || a.db == nil {
		return nil
	}
	return a.db.Close()
}

// archive stores an unmatched legacy run. Returns true when a new record was
// written. Existing keys are left untouched (never silently deleted).
func (a *legacyArchive) archive(run LegacyRunRecord) (bool, error) {
	b, err := json.Marshal(run)
	if err != nil {
		return false, err
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		return false, err
	}
	// Stamp archival metadata without overwriting factual fields.
	rec["archived_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["archive_reason"] = "plan_unresolved"
	_, _, err = a.col.InsertWithKey(run.RunID, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return false, nil
	}
	return err == nil, err
}

// ListLegacyArchive returns archived unmatched RunRecords (for tests / doctor).
func ListLegacyArchive(dataDir string) ([]LegacyRunRecord, error) {
	dir := filepath.Join(dataDir, filepath.FromSlash(legacyArchiveRel))
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	col, err := db.Collection(archiveCollection)
	if err != nil {
		return nil, err
	}
	rows, err := col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]LegacyRunRecord, 0, len(rows))
	for _, row := range rows {
		b, err := json.Marshal(row.Data)
		if err != nil {
			return nil, err
		}
		var r LegacyRunRecord
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}
