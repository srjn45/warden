package ctxstore

// Shared-handle construction: follows the warden-db porting pattern set by
// internal/pipeline/shared.go. NewStoreFromDB takes the shared *wardendb.DB (no
// path, no scriva.Open); Store.Close is a no-op for it and the handle owner
// closes the DB. Records stay schema-1 shaped at registry Version 1;
// production wiring is untouched.

import (
	"fmt"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	collectionName = "context_entries"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
)

// docs is the document surface Store needs from a collectionName; both the legacy
// *engine.Collection and the shared-handle resolver satisfy it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(f query.Filter) ([]engine.ScanResult, error)
	GetByKey(k string) (engine.Record, error)
	InsertWithKey(k string, m map[string]any) (uint64, time.Time, error)
	Upsert(k string, m map[string]any) (engine.Record, error)
	UpdateIfRev(k string, rev uint64, m map[string]any) (bool, error)
	UpdateIfMatch(k string, pred func(cur map[string]any) bool, m map[string]any) (bool, error)
	DeleteByKey(k string) error
}

// sharedDocs resolves the engine collectionName on every call.
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

func (d sharedDocs) InsertWithKey(k string, m map[string]any) (uint64, time.Time, error) {
	c, err := d.get()
	if err != nil {
		return 0, time.Time{}, err
	}
	return c.InsertWithKey(k, m)
}

func (d sharedDocs) Upsert(k string, m map[string]any) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.Upsert(k, m)
}

func (d sharedDocs) UpdateIfRev(k string, rev uint64, m map[string]any) (bool, error) {
	c, err := d.get()
	if err != nil {
		return false, err
	}
	return c.UpdateIfRev(k, rev, m)
}

func (d sharedDocs) UpdateIfMatch(k string, pred func(cur map[string]any) bool, m map[string]any) (bool, error) {
	c, err := d.get()
	if err != nil {
		return false, err
	}
	return c.UpdateIfMatch(k, pred, m)
}

func (d sharedDocs) DeleteByKey(k string) error {
	c, err := d.get()
	if err != nil {
		return err
	}
	return c.DeleteByKey(k)
}

// CollectionSpecs declares the collections this store owns on the shared
// handle. The strict policy forces SyncModeAlways on the shared handle
// (overriding any legacy SyncModeNone).
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{
		{Name: collectionName, Version: recordSchemaVersion},
	}
}

// CollectionIndexes lists the lookup indexes ensured per collectionName.
func CollectionIndexes() map[string][]string {
	return map[string][]string{}
}

// NewStoreFromDB builds a Store over the context_entries collectionName registered on the
// shared warden-db handle. The caller keeps ownership of db and closes it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	col, err := register[Entry](db, collectionName)
	if err != nil {
		return nil, err
	}
	return newStore(col), nil
}

func register[T any](db *wardendb.DB, name string) (docs, error) {
	var spec wardendb.Spec
	for _, s := range CollectionSpecs() {
		if s.Name == name {
			spec = s
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
			return nil, fmt.Errorf("ctxstore: ensure %s.%s index: %w", name, f, err)
		}
	}
	return sharedDocs{get: col.Engine}, nil
}
