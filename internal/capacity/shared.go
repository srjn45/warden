package capacity

// Shared-handle construction (warden-db porting pattern, see
// internal/projectstore/shared.go). NewDurableFenceStoreFromDB takes the shared
// *wardendb.DB: no path, no scriva.Open, no Close of the handle. Each claimed
// fence is one record of "quota_impact_fences" shaped as FenceRecord (schema-1),
// keyed by its snapshot-revision tuple so a repeated revision claim can never
// produce a second record. The JSONL log stays the production default;
// production wiring is untouched.

import (
	"encoding/json"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	fencesCollection = "quota_impact_fences"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
)

// docs is the document surface the shared fence store needs from a collection.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	InsertWithKey(string, map[string]any) (uint64, time.Time, error)
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

// CollectionSpecs declares the collection this store owns on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{{Name: fencesCollection, Version: recordSchemaVersion}}
}

// NewDurableFenceStoreFromDB builds a fence store over the
// "quota_impact_fences" collection registered on the shared warden-db handle.
// The caller keeps ownership of db and closes it; the collection must not
// already be registered on it.
func NewDurableFenceStoreFromDB(db *wardendb.DB) (*DurableFenceStore, error) {
	col, err := wardendb.Register[FenceRecord](db, CollectionSpecs()[0])
	if err != nil {
		return nil, err
	}
	return &DurableFenceStore{shared: sharedDocs{get: col.Engine}}, nil
}

// FenceKey is the shared-store key of r; the schema-2 migration uses it so
// imported fences collide with live claims exactly as native ones do.
func FenceKey(r FenceRecord) string { return fenceKey(r) }

// fenceKey is unambiguous (JSON array) even when keys contain separators.
func fenceKey(r FenceRecord) string {
	b, _ := json.Marshal([]any{r.SnapshotRevision, r.DomainKey, r.BucketKey, r.AgentID})
	return string(b)
}

func (s *DurableFenceStore) sharedRead() ([]FenceRecord, error) {
	res, err := s.shared.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]FenceRecord, 0, len(res))
	for _, r := range res {
		b, err := json.Marshal(r.Data)
		if err != nil {
			continue
		}
		var v FenceRecord
		if json.Unmarshal(b, &v) == nil && v.AgentID != "" && v.BucketKey != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

func (s *DurableFenceStore) sharedInsert(rec FenceRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	_, _, err = s.shared.InsertWithKey(fenceKey(rec), m)
	return err
}
