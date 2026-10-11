package snapshot

// Shared-handle construction (warden-db porting pattern; see
// internal/projectstore/shared.go for the reference port). NewStoreFromDB takes
// the shared *wardendb.DB: no path, no scriva.Open, no Close of the handle.
// Records stay schema-1 shaped; transcript blobs stay external files.

import (
	"os"
	"path/filepath"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	collectionName      = "snapshots"
	recordSchemaVersion = 1
)

// docs is the document surface Store needs from its collection; both the legacy
// *engine.Collection and the shared-handle resolver satisfy it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	GetByKey(string) (engine.Record, error)
	Upsert(string, map[string]any) (engine.Record, error)
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

func (d sharedDocs) Upsert(k string, m map[string]any) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.Upsert(k, m)
}

// CollectionSpecs declares the collections this store owns on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{{Name: collectionName, Version: recordSchemaVersion}}
}

// CollectionIndexes lists the lookup indexes ensured per collection.
func CollectionIndexes() map[string][]string { return map[string][]string{} }

// NewStoreFromDB builds a Store over the snapshots collection registered on the
// shared warden-db handle. The caller keeps ownership of db and closes it. The
// transcript blob directory is <dataDir>/snapshots, derived from the handle's
// location (dataDir/warden-db) and created (0700) if missing.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	dataDir := filepath.Dir(db.Dir())
	dir := filepath.Join(dataDir, "snapshots")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	spec := CollectionSpecs()[0]
	col, err := wardendb.Register[Snapshot](db, spec)
	if err != nil {
		return nil, err
	}
	if _, err := col.Engine(); err != nil {
		return nil, err
	}
	return &Store{dir: dir, dataDir: dataDir, col: sharedDocs{get: col.Engine}}, nil
}
