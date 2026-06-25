package snapshot

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound is returned by Store.Get for an unknown snapshot id.
var ErrNotFound = errors.New("snapshot not found")

// ErrBadID is returned when a snapshot id contains path separators or "..".
var ErrBadID = errors.New("invalid snapshot id")

// Store persists each snapshot as one pretty-printed JSON file under
// <dir>/<id>.json, with the captured transcript alongside as <dir>/<id>.transcript.
// It mirrors store.FileStore's conventions: an RWMutex serializes the daemon's
// concurrent callers, and writes go through a temp file + rename so a reader never
// observes a partially written file. The transcript is a blob (not inlined into
// the JSON) so a multi-megabyte scrollback never bloats the metadata record.
type Store struct {
	mu  sync.RWMutex
	dir string
}

// NewStore creates the snapshots dir (0700) and returns a ready store.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// safeID guards the id used as a filename component against path traversal and
// tmux/target separators — the id reaches the store from user input (`wd snapshot
// restore <id>`), so it is validated before it touches the filesystem.
func safeID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return ErrBadID
	}
	return nil
}

func (s *Store) metaPath(id string) string { return filepath.Join(s.dir, id+".json") }

// transcriptPath is the on-disk location of a snapshot's transcript blob.
func (s *Store) transcriptPath(id string) string {
	return filepath.Join(s.dir, id+".transcript")
}

// Put writes the snapshot metadata and (when non-empty) its transcript blob. It
// stamps snap.TranscriptPath/TranscriptLines from the blob it wrote, so the
// persisted record points at the transcript the operator can read back.
func (s *Store) Put(snap *Snapshot, transcript string) error {
	if err := safeID(snap.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if transcript != "" {
		if err := os.WriteFile(s.transcriptPath(snap.ID), []byte(transcript), 0o600); err != nil {
			return err
		}
		snap.TranscriptPath = s.transcriptPath(snap.ID)
		snap.TranscriptLines = countLines(transcript)
	}
	return atomicWriteJSON(s.metaPath(snap.ID), snap)
}

// Get loads one snapshot by id, mapping a missing file to ErrNotFound.
func (s *Store) Get(id string) (*Snapshot, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return readSnapshot(s.metaPath(id))
}

// List returns snapshots newest-first (by CreatedAt). A non-empty sessionID
// filters to snapshots owned by that agent; "" returns all.
func (s *Store) List(sessionID string) ([]*Snapshot, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []*Snapshot
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // skips .transcript blobs and .tmp-* temp files
		}
		snap, err := readSnapshot(filepath.Join(s.dir, e.Name()))
		if err != nil {
			slog.Warn("snapshot store: skipping unreadable snapshot", "file", e.Name(), "err", err)
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

// readSnapshot loads and decodes a snapshot file, mapping a missing file to
// ErrNotFound.
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

// atomicWriteJSON marshals v and writes it to path via a temp file + rename, so
// readers never observe a partial file (mirrors store.FileStore.atomicWriteJSON).
func atomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
