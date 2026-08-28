// Package projectstore persists the cockpit's recent-projects list (docs/specs/
// 2026-08-26-collaboration-groups.md §6.2 / impl Stage C5): one lean record per
// project the operator has opened, holding only a roster-style descriptor —
// project key, display name, remote/path, last-opened — never transcripts.
//
// It mirrors internal/groupstore and internal/backendstore: an embedded ScrivaDB
// collection opened SyncModeNone, one record per project keyed by its canonical
// project key (the B2 normalizer's output, internal/projectkey). The cockpit's
// Open Project panel is the only holder — the daemon never touches this store, so
// there is no cross-process writer to race (a second cockpit that fails to open it
// simply runs without a persisted recent list). This is a fresh store; there is no
// legacy JSON to import.
package projectstore

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

// Recent is one previously-opened project in the recent list. It carries only the
// lean descriptor the Open Project panel needs to re-open it: the canonical
// project key (from B2), a display name, the git remote (empty for a local-only
// repo), the local filesystem path the orchestrator spawns in, and when it was
// last opened. Never anything transcript- or log-sized.
type Recent struct {
	Key        string    `json:"key"`
	Name       string    `json:"name"`
	Remote     string    `json:"remote,omitempty"`
	Path       string    `json:"path"`
	LastOpened time.Time `json:"last_opened"`
}

// Store persists projects as records in an embedded ScrivaDB "projects"
// collection, one record per project keyed by its project key. Opened
// SyncModeNone: this is a localhost cockpit store, so last-write-survives-
// power-loss is not required (append-only segments rule out torn reads anyway).
//
// A single mutex serialises the compound Touch (upsert) read-modify-write,
// mirroring the group/backend stores; ScrivaDB does its own per-collection
// locking, so the store mutex only guards the read-then-write critical section.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

// NewStore opens (creating if needed) the ScrivaDB-backed project store at dir
// (~/.warden/projects). This is a fresh store: there is no legacy JSON to import.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("projects")
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

// toRecord decomposes a Recent into a ScrivaDB record body via a JSON round-trip
// so its fields stay real in the store rather than an opaque blob. The engine
// stamps the reserved key field on InsertWithKey/UpdateByKey, so it must NOT be
// present here.
func toRecord(r *Recent) (map[string]any, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fromRecord reconstructs a Recent from a record body. The reserved key field the
// engine stamped into the map is harmlessly dropped on unmarshal (Recent has no
// matching json tag).
func fromRecord(d map[string]any) (*Recent, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var r Recent
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Touch upserts a project into the recent list, stamping LastOpened to now. An
// existing record (same key) is updated in place — display name, remote and path
// are refreshed so a moved/renamed repo tracks its latest values — and a new key
// is inserted. The compound read-then-write is serialised under the store mutex.
// A blank key is rejected: every recent must key on the B2 project identity.
func (s *Store) Touch(r Recent) error {
	if r.Key == "" {
		return errors.New("projectstore: empty project key")
	}
	r.LastOpened = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := toRecord(&r)
	if err != nil {
		return err
	}
	// Insert-if-absent, update-if-present. InsertWithKey reports a duplicate for an
	// existing key, in which case we overwrite it with UpdateByKey.
	if _, _, err := s.col.InsertWithKey(r.Key, rec); err != nil {
		if errors.Is(err, engine.ErrDuplicateKey) {
			_, uerr := s.col.UpdateByKey(r.Key, rec)
			return uerr
		}
		return err
	}
	return nil
}

// List returns every recent project, most-recently-opened first. An undecodable
// record is skipped rather than failing the whole scan.
func (s *Store) List() ([]Recent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]Recent, 0, len(results))
	for _, res := range results {
		r, err := fromRecord(res.Data)
		if err != nil {
			continue
		}
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastOpened.After(out[j].LastOpened) })
	return out, nil
}

// Close flushes the ScrivaDB index and stops its background compaction goroutine.
func (s *Store) Close() error {
	return s.db.Close()
}
