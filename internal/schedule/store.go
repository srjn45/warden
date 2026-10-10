package schedule

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/legacyimport"
)

var (
	ErrNotFound = errors.New("schedule not found")
	ErrExists   = errors.New("schedule already exists")
)

// importedMarker names the sentinel written (last) once the one-time legacy-JSON
// import into the ScrivaDB "schedules" collection has completed. Its presence means
// the ScrivaDB is authoritative and no re-import runs; its absence means the import
// never finished, so the next open wipes the (derived) schedules-db and retries
// from the intact legacy JSON. See NewStore / importLegacy.
const importedMarker = ".schedules-filedb-imported"

// Store persists schedules as records in an embedded ScrivaDB "schedules"
// collection (github.com/srjn45/scriva), one record per schedule keyed by its
// ID. This replaces the previous single flat schedules.json rewritten atomically
// on every write. The collection is opened with SyncModeNone: this is a localhost
// daemon store, so the last write surviving a power-loss is not a requirement
// (append-only segments rule out torn reads regardless).
//
// The daemon is the only holder. A single mutex serialises the compound
// read-modify-write methods (Create's exists-check + insert, Update's
// read-modify-write), mirroring the sessions store; ScrivaDB does its own
// per-collection locking, so the store mutex only guards the read-then-write
// critical sections. Read-only methods take it too for a behaviour-identical
// faithful port of the previous mutex model.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

// NewStore opens (creating if needed) the ScrivaDB-backed schedule store. The path
// argument is the legacy schedules.json location (unchanged call site): the
// ScrivaDB directory is derived as a sibling of it (schedules.json → schedules-db/,
// mirroring the sessions store's sessions-db/ naming).
func NewStore(path string) (*Store, error) {
	dir := filepath.Dir(path)
	dbDir := strings.TrimSuffix(path, filepath.Ext(path)) + "-db"

	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, err
	}

	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("schedules")
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

// LegacyImport is the schedules.json -> schedules-db import for the migration registry.
var LegacyImport = legacyimport.Importer{
	Present: func(dir string) (bool, error) {
		return legacyimport.Exists(filepath.Join(dir, "schedules.json"))
	},
	Import: func(dir string) error {
		path := filepath.Join(dir, "schedules.json")
		if ok, _ := legacyimport.Exists(path); !ok {
			return nil
		}
		s, err := NewStore(path)
		if err != nil {
			return err
		}
		defer s.Close()
		return importLegacy(path, s.col)
	},
	Verify: func(dir string) error {
		path := filepath.Join(dir, "schedules.json")
		if ok, _ := legacyimport.Exists(path); !ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return nil
		}
		var m map[string]*Schedule
		if err := json.Unmarshal(data, &m); err != nil {
			return err
		}
		s, err := NewStore(path)
		if err != nil {
			return err
		}
		defer s.Close()
		var missing []string
		for id := range m {
			if exists, err := s.col.Exists(id); err != nil {
				return err
			} else if !exists {
				missing = append(missing, id)
			}
		}
		return legacyimport.VerifyNone("schedules", missing)
	},
}

// fileExists reports whether path exists, distinguishing a genuine stat error
// from a plain not-exist.
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

// importLegacy performs the one-time import of the legacy flat schedules.json
// (a single map keyed by schedule id) into the ScrivaDB collection. A missing file
// (fresh install) is simply an empty import. The legacy file is left untouched as
// a read-only backup.
func importLegacy(path string, col *engine.Collection) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // no legacy file → nothing to import
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	m := map[string]*Schedule{}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for id, sc := range m {
		if sc == nil {
			continue
		}
		rec, err := toRecord(sc)
		if err != nil {
			return err
		}
		if _, _, err := col.InsertWithKey(id, rec); err != nil {
			if errors.Is(err, engine.ErrDuplicateKey) {
				continue
			}
			return err
		}
	}
	return nil
}

// toRecord decomposes a Schedule into a ScrivaDB record body via a JSON round-trip,
// so its fields stay real in the store rather than an opaque blob. The engine
// stamps the reserved key field on InsertWithKey/UpdateByKey, so it must NOT be
// present here.
func toRecord(sc *Schedule) (map[string]any, error) {
	b, err := json.Marshal(sc)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	delete(m, "state") // derived on read, never stored
	return m, nil
}

// fromRecord reconstructs a Schedule from a record body. The reserved key field
// the engine stamped into the map is harmlessly dropped on unmarshal (Schedule
// has no matching json tag).
func fromRecord(d map[string]any) (*Schedule, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var sc Schedule
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// Create persists a new schedule, returning ErrExists if its id is taken.
func (s *Store) Create(sc *Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := toRecord(sc)
	if err != nil {
		return err
	}
	if _, _, err := s.col.InsertWithKey(sc.ID, rec); err != nil {
		if errors.Is(err, engine.ErrDuplicateKey) {
			return ErrExists
		}
		return err
	}
	return nil
}

// Get returns one schedule by id, or ErrNotFound.
func (s *Store) Get(id string) (*Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// get reads the schedule keyed id, mapping a key miss to ErrNotFound. The caller
// holds mu.
func (s *Store) get(id string) (*Schedule, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRecord(r.Data)
}

// List returns all schedules sorted by id. An undecodable record is skipped
// rather than failing the whole scan.
func (s *Store) List() ([]*Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Schedule, 0, len(results))
	for _, r := range results {
		sc, err := fromRecord(r.Data)
		if err != nil {
			continue
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update applies fn to the stored schedule under the lock and writes it back
// atomically. It returns ErrNotFound if the id is absent.
func (s *Store) Update(id string, fn func(*Schedule)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, err := s.get(id)
	if err != nil {
		return err
	}
	fn(sc)
	rec, err := toRecord(sc)
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(id, rec)
	return err
}

// Delete removes a schedule by id, returning ErrNotFound if absent.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.col.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

// Close flushes the ScrivaDB index and stops its background compaction goroutine.
func (s *Store) Close() error {
	return s.db.Close()
}
