package schedule

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

var (
	ErrNotFound = errors.New("schedule not found")
	ErrExists   = errors.New("schedule already exists")
)

// Store persists all schedules as a single JSON file (schedules.json), rewritten
// atomically under a mutex. Schedules are few and low-frequency, so one file
// (rather than one-file-per-record like the pipeline store) keeps the daemon's
// once-a-minute load/save trivial.
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore opens (creating the parent dir if needed) the schedules file at path.
func NewStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{path: path}, nil
}

// readAll loads the schedule map keyed by id ({} when the file is absent). The
// caller holds mu.
func (s *Store) readAll() (map[string]*Schedule, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*Schedule{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]*Schedule{}
	if len(data) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// writeAll atomically rewrites the file from m (temp file + rename). The caller
// holds mu.
func (s *Store) writeAll(m map[string]*Schedule) error {
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

// Create persists a new schedule, returning ErrExists if its id is taken.
func (s *Store) Create(sc *Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readAll()
	if err != nil {
		return err
	}
	if _, ok := m[sc.ID]; ok {
		return ErrExists
	}
	m[sc.ID] = sc
	return s.writeAll(m)
}

// Get returns one schedule by id, or ErrNotFound.
func (s *Store) Get(id string) (*Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readAll()
	if err != nil {
		return nil, err
	}
	sc, ok := m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return sc, nil
}

// List returns all schedules sorted by id.
func (s *Store) List() ([]*Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readAll()
	if err != nil {
		return nil, err
	}
	out := make([]*Schedule, 0, len(m))
	for _, sc := range m {
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update applies fn to the stored schedule under the lock and writes it back.
func (s *Store) Update(id string, fn func(*Schedule)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readAll()
	if err != nil {
		return err
	}
	sc, ok := m[id]
	if !ok {
		return ErrNotFound
	}
	fn(sc)
	return s.writeAll(m)
}

// Delete removes a schedule by id, returning ErrNotFound if absent.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.readAll()
	if err != nil {
		return err
	}
	if _, ok := m[id]; !ok {
		return ErrNotFound
	}
	delete(m, id)
	return s.writeAll(m)
}
