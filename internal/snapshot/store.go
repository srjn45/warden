package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/filedbv2/engine"
	"github.com/srjn45/filedbv2/filedb"
	"github.com/srjn45/filedbv2/query"
)

// ErrNotFound is returned by Store.Get for an unknown snapshot id.
var ErrNotFound = errors.New("snapshot not found")

// ErrBadID is returned when a snapshot id contains path separators or "..".
var ErrBadID = errors.New("invalid snapshot id")

// Store persists snapshot METADATA in an embedded FileDB (github.com/srjn45/filedbv2)
// "snapshots" collection rooted at a sibling <dir>-db/ directory, one record keyed by
// the snapshot id — so a write appends a single record instead of rewriting a whole
// per-snapshot JSON file. The collection is opened with SyncModeNone: like the
// previous single-file-per-record implementation this is a localhost daemon store, so
// the last write surviving a power-loss is not a requirement (append-only segments
// rule out torn reads regardless).
//
// The captured transcript is deliberately NOT stored in FileDB. It stays a flat blob
// alongside the original dir as <dir>/<id>.transcript, exactly as before: a
// multi-megabyte scrollback must never bloat the metadata record, and that reasoning
// applies equally to a FileDB record. The metadata's TranscriptPath points at that
// flat file.
type Store struct {
	dir string // original dir; transcript blobs live directly under it
	db  *filedb.DB
	col *engine.Collection
}

// importedMarker names the sentinel written (last) once the one-time legacy-JSON
// import into the FileDB collection has completed. Its presence means the FileDB is
// authoritative and no re-import runs; its absence means the import never finished, so
// the next open wipes the (derived) <dir>-db and retries from the intact legacy JSON.
// See NewStore / importLegacy.
const importedMarker = ".snapshots-filedb-imported"

// NewStore creates the snapshots dir (0700) — where transcript blobs live — and opens
// (creating if needed) the FileDB-backed metadata store in the sibling <dir>-db/
// directory. On first open it imports any legacy <dir>/<id>.json metadata into the
// collection. The import is guarded by importedMarker and is directory-atomic: if the
// sentinel is absent (never imported, or a prior attempt died partway) the derived
// <dir>-db is wiped and rebuilt from the read-only legacy JSON, then the sentinel is
// written LAST — so a crash mid-import loses nothing. The legacy <id>.json files are
// left in place as a read-only backup (transcripts were never touched).
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbDir := dir + "-db"
	sentinel := filepath.Join(filepath.Dir(dir), importedMarker)

	imported, err := fileExists(sentinel)
	if err != nil {
		return nil, err
	}
	if !imported {
		// Wipe any partial/failed prior attempt so the import starts from a clean
		// slate (a half-loaded collection would abort LoadJSONL on ErrDuplicateKey).
		// Safe: <dir>-db holds nothing not reproducible from the legacy JSON until the
		// sentinel says the import finished.
		if err := os.RemoveAll(dbDir); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, err
	}

	db, err := filedb.Open(dbDir, filedb.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("snapshots")
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{dir: dir, db: db, col: col}

	if !imported {
		if err := importLegacy(dir, col); err != nil {
			db.Close()
			return nil, err
		}
		// Sentinel LAST: only now is the FileDB authoritative.
		if err := os.WriteFile(sentinel, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

// fileExists reports whether path exists, distinguishing a genuine stat error from a
// plain not-exist.
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

// importLegacy performs the one-time import of the legacy per-file <dir>/<id>.json
// metadata into the FileDB collection. Each *.json is decoded individually (skip+warn
// on corrupt/unsafe-id, matching the old List's corrupt-file tolerance — a bad file
// never blocks the upgrade), then loaded into the collection with LoadJSONL, which is
// atomic (all-or-nothing). A missing/empty dir (fresh install) is an empty import. The
// .transcript blobs are ignored (the suffix filter skips them) — they stay put and the
// imported records keep pointing at them via TranscriptPath.
func importLegacy(dir string, col *engine.Collection) error {
	buf, err := legacyNDJSON(dir)
	if err != nil {
		return err
	}
	if buf.Len() == 0 {
		return nil // no legacy dir, or no readable records
	}
	_, err = col.LoadJSONL(&buf, "id")
	return err
}

// legacyNDJSON decodes every *.json in dir individually and returns the good records
// as an NDJSON buffer (one Snapshot per line, keyed by "id"). Corrupt or unsafe-id
// files are skipped with a warning, so the batch handed to LoadJSONL is always
// parseable and its all-or-nothing guarantee then protects only against a write-side
// failure.
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
			continue // skips .transcript blobs and .tmp-* temp files
		}
		snap, err := readSnapshot(filepath.Join(dir, e.Name()))
		if err != nil {
			slog.Warn("snapshot store: import skipping unreadable snapshot", "file", e.Name(), "err", err)
			continue
		}
		if err := safeID(snap.ID); err != nil {
			slog.Warn("snapshot store: import skipping snapshot with unsafe id", "file", e.Name(), "id", snap.ID)
			continue
		}
		if err := enc.Encode(snap); err != nil {
			return buf, err
		}
	}
	return buf, nil
}

