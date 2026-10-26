package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/wardendb"
)

type runPayload RunRecord

type schema3RunDocs struct {
	*wardendb.OwnedDocuments[runPayload]
	db *wardendb.DB
}

func schema3RunLive(value any) bool {
	switch value {
	case "starting", "active", "paused", "degraded", "healing":
		return true
	}
	return false
}

func NewSchema3RunStoreFromDB(db *wardendb.DB) (*RunStore, error) {
	contract := wardendb.OwnerContract{
		Identity: "run_id", Immutable: []string{"plan_id", "parent_agent_id"},
		References:   map[string]string{"plan_id": "plans", "parent_agent_id": "agents"},
		RestrictedBy: map[string]string{"agents": "autopilot_run_id"},
		Forbidden:    []string{"brain_id", "manager_agent_id", "brain_agent_id", "diagnostics"},
		Active:       func(m map[string]any) bool { return schema3RunLive(m["state"]) },
		Encode: func(body map[string]any) (map[string]any, error) {
			m, err := wardendb.LosslessDocument(body)
			if err != nil {
				return nil, err
			}
			delete(m, "brain_id")
			switch m["state"] {
			case "registered", "disabled", "starting", "active", "paused", "stopped", "degraded", "healing", "complete":
			default:
				return nil, fmt.Errorf("autopilot: unknown schema-3 run state")
			}
			planID, _ := m["plan_id"].(string)
			if schema3RunLive(m["state"]) && planID == "" {
				return nil, ErrPlanIDRequired
			}
			return m, nil
		},
		Validate: func(ctx context.Context, tx *dbtx.Tx, body, old map[string]any) error {
			if !schema3RunLive(body["state"]) {
				return nil
			}
			handle, err := db.Handle()
			if err != nil {
				return err
			}
			col, err := handle.Engine().Collection("autopilot_runs")
			if err != nil {
				return err
			}
			value, _ := json.Marshal(body["plan_id"])
			rows, err := col.Scan(&query.FieldFilter{Field: "plan_id", Op: query.OpEq, Value: string(value)})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.Data["run_id"] != body["run_id"] && schema3RunLive(row.Data["state"]) {
					return fmt.Errorf("autopilot: another operational run already owns the Plan")
				}
			}
			return nil
		},
	}
	col, err := wardendb.RegisterOwned[runPayload](db, runsCollection, contract, "project_id", "plan_id", "parent_agent_id", "state")
	if err != nil {
		return nil, err
	}
	return &RunStore{col: &schema3RunDocs{OwnedDocuments: col, db: db}}, nil
}

func (s *RunStore) IsSchema3() bool {
	if s == nil {
		return false
	}
	_, ok := s.col.(*schema3RunDocs)
	return ok
}

func (d *schema3RunDocs) manager(runID string) (string, error) {
	handle, err := d.db.Handle()
	if err != nil {
		return "", err
	}
	agents, err := handle.Engine().Collection("agents")
	if err != nil {
		return "", err
	}
	value, _ := json.Marshal(runID)
	rows, err := agents.Scan(&query.FieldFilter{Field: "autopilot_run_id", Op: query.OpEq, Value: string(value)})
	if err != nil {
		return "", err
	}
	manager := ""
	for _, row := range rows {
		if row.Data["autopilot_slot"] != "autopilot" || row.Data["lifecycle"] == "archived" {
			continue
		}
		switch row.Data["status"] {
		case "done", "closed", "terminated", "failed", "exited":
			continue
		}
		if manager != "" {
			return "", fmt.Errorf("autopilot: multiple live manager attempts")
		}
		manager, _ = row.Data["id"].(string)
	}
	return manager, nil
}
func (d *schema3RunDocs) GetByKey(key string) (engine.Record, error) {
	r, err := d.OwnedDocuments.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	manager, err := d.manager(key)
	if err != nil {
		return engine.Record{}, err
	}
	r.Data, err = wardendb.LosslessDocument(r.Data)
	if err != nil {
		return engine.Record{}, err
	}
	r.Data["brain_id"] = manager // response projection only
	return r, nil
}
func (d *schema3RunDocs) Scan(f query.Filter) ([]engine.ScanResult, error) {
	rows, err := d.OwnedDocuments.Scan(f)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		id, _ := rows[i].Data["run_id"].(string)
		manager, err := d.manager(id)
		if err != nil {
			return nil, err
		}
		rows[i].Data, err = wardendb.LosslessDocument(rows[i].Data)
		if err != nil {
			return nil, err
		}
		rows[i].Data["brain_id"] = manager
	}
	return rows, nil
}

