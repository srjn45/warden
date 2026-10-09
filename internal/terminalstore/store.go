// Package terminalstore persists plain terminal panes independently from agents.
//
// On first open the store copies kind=terminal records from the legacy active
// session collection. A Terminal deliberately stays flat: it only carries the
// terminal identity, its owning project, and the tmux pane that implements it.
package terminalstore

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

	"github.com/srjn45/warden/internal/legacyimport"
	"github.com/srjn45/warden/internal/store"
)

// Status is the process lifecycle of a terminal pane. It is distinct from
// store.Status — terminals have no AI-agent concerns (backend, approval, rate
// limit, context-window) and their lifecycle is simpler.
type Status string

const (
	// StatusRunning means the tmux pane process is alive.
	StatusRunning Status = "running"
	// StatusExited means the pane process ended normally; ExitCode is set.
	StatusExited Status = "exited"
	// StatusOrphaned means the pane disappeared without an observed exit (e.g.
	// after a daemon restart with no process record).
	StatusOrphaned Status = "orphaned"
)

// Terminal is the first-class representation of a plain shell pane.
// It owns its full process identity; AI-agent state (backend, role, context,
// approvals, pipeline/plan back-refs) is never stored here.
type Terminal struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id,omitempty"`
	Name        string    `json:"name,omitempty"`
	TmuxSession string    `json:"tmux_session"`
	Workdir     string    `json:"workdir,omitempty"`
	Shell       string    `json:"shell,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Status      Status    `json:"status,omitempty"`
	ExitCode    *int      `json:"exit_code,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

var (
	ErrNotFound    = errors.New("terminal not found")
	ErrExists      = errors.New("terminal already exists")
	ErrNotTerminal = errors.New("non-terminal sessions cannot be stored as terminals")
)

const importedMarker = ".terminals-from-sessions-imported"

// Store owns the ScrivaDB "terminals" collection at <data>/terminals-db.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

// New opens the terminal collection and imports terminal sessions once. The
// marker is written only after a successful import, so interrupted imports can
// be safely retried from the legacy active collection.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbDir := filepath.Join(dir, "terminals-db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		return nil, err
	}
	db, err := scriva.Open(dbDir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return nil, err
	}
	col, err := db.Collection("terminals")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, col: col}, nil
}

// LegacyImport is the kind=terminal sessions-db -> terminals-db import for the
// migration registry. It is additive and safe to re-run after a crash.
var LegacyImport = legacyimport.Importer{
	Present: func(dir string) (bool, error) {
		legacyDB := filepath.Join(dir, "sessions-db")
		return legacyimport.Exists(legacyDB)
	},
	Import: func(dir string) error {
		legacyDB := filepath.Join(dir, "sessions-db")
		if ok, _ := legacyimport.Exists(legacyDB); !ok {
			return nil
		}
		s, err := New(dir)
		if err != nil {
			return err
		}
		defer s.Close()
		return s.importActiveSessions(legacyDB)
	},
	Verify: func(dir string) error {
		legacyDB := filepath.Join(dir, "sessions-db")
		if ok, _ := legacyimport.Exists(legacyDB); !ok {
			return nil
		}
		s, err := New(dir)
		if err != nil {
			return err
		}
		defer s.Close()
		db, err := scriva.Open(legacyDB, scriva.WithSyncMode(engine.SyncModeNone))
		if err != nil {
			return err
		}
		defer db.Close()
		col, err := db.Collection("active")
		if err != nil {
			return nil
		}
		rows, err := col.Scan(query.MatchAll)
		if err != nil {
			return err
		}
		var missing []string
		for _, row := range rows {
			var session store.Session
			if err := decodeRecord(row.Data, &session); err != nil || !session.IsTerminal() {
				continue
			}
			ok, err := s.col.Exists(session.ID)
			if err != nil {
				return err
			}
			if !ok {
				missing = append(missing, session.ID)
			}
		}
		return legacyimport.VerifyNone("terminals", missing)
	},
}

