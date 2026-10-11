package autopilot

// Shared-handle construction (warden-db porting pattern; see
// internal/projectstore/shared.go for the reference port). Records stay
// schema-1 shaped; production wiring is untouched.

import (
	"fmt"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	runsCollection         = "autopilot_runs"
	runRecordSchemaVersion = 1
)

// runDocs is the document surface Store needs from a collection; both the legacy
// *engine.Collection and the shared-handle resolver satisfy it.
type runDocs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	GetByKey(string) (engine.Record, error)
	InsertWithKey(string, map[string]any) (uint64, time.Time, error)
	UpdateByKey(string, map[string]any) (time.Time, error)
	DeleteByKey(string) error
}

// sharedRunDocs resolves the engine collection on every call.
type sharedRunDocs struct {
	get func() (*engine.Collection, error)
}

func (d sharedRunDocs) Config() engine.CollectionConfig {
	c, err := d.get()
	if err != nil {
		return engine.CollectionConfig{}
	}
	return c.Config()
}

func (d sharedRunDocs) Scan(f query.Filter) ([]engine.ScanResult, error) {
	c, err := d.get()
	if err != nil {
		return nil, err
	}
	return c.Scan(f)
}

func (d sharedRunDocs) GetByKey(k string) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.GetByKey(k)
}

func (d sharedRunDocs) InsertWithKey(k string, m map[string]any) (uint64, time.Time, error) {
	c, err := d.get()
	if err != nil {
		return 0, time.Time{}, err
	}
	return c.InsertWithKey(k, m)
}

func (d sharedRunDocs) UpdateByKey(k string, m map[string]any) (time.Time, error) {
	c, err := d.get()
	if err != nil {
		return time.Time{}, err
	}
	return c.UpdateByKey(k, m)
}

func (d sharedRunDocs) DeleteByKey(k string) error {
	c, err := d.get()
	if err != nil {
		return err
	}
	return c.DeleteByKey(k)
}

// RunCollectionSpecs declares the collections this store owns on the shared
// handle. Exposed so parity tests assert one declaration.
func RunCollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{
		{Name: runsCollection, Version: runRecordSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("run_id")}},
	}
}

// RunCollectionIndexes lists the lookup indexes ensured per collection.
func RunCollectionIndexes() map[string][]string {
	return map[string][]string{
		runsCollection: {"project_id", "plan_id", "state"},
	}
}

// NewRunStoreFromDB builds a RunStore over the collection registered on the shared
// warden-db handle. The caller keeps ownership of db and closes it.
func NewRunStoreFromDB(db *wardendb.DB) (*RunStore, error) {
	spec := RunCollectionSpecs()[0]
	col, err := wardendb.Register[RunRecord](db, spec)
	if err != nil {
		return nil, err
	}
	eng, err := col.Engine()
	if err != nil {
		return nil, err
	}
	for _, f := range RunCollectionIndexes()[spec.Name] {
		if err := eng.EnsureIndex(f); err != nil {
			return nil, fmt.Errorf("autopilot: ensure %s.%s index: %w", spec.Name, f, err)
		}
	}
	return &RunStore{col: sharedRunDocs{get: col.Engine}}, nil
}