func (c *Controller) ownedRunViews(ctx context.Context) ([]*autopilotstore.Autopilot, error) {
	records, err := c.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]*autopilotstore.Autopilot, 0)
	for _, r := range records {
		if !schema3RunLive(string(r.State)) {
			continue
		}
		out = append(out, &autopilotstore.Autopilot{ID: r.RunID, ProjectID: r.ProjectID, PlanID: r.PlanID, Name: autopilotstore.DisplayName(r.Name), ParentAgentID: r.ParentAgentID, ManagerAgentID: r.BrainID,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Diagnostics: autopilotstore.Diagnostics{State: string(r.State), Repo: r.Repo, PlanFile: r.PlanFile, Gate: r.Gate, Strategy: r.Strategy, SlotScope: r.SlotScope, IntegrationBranch: r.IntegrationBranch, LastError: r.LastError}})
	}
	return out, nil
}

func (c *Controller) startOwnedFromPlanLocked(ctx context.Context, req PlanStartRequest) (PlanStartResult, error) {
	if c.storeErr != nil {
		return PlanStartResult{}, c.storeErr
	}
	if req.ProjectID == "" || req.Repo == "" {
		return PlanStartResult{}, fmt.Errorf("autopilot: direct project and repository required")
	}
	if !filepath.IsAbs(req.Repo) {
		return PlanStartResult{}, fmt.Errorf("autopilot: absolute repository required")
	}
	runID := PlanBoundRunID(req.Repo, req.PlanID)
	if existing := c.runs[runID]; existing != nil && schema3RunLive(string(existing.state)) {
		if existing.planID != req.PlanID || existing.projectID != req.ProjectID || existing.parentAgentID != req.ParentAgentID {
			return PlanStartResult{}, fmt.Errorf("autopilot: immutable run ownership")
		}
		return PlanStartResult{AutopilotID: runID, ManagerAgentID: managerIDOf(existing), Status: c.runStatusLocked(existing)}, nil
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = req.PlanID
	}
	if err := validatePlanNameReservedSuffixes(name); err != nil {
		return PlanStartResult{}, err
	}
	scope, err := c.allocateSlotScopeLocked(name, runID)
	if err != nil {
		return PlanStartResult{}, err
	}
	branch, err := resolveIntegrationBranch(branchResolveOpts{planName: name, runID: runID, template: c.integrationBranch, taken: c.branchTakenLocked(req.Repo, runID, nil)})
	if err != nil {
		return PlanStartResult{}, err
	}
	r := &run{runID: runID, name: name, planID: req.PlanID, projectID: req.ProjectID, parentAgentID: req.ParentAgentID,
		repo: filepath.Clean(req.Repo), planFile: req.PlanFile, absPlanFile: req.PlanFile, state: StateStarting,
		plan: c.resolvePlanDefinition(ctx, req), resolvedGate: c.gate, slotScope: scope, integrationBranch: branch, tried: map[string]bool{}}
	if err := c.claims.claim(runID, scope); err != nil {
		return PlanStartResult{}, err
	}
	if err := c.persistRunLockedErr(r); err != nil {
		c.claims.release(runID)
		return PlanStartResult{}, err
	}
	c.runs[runID] = r
	sel := c.selectBrain(nil)
	r.tier = sel.Tier
	if err := c.spawnManager(ctx, r, sel.Backend); err != nil {
		r.backoffLastErr = err.Error()
		c.persistRunLocked(r)
		return PlanStartResult{AutopilotID: runID, Status: c.runStatusLocked(r)}, err
	}
	if err := c.persistRunLockedErr(r); err != nil {
		return PlanStartResult{}, err
	}
	return PlanStartResult{AutopilotID: runID, ManagerAgentID: managerIDOf(r), Status: c.runStatusLocked(r)}, nil
}
