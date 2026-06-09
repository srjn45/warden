package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrBadID is returned when a session id contains path separators or "..".
var ErrBadID = errors.New("invalid session id")

// FileStore persists each session as one pretty-printed JSON file under
// <dir>/sessions/<id>.json. Archived sessions move to <dir>/closed/<id>.json.
// The daemon is the only holder; an RWMutex serializes its concurrent callers
// (HTTP handlers, poller, classify goroutine). Writes go through a temp file +
// rename, so a concurrent reader never observes a partially written file. (This
// guards torn reads, not power-loss durability — the store is not fsync'd; for a
// localhost session store the last write surviving a crash is not a requirement.)
type FileStore struct {
	mu       sync.RWMutex
	dir      string
	sessions string
	closed   string
}

// NewFileStore creates the data dir layout and returns a ready store.
func NewFileStore(dir string) (*FileStore, error) {
	fs := &FileStore{
		dir:      dir,
		sessions: filepath.Join(dir, "sessions"),
		closed:   filepath.Join(dir, "closed"),
	}
	if err := os.MkdirAll(fs.sessions, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(fs.closed, 0o700); err != nil {
		return nil, err
	}
	return fs, nil
}

func safeID(id string) error {
	// "/" and "\" plus ".." guard against path traversal (the id is a filename
	// component); ":" is a tmux target separator (session:window) that would
	// silently break `tmux -t <id>` targeting.
	if id == "" || strings.ContainsAny(id, `/\:`) || strings.Contains(id, "..") {
		return ErrBadID
	}
	return nil
}

// SafeID reports whether id is a valid session id (no path separators or "..").
// Exported for callers that validate a candidate id before insert (e.g. adopt).
func SafeID(id string) error { return safeID(id) }

func (fs *FileStore) activePath(id string) string { return filepath.Join(fs.sessions, id+".json") }
func (fs *FileStore) closedPath(id string) string { return filepath.Join(fs.closed, id+".json") }

// atomicWriteJSON marshals v and writes it to path via a temp file + rename, so
// readers never observe a partial file.
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

// readSession loads and decodes a session file, mapping a missing file to
// ErrNotFound.
func readSession(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (fs *FileStore) Insert(ctx context.Context, s *Session) error {
	if err := safeID(s.ID); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	path := fs.activePath(s.ID)
	if _, err := os.Stat(path); err == nil {
		return ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	if s.Events == nil {
		s.Events = []Event{}
	}
	return atomicWriteJSON(path, s)
}

func (fs *FileStore) Get(ctx context.Context, id string) (*Session, error) {
	if err := safeID(id); err != nil {
		return nil, err
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return readSession(fs.activePath(id))
}

func (fs *FileStore) List(ctx context.Context) ([]*Session, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	entries, err := os.ReadDir(fs.sessions)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue // skips .tmp-* temp files too
		}
		s, err := readSession(filepath.Join(fs.sessions, e.Name()))
		if err != nil {
			log.Printf("filestore: skipping %s: %v", e.Name(), err)
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// mutate loads the active session, applies fn, bumps UpdatedAt, and writes it
// back atomically — the read-check-write runs under the write lock.
func (fs *FileStore) mutate(id string, fn func(*Session)) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := readSession(fs.activePath(id))
	if err != nil {
		return err
	}
	fn(s)
	s.UpdatedAt = time.Now().UTC()
	return atomicWriteJSON(fs.activePath(id), s)
}

func (fs *FileStore) UpdateStatus(ctx context.Context, id string, status Status) error {
	return fs.mutate(id, func(s *Session) { s.Status = status })
}

// UpdateStatusIf sets status to next only when the stored status still equals
// expected. A missing document returns (false, nil) — not an error — so a
// compare-and-swap against an archived/deleted session is a no-op, not a failure.
func (fs *FileStore) UpdateStatusIf(ctx context.Context, id string, expected, next Status) (bool, error) {
	if err := safeID(id); err != nil {
		return false, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := readSession(fs.activePath(id))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if s.Status != expected {
		return false, nil
	}
	s.Status = next
	s.UpdatedAt = time.Now().UTC()
	if err := atomicWriteJSON(fs.activePath(id), s); err != nil {
		return false, err
	}
	return true, nil
}

func (fs *FileStore) UpdateType(ctx context.Context, id string, t Type) error {
	return fs.mutate(id, func(s *Session) { s.Type = t })
}

func (fs *FileStore) UpdateSubject(ctx context.Context, id, subject string) error {
	return fs.mutate(id, func(s *Session) { s.Subject = subject })
}

func (fs *FileStore) UpdatePane(ctx context.Context, id, excerpt string) error {
	return fs.mutate(id, func(s *Session) { s.LastPaneExcerpt = excerpt })
}

func (fs *FileStore) ClearWorktree(ctx context.Context, id string) error {
	return fs.mutate(id, func(s *Session) { s.Worktree = ""; s.Branch = "" })
}

func (fs *FileStore) AppendEvent(ctx context.Context, id string, ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	return fs.mutate(id, func(s *Session) { s.Events = append(s.Events, ev) })
}

func (fs *FileStore) AppendEventStatus(ctx context.Context, id string, ev Event, status Status) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	return fs.mutate(id, func(s *Session) {
		s.Events = append(s.Events, ev)
		if status != "" {
			s.Status = status
		}
	})
}

// Compile-time check that FileStore satisfies the full Store interface.
var _ Store = (*FileStore)(nil)

// Archive moves the session to closed/<id>.json (soft delete). It writes the
// closed copy first and removes the active file second, so a crash between the
// two leaves the session recoverable in active, never lost (at worst it appears
// in both). An existing closed/<id>.json for the same id is overwritten.
func (fs *FileStore) Archive(ctx context.Context, id string) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := readSession(fs.activePath(id))
	if err != nil {
		return err
	}
	if err := atomicWriteJSON(fs.closedPath(id), s); err != nil {
		return err
	}
	return os.Remove(fs.activePath(id))
}

func (fs *FileStore) Delete(ctx context.Context, id string) error {
	if err := safeID(id); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	err := os.Remove(fs.activePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (fs *FileStore) Ping(ctx context.Context) error {
	info, err := os.Stat(fs.sessions)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("filestore: %s is not a directory", fs.sessions)
	}
	return nil
}

func (fs *FileStore) Close(ctx context.Context) error { return nil }