// safeID guards the id used as a filename component (the transcript blob) and as the
// FileDB record key against path traversal and tmux/target separators — the id reaches
// the store from user input (`wd snapshot restore <id>`), so it is validated before it
// touches the filesystem.
func safeID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return ErrBadID
	}
	return nil
}

// transcriptPath is the on-disk location of a snapshot's transcript blob, unchanged
// from the pre-FileDB layout: a flat file directly under the original dir.
func (s *Store) transcriptPath(id string) string {
	return filepath.Join(s.dir, id+".transcript")
}

// Put writes the snapshot metadata (into FileDB) and (when non-empty) its transcript
// blob (as a flat <dir>/<id>.transcript file, unchanged). It stamps
// snap.TranscriptPath/TranscriptLines from the blob it wrote, so the persisted record
// points at the transcript the operator can read back.
func (s *Store) Put(snap *Snapshot, transcript string) error {
	if err := safeID(snap.ID); err != nil {
		return err
	}
	if transcript != "" {
		if err := os.WriteFile(s.transcriptPath(snap.ID), []byte(transcript), 0o600); err != nil {
			return err
		}
		snap.TranscriptPath = s.transcriptPath(snap.ID)
		snap.TranscriptLines = countLines(transcript)
	}
	rec, err := toRecord(snap)
	if err != nil {
		return err
	}
	_, err = s.col.Upsert(snap.ID, rec)
	return err
}

// Get loads one snapshot by id, mapping a missing record to ErrNotFound.
func (s *Store) Get(id string) (*Snapshot, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRecord(r.Data)
}

// List returns snapshots newest-first (by CreatedAt). A non-empty sessionID filters to
// snapshots owned by that agent; "" returns all. An undecodable record is skipped with
// a warning rather than failing the whole scan (matching the old List's corrupt-file
// tolerance).
func (s *Store) List(sessionID string) ([]*Snapshot, error) {
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	var out []*Snapshot
	for _, r := range results {
		snap, err := fromRecord(r.Data)
		if err != nil {
			key, _ := r.Data[engine.KeyField].(string)
			slog.Warn("snapshot store: skipping undecodable snapshot record", "key", key, "err", err)
			continue
		}
		if sessionID != "" && snap.SessionID != sessionID {
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Close flushes the FileDB index and stops its background compaction goroutine. The
// daemon defers it on shutdown.
func (s *Store) Close() error { return s.db.Close() }

// readSnapshot loads and decodes a legacy snapshot JSON file, mapping a missing file
// to ErrNotFound. It backs the one-time legacy import.
func readSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// toRecord decomposes a Snapshot into a FileDB record body via a JSON round-trip, so
// its fields stay real in the store (indexable in future) rather than an opaque blob.
// Always round-trip through JSON — never read typed business logic off the raw map —
// because a map[string]any returns numbers as float64, times as strings, etc.; the
// JSON round-trip through Snapshot's own tags is lossless. The engine stamps the
// reserved _key on Upsert, so it must NOT be present here.
func toRecord(snap *Snapshot) (map[string]any, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// fromRecord reconstructs a Snapshot from a record body. The reserved _key the engine
// stamped into the map is harmlessly dropped on unmarshal (Snapshot has no _key json
// tag).
func fromRecord(d map[string]any) (*Snapshot, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
