package autopilotstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

// Store owns the ScrivaDB "autopilots" collection at <data>/autopilots-db.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

// New opens (creating if needed) the live Autopilot store at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbDir := filepath.Join(dir, "autopilots-db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("autopilots")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

func toRecord(a *Autopilot) (map[string]any, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(b, &out)
	return out, err
}

func fromRecord(m map[string]any) (*Autopilot, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out Autopilot
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Create persists a new live Autopilot. PlanID is required.
func (s *Store) Create(ctx context.Context, a *Autopilot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateCreate(a); err != nil {
		return err
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	}
	if a.Name == "" {
		a.Name = DisplayName("")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := toRecord(a)
	if err != nil {
		return err
	}
	_, _, err = s.col.InsertWithKey(a.ID, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return ErrExists
	}
	return err
}

func (s *Store) get(id string) (*Autopilot, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRecord(r.Data)
}

// Get returns one Autopilot by ID.
func (s *Store) Get(ctx context.Context, id string) (*Autopilot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// List returns all live Autopilots ordered by ID.
func (s *Store) List(ctx context.Context) ([]*Autopilot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Autopilot, 0, len(rows))
	for _, row := range rows {
		a, err := fromRecord(row.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListByProject returns live Autopilots for one project, ordered by ID.
func (s *Store) ListByProject(ctx context.Context, projectID string) ([]*Autopilot, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Autopilot, 0)
	for _, a := range all {
		if a.ProjectID == projectID {
			out = append(out, a)
		}
	}
	return out, nil
}

// ListByPlan returns live Autopilots bound to planID, ordered by ID.
func (s *Store) ListByPlan(ctx context.Context, planID string) ([]*Autopilot, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Autopilot, 0)
	for _, a := range all {
		if a.PlanID == planID {
			out = append(out, a)
		}
	}
	return out, nil
}

// Update applies fn to the Autopilot identified by id (RMW).
func (s *Store) Update(ctx context.Context, id string, fn func(*Autopilot) error) (*Autopilot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(id)
	if err != nil {
		return nil, err
	}
	if err := fn(a); err != nil {
		return nil, err
	}
	// PlanID remains required after update.
	if err := ValidateCreate(a); err != nil {
		return nil, err
	}
	a.UpdatedAt = time.Now().UTC()
	rec, err := toRecord(a)
	if err != nil {
		return nil, err
	}
	if _, err = s.col.UpdateByKey(id, rec); err != nil {
		return nil, err
	}
	return a, nil
}

// Delete removes a live Autopilot. Missing IDs are a no-op (idempotent teardown).
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.col.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil
	}
	return err
}

// Close releases the ScrivaDB handle.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}
