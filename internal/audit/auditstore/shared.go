// Package auditstore is the shared warden-db implementation of audit.Trail. It
// lives beside internal/audit because internal/wardendb itself imports audit.
package auditstore

// Shared-handle construction: follows the warden-db porting pattern set by
// internal/ctxstore/shared.go. NewStoreFromDB takes the shared *wardendb.DB (no
// path, no scriva.Open); Store never closes the handle — its owner does. The
// file-backed Writer stays the production default and the on-disk trail;
// production wiring is untouched. Records stay schema-1 shaped (the audit.Event JSON
// object) at registry Version 1.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	collectionName = "audit_events"
	// recordSchemaVersion is the registry version of the schema-1 shaped
	// records this port persists.
	recordSchemaVersion = 1
)

// docs is the document surface Store needs from the collection; the shared
// handle resolver satisfies it.
type docs interface {
	Config() engine.CollectionConfig
	Scan(f query.Filter) ([]engine.ScanResult, error)
	InsertWithKey(k string, m map[string]any) (uint64, time.Time, error)
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

// Store is a typed audit trail over the shared warden-db handle. Like Writer it
// is best-effort on Log and a nil *Store is a no-op sink.
var _ audit.Trail = (*Store)(nil)

type Store struct {
	col docs
	seq atomic.Uint64
}

// NewStoreFromDB builds a Store over the audit_events collection registered on
// the shared warden-db handle. The caller keeps ownership of db and closes it.
func NewStoreFromDB(db *wardendb.DB) (*Store, error) {
	spec := CollectionSpecs()[0]
	col, err := wardendb.Register[audit.Event](db, spec)
	if err != nil {
		return nil, err
	}
	eng, err := col.Engine()
	if err != nil {
		return nil, err
	}
	for _, f := range CollectionIndexes()[spec.Name] {
		if err := eng.EnsureIndex(f); err != nil {
			return nil, fmt.Errorf("audit: ensure %s.%s index: %w", spec.Name, f, err)
		}
	}
	return &Store{col: sharedDocs{get: col.Engine}}, nil
}

// Log appends ev, stamping Time if unset. Best-effort by contract: failures are
// logged and swallowed. A nil Store is a no-op.
func (s *Store) Log(ev audit.Event) {
	if s == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if err := s.append(ev); err != nil {
		slog.Warn("audit: failed to record event", "action", ev.Action, "target", ev.Target, "err", err)
	}
}

func (s *Store) append(ev audit.Event) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	// Keys sort in arrival order: wall clock, then a per-process sequence, then
	// randomness so concurrent daemons can never collide.
	var r [4]byte
	_, _ = rand.Read(r[:])
	key := fmt.Sprintf("%020d-%012d-%s", time.Now().UnixNano(), s.seq.Add(1), hex.EncodeToString(r[:]))
	_, _, err = s.col.InsertWithKey(key, m)
	return err
}

// Read returns matching events in append order, oldest first, with the same
// audit.Filter semantics as the package-level Read. A nil Store is empty.
func (s *Store) Read(f audit.Filter) ([]audit.Event, error) {
	if s == nil {
		return []audit.Event{}, nil
	}
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	sort.Slice(results, func(i, j int) bool {
		ki, _ := results[i].Data[engine.KeyField].(string)
		kj, _ := results[j].Data[engine.KeyField].(string)
		return ki < kj
	})
	out := []audit.Event{}
	for _, r := range results {
		raw, err := json.Marshal(r.Data)
		if err != nil {
			continue
		}
		var ev audit.Event
		if json.Unmarshal(raw, &ev) != nil {
			continue
		}
		if f.Matches(ev) {
			out = append(out, ev)
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, nil
}
