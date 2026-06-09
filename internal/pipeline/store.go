package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/srjn45/warden/internal/store"
)

var (
	ErrNotFound = errors.New("pipeline not found")
	ErrExists   = errors.New("pipeline already exists")
)

// Store persists each pipeline as one JSON file (<dir>/<id>.json), mutated
// atomically under a mutex — mirrors internal/ctxstore.
type Store struct {
	mu  sync.Mutex
	dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) (string, error) {
	if err := store.SafeID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s *Store) read(path string) (*Pipeline, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Pipeline
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) write(path string, p *Pipeline) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
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
	return os.Rename(tmpName, path)
}

func (s *Store) Create(p *Pipeline) error {
	path, err := s.path(p.ID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.write(path, p)
}

func (s *Store) Get(id string) (*Pipeline, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(path)
}

func (s *Store) List() ([]*Pipeline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []*Pipeline{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p, err := s.read(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update applies fn to the stored pipeline under the lock and writes it back.
func (s *Store) Update(id string, fn func(*Pipeline)) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.read(path)
	if err != nil {
		return err
	}
	fn(p)
	return s.write(path, p)
}

// Delete removes a pipeline's record file, returning ErrNotFound if absent.
func (s *Store) Delete(id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return os.Remove(path)
}
