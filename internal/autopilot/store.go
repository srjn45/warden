package autopilot

import (
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

var (
	ErrRunNotFound = errors.New("autopilot run not found")
	ErrRunExists   = errors.New("autopilot run already exists")
)

// RunRecord is the durable lifecycle authority for an autopilot run. Runtime
// fields on Plan remain mirrors used for recovery and owner-facing writeback.
type RunRecord struct {
	RunID             string    `json:"run_id"`
	Name              string    `json:"name"`
	Repo              string    `json:"repo"`
	PlanFile          string    `json:"plan_file"`
	State             RunState  `json:"state"`
	IntegrationBranch string    `json:"integration_branch,omitempty"`
	Gate              string    `json:"gate,omitempty"`
	Strategy          string    `json:"strategy,omitempty"`
	DeleteBranch      bool      `json:"delete_branch,omitempty"`
	BrainID           string    `json:"brain_id,omitempty"`
	GuardianID        string    `json:"guardian_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// RunStore serializes compound read-modify-write operations over the single
// autopilot_runs Scriva collection.
type RunStore struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

func NewRunStore(dataDir string) (*RunStore, error) {
	dir := filepath.Join(dataDir, "autopilot", "runs-db")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("autopilot_runs")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &RunStore{db: db, col: col}, nil
}

func runToRecord(r RunRecord) (map[string]any, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(b, &out)
	return out, err
}

func runFromRecord(m map[string]any) (RunRecord, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return RunRecord{}, err
	}
	var out RunRecord
	err = json.Unmarshal(b, &out)
	return out, err
}

func (s *RunStore) Create(r RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := runToRecord(r)
	if err != nil {
		return err
	}
	_, _, err = s.col.InsertWithKey(r.RunID, rec)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return ErrRunExists
	}
	return err
}

func (s *RunStore) get(id string) (RunRecord, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return RunRecord{}, ErrRunNotFound
	}
	if err != nil {
		return RunRecord{}, err
	}
	return runFromRecord(r.Data)
}

func (s *RunStore) Get(id string) (RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

func (s *RunStore) List() ([]RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, 0, len(rows))
	for _, row := range rows {
		r, err := runFromRecord(row.Data)
		if err == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

func (s *RunStore) Update(id string, fn func(*RunRecord) error) (RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.get(id)
	if err != nil {
		return RunRecord{}, err
	}
	if err := fn(&r); err != nil {
		return RunRecord{}, err
	}
	rec, err := runToRecord(r)
	if err != nil {
		return RunRecord{}, err
	}
	if _, err = s.col.UpdateByKey(id, rec); err != nil {
		return RunRecord{}, err
	}
	return r, nil
}

func (s *RunStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.col.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil
	}
	return err
}

func (s *RunStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}
