package pipeline

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

type jobPayload Job

// JobRecord keeps every production Job business/runtime field. Assignment is
// derived from immutable agent attempt references and is never stored here.
type JobRecord struct {
	jobPayload
	ID         string `json:"id"`
	JobID      string `json:"job_id"`
	PipelineID string `json:"pipeline_id"`
	ProjectID  string `json:"project_id"`
	Seq        int    `json:"seq"`
	Synthetic  bool   `json:"synthetic"`
}

type pipelineHeader Pipeline

type schema3Pipelines struct {
	mu                  sync.Mutex
	db                  *wardendb.DB
	header, jobs        docs
	boundary            *dbtx.Boundary
	pipelinesTx, jobsTx *dbtx.Coll[map[string]any]
}

func jobKey(pipelineID, jobID string) string {
	b, _ := json.Marshal([2]string{pipelineID, jobID})
	return string(b)
}

func registerSchema3[T any](db *wardendb.DB, name string, fields ...string) (docs, error) {
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

func NewSchema3StoreFromDB(db *wardendb.DB) (*Store, error) {
	header, err := registerSchema3[pipelineHeader](db, "pipelines", "project_id", "parent_agent_id", "plan_id", "status")
	if err != nil {
		return nil, err
	}
	jobs, err := registerSchema3[JobRecord](db, "pipeline_jobs", "pipeline_id", "project_id", "status", "seq")
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
	pipelinesTx, err := dbtx.Bind(b, "pipelines", dbtx.MapCodec{})
	if err != nil {
		return nil, err
	}
	jobsTx, err := dbtx.Bind(b, "pipeline_jobs", dbtx.MapCodec{})
	if err != nil {
		return nil, err
	}
	return &Store{col: &schema3Pipelines{db: db, header: header, jobs: jobs, boundary: b, pipelinesTx: pipelinesTx, jobsTx: jobsTx}}, nil
}

func (d *schema3Pipelines) Config() engine.CollectionConfig { return d.header.Config() }

func (d *schema3Pipelines) jobRows(pipelineID string) ([]engine.ScanResult, []JobRecord, error) {
	value, _ := json.Marshal(pipelineID)
	physical, err := d.jobs.Scan(&query.FieldFilter{Field: "pipeline_id", Op: query.OpEq, Value: string(value)})
	if err != nil {
		return nil, nil, err
	}
	rows := make([]JobRecord, 0, len(physical))
	for _, r := range physical {
		var row JobRecord
		if err := wardendb.StrictRecord(r.Data, &row, "agent_id", "session_id"); err != nil {
			return nil, nil, err
		}
		if row.ID != jobKey(pipelineID, row.JobID) || row.PipelineID != pipelineID || row.JobID == "" {
			return nil, nil, fmt.Errorf("pipeline: invalid job identity")
		}
		if row.Synthetic != (row.Type == "span-in" || row.Type == "span-out") {
			return nil, nil, fmt.Errorf("pipeline: synthetic provenance mismatch")
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	for i, row := range rows {
		if row.Seq != i {
			return nil, nil, fmt.Errorf("pipeline: noncontiguous/duplicate job sequence")
		}
	}
	return physical, rows, nil
}

func (d *schema3Pipelines) hydrate(record engine.Record) (engine.Record, error) {
	var header pipelineHeader
	if err := wardendb.StrictRecord(record.Data, &header, "jobs", "job_count", "jobs_done"); err != nil {
		return engine.Record{}, err
	}
	p := Pipeline(header)
	_, rows, err := d.jobRows(p.ID)
	if err != nil {
		return engine.Record{}, err
	}
	p.Jobs = make([]Job, 0, len(rows))
	for _, row := range rows {
		if row.ProjectID != p.ProjectID {
			return engine.Record{}, fmt.Errorf("pipeline: cross-project job")
		}
		j := Job(row.jobPayload)
		j.ID = row.JobID
		id, err := d.agentAttempt(p.ID, j.ID, p.ProjectID, j.Status)
		if err != nil {
			return engine.Record{}, err
		}
		j.SetAgentID(id)
		p.Jobs = append(p.Jobs, j)
	}
	if err := validateJobDAG(&p); err != nil {
		return engine.Record{}, err
	}
	record.Data, err = toRecord(&p)
	return record, err
}

func (d *schema3Pipelines) GetByKey(key string) (engine.Record, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.header.GetByKey(key)
	if err != nil {
		return engine.Record{}, err
	}
	return d.hydrate(r)
}

func (d *schema3Pipelines) Scan(f query.Filter) ([]engine.ScanResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.header.Scan(f)
	if err != nil {
		return nil, err
	}
	for i, row := range rows {
		r, err := d.hydrate(engine.Record{ID: row.ID, Rev: row.Rev, Data: row.Data})
		if err != nil {
			return nil, err
		}
		rows[i].Data = r.Data
	}
	return rows, nil
}

func (d *schema3Pipelines) agentAttempt(pipelineID, jobID, projectID string, status JobStatus) (string, error) {
	if status == JobPending {
		return "", nil
	}
	handle, err := d.db.Handle()
	if err != nil {
		return "", err
	}
	agents, err := handle.Engine().Collection("agents")
	if err != nil {
		return "", err
	}
	value, _ := json.Marshal(pipelineID)
	rows, err := agents.Scan(&query.FieldFilter{Field: "pipeline_id", Op: query.OpEq, Value: string(value)})
	if err != nil {
		return "", err
	}
	var live, last string
	var lastCreated string
	for _, row := range rows {
		if row.Data["job_id"] != jobID {
			continue
		}
		if row.Data["project_id"] != projectID {
			return "", fmt.Errorf("pipeline: cross-project agent attempt")
		}
		id, _ := row.Data["id"].(string)
		created, _ := row.Data["created_at"].(string)
		if last == "" || created > lastCreated || created == lastCreated && id > last {
			last, lastCreated = id, created
		}
		terminal := row.Data["lifecycle"] == "archived" || row.Data["archived_at"] != nil || row.Data["closed_at"] != nil
		switch row.Data["status"] {
		case "done", "closed", "terminated", "failed", "exited":
			terminal = true
		}
		if !terminal {
			if live != "" {
				return "", fmt.Errorf("pipeline: multiple live agents for one job")
			}
			live = id
		}
	}
	if live != "" {
		return live, nil
	}
	return last, nil
}

func validateJobDAG(p *Pipeline) error {
	if p.ID == "" || p.ProjectID == "" {
		return fmt.Errorf("pipeline: immutable direct pipeline/project identity required")
	}
	jobs := make(map[string]Job, len(p.Jobs))
	for _, job := range p.Jobs {
		if job.ID == "" {
			return fmt.Errorf("pipeline: empty job identity")
		}
		if _, found := jobs[job.ID]; found {
			return fmt.Errorf("pipeline: duplicate job identity")
		}
		jobs[job.ID] = job
	}
	state := make(map[string]int)
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("pipeline: cyclic job DAG")
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dep := range jobs[id].DependsOn {
			if _, found := jobs[dep]; !found {
				return fmt.Errorf("pipeline: missing dependency %s", dep)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range jobs {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func (d *schema3Pipelines) InsertWithKey(key string, body map[string]any) (uint64, time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.write(key, body, true)
}
func (d *schema3Pipelines) UpdateByKey(key string, body map[string]any) (time.Time, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, at, err := d.write(key, body, false)
	return at, err
}
func (d *schema3Pipelines) write(key string, body map[string]any, insert bool) (uint64, time.Time, error) {
	// Decode the complete API object, including computed counters. Persistence
	// below uses the owning alias to exclude those response-only fields.
	p, err := fromRecord(body)
	if err != nil {
		return 0, time.Time{}, err
	}
	if p.ID != key {
		return 0, time.Time{}, fmt.Errorf("pipeline: immutable pipeline identity")
	}
	if err := validateJobDAG(p); err != nil {
		return 0, time.Time{}, err
	}
	var numericID uint64
	_, err = d.boundary.Run(context.Background(), "pipeline/schema3-write", func(ctx context.Context, tx *dbtx.Tx) error {
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
			var old pipelineHeader
			if err := wardendb.StrictRecord(current.Data, &old, "jobs", "job_count", "jobs_done"); err != nil {
				return err
			}
			if old.ProjectID != p.ProjectID || old.PlanID != p.PlanID || old.ParentAgentID != p.ParentAgentID {
				return fmt.Errorf("pipeline: immutable structural ownership")
			}
		}
		handle, err := d.db.Handle()
		if err != nil {
			return err
		}
		for _, owner := range []struct {
			collection, id string
			required       bool
		}{{"projects", p.ProjectID, true}, {"plans", p.PlanID, false}, {"agents", p.ParentAgentID, false}} {
			if owner.id == "" && !owner.required {
				continue
			}
			col, err := handle.Engine().Collection(owner.collection)
			if err != nil {
				return err
			}
			parent, err := col.GetByKey(owner.id)
			if err != nil {
				return err
			}
			if owner.collection == "projects" {
				if insert && parent.Data["status"] == "closed" {
					return wardendb.ErrProjectClosed
				}
			} else if parent.Data["project_id"] != p.ProjectID {
				return fmt.Errorf("pipeline: cross-project parent")
			}
			parentTx, err := dbtx.Bind(d.boundary, owner.collection, dbtx.MapCodec{})
			if err != nil {
				return err
			}
			if err := parentTx.Update(tx, parent.ID, parent.Data, parent.Rev); err != nil {
				return err
			}
		}
		raw, prior, err := d.jobRows(key)
		if err != nil {
			return err
		}
		if !insert && len(prior) != len(p.Jobs) {
			return fmt.Errorf("pipeline: immutable job identities/order")
		}
		physical := make(map[string]engine.ScanResult, len(raw))
		for _, row := range raw {
			id, _ := row.Data["job_id"].(string)
			physical[id] = row
		}
		for seq, job := range p.Jobs {
			if !insert && prior[seq].JobID != job.ID {
				return fmt.Errorf("pipeline: immutable job identities/order")
			}
			if assigned := job.AgentRef(); assigned != "" {
				agents, err := handle.Engine().Collection("agents")
				if err != nil {
					return err
				}
				agent, err := agents.GetByKey(assigned)
				if err != nil {
					return err
				}
				if agent.Data["pipeline_id"] != p.ID || agent.Data["job_id"] != job.ID || agent.Data["project_id"] != p.ProjectID {
					return fmt.Errorf("pipeline: assignment disagrees with immutable attempt refs")
				}
			}
			payload := jobPayload(job)
			payload.AgentID, payload.SessionID = "", ""
			row := JobRecord{jobPayload: payload, ID: jobKey(p.ID, job.ID), JobID: job.ID, PipelineID: p.ID, ProjectID: p.ProjectID, Seq: seq, Synthetic: job.IsSynthetic()}
			m, err := wardendb.LosslessDocument(row)
			if err != nil {
				return err
			}
			m[engine.KeyField] = row.ID
			if existing, found := physical[job.ID]; found {
				if err := d.jobsTx.Update(tx, existing.ID, m, existing.Rev); err != nil {
					return err
				}
			} else if _, err := d.jobsTx.Insert(tx, m); err != nil {
				return err
			}
		}
		header, err := wardendb.LosslessDocument(pipelineHeader(*p))
		if err != nil {
			return err
		}
		delete(header, "jobs")
		header[engine.KeyField] = p.ID
		if insert {
			numericID, err = d.pipelinesTx.Insert(tx, header)
			return err
		}
		numericID = current.ID
		return d.pipelinesTx.Update(tx, current.ID, header, current.Rev)
	})
	return numericID, time.Now().UTC(), err
}

func (d *schema3Pipelines) DeleteByKey(key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.header.GetByKey(key); err != nil {
		return err
	}
	return wardendb.ErrReferenced // archive/cancel; never erase attempt ownership
}
