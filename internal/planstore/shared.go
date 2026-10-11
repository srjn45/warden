package planstore

// Shared-handle construction: follows the warden-db porting pattern set by
// internal/projectstore/shared.go. NewStoreFromDB takes the shared
// *wardendb.DB (no path, no scriva.Open); Store.Close is a no-op for it and the
// handle owner closes the DB. Records stay schema-1 shaped at registry
// Version 1; production wiring is untouched.

import (
	"fmt"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	plansCollection  = "plans"
	eventsCollection = "plan_events"
	notesCollection  = "plan_notes"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
	// sharedSegmentMaxSize mirrors the 512 KiB segment cap the legacy plans
	// DB opens with; it is expressed per collection on the shared handle.
	sharedSegmentMaxSize = 512 * 1024
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
// handle. plan_events and plan_notes are plan child collections that the
// schema-2 contract catalogue gained for this port.
//
// Legacy open options vs the shared handle: SyncModeAlways is identical (the
// strict policy); WithSegmentMaxSize is expressed per collection below;
// WithCompactInterval(2m) CANNOT be expressed per collection — the strict
// policy owns the compaction cadence handle-wide and overrides it.
func CollectionSpecs() []wardendb.Spec {
	opts := func() []scriva.CollectionOption {
		return []scriva.CollectionOption{scriva.WithUniqueIndex("id"), scriva.WithCollectionSegmentMaxSize(sharedSegmentMaxSize)}
	}
	return []wardendb.Spec{
		{Name: plansCollection, Version: recordSchemaVersion, Options: opts()},
		{Name: eventsCollection, Version: recordSchemaVersion, Options: opts()},
		{Name: notesCollection, Version: recordSchemaVersion, Options: opts()},
	}
}

// CollectionIndexes lists the lookup indexes ensured per collection.
func CollectionIndexes() map[string][]string {
	return map[string][]string{
		plansCollection:  {"status", "project_id"},
		eventsCollection: {"plan_id"},
		notesCollection:  {"plan_id"},
	}
}

// NewStoreFromDB builds a Store over the plans, plan_events and plan_notes
// collections registered on the shared warden-db handle. The caller keeps
// ownership of db and closes it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	plans, err := register[Plan](db, plansCollection)
	if err != nil {
		return nil, err
	}
	evts, err := register[PlanExecutionEvent](db, eventsCollection)
	if err != nil {
		return nil, err
	}
	nts, err := register[ExecutionNote](db, notesCollection)
	if err != nil {
		return nil, err
	}
	seq, err := initSeqCounter(evts)
	if err != nil {
		return nil, err
	}
	return &Store{col: plans, events: evts, notes: nts, seqCounter: seq}, nil
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
			return nil, fmt.Errorf("planstore: ensure %s.%s index: %w", name, f, err)
		}
	}
	return sharedDocs{get: col.Engine}, nil
}
