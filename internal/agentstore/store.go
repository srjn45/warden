// Package agentstore persists AI agents independently from terminal sessions.
//
// The legacy session store remains the home for terminal panes and archived
// session history. On its first open this store copies every non-terminal live
// session from the legacy active collection into its own ScrivaDB collection.
package agentstore

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

	"github.com/srjn45/warden/internal/store"
)

// Agent is the AI-agent projection of the former Session entity. Keeping the
// same underlying shape preserves the wire contract and every field used by the
// poller, recovery, autopilot, pipeline jobs, and hierarchy, while giving the
// agent collection a first-class Go entity to depend on.
type Agent store.Session

func (a *Agent) IsTerminal() bool { return a.Kind == store.KindTerminal }

// HasTag retains Session's normalized tag lookup for callers moving to Agent.
func (a *Agent) HasTag(tag string) bool { return (*store.Session)(a).HasTag(tag) }

var (
	ErrNotFound = errors.New("agent not found")
	ErrExists   = errors.New("agent already exists")
	ErrNotAgent = errors.New("terminal sessions cannot be stored as agents")
	// ErrNotOrphaned prevents recovery from reviving an agent that was not
	// explicitly marked orphaned. Recovery is deliberately narrower than a
	// generic status update: it is the safe repair path after daemon loss.
	ErrNotOrphaned = errors.New("agent is not orphaned")
)

const importedMarker = ".agents-from-sessions-imported"

// Store owns the ScrivaDB "agents" collection at <data>/agents-db.
type Store struct {
	mu  sync.Mutex
	db  *scriva.DB
	col *engine.Collection
}

// New opens the agent collection and, once, imports the legacy active records
// whose Kind is not terminal. The marker is written last, making a failed import
// retryable without duplicating data (the destination is rebuilt first).
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbDir := filepath.Join(dir, "agents-db")
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
	col, err := db.Collection("agents")
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
		if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
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
		a, err := fromRecord(row.Data)
		if err != nil || a.IsTerminal() {
			continue
		}
		rec, err := toRecord(a)
		if err != nil {
			return err
		}
		if _, _, err = s.col.InsertWithKey(a.ID, rec); err != nil && !errors.Is(err, engine.ErrDuplicateKey) {
			return err
		}
	}
	return nil
}

func toRecord(a *Agent) (map[string]any, error) {
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, err
	}
	return rec, nil
}

func fromRecord(rec map[string]any) (*Agent, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	var a Agent
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) get(id string) (*Agent, error) {
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromRecord(r.Data)
}

// Insert creates an agent, initializing its lifecycle timestamps and events.
func (s *Store) Insert(ctx context.Context, a *Agent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.IsTerminal() {
		return ErrNotAgent
	}
	if err := store.SafeID(a.ID); err != nil {
		return err
	}
	if err := store.ValidateName(a.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok, err := s.col.Exists(a.ID); err != nil {
		return err
	} else if ok {
		return ErrExists
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.Status = a.Status.Canonical()
	a.UpdatedAt = now
	if a.Events == nil {
		a.Events = []store.Event{}
	}
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

// Create reserves and persists an agent before its runtime has started. It is
// an internal lifecycle primitive; callers serving an operator request should
// use Spawn so no half-initialized record is exposed on success.
func (s *Store) Create(ctx context.Context, a *Agent) error { return s.Insert(ctx, a) }

// Init records the runtime identity discovered when an agent starts. Tmux and
// the AI CLI use different identifiers, both of which must survive restarts:
// TmuxSession addresses the pane, while ClaudeSessionID pins the CLI resume
// conversation (the field name is retained for wire compatibility).
func (s *Store) Init(ctx context.Context, id, tmuxSession, aiCLISessionID string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.TmuxSession = tmuxSession
		a.ClaudeSessionID = aiCLISessionID
		return nil
	})
}

// Spawn is the user-facing lifecycle entry point. It creates the durable agent
// row and initializes its runtime identities as one logical operation. Should
// initialization fail, the newly-created row is removed so callers do not see
// an agent that cannot be addressed or resumed.
func (s *Store) Spawn(ctx context.Context, a *Agent, tmuxSession, aiCLISessionID string) error {
	if err := s.Create(ctx, a); err != nil {
		return err
	}
	if err := s.Init(ctx, a.ID, tmuxSession, aiCLISessionID); err != nil {
		_ = s.Delete(context.Background(), a.ID)
		return err
	}
	return nil
}

// Terminate marks an agent terminal after its tmux process has been stopped by
// the lifecycle runner. Process management stays outside this persistence
// package; this method owns only the durable state transition.
func (s *Store) Terminate(ctx context.Context, id string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.Status = store.StatusDone
		return nil
	})
}

// Recover revives only an orphaned agent. Done, idle, and active agents are
// never valid recovery inputs: allowing them would turn an ordinary operator
// action into an accidental restart.
func (s *Store) Recover(ctx context.Context, id string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		if a.Status.Canonical() != store.StatusOrphaned {
			return ErrNotOrphaned
		}
		a.Status = store.StatusWorking
		return nil
	})
}

func (s *Store) Get(ctx context.Context, id string) (*Agent, error) {
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

// List returns all agents newest-updated first.
func (s *Store) List(ctx context.Context) ([]*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.col.Scan(query.MatchAll)
	if err != nil {
		return nil, err
	}
	out := make([]*Agent, 0, len(rows))
	for _, row := range rows {
		a, err := fromRecord(row.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Update atomically applies fn and stamps UpdatedAt.
func (s *Store) Update(ctx context.Context, id string, fn func(*Agent) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.get(id)
	if err != nil {
		return err
	}
	if err := fn(a); err != nil {
		return err
	}
	a.Status = a.Status.Canonical()
	a.UpdatedAt = time.Now().UTC()
	rec, err := toRecord(a)
	if err != nil {
		return err
	}
	_, err = s.col.UpdateByKey(id, rec)
	return err
}

// Delete permanently removes an agent. A missing id returns ErrNotFound.
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
