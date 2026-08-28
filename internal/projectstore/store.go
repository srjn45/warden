// Package projectstore persists first-class projects (docs/specs/
// 2026-08-28-project-centric-ui.md Phase 1): one record per project in an embedded
// ScrivaDB "projects" collection, keyed by the project's canonical ID (the main
// checkout's local path or the remote URL).
//
// A project is the parent that agents (store.Session) and pipelines
// (pipeline.Pipeline) group under via their optional ProjectID back-ref. Worktrees
// are deliberately NOT projects: an agent in a worktree links to its parent
// project's ID and keeps its own worktree path on the session record, so a repo
// has exactly one project regardless of how many worktrees it fans out into.
//
// Closing a project never deletes it (IDE-like hibernation): CloseProject only
// flips Status to closed; OpenProject flips it back (and upserts a new project).
package projectstore

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"
)

var (
	// ErrNotFound is the store-boundary translation of engine.ErrKeyNotFound.
	ErrNotFound = errors.New("project not found")
	// ErrExists is the store-boundary translation of engine.ErrDuplicateKey.
	ErrExists = errors.New("project already exists")
	// ErrInvalidID is returned when a project id is empty.
	ErrInvalidID = errors.New("project id cannot be empty")
)

// Store persists projects as records in an embedded ScrivaDB "projects"
// collection, one record per project keyed by its ID. Opened SyncModeNone: this
// is a localhost daemon store, so last-write-survives-power-loss is not required
// (append-only segments rule out torn reads regardless) — mirrors backendstore.
//
// A single mutex serialises the read-modify-write methods (OpenProject /
// CloseProject) and the exists-check-then-write in Upsert; ScrivaDB does its own
// per-collection locking, so the store mutex only guards the read-then-write
// critical sections. Read-only methods take it too for a behaviour-identical
// mutex model.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
	now func() time.Time // injectable clock for tests; defaults to time.Now
}

// NewStore opens (creating if needed) the ScrivaDB-backed project store at dir
// (~/.warden/projects). This is a fresh store: there is no legacy JSON to import.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("projects")
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, col: col, now: time.Now}, nil
}

// toRecord decomposes v into a ScrivaDB record body via a JSON round-trip, so its
// fields stay real in the store rather than an opaque blob. The engine stamps the
// reserved key field on InsertWithKey/UpdateByKey, so it must NOT be present here.
func toRecord(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// projectFromRecord reconstructs a Project from a record body. The reserved key
// field the engine stamped into the map is harmlessly dropped on unmarshal
// (Project has no matching json tag beyond "id", which round-trips identically).
func projectFromRecord(d map[string]any) (Project, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return Project{}, err
	}
	var out Project
	if err := json.Unmarshal(b, &out); err != nil {
		return Project{}, err
	}
	return out, nil
}

// List returns every project sorted by name (then id, for stable ordering when
// names collide). An undecodable record is skipped rather than failing the scan.
func (s *Store) List() ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list()
}

// list is the unlocked body of List; callers hold s.mu.
func (s *Store) list() ([]Project, error) {
	results, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]Project, 0, len(results))
	for _, r := range results {
		p, err := projectFromRecord(r.Data)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Get returns one project by id, or ErrNotFound.
func (s *Store) Get(id string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// get reads the project keyed id, mapping a key miss to ErrNotFound. The caller
// holds s.mu.
func (s *Store) get(id string) (Project, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	return projectFromRecord(r.Data)
}

// Upsert inserts p or updates it in place, keyed by p.ID. Status is normalized
// (empty ⇒ open) and timestamps are stamped: CreatedAt is preserved from the
// existing row on update, UpdatedAt is always refreshed.
func (s *Store) Upsert(p Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsert(p)
}

// upsert is the unlocked body of Upsert; callers hold s.mu.
func (s *Store) upsert(p Project) error {
	if p.ID == "" {
		return ErrInvalidID
	}
	p.Status = NormalizeStatus(p.Status)
	now := s.now()
	existing, err := s.col.GetByKey(p.ID)
	switch {
	case errors.Is(err, engine.ErrKeyNotFound):
		if p.CreatedAt.IsZero() {
			p.CreatedAt = now
		}
		p.UpdatedAt = now
		rec, rerr := toRecord(p)
		if rerr != nil {
			return rerr
		}
		if _, _, ierr := s.col.InsertWithKey(p.ID, rec); ierr != nil {
			if errors.Is(ierr, engine.ErrDuplicateKey) {
				return ErrExists
			}
			return ierr
		}
		return nil
	case err != nil:
		return err
	default:
		// Preserve the original CreatedAt across updates.
		if prev, perr := projectFromRecord(existing.Data); perr == nil && !prev.CreatedAt.IsZero() {
			p.CreatedAt = prev.CreatedAt
		} else if p.CreatedAt.IsZero() {
			p.CreatedAt = now
		}
		p.UpdatedAt = now
		rec, rerr := toRecord(p)
		if rerr != nil {
			return rerr
		}
		_, uerr := s.col.UpdateByKey(p.ID, rec)
		return uerr
	}
}

// OpenProject registers a project and marks it Open (RMW). It is the store side of
// the "Open Project" operation: a first open inserts the row; reopening a closed
// project flips Status back to open. A non-empty name/path overwrites the stored
// value; an empty name/path leaves the existing value intact (so reopening by id
// alone does not blank the display name). Returns the resulting project.
func (s *Store) OpenProject(id, name, path string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		return Project{}, ErrInvalidID
	}
	p, err := s.get(id)
	if errors.Is(err, ErrNotFound) {
		p = Project{ID: id, Name: name, Path: path}
	} else if err != nil {
		return Project{}, err
	} else {
		if name != "" {
			p.Name = name
		}
		if path != "" {
			p.Path = path
		}
	}
	p.Status = StatusOpen
	if err := s.upsert(p); err != nil {
		return Project{}, err
	}
	return s.get(id)
}

// CloseProject marks a project Closed (RMW hibernation): the row is kept, only
// Status flips. Returns ErrNotFound if id is absent, else the updated project.
func (s *Store) CloseProject(id string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.get(id)
	if err != nil {
		return Project{}, err
	}
	p.Status = StatusClosed
	if err := s.upsert(p); err != nil {
		return Project{}, err
	}
	return s.get(id)
}

// Delete hard-removes the project row keyed id. A missing row is not an error
// (idempotent). Close (hibernation) is the normal path; Delete is a genuine purge.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.col.DeleteByKey(id); err != nil && !errors.Is(err, engine.ErrKeyNotFound) {
		return err
	}
	return nil
}

// Close flushes the ScrivaDB index and stops its background compaction goroutine.
// This shuts down the DB — it is unrelated to CloseProject (which hibernates one
// project row).
func (s *Store) Close() error {
	return s.db.Close()
}
