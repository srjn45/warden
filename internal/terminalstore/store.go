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

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/store"
)

// Terminal is the flat, first-class representation of a plain shell pane.
// Agent-specific state such as prompts, hierarchy, and backend metadata does
// not belong here.
type Terminal struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id,omitempty"`
	TmuxSession string `json:"tmux_session"`
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
	marker := filepath.Join(dir, importedMarker)
	_, err := os.Stat(marker)
	imported := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !imported {
		if err := os.RemoveAll(dbDir); err != nil {
			return nil, err
		}
	}
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
	s := &Store{db: db, col: col}
	if !imported {
		if err := s.importActiveSessions(filepath.Join(dir, "sessions-db")); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := os.WriteFile(marker, []byte("imported\n"), 0o600); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
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
		if err := s.insert(&Terminal{ID: session.ID, ProjectID: session.ProjectID, TmuxSession: session.TmuxSession}); err != nil && !errors.Is(err, ErrExists) {
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
