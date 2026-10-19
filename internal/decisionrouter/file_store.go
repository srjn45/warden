package decisionrouter

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

// FileLearnedShapeStore is a MemoryLearnedShapeStore that survives a daemon
// restart: every mutation rewrites one JSON file atomically (0600). A missing
// file is an empty store; a corrupt one is an error rather than a silent reset,
// because dropping retirements would resurrect struck shapes.
type FileLearnedShapeStore struct {
	*MemoryLearnedShapeStore
	path string
}

// OpenFileLearnedShapeStore loads path (creating an empty store if absent).
func OpenFileLearnedShapeStore(path string) (*FileLearnedShapeStore, error) {
	s := &FileLearnedShapeStore{MemoryLearnedShapeStore: NewMemoryLearnedShapeStore(), path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var shapes []ptyevent.LearnedShape
	if err := json.Unmarshal(b, &shapes); err != nil {
		return nil, err
	}
	for _, sh := range shapes {
		s.shapes[sh.ID] = sh
		s.byFP[sh.Fingerprint] = sh.ID
	}
	return s, nil
}

func (s *FileLearnedShapeStore) persist() error {
	s.mu.RLock()
	shapes := make([]ptyevent.LearnedShape, 0, len(s.shapes))
	for _, sh := range s.shapes {
		shapes = append(shapes, sh)
	}
	s.mu.RUnlock()
	sort.Slice(shapes, func(i, j int) bool { return shapes[i].ID < shapes[j].ID })
	b, err := json.MarshalIndent(shapes, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path, append(b, '\n'))
}

// AddCandidate implements LearnedShapeStore.
func (s *FileLearnedShapeStore) AddCandidate(sh ptyevent.LearnedShape) (ptyevent.LearnedShape, error) {
	out, err := s.MemoryLearnedShapeStore.AddCandidate(sh)
	if err != nil {
		return out, err
	}
	return out, s.persist()
}

// RecordObservation implements LearnedShapeStore.
func (s *FileLearnedShapeStore) RecordObservation(fp, fixtureID string, humanConfirmed, parserAgreed bool) (ptyevent.LearnedShape, error) {
	out, err := s.MemoryLearnedShapeStore.RecordObservation(fp, fixtureID, humanConfirmed, parserAgreed)
	if err != nil {
		return out, err
	}
	return out, s.persist()
}

// AttachFixture implements LearnedShapeStore.
func (s *FileLearnedShapeStore) AttachFixture(fp, fixtureID string) (ptyevent.LearnedShape, error) {
	out, err := s.MemoryLearnedShapeStore.AttachFixture(fp, fixtureID)
	if err != nil {
		return out, err
	}
	return out, s.persist()
}

// Validate implements LearnedShapeStore.
func (s *FileLearnedShapeStore) Validate(id string, now time.Time, pol ptyevent.ShapePolicy) (ptyevent.LearnedShape, error) {
	out, err := s.MemoryLearnedShapeStore.Validate(id, now, pol)
	if err != nil {
		return out, err
	}
	return out, s.persist()
}

// Pin implements LearnedShapeStore.
func (s *FileLearnedShapeStore) Pin(id string, now time.Time, cliVersion string, kind ptyevent.ShapeScopeKind, scopeID string, ttl time.Duration, pol ptyevent.ShapePolicy) (ptyevent.LearnedShape, error) {
	out, err := s.MemoryLearnedShapeStore.Pin(id, now, cliVersion, kind, scopeID, ttl, pol)
	if err != nil {
		return out, err
	}
	return out, s.persist()
}

// Retire implements LearnedShapeStore.
func (s *FileLearnedShapeStore) Retire(id string, now time.Time, reason string) (ptyevent.LearnedShape, error) {
	out, err := s.MemoryLearnedShapeStore.Retire(id, now, reason)
	if err != nil {
		return out, err
	}
	return out, s.persist()
}
