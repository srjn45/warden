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

// ErrBadKey is returned when a key is empty/blank.
var ErrBadKey = errors.New("invalid context key")

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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	if strings.TrimSpace(key) == "" {
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
