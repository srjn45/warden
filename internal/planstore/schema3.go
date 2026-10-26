package planstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/dbtx"
	"github.com/srjn45/warden/internal/wardendb"
)

const tasksCollection = "plan_tasks"

// TaskRecord is the full current task authority. Execution snapshots and past
// dispatch stay on their historical executions; they are never live task rows.
type TaskRecord struct {
	ID        string        `json:"id"`
	TaskID    string        `json:"task_id"`
	PlanID    string        `json:"plan_id"`
	ProjectID string        `json:"project_id"`
	Prompt    string        `json:"prompt"`
	After     []string      `json:"after,omitempty"`
	Seq       int           `json:"seq"`
	Status    string        `json:"status"`
	Outcome   *TaskOutcome  `json:"outcome,omitempty"`
	Dispatch  *TaskDispatch `json:"dispatch,omitempty"`
}

// planHeader declares the complete owning Plan payload, not a reduced
// relationship record. Its persistence codec below removes response projections.
type planHeader Plan

type schema3Plans struct {
	mu               sync.Mutex
	db               *wardendb.DB
	header, tasks    docs
	boundary         *dbtx.Boundary
	plansTx, tasksTx *dbtx.Coll[map[string]any]
}

func taskKey(planID, taskID string) string {
	b, _ := json.Marshal([2]string{planID, taskID})
	return string(b)
}

