package metrics

// Shared-handle construction: follows the warden-db porting pattern set by
// internal/ctxstore/shared.go. NewStoreFromDB takes the shared *wardendb.DB (no
// path, no scriva.Open); Store never closes the handle — its owner does. The
// day-file Recorder stays the production default; production wiring is
// untouched. Records stay schema-1 shaped (the Sample JSON object) at registry
// Version 1.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	collectionName = "metrics_samples"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
)

// SampleStore is the metric-history surface shared by the file-backed Recorder
// and the shared-handle Store.
type SampleStore interface {
	Record(s Sample) error
	History(since time.Time, limit int) ([]Sample, error)
	Prune(now time.Time, keepDays int) error
}

var (
	_ SampleStore = (*Recorder)(nil)
	_ SampleStore = (*Store)(nil)
)

// docs is the document surface Store needs from the collection; the shared
// handle resolver satisfies it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(f query.Filter) ([]engine.ScanResult, error)
	InsertWithKey(k string, m map[string]any) (uint64, time.Time, error)
	DeleteByKey(k string) error
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

// CollectionSpecs declares the collections this store owns on the shared
// handle. The strict policy forces SyncModeAlways on the shared handle.
func CollectionSpecs() []wardendb.Spec {
	return []wardendb.Spec{
		{Name: collectionName, Version: recordSchemaVersion},
	}
}

// CollectionIndexes lists the lookup indexes ensured per collection.
func CollectionIndexes() map[string][]string {
	return map[string][]string{}
}

// Store records and reads Samples in the shared warden-db metrics_samples
// collection. Safe for concurrent use.
type Store struct {
	col docs
}

// NewStoreFromDB builds a Store over the metrics_samples collection registered
// on the shared warden-db handle. The caller keeps ownership of db and closes it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	spec := CollectionSpecs()[0]
	col, err := wardendb.Register[Sample](db, spec)
	if err != nil {
		return nil, err
	}
	eng, err := col.Engine()
	if err != nil {
		return nil, err
	}
	for _, f := range CollectionIndexes()[spec.Name] {
		if err := eng.EnsureIndex(f); err != nil {
			return nil, fmt.Errorf("metrics: ensure %s.%s index: %w", spec.Name, f, err)
		}
	}
	return &Store{col: sharedDocs{get: col.Engine}}, nil
}

// Record appends one sample, keyed "<taken_at unix-nano>:<random id>" so keys
// sort by TakenAt and two samples at the same instant never collide.
func (s *Store) Record(smp Sample) error {
	raw, err := json.Marshal(smp)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	var r [6]byte
	_, _ = rand.Read(r[:])
	key := fmt.Sprintf("%020d:%s", smp.TakenAt.UnixNano(), hex.EncodeToString(r[:]))
	_, _, err = s.col.InsertWithKey(key, m)
	return err
}

func (s *Store) scan() ([]keyed, error) {
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]keyed, 0, len(results))
	for _, r := range results {
		raw, err := json.Marshal(r.Data)
		if err != nil {
			continue
		}
		var smp Sample
		if json.Unmarshal(raw, &smp) != nil {
			continue
		}
		key, _ := r.Data[engine.KeyField].(string)
		out = append(out, keyed{key: key, s: smp})
	}
	return out, nil
}

type keyed struct {
	key string
	s   Sample
}

// History returns samples with TakenAt >= since, newest-first, capped at limit
// (<=0 ⇒ no cap).
func (s *Store) History(since time.Time, limit int) ([]Sample, error) {
	all, err := s.scan()
	if err != nil {
		return nil, err
	}
	var out []Sample
	for _, k := range all {
		if !since.IsZero() && k.s.TakenAt.Before(since) {
			continue
		}
		out = append(out, k.s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TakenAt.After(out[j].TakenAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Prune deletes samples older than keepDays relative to now, on the same UTC
// day boundary the Recorder uses for its day-files.
func (s *Store) Prune(now time.Time, keepDays int) error {
	cutoff := now.UTC().AddDate(0, 0, -keepDays).Truncate(24 * time.Hour)
	all, err := s.scan()
	if err != nil {
		return err
	}
	for _, k := range all {
		if k.s.TakenAt.UTC().Truncate(24 * time.Hour).Before(cutoff) {
			if err := s.col.DeleteByKey(k.key); err != nil {
				return err
			}
		}
	}
	return nil
}
