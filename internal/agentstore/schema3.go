package agentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/wardendb"
)

type agentPayload Agent
type agentRecord struct {
	agentPayload
	ParentAgentID   string `json:"parent_agent_id,omitempty"`
	Backend         string `json:"backend,omitempty"`
	ClaudeSessionID string `json:"claude_session_id"`
}

type schema3Agents struct {
	mu         *sync.Mutex
	db         *wardendb.DB
	native     docs
	archived   bool
	boundary   *dbtx.Boundary
	collection *dbtx.Coll[map[string]any]
}

func (s *Store) IsSchema3() bool {
	if s == nil {
		return false
	}
	_, ok := s.col.(*schema3Agents)
	return ok
}

func NewSchema3StoreFromDB(db *wardendb.DB) (*Store, error) {
	c, err := wardendb.Register[agentRecord](db, wardendb.Spec{Name: "agents", Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id", engine.KeyField)}})
	if err != nil {
		return nil, err
	}
	eng, err := c.Engine()
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"project_id", "parent_agent_id", "plan_id", "pipeline_id", "job_id", "autopilot_run_id", "autopilot_task_id", "autopilot_slot", "status", "lifecycle"} {
		if err := eng.EnsureIndex(field); err != nil {
			return nil, err
		}
	}
	handle, err := db.Handle()
	if err != nil {
		return nil, err
	}
	b, err := dbtx.New(handle)
	if err != nil {
		return nil, err
	}
	col, err := dbtx.Bind(b, "agents", dbtx.MapCodec{})
	if err != nil {
		return nil, err
	}
	native := sharedDocs{get: c.Engine}
	gate := &sync.Mutex{}
	active := &schema3Agents{mu: gate, db: db, native: native, boundary: b, collection: col}
	archived := &schema3Agents{mu: gate, db: db, native: native, archived: true, boundary: b, collection: col}
	s := &Store{col: active, closed: archived, gate: newWriteGate()}
	if err := s.loadInitialSnapshot(); err != nil {
		return nil, err
	}
	state := StateOK
	s.state.Store(&state)
	s.auditor = NewAuditor(s, AuditorOptions{})
	s.auditor.Start()
	return s, nil
}

func decodeAgentRecord(body map[string]any) (agentRecord, error) {
	var row agentRecord
	err := wardendb.StrictRecord(body, &row, "parent_id", "child_agents", "child_pipelines", "child_autopilots")
	if err != nil {
		return row, err
	}
	if row.Lifecycle != "active" && row.Lifecycle != "archived" {
		return row, fmt.Errorf("agentstore: explicit lifecycle required")
	}
	if row.ProjectID == "" && row.Scope != "global" && row.Scope != "system" {
		return row, fmt.Errorf("agentstore: explicit project ownership or global/system scope required")
	}
	return row, nil
}

func (d *schema3Agents) Config() engine.CollectionConfig { return d.native.Config() }
func (d *schema3Agents) view(r engine.Record) (engine.Record, error) {
	row, err := decodeAgentRecord(r.Data)
	if err != nil {
		return engine.Record{}, err
	}
	if (row.Lifecycle == "archived") != d.archived {
		return engine.Record{}, engine.ErrKeyNotFound
	}
	a := Agent(row.agentPayload)
	a.ParentID = row.ParentAgentID
	a.ChildAgents, a.ChildPipelines, a.ChildAutopilots = nil, nil, nil
	if a.AiCli == "" {
		a.AiCli = row.Backend
	}
	if a.AICLISessionID == "" {
		a.AICLISessionID = row.ClaudeSessionID
	}
	r.Data, err = wardendb.LosslessDocument(a)
	if err != nil {
		return engine.Record{}, err
	}
	r.Data[engine.KeyField] = a.ID
	return r, nil
}
func (d *schema3Agents) GetByKey(key string) (engine.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.native.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	return d.view(r)
}
func (d *schema3Agents) Scan(f query.Filter) ([]engine.ScanResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.native.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]engine.ScanResult, 0, len(rows))
	for _, row := range rows {
		r, err := d.view(engine.Record{ID: row.ID, Rev: row.Rev, Data: row.Data})
		if errors.Is(err, engine.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if f == nil || f.Match(r.Data) {
			row.Data = r.Data
			out = append(out, row)
		}
	}
	return out, nil
}
func (d *schema3Agents) Count(f query.Filter) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	total, err := d.native.Count(nil)
	if err != nil {
		return 0, err
	}
	rows, err := d.native.Scan(query.MatchAll)
	if err != nil {
		return 0, err
	}
	if uint64(len(rows)) != total {
		return 0, fmt.Errorf("agentstore: unified scan omitted indexed records")
	}
	var count uint64
	for _, row := range rows {
		r, err := d.view(engine.Record{ID: row.ID, Rev: row.Rev, Data: row.Data})
		if errors.Is(err, engine.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if f == nil || f.Match(r.Data) {
			count++
		}
	}
	return count, nil
}
func (d *schema3Agents) Verify(ctx context.Context, options engine.VerifyOptions) (*engine.IntegrityReport, error) {
	return d.native.Verify(ctx, options)
}
func (d *schema3Agents) Exists(key string) (bool, error) {
	_, err := d.GetByKey(key)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return false, nil
	}
	return err == nil, err
}

