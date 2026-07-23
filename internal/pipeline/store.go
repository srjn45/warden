package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/store"
)

var (
	ErrNotFound = errors.New("pipeline not found")
	ErrExists   = errors.New("pipeline already exists")
)

// Store persists pipelines in an embedded ScrivaDB (github.com/srjn45/scriva)
// collection "pipelines" rooted at <dir>-db/, one record per pipeline keyed by
// its ID. A write appends one record instead of rewriting a whole per-pipeline
// JSON file (the write-amplification the previous store carried). The collection
// is opened with SyncModeNone: like the previous implementation this is a
// localhost daemon store, so the last write surviving a power-loss is not a
// requirement (append-only segments rule out torn reads regardless).
//
// The original dir (e.g. <data>/pipelines) is left in place holding the legacy
// <id>.json files as a read-only backup after the one-time import; the ScrivaDB
// lives in the sibling <dir>-db (mirroring sessions-db/). A single mutex
// serialises the compound read-modify-write methods (Create's check + write,
// Update's read-mutate-write); ScrivaDB does its own per-collection locking, so
// the mutex only guards those read-then-write critical sections.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

// importedMarker names the sentinel written (last) once the one-time legacy-JSON
// import into the ScrivaDB collection has completed. Its presence means the ScrivaDB
// is authoritative and no re-import runs; its absence means the import never
// finished, so the next open wipes the (derived) <dir>-db and retries from the
// intact legacy JSON. See NewStore / importLegacy.
const importedMarker = ".pipelines-filedb-imported"

// NewStore opens (creating if needed) the ScrivaDB-backed pipeline store. The dir
// argument keeps its historical meaning — the directory that holds the legacy
// <id>.json files — for signature compatibility with every caller and test; the
// ScrivaDB itself is rooted at the sibling <dir>-db. On first open any legacy
// <dir>/*.json records are imported into the collection. The import is guarded by
// importedMarker and is directory-atomic: if the sentinel is absent (never
// imported, or a prior attempt died partway) the derived <dir>-db is wiped and
// rebuilt from the read-only legacy JSON, then the sentinel is written LAST — so
// a crash mid-import loses nothing. A fresh empty dir (the common case in tests)
// imports zero records and proceeds with no special-casing.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbDir := dir + "-db"
	// The sentinel lives beside the data dir (in dir's parent), mirroring how the
	// sessions store parks .sessions-filedb-imported next to sessions-db/.
	sentinel := filepath.Join(filepath.Dir(dir), importedMarker)

	imported, err := fileExists(sentinel)
	if err != nil {
		return nil, err
	}
	if !imported {
		// Wipe any partial/failed prior attempt so the import starts clean (a
		// half-loaded collection would abort LoadJSONL on ErrDuplicateKey). Safe:
		// <dir>-db holds nothing not reproducible from the legacy JSON until the
		// sentinel says the import finished.
		if err := os.RemoveAll(dbDir); err != nil {
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
	col, err := db.Collection("pipelines")
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, col: col}

	if !imported {
		if err := importLegacy(dir, col); err != nil {
			db.Close()
			return nil, err
		}
		// Sentinel LAST: only now is the ScrivaDB authoritative.
		if err := os.WriteFile(sentinel, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
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

// importLegacy performs the one-time import of the legacy per-file JSON in dir
// into the ScrivaDB collection. Each *.json is decoded individually (skip+warn on
// a corrupt file or unsafe id — a bad file never blocks the upgrade), then the
// good records are loaded with LoadJSONL, which is atomic (all-or-nothing). A
// dir with no readable records (fresh install) is simply an empty import.
func importLegacy(dir string, col *engine.Collection) error {
	buf, err := legacyNDJSON(dir)
	if err != nil {
		return err
	}
	if buf.Len() == 0 {
		return nil // no legacy files, or none readable
	}
	_, err = col.LoadJSONL(&buf, "id")
	return err
}

// legacyNDJSON decodes every <id>.json in dir individually and returns the good
// records as an NDJSON buffer (one Pipeline per line, keyed by "id"). Corrupt or
// unsafe-id files are skipped with a warning so the batch handed to LoadJSONL is
// always parseable.
func legacyNDJSON(dir string) (bytes.Buffer, error) {
	var buf bytes.Buffer
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return buf, nil // no legacy dir → nothing to import
		}
		return buf, err
	}
	enc := json.NewEncoder(&buf) // Encode writes one compact JSON line + newline
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // skips .tmp-* temp files too
		}
		p, err := readPipeline(filepath.Join(dir, e.Name()))
		if err != nil {
			slog.Warn("pipeline store: import skipping unreadable legacy pipeline", "file", e.Name(), "err", err)
			continue
		}
		if err := store.SafeID(p.ID); err != nil {
			slog.Warn("pipeline store: import skipping legacy pipeline with unsafe id", "file", e.Name(), "id", p.ID)
			continue
		}
		if err := enc.Encode(p); err != nil {
			return buf, err
		}
	}
	return buf, nil
}

// readPipeline loads and decodes a legacy pipeline JSON file.
func readPipeline(path string) (*Pipeline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Pipeline
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// toRecord decomposes a Pipeline into a ScrivaDB record body via a JSON round-trip
// through its own tags, so nested fields (Jobs, Digest) stay lossless. The engine
// stamps the reserved _key on write, so it must NOT be present here.
func toRecord(p *Pipeline) (map[string]any, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fromRecord reconstructs a Pipeline from a record body. The reserved _key the
// engine stamped into the map is harmlessly dropped on unmarshal (Pipeline has
// no _key json tag).
func fromRecord(d map[string]any) (*Pipeline, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var p Pipeline
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// getFrom reads the pipeline keyed id, mapping a key miss to ErrNotFound.
func (s *Store) getFrom(id string) (*Pipeline, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRecord(r.Data)
}

func (s *Store) Create(p *Pipeline) error {
	if err := store.SafeID(p.ID); err != nil {
		return err
	}
	rec, err := toRecord(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, err := s.col.InsertWithKey(p.ID, rec); err != nil {
		if errors.Is(err, engine.ErrDuplicateKey) {
			return ErrExists
		}
		return err
	}
	return nil
}

func (s *Store) Get(id string) (*Pipeline, error) {
	if err := store.SafeID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getFrom(id)
}

// List returns every pipeline, sorted by ID. Always returns a non-nil slice. An
// undecodable record is skipped with a warning rather than failing the whole
// scan (matching the old List's corrupt-file tolerance).
func (s *Store) List() ([]*Pipeline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := []*Pipeline{}
	for _, r := range results {
		p, err := fromRecord(r.Data)
		if err != nil {
			key, _ := r.Data[engine.KeyField].(string)
			slog.Warn("pipeline store: skipping undecodable pipeline record", "key", key, "err", err)
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update applies fn to the stored pipeline under the lock and writes it back.
func (s *Store) Update(id string, fn func(*Pipeline)) error {
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.getFrom(id)
	if err != nil {
		return err
	}
	fn(p)
	rec, err := toRecord(p)
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(id, rec)
	return err
}

// Delete removes a pipeline's record, returning ErrNotFound if absent.
func (s *Store) Delete(id string) error {
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.col.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

// Close flushes the ScrivaDB index and stops its background compaction goroutine.
// The daemon defers it on shutdown.
func (s *Store) Close() error {
	return s.db.Close()
}
