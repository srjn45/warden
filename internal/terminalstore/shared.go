package terminalstore

// Shared-handle construction (warden-db porting pattern; see
// internal/projectstore/shared.go for the reference port).
//
// NewStoreFromDB registers the "terminals" collection on the shared
// *wardendb.DB: no path, no scriva.Open, and Store.Close is a no-op because
// the handle owner closes the handle. The legacy-directory import is not part
// of this constructor. Records stay schema-1 shaped (registry version 1).

import (
	"fmt"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	terminalsCollection = "terminals"
	recordSchemaVersion = 1
)

// docs is the document surface Store needs from a collection; both the legacy
// *engine.Collection and the shared-handle resolver satisfy it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	GetByKey(string) (engine.Record, error)
	Exists(string) (bool, error)
	InsertWithKey(string, map[string]any) (uint64, time.Time, error)
	UpdateByKey(string, map[string]any) (time.Time, error)
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

func (d sharedDocs) DeleteByKey(k string) error {
	c, err := d.get()
	if err != nil {
		return err
	}
	return c.DeleteByKey(k)
}

// CollectionSpecs declares the collections this store owns on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{
		{Name: terminalsCollection, Version: recordSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
	}
}

// CollectionIndexes lists the lookup indexes ensured per collection.
func CollectionIndexes() map[string][]string {
	return map[string][]string{
		terminalsCollection: {"project_id", "status"},
	}
}

// NewStoreFromDB builds a Store over the "terminals" collection registered on
// the shared warden-db handle. The caller keeps ownership of db and closes it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	spec := CollectionSpecs()[0]
	col, err := wardendb.Register[Terminal](db, spec)
	if err != nil {
		return nil, err
	}
	eng, err := col.Engine()
	if err != nil {
		return nil, err
	}
	for _, f := range CollectionIndexes()[spec.Name] {
		if err := eng.EnsureIndex(f); err != nil {
			return nil, fmt.Errorf("terminalstore: ensure %s.%s index: %w", spec.Name, f, err)
		}
	}
	return &Store{col: sharedDocs{get: col.Engine}}, nil
}