func agentTuple(owner, local string) string {
	b, _ := json.Marshal([2]string{owner, local})
	return string(b)
}
func agentLive(row agentRecord) bool {
	if row.Lifecycle == "archived" || row.ArchivedAt != nil || row.ClosedAt != nil {
		return false
	}
	switch string(row.Status) {
	case "done", "closed", "terminated", "failed", "exited":
		return false
	}
	return true
}

func (d *schema3Agents) write(key string, body map[string]any, mode string) (engine.Record, error) {
	var a Agent
	// Agent's custom API decoder accepts business aliases; persistence below
	// uses agentRecord's full strict codec without that permissive decoder.
	b, err := json.Marshal(body)
	if err != nil {
		return engine.Record{}, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return engine.Record{}, err
	}
	if a.ID != key {
		return engine.Record{}, ErrIdentityChanged
	}
	var id uint64
	_, err = d.boundary.Run(context.Background(), "agentstore/schema3-write", func(ctx context.Context, tx *dbtx.Tx) error {
		current, readErr := d.native.GetByKey(key)
		exists := readErr == nil
		if readErr != nil && !errors.Is(readErr, engine.ErrKeyNotFound) {
			return readErr
		}
		if mode == "insert" && exists {
			return engine.ErrDuplicateKey
		}
		if mode == "update" && !exists {
			return engine.ErrKeyNotFound
		}
		row := agentRecord{agentPayload: agentPayload(a), ParentAgentID: a.ParentID, Backend: a.AiCli, ClaudeSessionID: a.AICLISessionID}
		row.ParentID = ""
		row.ChildAgents, row.ChildPipelines, row.ChildAutopilots = nil, nil, nil
		row.Lifecycle = "active"
		if d.archived {
			row.Lifecycle = "archived"
			if row.ArchivedAt == nil {
				now := time.Now().UTC()
				row.ArchivedAt = &now
			}
		}
		if exists {
			old, err := decodeAgentRecord(current.Data)
			if err != nil {
				return err
			}
			for field, pair := range map[string][2]string{
				"project_id": {old.ProjectID, row.ProjectID}, "parent_agent_id": {old.ParentAgentID, row.ParentAgentID}, "scope": {old.Scope, row.Scope},
				"plan_id": {old.PlanID, row.PlanID}, "pipeline_id": {old.PipelineID, row.PipelineID}, "job_id": {old.JobID, row.JobID},
				"autopilot_run_id": {old.AutopilotRunID, row.AutopilotRunID}, "autopilot_task_id": {old.AutopilotTaskID, row.AutopilotTaskID}, "autopilot_slot": {old.AutopilotSlot, row.AutopilotSlot},
			} {
				if pair[0] != pair[1] {
					return fmt.Errorf("agentstore: immutable %s", field)
				}
			}
			if !d.archived && old.Lifecycle == "archived" {
				return fmt.Errorf("agentstore: archived identity cannot be reused for a replacement")
			}
			if old.AiCli == row.AiCli {
				row.Backend = old.Backend
			}
			if old.AICLISessionID == row.AICLISessionID {
				row.ClaudeSessionID = old.ClaudeSessionID
			}
		}
		if err := d.validateOwners(tx, row, !exists); err != nil {
			return err
		}
		m, err := wardendb.LosslessDocument(row)
		if err != nil {
			return err
		}
		m[engine.KeyField] = key
		if exists {
			id = current.ID
			return d.collection.Update(tx, current.ID, m, current.Rev)
		}
		id, err = d.collection.Insert(tx, m)
		return err
	})
	if err != nil {
		return engine.Record{}, err
	}
	r, err := d.native.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	if r.ID != id {
		return engine.Record{}, fmt.Errorf("agentstore: committed identity mismatch")
	}
	return d.view(r)
}

