package planexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

var (
	// ErrRecordNotFound is returned when no export record matches the key.
	ErrRecordNotFound = errors.New("plan export record not found")
	// ErrRecordExists is returned when Create is called for an existing key.
	ErrRecordExists = errors.New("plan export record already exists")
)

// RecordStore persists per-repository export records. Implementations must be
// safe for concurrent use.
type RecordStore interface {
	Upsert(ctx context.Context, rec *Record) error
	Get(ctx context.Context, id string) (*Record, error)
	Find(ctx context.Context, planID, repository, targetRef, outputPath string) (*Record, error)
	ListByPlan(ctx context.Context, planID string) ([]*Record, error)
	Close() error
}

// Store is the ScrivaDB-backed RecordStore at <dir>/plan-exports-db.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

var _ RecordStore = (*Store)(nil)

// NewStore opens (creating if needed) the plan-exports ScrivaDB under dir.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbDir := filepath.Join(dir, "plan-exports-db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("plan-exports")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

// Close releases the underlying database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Upsert inserts or replaces a record. ID is derived from the key tuple when empty.
func (s *Store) Upsert(ctx context.Context, rec *Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("planexport: record is nil")
	}
	if rec.PlanID == "" || rec.Repository == "" || rec.TargetRef == "" || rec.OutputPath == "" {
		return fmt.Errorf("planexport: plan_id, repository, target_ref, and output_path are required")
	}
	if !rec.Outcome.Valid() {
		return fmt.Errorf("planexport: invalid outcome %q", rec.Outcome)
	}
	if rec.ID == "" {
		rec.ID = RecordID(rec.PlanID, rec.Repository, rec.TargetRef, rec.OutputPath)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if rec.ExportedAt.IsZero() {
		rec.ExportedAt = now
	} else {
		rec.ExportedAt = rec.ExportedAt.UTC().Truncate(time.Second)
	}
	rec.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := encodeExportRecord(rec)
	if err != nil {
		return err
	}
	_, err = s.col.GetByKey(rec.ID)
	if errors.Is(err, engine.ErrKeyNotFound) {
		_, _, err = s.col.InsertWithKey(rec.ID, data)
		return err
	}
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(rec.ID, data)
	return err
}

// Get returns the record with the given ID.
func (s *Store) Get(ctx context.Context, id string) (*Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// Find looks up the record for a (plan, repo, ref, path) key.
func (s *Store) Find(ctx context.Context, planID, repository, targetRef, outputPath string) (*Record, error) {
	return s.Get(ctx, RecordID(planID, repository, targetRef, outputPath))
}

// ListByPlan returns all export records for planID, newest UpdatedAt first.
func (s *Store) ListByPlan(ctx context.Context, planID string) ([]*Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0)
	for _, row := range rows {
		rec, err := decodeExportRecord(row.Data)
		if err != nil {
			return nil, err
		}
		if rec.PlanID == planID {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

func (s *Store) get(id string) (*Record, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeExportRecord(r.Data)
}

func encodeExportRecord(rec *Record) (map[string]any, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func decodeExportRecord(data map[string]any) (*Record, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}
