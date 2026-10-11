package backendusage

// Shared-handle construction (warden-db porting pattern, see
// internal/projectstore/shared.go). NewSnapshotStoreFromDB takes the shared
// *wardendb.DB: no path, no scriva.Open, no Close of the handle. Each
// observation is one record of "usage_snapshots" keyed by its zero-padded
// revision, shaped as UsageSnapshot (schema-1). UsageSnapshot carries capacity
// percentages and reset times only — never tokens or credentials — and the
// port persists nothing else. The JSONL file stays the production default;
// production wiring is untouched.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	snapshotsCollection = "usage_snapshots"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
)

// docs is the document surface the shared store needs from a collection.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	InsertWithKey(string, map[string]any) (uint64, time.Time, error)
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

func (d sharedDocs) InsertWithKey(k string, m map[string]any) (uint64, time.Time, error) {
	c, err := d.get()
	if err != nil {
		return 0, time.Time{}, err
	}
	return c.InsertWithKey(k, m)
}

func (d sharedDocs) DeleteByKey(k string) error {
	c, err := d.get()
	if err != nil {
		return err
	}
	return c.DeleteByKey(k)
}

// CollectionSpecs declares the collection this store owns on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{{Name: snapshotsCollection, Version: recordSchemaVersion}}
}

// NewSnapshotStoreFromDB builds a snapshot store over the "usage_snapshots"
// collection registered on the shared warden-db handle. The caller keeps
// ownership of db and closes it; the collection must not already be registered.
func NewSnapshotStoreFromDB(db *wardendb.DB) (*SnapshotStore, error) {
	col, err := wardendb.Register[UsageSnapshot](db, CollectionSpecs()[0])
	if err != nil {
		return nil, err
	}
	return &SnapshotStore{shared: sharedDocs{get: col.Engine}, retention: SnapshotRetention}, nil
}

func snapshotKey(rev uint64) string { return fmt.Sprintf("%020d", rev) }

// sharedRead returns every retained observation; an undecodable record is
// skipped like a corrupt JSONL line.
func (s *SnapshotStore) sharedRead() ([]UsageSnapshot, error) {
	res, err := s.shared.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]UsageSnapshot, 0, len(res))
	for _, r := range res {
		b, err := json.Marshal(r.Data)
		if err != nil {
			continue
		}
		var v UsageSnapshot
		if json.Unmarshal(b, &v) == nil && v.Domain.Provider != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// sharedWrite inserts the new observation and prunes those older than cutoff,
// mirroring the legacy rewrite-with-retention in per-record operations.
func (s *SnapshotStore) sharedWrite(existing []UsageSnapshot, in UsageSnapshot, cutoff time.Time) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if _, _, err := s.shared.InsertWithKey(snapshotKey(in.Revision), m); err != nil {
		return err
	}
	for _, v := range existing {
		if v.RecordedAt.Before(cutoff) {
			if err := s.shared.DeleteByKey(snapshotKey(v.Revision)); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
				return err
			}
		}
	}
	return nil
}
