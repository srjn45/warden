package decisionrouter

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

// MemoryLearnedShapeStore implements LearnedShapeStore in-memory with thread safety.
type MemoryLearnedShapeStore struct {
	mu     sync.RWMutex
	shapes map[string]ptyevent.LearnedShape // key: id
	byFP   map[string]string                // fingerprint -> id
}

// NewMemoryLearnedShapeStore returns a new empty MemoryLearnedShapeStore.
func NewMemoryLearnedShapeStore() *MemoryLearnedShapeStore {
	return &MemoryLearnedShapeStore{
		shapes: make(map[string]ptyevent.LearnedShape),
		byFP:   make(map[string]string),
	}
}

// Get retrieves a shape by ID.
func (s *MemoryLearnedShapeStore) Get(id string) (ptyevent.LearnedShape, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.shapes[id]
	return sh, ok
}

// GetByFingerprint retrieves a shape by Fingerprint.
func (s *MemoryLearnedShapeStore) GetByFingerprint(fp string) (ptyevent.LearnedShape, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byFP[fp]
	if !ok {
		return ptyevent.LearnedShape{}, false
	}
	sh, ok := s.shapes[id]
	return sh, ok
}

// AddCandidate registers a candidate learned shape, or updates an existing candidate's observations.
func (s *MemoryLearnedShapeStore) AddCandidate(shape ptyevent.LearnedShape) (ptyevent.LearnedShape, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if shape.Fingerprint == "" {
		return shape, errors.New("missing fingerprint")
	}

	if existingID, ok := s.byFP[shape.Fingerprint]; ok {
		existing := s.shapes[existingID]
		if existing.Status == ptyevent.ShapeCandidate {
			existing.Observations++
			s.shapes[existingID] = existing
			return existing, nil
		}
		// If existing is already validated/versioned, return it
		return existing, nil
	}

	if shape.ID == "" {
		shape.ID = fmt.Sprintf("shape-%s", shape.Fingerprint)
	}
	shape.Status = ptyevent.ShapeCandidate
	if shape.Observations == 0 {
		shape.Observations = 1
	}
	if shape.CreatedAt.IsZero() {
		shape.CreatedAt = time.Now()
	}

	s.shapes[shape.ID] = shape
	s.byFP[shape.Fingerprint] = shape.ID
	return shape, nil
}

// RecordObservation records a new observation for a fingerprint with regression fixture and confirmation.
func (s *MemoryLearnedShapeStore) RecordObservation(fp string, fixtureID string, humanConfirmed, parserAgreed bool) (ptyevent.LearnedShape, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, ok := s.byFP[fp]
	if !ok {
		return ptyevent.LearnedShape{}, fmt.Errorf("no shape found for fingerprint %s", fp)
	}

	sh := s.shapes[id]
	sh.Observations++
	if humanConfirmed {
		sh.HumanConfirmed = true
	}
	if parserAgreed {
		sh.ParserAgreed = true
	}
	if fixtureID != "" {
		hasFix := false
		for _, f := range sh.Fixtures {
			if f == fixtureID {
				hasFix = true
				break
			}
		}
		if !hasFix {
			sh.Fixtures = append(sh.Fixtures, fixtureID)
		}
	}
	s.shapes[id] = sh
	return sh, nil
}

// Validate promotes candidate -> validated.
func (s *MemoryLearnedShapeStore) Validate(id string, now time.Time, pol ptyevent.ShapePolicy) (ptyevent.LearnedShape, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sh, ok := s.shapes[id]
	if !ok {
		return ptyevent.LearnedShape{}, fmt.Errorf("shape %s not found", id)
	}

	val, err := sh.Validate(now, pol)
	if err != nil {
		return sh, err
	}
	s.shapes[id] = val
	return val, nil
}

// Pin promotes validated -> versioned (or renews an expired versioned shape).
func (s *MemoryLearnedShapeStore) Pin(id string, now time.Time, cliVersion string, kind ptyevent.ShapeScopeKind, scopeID string, ttl time.Duration, pol ptyevent.ShapePolicy) (ptyevent.LearnedShape, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sh, ok := s.shapes[id]
	if !ok {
		return ptyevent.LearnedShape{}, fmt.Errorf("shape %s not found", id)
	}

	ver, err := sh.Pin(now, cliVersion, kind, scopeID, ttl, pol)
	if err != nil {
		return sh, err
	}
	s.shapes[id] = ver
	return ver, nil
}

// Retire marks a shape as retired (struck).
func (s *MemoryLearnedShapeStore) Retire(id string, now time.Time, reason string) (ptyevent.LearnedShape, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sh, ok := s.shapes[id]
	if !ok {
		return ptyevent.LearnedShape{}, fmt.Errorf("shape %s not found", id)
	}

	ret := sh.Retire(now, reason)
	s.shapes[id] = ret
	return ret, nil
}

// FindUsable finds all versioned shapes that are usable for query q.
func (s *MemoryLearnedShapeStore) FindUsable(q ptyevent.ShapeQuery) []ptyevent.LearnedShape {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var usable []ptyevent.LearnedShape
	for _, sh := range s.shapes {
		if sh.Usable(q) {
			usable = append(usable, sh)
		}
	}
	return usable
}