// sessionToTerminal converts a legacy kind=terminal store.Session to a full
// Terminal record. The ID is preserved so callers referencing the old session
// ID still resolve to the same Terminal after migration.
func sessionToTerminal(s *store.Session) *Terminal {
	return &Terminal{
		ID:          s.ID,
		ProjectID:   s.ProjectID,
		Name:        s.Name,
		TmuxSession: s.TmuxSession,
		Workdir:     s.Workdir,
		PID:         s.PID,
		Status:      mapSessionStatus(s.Status),
		ExitCode:    s.ExitCode,
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

// mapSessionStatus translates a store.Status value to the terminal-specific
// Status enum. Terminal panes never carry rate-limit or AI-context states, so
// those collapse into StatusRunning (the pane process is still alive).
func mapSessionStatus(s store.Status) Status {
	switch s {
	case store.StatusDone, store.StatusErrored:
		return StatusExited
	case store.StatusOrphaned:
		return StatusOrphaned
	default:
		return StatusRunning
	}
}

func (s *Store) importActiveSessions(legacyDB string) error {
	if _, err := os.Stat(legacyDB); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	db, err := scriva.Open(legacyDB, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return err
	}
	defer db.Close()
	active, err := db.Collection("active")
	if err != nil {
		return err
	}
	rows, err := active.Scan(query.MatchAll)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var session store.Session
		if err := decodeRecord(row.Data, &session); err != nil || !session.IsTerminal() {
			continue
		}
		t := sessionToTerminal(&session)
		if err := s.insert(t); err != nil && !errors.Is(err, ErrExists) {
			return err
		}
	}
	return nil
}

func encodeRecord(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var record map[string]any
	if err := json.Unmarshal(b, &record); err != nil {
		return nil, err
	}
	return record, nil
}

func decodeRecord(record map[string]any, v any) error {
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) get(id string) (*Terminal, error) {
	record, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var terminal Terminal
	if err := decodeRecord(record.Data, &terminal); err != nil {
		return nil, err
	}
	return &terminal, nil
}

func (s *Store) insert(terminal *Terminal) error {
	if err := store.SafeID(terminal.ID); err != nil {
		return err
	}
	if ok, err := s.col.Exists(terminal.ID); err != nil {
		return err
	} else if ok {
		return ErrExists
	}
	record, err := encodeRecord(terminal)
	if err != nil {
		return err
	}
	_, _, err = s.col.InsertWithKey(terminal.ID, record)
	if errors.Is(err, engine.ErrDuplicateKey) {
		return ErrExists
	}
	return err
}

// Insert creates a terminal record.
func (s *Store) Insert(ctx context.Context, terminal *Terminal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insert(terminal)
}

// Create reserves a terminal record before its tmux pane is available. It is
// the internal half of Spawn; operator-facing callers should use Spawn.
func (s *Store) Create(ctx context.Context, terminal *Terminal) error { return s.Insert(ctx, terminal) }

// Init persists the tmux identity assigned to a newly-created terminal pane.
func (s *Store) Init(ctx context.Context, id, tmuxSession string) error {
	return s.Update(ctx, id, func(terminal *Terminal) error {
		terminal.TmuxSession = tmuxSession
		return nil
	})
}

// Spawn creates and initializes a terminal as one user-facing operation. A
// failed initialization removes the new record to avoid a stranded terminal.
func (s *Store) Spawn(ctx context.Context, terminal *Terminal, tmuxSession string) error {
	if err := s.Create(ctx, terminal); err != nil {
		return err
	}
	if err := s.Init(ctx, terminal.ID, tmuxSession); err != nil {
		_ = s.Delete(context.Background(), terminal.ID)
		return err
	}
	return nil
}

// Terminate removes the durable terminal record after the lifecycle runner has
// stopped its pane. Unlike AI agents, terminals have no retained done state.
func (s *Store) Terminate(ctx context.Context, id string) error { return s.Delete(ctx, id) }

func (s *Store) Get(ctx context.Context, id string) (*Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := store.SafeID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

// List returns terminals in stable id order. Terminals have no agent state or
// lifecycle timestamps, so no activity ordering is implied.
func (s *Store) List(ctx context.Context) ([]*Terminal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Terminal, 0, len(rows))
	for _, row := range rows {
		var terminal Terminal
		if err := decodeRecord(row.Data, &terminal); err != nil {
			return nil, err
		}
		out = append(out, &terminal)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update atomically applies fn to a terminal record.
func (s *Store) Update(ctx context.Context, id string, fn func(*Terminal) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	terminal, err := s.get(id)
	if err != nil {
		return err
	}
	if err := fn(terminal); err != nil {
		return err
	}
	if terminal.ID != id {
		return errors.New("terminal id cannot change")
	}
	record, err := encodeRecord(terminal)
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(id, record)
	return err
}

// Delete permanently removes a terminal. A missing id returns ErrNotFound.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.col.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *Store) Close() error { return s.db.Close() }
