package agentstore

// Shared-handle construction (warden-db porting pattern; see
// internal/projectstore/shared.go for the reference port).
//
// NewStoreFromDB registers the active and archived agent collections on the
// shared *wardendb.DB: no path, no scriva.Open, no ownership lock (the handle
// owner already holds the single-writer guarantee), and Store.Close leaves the
// handle open — the handle owner closes it. The legacy sessions-db import, the
// offline verify/repair path and the ownership lock stay on the legacy
// New(dir) path. Records stay schema-1 shaped (registry version 1): the
// schema-2 in-place archive (status+archived_at in one collection) is the
// migration/cutover task, not this port, so the legacy "closed" collection maps
// 1:1 onto the "archived_agents" catalogue entry.

import (
	"context"
	"fmt"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	agentsCollection         = "agents"
	archivedAgentsCollection = "archived_agents"
	recordSchemaVersion      = 1
)

// docs is the document surface Store needs from a collection; both the legacy
// *engine.Collection and the shared-handle resolver satisfy it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	Count(query.Filter) (uint64, error)
	Verify(context.Context, engine.VerifyOptions) (*engine.IntegrityReport, error)
	GetByKey(string) (engine.Record, error)
	Exists(string) (bool, error)
	InsertWithKey(string, map[string]any) (uint64, time.Time, error)
	UpdateByKey(string, map[string]any) (time.Time, error)
	Upsert(string, map[string]any) (engine.Record, error)
	DeleteByKey(string) error
}

// sharedDocs resolves the engine collection on every call (Engine() fails with
// ErrClosed after handle retirement).
type sharedDocs struct {
	get func() (*engine.Collection, error)
}

func (d sharedDocs) Config() engine.CollectionConfig {
	c, err := d.get()
	if err != nil {
		return engine.CollectionConfig{}
	}
	return c.Config()
}

func (d sharedDocs) Scan(f query.Filter) ([]engine.ScanResult, error) {
	c, err := d.get()
	if err != nil {
		return nil, err
	}
	return c.Scan(f)
}

func (d sharedDocs) Count(f query.Filter) (uint64, error) {
	c, err := d.get()
	if err != nil {
		return 0, err
	}
	return c.Count(f)
}

func (d sharedDocs) Verify(ctx context.Context, o engine.VerifyOptions) (*engine.IntegrityReport, error) {
	c, err := d.get()
	if err != nil {
		return nil, err
	}
	return c.Verify(ctx, o)
}

func (d sharedDocs) GetByKey(k string) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.GetByKey(k)
}

func (d sharedDocs) Exists(k string) (bool, error) {
	c, err := d.get()
	if err != nil {
		return false, err
	}
	return c.Exists(k)
}

func (d sharedDocs) InsertWithKey(k string, m map[string]any) (uint64, time.Time, error) {
	c, err := d.get()
	if err != nil {
		return 0, time.Time{}, err
	}
	return c.InsertWithKey(k, m)
}

func (d sharedDocs) UpdateByKey(k string, m map[string]any) (time.Time, error) {
	c, err := d.get()
	if err != nil {
		return time.Time{}, err
	}
	return c.UpdateByKey(k, m)
}

func (d sharedDocs) Upsert(k string, m map[string]any) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.Upsert(k, m)
}

func (d sharedDocs) DeleteByKey(k string) error {
	c, err := d.get()
	if err != nil {
		return err
	}
	return c.DeleteByKey(k)
}

// CollectionSpecs declares the collections this store owns on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	opts := func() []scriva.CollectionOption {
		return []scriva.CollectionOption{scriva.WithUniqueIndex("id")}
	}
	return []wardendb.Spec{
		{Name: agentsCollection, Version: recordSchemaVersion, Options: opts()},
		{Name: archivedAgentsCollection, Version: recordSchemaVersion, Options: opts()},
	}
}

// CollectionIndexes lists the lookup indexes ensured per collection.
func CollectionIndexes() map[string][]string {
	return map[string][]string{
		agentsCollection:         {"project_id", "status", "parent_id", "plan_id", "pipeline_id"},
		archivedAgentsCollection: {"project_id"},
	}
}

// NewStoreFromDB builds a Store over the agent collections registered on the
// shared warden-db handle. The caller keeps ownership of db and closes it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	col, err := registerDocs[Agent](db, agentsCollection)
	if err != nil {
		return nil, err
	}
	closed, err := registerDocs[Agent](db, archivedAgentsCollection)
	if err != nil {
		return nil, err
	}
	s := &Store{col: col, closed: closed, gate: newWriteGate()}
	if err := s.loadInitialSnapshot(); err != nil {
		return nil, err
	}
	st := StateOK
	s.state.Store(&st)
	s.auditor = NewAuditor(s, AuditorOptions{})
	s.auditor.Start()
	return s, nil
}

func registerDocs[T any](db *wardendb.DB, name string) (docs, error) {
	var spec wardendb.Spec
	for _, sp := range CollectionSpecs() {
		if sp.Name == name {
			spec = sp
		}
	}
	col, err := wardendb.Register[T](db, spec)
	if err != nil {
		return nil, err
	}
	eng, err := col.Engine()
	if err != nil {
		return nil, err
	}
	for _, f := range CollectionIndexes()[name] {
		if err := eng.EnsureIndex(f); err != nil {
			return nil, fmt.Errorf("agentstore: ensure %s.%s index: %w", name, f, err)
		}
	}
	return sharedDocs{get: col.Engine}, nil
}
