package projectstore

// Shared-handle construction (warden-db porting pattern, task core-entities-1).
//
// This is the reference port other stores copy:
//
//  1. NewStoreFromDB(db) takes the shared *wardendb.DB: no path, no scriva.Open,
//     no Close of the handle. The handle owner closes it; Store.Close is a no-op.
//  2. Each collection is declared once with wardendb.Register[T] using a Spec
//     whose Name is the contract catalogue name; secondary indexes are ensured
//     right after registration.
//  3. A typed collection's engine must be re-fetched per operation (Engine()
//     fails with ErrClosed after handle retirement), so the store holds a small
//     per-call resolver (sharedDocs) behind the same doc-level interface the
//     legacy path uses — every method body of Store stays untouched.
//  4. Registered-record type T is the owning package's own record type, so a
//     second package cannot reach the collection under another shape.
//
// Records stay schema-1 shaped (authoritative membership lists, group
// project_ids): the schema-2 reshape (child FKs, projects.group_id) is the
// migration/cutover task, not this port. Production wiring is untouched.

import (
	"fmt"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	projectsCollection = "projects"
	groupsCollection   = "project_groups"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists (schema-2 collections declare 2).
	recordSchemaVersion = 1
)

// docs is the document surface Store needs from a collection; both the legacy
// *engine.Collection and the shared-handle resolver satisfy it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	GetByKey(string) (engine.Record, error)
	InsertWithKey(string, map[string]any) (uint64, time.Time, error)
	UpdateByKey(string, map[string]any) (time.Time, error)
	DeleteByKey(string) error
}

// sharedDocs resolves the engine collection on every call.
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

// CollectionSpecs declares the collections this store owns on the shared
// handle and the secondary indexes ensured on each. Exposed so parity tests
// (and later schema checks) assert one declaration.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{
		{Name: projectsCollection, Version: recordSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
		{Name: groupsCollection, Version: recordSchemaVersion, Options: []scriva.CollectionOption{scriva.WithUniqueIndex("id")}},
	}
}

// CollectionIndexes lists the lookup indexes ensured per collection.
func CollectionIndexes() map[string][]string {
	return map[string][]string{
		projectsCollection: {"status"},
		groupsCollection:   {"name"},
	}
}

// NewStoreFromDB builds a Store over collections registered on the shared
// warden-db handle. The caller keeps ownership of db and closes it; the
// collections must not already be registered on it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	proj, err := register[Project](db, projectsCollection)
	if err != nil {
		return nil, err
	}
	grp, err := register[ProjectGroup](db, groupsCollection)
	if err != nil {
		return nil, err
	}
	return &Store{col: proj, groups: grp, now: time.Now}, nil
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
			return nil, fmt.Errorf("projectstore: ensure %s.%s index: %w", name, f, err)
		}
	}
	return sharedDocs{get: col.Engine}, nil
}