func (d *schema3Agents) validateOwners(tx *dbtx.Tx, row agentRecord, creating bool) error {
	if row.Scope != "" && row.Scope != "global" && row.Scope != "system" {
		return fmt.Errorf("agentstore: invalid scope")
	}
	if row.ProjectID == "" {
		if row.Scope != "global" && row.Scope != "system" {
			return fmt.Errorf("agentstore: direct project ownership required")
		}
		if row.ParentAgentID != "" || row.PlanID != "" || row.PipelineID != "" || row.AutopilotRunID != "" || row.JobID != "" || row.AutopilotTaskID != "" {
			return fmt.Errorf("agentstore: global/system agent cannot own project-scoped references")
		}
		return nil
	}
	handle, err := d.db.Handle()
	if err != nil {
		return err
	}
	for _, ref := range []struct{ collection, key string }{{"projects", row.ProjectID}, {"agents", row.ParentAgentID}, {"plans", row.PlanID}, {"pipelines", row.PipelineID}, {"autopilot_runs", row.AutopilotRunID}} {
		if ref.key == "" {
			continue
		}
		if ref.collection == "agents" && ref.key == row.ID {
			return wardendb.ErrAgentCycle
		}
		col, err := handle.Engine().Collection(ref.collection)
		if err != nil {
			return err
		}
		parent, err := col.GetByKey(ref.key)
		if err != nil {
			return err
		}
		if ref.collection == "projects" {
			if creating && parent.Data["status"] == "closed" {
				return wardendb.ErrProjectClosed
			}
		} else if parent.Data["project_id"] != row.ProjectID {
			return fmt.Errorf("agentstore: cross-project structural reference")
		}
		bound, err := dbtx.Bind(d.boundary, ref.collection, dbtx.MapCodec{})
		if err != nil {
			return err
		}
		if err := bound.Update(tx, parent.ID, parent.Data, parent.Rev); err != nil {
			return err
		}
	}
	if row.JobID != "" {
		if row.PipelineID == "" {
			return fmt.Errorf("agentstore: job requires pipeline ownership")
		}
		jobs, err := handle.Engine().Collection("pipeline_jobs")
		if err != nil {
			return err
		}
		job, err := jobs.GetByKey(agentTuple(row.PipelineID, row.JobID))
		if err != nil {
			return err
		}
		if job.Data["project_id"] != row.ProjectID {
			return fmt.Errorf("agentstore: cross-project job")
		}
	}
	if row.AutopilotTaskID != "" {
		if row.AutopilotRunID == "" || row.PlanID == "" {
			return fmt.Errorf("agentstore: dispatched task requires run and Plan ownership")
		}
		tasks, err := handle.Engine().Collection("plan_tasks")
		if err != nil {
			return err
		}
		task, err := tasks.GetByKey(agentTuple(row.PlanID, row.AutopilotTaskID))
		if err != nil {
			return err
		}
		if task.Data["project_id"] != row.ProjectID {
			return fmt.Errorf("agentstore: cross-project task")
		}
	}
	if agentLive(row) {
		others, err := d.native.Scan(query.MatchAll)
		if err != nil {
			return err
		}
		for _, other := range others {
			old, err := decodeAgentRecord(other.Data)
			if err != nil {
				return err
			}
			if old.ID == row.ID || !agentLive(old) {
				continue
			}
			if row.JobID != "" && old.JobID == row.JobID && old.PipelineID == row.PipelineID {
				return fmt.Errorf("agentstore: one live attempt per job")
			}
			if row.AutopilotRunID != "" && (row.AutopilotSlot == "autopilot" || row.AutopilotSlot == "brain") && old.AutopilotRunID == row.AutopilotRunID && old.AutopilotSlot == row.AutopilotSlot {
				return fmt.Errorf("agentstore: one live manager/brain per run slot")
			}
		}
	}
	return nil
}

func (d *schema3Agents) InsertWithKey(key string, body map[string]any) (uint64, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.write(key, body, "insert")
	return r.ID, time.Now().UTC(), err
}
func (d *schema3Agents) UpdateByKey(key string, body map[string]any) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.write(key, body, "update")
	return time.Now().UTC(), err
}
func (d *schema3Agents) Upsert(key string, body map[string]any) (engine.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.write(key, body, "upsert")
}
func (d *schema3Agents) DeleteByKey(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.native.GetByKey(key)
	if err != nil {
		return err
	}
	row, err := decodeAgentRecord(r.Data)
	if err != nil {
		return err
	}
	if !d.archived && row.Lifecycle == "archived" {
		return nil
	} // Archive's old active-view removal
	return wardendb.ErrReferenced
}
