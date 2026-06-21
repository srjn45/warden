// Package ctxstore is a daemon-owned, namespaced key/value store that agents
// read and write to share results and state (the "shared context" / blackboard).
// Keys are free-form dot-namespaced strings (e.g. "global.foo",
// "pipeline.<pid>.<job>.output"). All entries live in one JSON file under the
// data dir, rewritten atomically (temp file + rename) on each mutation — this is
// a localhost session store, not a database; the last write surviving a crash is
// not a requirement, but a reader never observes a torn file.
package ctxstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("context key not found")

// ErrBadKey is returned for an empty/blank key or one containing a path
// separator.
var ErrBadKey = errors.New("invalid context key")

// ErrConflict is returned by CompareAndSet when the current value does not match
// the caller's expected value — another writer won the race.
var ErrConflict = errors.New("context value conflict")

// validKey rejects empty/blank keys and keys containing a path separator. Keys
// are dot-namespaced strings that travel through a URL path segment; a "/" is
// not decoded back by the router, so it would be stored under a corrupted key
// and break prefix operations (e.g. pipeline-scoped cleanup). Reject it at the
// single write gate.
func validKey(key string) bool {
	if strings.TrimSpace(key) == "" {
		return false
	}
	return !strings.ContainsAny(key, `/\`)
}

// Entry is one stored value plus its provenance.
type Entry struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store persists all entries in one JSON file, serialized by an RWMutex.
type Store struct {
	mu   sync.RWMutex
	path string
}

// New creates dir (if needed) and returns a store writing to <dir>/context.json.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{path: filepath.Join(dir, "context.json")}, nil
}

// load reads the whole map; a missing file is an empty map, not an error.
func (s *Store) load() (map[string]Entry, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]Entry{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// save writes the whole map via temp file + rename so readers never see a
// partial write.
func (s *Store) save(m map[string]Entry) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// Set writes value at key, recording the writer (by) and current time.
func (s *Store) Set(key, value, by string) (Entry, error) {
	if !validKey(key) {
		return Entry{}, ErrBadKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: time.Now().UTC()}
	m[key] = e
	if err := s.save(m); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// CompareAndSet writes value at key only if the current value equals expected
// (expected "" means "the key must be absent"). On a mismatch it makes no change
// and returns ErrConflict, so an agent doing read-modify-write on the shared
// blackboard can re-read and retry instead of silently losing a concurrent
// writer's update — the atomic alternative to Get-then-Set.
func (s *Store) CompareAndSet(key, expected, value, by string) (Entry, error) {
	if !validKey(key) {
		return Entry{}, ErrBadKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	cur, ok := m[key]
	if (!ok && expected != "") || (ok && cur.Value != expected) {
		return Entry{}, ErrConflict
	}
	e := Entry{Key: key, Value: value, UpdatedBy: by, UpdatedAt: time.Now().UTC()}
	m[key] = e
	if err := s.save(m); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Append atomically sets key to its current value + sep + value, creating the
// key (with no leading sep) when absent. It is the race-free form of the common
// "accumulate into a shared key" pattern that a Get-then-Set would corrupt under
// concurrent writers.
func (s *Store) Append(key, value, sep, by string) (Entry, error) {
	if !validKey(key) {
		return Entry{}, ErrBadKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	next := value
	if cur, ok := m[key]; ok {
		next = cur.Value + sep + value
	}
	e := Entry{Key: key, Value: next, UpdatedBy: by, UpdatedAt: time.Now().UTC()}
	m[key] = e
	if err := s.save(m); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// Get returns the entry at key, or ErrNotFound.
func (s *Store) Get(key string) (Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.load()
	if err != nil {
		return Entry{}, err
	}
	e, ok := m[key]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// List returns all entries whose key starts with prefix (empty = all), sorted
// by key. Always returns a non-nil slice.
func (s *Store) List(prefix string) ([]Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for k, e := range m {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// DelPrefix removes every key starting with prefix and returns how many were
// removed. An empty prefix is rejected (ErrBadKey) so a caller can never wipe
// the whole store by accident — this backs pipeline-scoped cleanup, where the
// caller passes "pipeline.<id>." (trailing dot included so "p1" can't match
// "p10"). A no-match prefix is not an error; it returns (0, nil).
func (s *Store) DelPrefix(prefix string) (int, error) {
	if prefix == "" {
		return 0, ErrBadKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return 0, err
	}
	n := 0
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			delete(m, k)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.save(m); err != nil {
		return 0, err
	}
	return n, nil
}

// Del removes key, returning ErrNotFound if it was absent.
func (s *Store) Del(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := m[key]; !ok {
		return ErrNotFound
	}
	delete(m, key)
	return s.save(m)
}
