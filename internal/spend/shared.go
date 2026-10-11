package spend

// Shared-handle construction (warden-db porting pattern, see
// internal/projectstore/shared.go). NewStoreFromDB takes the shared
// *wardendb.DB: no path, no scriva.Open, no Close of the handle. Each session's
// running gauge is one record of "spend_records" keyed by session id, shaped as
// Entry (schema-1). The spend.json file stays the production default;
// production wiring is untouched.

import (
	"encoding/json"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	recordsCollection = "spend_records"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
)

// docs is the document surface the shared gauge needs from a collection.
type docs interface {
	Config() engine.CollectionConfig
	Scan(query.Filter) ([]engine.ScanResult, error)
	Upsert(string, map[string]any) (engine.Record, error)
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

func (d sharedDocs) Upsert(k string, m map[string]any) (engine.Record, error) {
	c, err := d.get()
	if err != nil {
		return engine.Record{}, err
	}
	return c.Upsert(k, m)
}

// CollectionSpecs declares the collection this store owns on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{{Name: recordsCollection, Version: recordSchemaVersion}}
}

// NewStoreFromDB builds a gauge over the "spend_records" collection registered
// on the shared warden-db handle. The caller keeps ownership of db and closes
// it; the collection must not already be registered on it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	col, err := wardendb.Register[Entry](db, CollectionSpecs()[0])
	if err != nil {
		return nil, err
	}
	return &Store{shared: sharedDocs{get: col.Engine}, now: time.Now}, nil
}

// sharedLoad reads every session's gauge keyed by session id. The record key is
// stamped into the body under the engine's reserved field; an undecodable
// record is skipped, mirroring the legacy corrupt-file fail-open.
func (s *Store) sharedLoad() (map[string]Entry, error) {
	res, err := s.shared.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Entry, len(res))
	for _, r := range res {
		key, _ := r.Data[engine.KeyField].(string)
		if key == "" {
			continue
		}
		b, err := json.Marshal(r.Data)
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		out[key] = e
	}
	return out, nil
}

func (s *Store) sharedSave(id string, e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	_, err = s.shared.Upsert(id, m)
	return err
}