func schema3Collection[T any](db *wardendb.DB, name string, fields ...string) (docs, error) {
	// dbtx writes the complete keyed payload atomically. Both indexes must
	// exist before its first insert; keyed collection APIs normally create the
	// reserved key index lazily, which cannot occur inside a transaction.
	c, err := wardendb.Register[T](db, wardendb.Spec{Name: name, Version: 3, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id", engine.KeyField)}})
	if err != nil {
		return nil, err
	}
	eng, err := c.Engine()
	if err != nil {
		return nil, err
	}
	for _, field := range fields {
		if err := eng.EnsureIndex(field); err != nil {
			return nil, err
		}
	}
	return sharedDocs{get: c.Engine}, nil
}

// NewSchema3StoreFromDB registers full schema-3 Plan codecs and materialized
// tasks. The composition root selects this constructor only at the format-3
// boundary; it performs no imports, backfills or format adoption.
func NewSchema3StoreFromDB(db *wardendb.DB) (*Store, error) {
	header, err := schema3Collection[planHeader](db, plansCollection, "project_id", "status", "execution_mode")
	if err != nil {
		return nil, err
	}
	tasks, err := schema3Collection[TaskRecord](db, tasksCollection, "plan_id", "project_id", "status", "seq")
	if err != nil {
		return nil, err
	}
	events, err := schema3Collection[PlanExecutionEvent](db, eventsCollection, "plan_id", "project_id")
	if err != nil {
		return nil, err
	}
	notes, err := schema3Collection[ExecutionNote](db, notesCollection, "plan_id", "project_id")
	if err != nil {
		return nil, err
	}
	handle, err := db.Handle()
	if err != nil {
		return nil, err
	}
	b, err := dbtx.New(handle)
	if err != nil {
		return nil, err
	}
	plansTx, err := dbtx.Bind(b, plansCollection, dbtx.MapCodec{})
	if err != nil {
		return nil, err
	}
	tasksTx, err := dbtx.Bind(b, tasksCollection, dbtx.MapCodec{})
	if err != nil {
		return nil, err
	}
	d := &schema3Plans{db: db, header: header, tasks: tasks, boundary: b, plansTx: plansTx, tasksTx: tasksTx}
	events = &schema3Evidence{plans: d, native: events, collection: eventsCollection, validate: func(m map[string]any) error {
		var value PlanExecutionEvent
		return wardendb.StrictRecord(m, &value)
	}}
	notes = &schema3Evidence{plans: d, native: notes, collection: notesCollection, validate: func(m map[string]any) error {
		var value ExecutionNote
		return wardendb.StrictRecord(m, &value)
	}}
	seq, err := initSeqCounter(events)
	if err != nil {
		return nil, err
	}
	return &Store{col: d, events: events, notes: notes, seqCounter: seq}, nil
}

func (d *schema3Plans) Config() engine.CollectionConfig { return d.header.Config() }

func (d *schema3Plans) taskRows(planID string) ([]engine.ScanResult, []TaskRecord, error) {
	value, _ := json.Marshal(planID)
	raw, err := d.tasks.Scan(&query.FieldFilter{Field: "plan_id", Op: query.OpEq, Value: string(value)})
	if err != nil {
		return nil, nil, err
	}
	rows := make([]TaskRecord, 0, len(raw))
	for _, r := range raw {
		var task TaskRecord
		if err := wardendb.StrictRecord(r.Data, &task); err != nil {
			return nil, nil, err
		}
		if task.ID != taskKey(planID, task.TaskID) || task.PlanID != planID || task.TaskID == "" {
			return nil, nil, fmt.Errorf("planstore: invalid materialized task identity %q", task.ID)
		}
		rows = append(rows, task)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	for i, task := range rows {
		if task.Seq != i {
			return nil, nil, fmt.Errorf("planstore: noncontiguous/duplicate task sequence for %s", planID)
		}
	}
	return raw, rows, nil
}

func (d *schema3Plans) hydrate(record engine.Record) (engine.Record, error) {
	var p Plan
	if err := wardendb.StrictRecord(record.Data, &p, "tasks", "task_progress", "task_outcomes", "pipeline_id", "autopilot_run_id", "orchestrator_id"); err != nil {
		return engine.Record{}, err
	}
	if active, ok := record.Data["active_execution"].(map[string]any); ok {
		if _, present := active["task_progress"]; present {
			return engine.Record{}, fmt.Errorf("planstore: forbidden active_execution.task_progress")
		}
	}
	_, rows, err := d.taskRows(p.ID)
	if err != nil {
		return engine.Record{}, err
	}
	p.Tasks = make([]PlanTask, 0, len(rows))
	p.TaskProgress = make(map[string]string, len(rows))
	p.TaskOutcomes = make(map[string]TaskOutcome)
	for _, row := range rows {
		if row.ProjectID != p.ProjectID {
			return engine.Record{}, fmt.Errorf("planstore: cross-project task %s", row.ID)
		}
		p.Tasks = append(p.Tasks, PlanTask{ID: row.TaskID, Prompt: row.Prompt, After: row.After})
		p.TaskProgress[row.TaskID] = row.Status
		if row.Outcome != nil {
			p.TaskOutcomes[row.TaskID] = *row.Outcome
		}
		if row.Dispatch != nil && (p.ActiveExecution == nil || row.Dispatch.ExecutionID != p.ActiveExecution.ID) {
			return engine.Record{}, fmt.Errorf("planstore: task dispatch belongs to another execution")
		}
	}
	if err := validateMaterializedPlan(&p); err != nil {
		return engine.Record{}, err
	}
	if active := p.ActiveExecution; active != nil {
		active.TaskProgress = make(map[string]string, len(p.TaskProgress))
		for id, state := range p.TaskProgress {
			active.TaskProgress[id] = state
		}
		switch active.ExecutionMode {
		case PlanModePipeline:
			p.PipelineID = active.ExecutorID
		case PlanModeAutopilot:
			p.AutopilotRunID = active.ExecutorID
		case PlanModeOrchestratorWorker:
			p.OrchestratorID = active.ExecutorID
		}
	}
	record.Data, err = encodeRecord(&p)
	return record, err
}

func (d *schema3Plans) GetByKey(key string) (engine.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.header.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	return d.hydrate(r)
}

func (d *schema3Plans) Scan(filter query.Filter) ([]engine.ScanResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	raw, err := d.header.Scan(filter)
	if err != nil {
		return nil, err
	}
	for i, row := range raw {
		hydrated, err := d.hydrate(engine.Record{ID: row.ID, Rev: row.Rev, Data: row.Data})
		if err != nil {
			return nil, err
		}
		raw[i].Data = hydrated.Data
	}
	return raw, nil
}

func validateMaterializedPlan(p *Plan) error {
	if p.ID == "" || p.ProjectID == "" {
		return fmt.Errorf("planstore: direct Plan/project identity required")
	}
	if p.ContentHash != ComputeContentHash(p) {
		return fmt.Errorf("planstore: materialized definition content hash mismatch")
	}
	tasks := make(map[string]PlanTask, len(p.Tasks))
	for _, task := range p.Tasks {
		if task.ID == "" {
			return fmt.Errorf("planstore: empty task identity")
		}
		if _, found := tasks[task.ID]; found {
			return fmt.Errorf("planstore: duplicate task identity %s", task.ID)
		}
		tasks[task.ID] = task
	}
	state := make(map[string]int)
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("planstore: cyclic task dependencies")
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dep := range tasks[id].After {
			if _, found := tasks[dep]; !found {
				return fmt.Errorf("planstore: missing dependency %s", dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range tasks {
		if err := visit(id); err != nil {
			return err
		}
	}
	for id, status := range p.TaskProgress {
		if _, found := tasks[id]; !found {
			return fmt.Errorf("planstore: progress without task %s", id)
		}
		switch status {
		case "pending", "in_progress", "done", "skipped":
		default:
			return fmt.Errorf("planstore: invalid task progress %q", status)
		}
	}
	for id := range p.TaskOutcomes {
		if _, found := tasks[id]; !found {
			return fmt.Errorf("planstore: outcome without task %s", id)
		}
	}
	return nil
}

func (d *schema3Plans) InsertWithKey(key string, body map[string]any) (uint64, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.write(key, body, true)
}

func (d *schema3Plans) UpdateByKey(key string, body map[string]any) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, at, err := d.write(key, body, false)
	return at, err
}

func (d *schema3Plans) write(key string, body map[string]any, insert bool) (uint64, time.Time, error) {
	var p Plan
	if err := wardendb.StrictRecord(body, &p); err != nil {
		return 0, time.Time{}, err
	}
	if key != p.ID {
		return 0, time.Time{}, fmt.Errorf("planstore: immutable Plan identity")
	}
	if err := validateMaterializedPlan(&p); err != nil {
		return 0, time.Time{}, err
	}
	var numericID uint64
	_, err := d.boundary.Run(context.Background(), "planstore/schema3-write", func(ctx context.Context, tx *dbtx.Tx) error {
		current, err := d.header.GetByKey(key)
		if insert && err == nil {
			return engine.ErrDuplicateKey
		}
		if !insert && err != nil {
			return err
		}
		if insert && !errors.Is(err, engine.ErrKeyNotFound) {
			return err
		}
		if !insert {
			var old Plan
			if err := wardendb.StrictRecord(current.Data, &old, "tasks", "task_progress", "task_outcomes", "pipeline_id", "autopilot_run_id", "orchestrator_id"); err != nil {
				return err
			}
			if old.ProjectID != p.ProjectID {
				return fmt.Errorf("planstore: immutable project ownership")
			}
			if old.ContentHash != p.ContentHash && old.Status != PlanStatusPending {
				return fmt.Errorf("planstore: definition edits require a pending Plan")
			}
		}
		// Fence the owning project so a concurrent close/delete cannot race a
		// new Plan into a closed or absent project. Evidence writes remain valid.
		handle, err := d.db.Handle()
		if err != nil {
			return err
		}
		projects, err := handle.Engine().Collection("projects")
		if err != nil {
			return err
		}
		project, err := projects.GetByKey(p.ProjectID)
		if err != nil {
			return fmt.Errorf("planstore: project: %w", err)
		}
		if insert && project.Data["status"] == "closed" {
			return wardendb.ErrProjectClosed
		}
		projectTx, err := dbtx.Bind(d.boundary, "projects", dbtx.MapCodec{})
		if err != nil {
			return err
		}
		if err := projectTx.Update(tx, project.ID, project.Data, project.Rev); err != nil {
			return err
		}
		rawTasks, oldTasks, err := d.taskRows(key)
		if err != nil {
			return err
		}
		old := make(map[string]TaskRecord, len(oldTasks))
		for _, row := range oldTasks {
			old[row.TaskID] = row
		}
		physical := make(map[string]engine.ScanResult, len(rawTasks))
		for _, row := range rawTasks {
			id, _ := row.Data["task_id"].(string)
			physical[id] = row
		}
		keep := make(map[string]bool, len(p.Tasks))
		for seq, task := range p.Tasks {
			status := p.TaskProgress[task.ID]
			if status == "" {
				status = "pending"
			}
			row := TaskRecord{ID: taskKey(key, task.ID), TaskID: task.ID, PlanID: key, ProjectID: p.ProjectID, Prompt: task.Prompt, After: task.After, Seq: seq, Status: status}
			if outcome, found := p.TaskOutcomes[task.ID]; found {
				row.Outcome = &outcome
			}
			prior := old[task.ID]
			if prior.Dispatch != nil {
				if p.ActiveExecution != nil && prior.Dispatch.ExecutionID == p.ActiveExecution.ID {
					row.Dispatch = prior.Dispatch
				} else {
					preserved := false
					for i := range p.ExecutionHistory {
						past := &p.ExecutionHistory[i]
						if past.ID == prior.Dispatch.ExecutionID {
							if past.TaskDispatch == nil {
								past.TaskDispatch = make(map[string]TaskDispatch)
							}
							past.TaskDispatch[task.ID] = *prior.Dispatch
							preserved = true
						}
					}
					if !preserved {
						return fmt.Errorf("planstore: dispatch history would be lost")
					}
				}
			}
			m, err := wardendb.LosslessDocument(row)
			if err != nil {
				return err
			}
			m[engine.KeyField] = row.ID
			if existing, found := physical[task.ID]; found {
				if err := d.tasksTx.Update(tx, existing.ID, m, existing.Rev); err != nil {
					return err
				}
			} else if _, err := d.tasksTx.Insert(tx, m); err != nil {
				return err
			}
			keep[task.ID] = true
		}
		for id, existing := range physical {
			if !keep[id] {
				if old[id].Dispatch != nil {
					return fmt.Errorf("planstore: cannot delete task with current dispatch")
				}
				if err := d.tasksTx.Delete(tx, existing.ID, existing.Rev); err != nil {
					return err
				}
			}
		}
		header, err := encodeRecord(&p)
		if err != nil {
			return err
		}
		for _, field := range []string{"tasks", "task_progress", "task_outcomes", "pipeline_id", "autopilot_run_id", "orchestrator_id"} {
			delete(header, field)
		}
		if active, ok := header["active_execution"].(map[string]any); ok {
			delete(active, "task_progress")
		}
		header[engine.KeyField] = p.ID
		if insert {
			numericID, err = d.plansTx.Insert(tx, header)
			return err
		}
		numericID = current.ID
		return d.plansTx.Update(tx, current.ID, header, current.Rev)
	})
	return numericID, time.Now().UTC(), err
}

func (d *schema3Plans) DeleteByKey(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Deleting a referenced Plan requires explicit archival/finalization, not
	// implicit removal of its canonical definitions or execution evidence.
	_, tasks, err := d.taskRows(key)
	if err != nil {
		return err
	}
	if len(tasks) != 0 {
		return wardendb.ErrReferenced
	}
	return d.header.DeleteByKey(key)
}

type schema3Evidence struct {
	plans      *schema3Plans
	native     docs
	collection string
	validate   func(map[string]any) error
}

func (d *schema3Evidence) Config() engine.CollectionConfig { return d.native.Config() }
func (d *schema3Evidence) GetByKey(key string) (engine.Record, error) {
	r, err := d.native.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	if err := d.validate(r.Data); err != nil {
		return engine.Record{}, err
	}
	if owner, _ := r.Data["project_id"].(string); owner == "" {
		return engine.Record{}, fmt.Errorf("planstore: evidence missing direct project ownership")
	}
	return r, nil
}
func (d *schema3Evidence) Scan(f query.Filter) ([]engine.ScanResult, error) {
	rows, err := d.native.Scan(f)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if err := d.validate(row.Data); err != nil {
			return nil, err
		}
		if owner, _ := row.Data["project_id"].(string); owner == "" {
			return nil, fmt.Errorf("planstore: evidence missing direct project ownership")
		}
	}
	return rows, nil
}
func (d *schema3Evidence) InsertWithKey(key string, body map[string]any) (uint64, time.Time, error) {
	d.plans.mu.Lock()
	defer d.plans.mu.Unlock()
	m, err := wardendb.LosslessDocument(body)
	if err != nil {
		return 0, time.Time{}, err
	}
	planID, _ := m["plan_id"].(string)
	if planID == "" {
		return 0, time.Time{}, fmt.Errorf("planstore: evidence requires direct Plan ownership")
	}
	var numericID uint64
	_, err = d.plans.boundary.Run(context.Background(), "planstore/schema3-evidence", func(ctx context.Context, tx *dbtx.Tx) error {
		parent, err := d.plans.header.GetByKey(planID)
		if err != nil {
			return err
		}
		projectID, _ := parent.Data["project_id"].(string)
		if supplied, _ := m["project_id"].(string); supplied != "" && supplied != projectID {
			return fmt.Errorf("planstore: cross-project evidence")
		}
		m["project_id"] = projectID
		m[engine.KeyField] = key
		if err := d.validate(m); err != nil {
			return err
		}
		if err := d.plans.plansTx.Update(tx, parent.ID, parent.Data, parent.Rev); err != nil {
			return err
		}
		evidence, err := dbtx.Bind(d.plans.boundary, d.collection, dbtx.MapCodec{})
		if err != nil {
			return err
		}
		numericID, err = evidence.Insert(tx, m)
		return err
	})
	return numericID, time.Now().UTC(), err
}
func (*schema3Evidence) UpdateByKey(string, map[string]any) (time.Time, error) {
	return time.Time{}, fmt.Errorf("planstore: execution evidence is append-only")
}
func (*schema3Evidence) DeleteByKey(string) error {
	return fmt.Errorf("planstore: execution evidence is append-only")
}
