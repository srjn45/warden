// Package agentstore persists AI agents independently from terminal sessions.
//
// Legacy active and archived AI records are imported once; terminals belong
// exclusively to terminalstore.
package agentstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/store"
)

var (
	ErrNotFound    = errors.New("agent not found")
	ErrExists      = errors.New("agent already exists")
	ErrNameExists  = store.ErrNameExists
	ErrInvalidName = store.ErrInvalidName
	// ErrNotOrphaned prevents recovery from reviving an agent that was not
	// explicitly marked orphaned. Recovery is deliberately narrower than a
	// generic status update: it is the safe repair path after daemon loss.
	ErrNotOrphaned = errors.New("agent is not orphaned")
)

const importedMarker = ".agents-from-sessions-imported"
const closedImportedMarker = ".archived-agents-from-sessions-imported"

// Store owns the ScrivaDB "agents" and "closed" collections at <data>/agents-db.
type Store struct {
	mu          sync.Mutex
	snap        atomic.Pointer[Snapshot]
	db          *scriva.DB
	col         *engine.Collection
	closed      *engine.Collection
	lock        *flockFile
	preflight   *UnhealthyError
	scratch     string // throwaway copy of a damaged agents-db; removed on Close
	closedState atomic.Bool
}

var _ AgentStore = (*Store)(nil)

// Snapshot returns the current immutable published snapshot.
func (s *Store) Snapshot() *Snapshot {
	return s.snap.Load()
}

// New opens the agent collection and, once, imports the legacy active and closed
// records whose Kind is not terminal. The marker is written last, making a failed
// import retryable without duplicating data (the destination is rebuilt first).
//
// New takes the exclusive agent-store ownership lock before any import, wipe or
// open and holds it until Close; a second opener gets *OwnershipError.
func New(dir string) (*Store, error) {
	lock, canon, err := acquireOwnership(dir)
	if err != nil {
		return nil, err
	}
	legacy, err := acquireLegacyRead(canon)
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	defer func() { _ = legacy.release() }()
	// Verify the persisted agent database before opening it. ScrivaDB may rebuild
	// derived indexes at open, but that must not turn an already-damaged or
	// missing index into a silently healthy fleet from Warden's perspective.
	var preflight *UnhealthyError
	_, markErr := os.Stat(filepath.Join(canon, importedMarker))
	if _, statErr := os.Stat(filepath.Join(canon, "agents-db")); statErr == nil && markErr == nil {
		if rep, verifyErr := VerifyAgentStore(context.Background(), canon); verifyErr != nil {
			preflight = readFailure("agents", "", verifyErr)
		} else if failures := ReportFailures(rep); len(failures) > 0 {
			preflight = newUnhealthy(failures...)
		}
	}
	var s *Store
	if preflight != nil {
		// ScrivaDB rebuilds a damaged index at open. Open a throwaway copy so
		// the on-disk store stays byte-identical until an explicit repair.
		scratch, cerr := copyTree(filepath.Join(canon, "agents-db"))
		if cerr != nil {
			_ = lock.release()
			return nil, cerr
		}
		s, err = openAt(canon, filepath.Join(scratch, "agents-db"))
		if err != nil {
			_ = os.RemoveAll(scratch)
		} else {
			s.scratch = scratch
		}
	} else {
		s, err = open(canon)
	}
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	s.lock = lock
	s.preflight = preflight
	return s, nil
}

func open(dir string) (*Store, error) { return openAt(dir, filepath.Join(dir, "agents-db")) }

// copyTree copies src into <tmp>/agents-db and returns tmp.
func copyTree(src string) (string, error) {
	tmp, err := os.MkdirTemp("", "warden-agents-scratch-")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(tmp, "agents-db")
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o600)
	})
	if err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

func openAt(dir, dbDir string) (*Store, error) {
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
	closed, err := db.Collection("closed")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, col: col, closed: closed}
	if !imported {
		if err := s.importSessions(filepath.Join(dir, "sessions-db"), "active", s.col); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	// Archive import has its own marker: the earlier Agent store imported only
	// active records. Never rebuild or overwrite live agents during this upgrade.
	closedMarker := filepath.Join(dir, closedImportedMarker)
	if _, err := os.Stat(closedMarker); errors.Is(err, os.ErrNotExist) {
		if err := s.importSessions(filepath.Join(dir, "sessions-db"), "closed", s.closed); err != nil {
			_ = db.Close()
			return nil, err
		}
		if err := os.WriteFile(closedMarker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			_ = db.Close()
			return nil, err
		}
	} else if err != nil {
		_ = db.Close()
		return nil, err
	}

	activeRows, _, err := scanVerified(s.col, "agents", false)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	closedRows, skipped, err := scanVerified(s.closed, "closed", true)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	initSnap := buildSnapshot(1, time.Now().UTC(), activeRows, closedRows, skipped)
	s.snap.Store(initSnap)

	return s, nil
}

// isTerminalRecord probes a raw DB record for kind=terminal without decoding
// the full Agent shape. Terminal records are excluded from the agent collection;
// they belong in terminalstore.
func isTerminalRecord(data map[string]any) bool {
	kind, _ := data["kind"].(string)
	return kind == string(store.KindTerminal)
}

func (s *Store) importSessions(legacyDB, collection string, destination *engine.Collection) error {
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
	source, err := db.Collection(collection)
	if err != nil {
		return err
	}
	rows, err := source.Scan(query.MatchAll)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if isTerminalRecord(row.Data) {
			continue
		}
		a, err := fromRecord(row.Data)
		if err != nil {
			return err
		}
		rec, err := toRecord(a)
		if err != nil {
			return err
		}
		if _, _, err := destination.InsertWithKey(a.ID, rec); err != nil && !errors.Is(err, engine.ErrDuplicateKey) {
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
	// Lazy migration: legacy records carry Type but no Role. Backfill Role on
	// read so callers see a consistent canonical field without a bulk DB rewrite.
	if a.Role == "" && a.Type != "" {
		a.Role = store.RoleFromDeprecatedType(string(a.Type))
	}
	return &a, nil
}

func (s *Store) get(id string) (*Agent, error) {
	if s.preflight != nil {
		return nil, s.preflight
	}
	r, err := s.col.GetByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		// The engine reports ErrKeyNotFound both for an absent index entry and
		// for an entry whose offset decodes to a different record. Distinguish
		// them through the primary index so the latter cannot masquerade as a
		// legitimate missing agent.
		exists, existsErr := s.col.Exists(id)
		if existsErr != nil {
			return nil, readFailure("agents", id, existsErr)
		}
		if exists {
			return nil, newUnhealthy(integrityFailure("agents", id, "index contains key but its offset did not decode to that record"))
		}
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, readFailure("agents", id, err)
	}
	if err := verifyRecord("agents", id, r); err != nil {
		return nil, err
	}
	a, err := fromRecord(r.Data)
	if err != nil {
		return nil, newUnhealthy(store.ScanFailure{Collection: "agents", Key: id, Class: store.DegradeDecode, Detail: err.Error()})
	}
	return a, nil
}

// Insert creates an agent, initializing its lifecycle timestamps and events.
func (s *Store) Insert(ctx context.Context, a *Agent) error {
	if s.preflight != nil {
		return s.preflight
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SafeID(a.ID); err != nil {
		return err
	}
	if err := store.SafeSessionRef(a.AICLISessionID); err != nil {
		return err
	}
	if err := store.ValidateName(a.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fireWriteSeam("Insert")
	if ok, err := s.col.Exists(a.ID); err != nil {
		return err
	} else if ok {
		return ErrExists
	}
	currentSnap := s.snap.Load()
	if a.Name != "" && currentSnap != nil {
		if other, ok := currentSnap.ByName[a.Name]; ok && other.ID != a.ID {
			return ErrNameExists
		}
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
	if err != nil {
		return err
	}

	verified, err := s.get(a.ID)
	if err != nil {
		return err
	}
	*a = *cloneAgent(verified)
	s.publishInsert(currentSnap, verified)
	return nil
}

// Create reserves and persists an agent before its runtime has started. It is
// an internal lifecycle primitive; callers serving an operator request should
// use Spawn so no half-initialized record is exposed on success.
func (s *Store) Create(ctx context.Context, a *Agent) error { return s.Insert(ctx, a) }

// Init records the runtime identity discovered when an agent starts. TmuxSession
// addresses the pane; AICLISessionID pins the AI CLI resume conversation so the
// transcript and backend resume off the exact session, not directory-scoping.
func (s *Store) Init(ctx context.Context, id, tmuxSession, aiCLISessionID string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.TmuxSession = tmuxSession
		a.AICLISessionID = aiCLISessionID
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
	fireReadSeam("Get")
	snap := s.snap.Load()
	if snap == nil {
		return nil, ErrNotFound
	}
	return snap.Get(id)
}

// List returns all agents newest-updated first.
func (s *Store) List(ctx context.Context) ([]*Agent, error) {
	if s.preflight != nil {
		return nil, s.preflight
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fireReadSeam("List")
	snap := s.snap.Load()
	if snap == nil {
		return nil, nil
	}
	return snap.List(), nil
}

// Update atomically applies fn and stamps UpdatedAt.
func (s *Store) Update(ctx context.Context, id string, fn func(*Agent) error) error {
	if s.preflight != nil {
		return s.preflight
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fireWriteSeam("Update")
	a, err := s.get(id)
	if err != nil {
		return err
	}
	aPrivate := cloneAgent(a)
	if err := fn(aPrivate); err != nil {
		return err
	}
	aPrivate.Status = aPrivate.Status.Canonical()
	aPrivate.UpdatedAt = time.Now().UTC()
	rec, err := toRecord(aPrivate)
	if err != nil {
		return err
	}
	if _, err := s.col.UpdateByKey(id, rec); err != nil {
		return err
	}

	verified, err := s.get(id)
	if err != nil {
		return err
	}
	currentSnap := s.snap.Load()
	s.publishUpdate(currentSnap, verified)
	return nil
}

// GetByNameOrID looks up an agent by name first (exact case-sensitive match
// among active agents), falling back to ID lookup if no name matches.
// Returns ErrNotFound if neither name nor ID match any active agent.
func (s *Store) GetByNameOrID(ctx context.Context, nameOrID string) (*Agent, error) {
	if s.preflight != nil {
		return nil, s.preflight
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if nameOrID == "" {
		return nil, ErrNotFound
	}
	fireReadSeam("GetByNameOrID")
	snap := s.snap.Load()
	if snap == nil {
		return nil, ErrNotFound
	}
	return snap.GetByNameOrID(nameOrID)
}

// ListClosed returns all archived (closed) agents, newest updated first.
func (s *Store) ListClosed(ctx context.Context) ([]*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fireReadSeam("ListClosed")
	snap := s.snap.Load()
	if snap == nil {
		return nil, nil
	}
	return snap.ListClosed(), nil
}

// ListClosedDegraded returns all archived agents, reporting how many records were skipped due to decode errors.
func (s *Store) ListClosedDegraded(ctx context.Context) ([]*Agent, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	fireReadSeam("ListClosedDegraded")
	snap := s.snap.Load()
	if snap == nil {
		return nil, 0, nil
	}
	list, skipped := snap.ListClosedDegraded()
	return list, skipped, nil
}

// Archive moves the agent doc from active to closed collection.
func (s *Store) Archive(ctx context.Context, id string) error {
	if s.preflight != nil {
		return s.preflight
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fireWriteSeam("Archive")
	a, err := s.get(id)
	if err != nil {
		return err
	}
	rec, err := toRecord(a)
	if err != nil {
		return err
	}
	if _, err := s.closed.Upsert(id, rec); err != nil {
		return err
	}
	rClosed, err := s.closed.GetByKey(id)
	if err != nil {
		return readFailure("closed", id, err)
	}
	if err := verifyRecord("closed", id, rClosed); err != nil {
		return err
	}
	archivedAgent, err := fromRecord(rClosed.Data)
	if err != nil {
		return newUnhealthy(store.ScanFailure{Collection: "closed", Key: id, Class: store.DegradeDecode, Detail: err.Error()})
	}

	if err := s.col.DeleteByKey(id); err != nil {
		return err
	}
	exists, err := s.col.Exists(id)
	if err != nil {
		return readFailure("agents", id, err)
	}
	if exists {
		return newUnhealthy(integrityFailure("agents", id, "record still exists in index after DeleteByKey"))
	}

	currentSnap := s.snap.Load()
	s.publishArchive(currentSnap, id, archivedAgent)
	return nil
}

// UpdateStatus updates the status of an agent.
func (s *Store) UpdateStatus(ctx context.Context, id string, status store.Status) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.Status = status
		return nil
	})
}

// UpdateStatusIf is a compare-and-swap on agent status.
func (s *Store) UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := store.SafeID(id); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fireWriteSeam("UpdateStatusIf")
	a, err := s.get(id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if a.Status.Canonical() != expected.Canonical() {
		return false, nil
	}
	aPrivate := cloneAgent(a)
	aPrivate.Status = next.Canonical()
	aPrivate.UpdatedAt = time.Now().UTC()
	rec, err := toRecord(aPrivate)
	if err != nil {
		return false, err
	}
	_, err = s.col.UpdateByKey(id, rec)
	if err != nil {
		return false, err
	}

	verified, err := s.get(id)
	if err != nil {
		return false, err
	}
	currentSnap := s.snap.Load()
	s.publishUpdate(currentSnap, verified)
	return true, nil
}

// FinalizeExit transitions the agent to next status and records exit code atomically.
func (s *Store) FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := store.SafeID(id); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fireWriteSeam("FinalizeExit")
	a, err := s.get(id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if a.Status.Canonical() != expected.Canonical() {
		return false, nil
	}
	aPrivate := cloneAgent(a)
	now := time.Now().UTC()
	aPrivate.Status = next.Canonical()
	aPrivate.ExitCode = &code
	aPrivate.UpdatedAt = now
	if code != 0 {
		aPrivate.Events = append(aPrivate.Events, store.Event{
			TS:     now,
			Type:   "exit",
			Detail: exitDetail(code),
		})
	}
	rec, err := toRecord(aPrivate)
	if err != nil {
		return false, err
	}
	_, err = s.col.UpdateByKey(id, rec)
	if err != nil {
		return false, err
	}

	verified, err := s.get(id)
	if err != nil {
		return false, err
	}
	currentSnap := s.snap.Load()
	s.publishUpdate(currentSnap, verified)
	return true, nil
}

func (s *Store) AppendEvent(ctx context.Context, id string, ev store.Event) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.Events = append(a.Events, ev)
		return nil
	})
}

func (s *Store) AppendEventStatus(ctx context.Context, id string, ev store.Event, status store.Status) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.Events = append(a.Events, ev)
		if status != "" {
			a.Status = status
		}
		return nil
	})
}

func (s *Store) SetRestart(ctx context.Context, id string, count int, at time.Time) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.RestartCount = count
		a.LastRestartAt = &at
		return nil
	})
}

func (s *Store) UpdateContext(ctx context.Context, id string, tokens int, state string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		oldState := a.ContextState
		a.ContextTokens = tokens
		a.ContextState = state
		a.ContextCheckedAt = time.Now().UTC()
		if state != "" && state != oldState {
			a.Events = append(a.Events, store.Event{
				TS:     a.ContextCheckedAt,
				Type:   "context",
				Detail: fmt.Sprintf("context %s→%s (%dk)", orNone(oldState), state, tokens/1000),
			})
		}
		return nil
	})
}

func (s *Store) StampCompact(ctx context.Context, id string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		now := time.Now().UTC()
		a.LastCompactAt = &now
		return nil
	})
}

func (s *Store) UpdateAutoApprove(ctx context.Context, id string, enabled bool) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.AutoApprove = enabled
		return nil
	})
}

func (s *Store) SetForceCompact(ctx context.Context, id string, v *bool) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.ForceCompact = v
		return nil
	})
}

func (s *Store) UpdatePermissionMode(ctx context.Context, id string, mode string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.PermissionMode = mode
		return nil
	})
}

func (s *Store) UpdateRole(ctx context.Context, id string, role string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.Role = role
		return nil
	})
}

func (s *Store) ClearWorktree(ctx context.Context, id string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.Worktree = ""
		a.Branch = ""
		return nil
	})
}

func (s *Store) SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error {
	return s.Update(ctx, id, func(a *Agent) error {
		now := time.Now().UTC()
		if a.RateLimitedAt == nil {
			a.RateLimitedAt = &now
		}
		a.RateLimitRestoreAt = &restoreAt
		a.RateLimitRetryCount = retryCount
		a.Events = append(a.Events, store.Event{TS: now, Type: "rate-limit", Detail: fmt.Sprintf("scheduled resume at %s (retry %d)", restoreAt.Format(time.RFC3339), retryCount)})
		return nil
	})
}

func (s *Store) ClearRateLimit(ctx context.Context, id string) error {
	return s.Update(ctx, id, func(a *Agent) error {
		a.RateLimitedAt = nil
		a.RateLimitRestoreAt = nil
		a.RateLimitRetryCount = 0
		a.Events = append(a.Events, store.Event{TS: time.Now().UTC(), Type: "rate-limit-resumed", Detail: "successfully resumed after rate limit"})
		return nil
	})
}

func (s *Store) SetSessionID(ctx context.Context, id, sessionID string) error {
	if err := store.SafeSessionRef(sessionID); err != nil {
		return err
	}
	return s.Update(ctx, id, func(a *Agent) error {
		a.AICLISessionID = sessionID
		return nil
	})
}

func (s *Store) SetAICLISessionID(ctx context.Context, id, sessionID string) error {
	return s.SetSessionID(ctx, id, sessionID)
}

func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closedState.Load() {
		return errors.New("agent store closed")
	}
	return nil
}

// Delete permanently removes an agent. A missing id returns ErrNotFound.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s.preflight != nil {
		return s.preflight
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.SafeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fireWriteSeam("Delete")
	err := s.col.DeleteByKey(id)
	if errors.Is(err, engine.ErrKeyNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	exists, err := s.col.Exists(id)
	if err != nil {
		return readFailure("agents", id, err)
	}
	if exists {
		return newUnhealthy(integrityFailure("agents", id, "record still exists in index after DeleteByKey"))
	}

	currentSnap := s.snap.Load()
	s.publishDelete(currentSnap, id)
	return nil
}

func (s *Store) Close() error {
	s.closedState.Store(true)
	err := s.db.Close()
	if s.scratch != "" {
		_ = os.RemoveAll(s.scratch)
	}
	if lerr := s.lock.release(); err == nil {
		err = lerr
	}
	return err
}

func exitDetail(code int) string {
	if sig := signalName(code - 128); code > 128 && code <= 128+64 && sig != "" {
		return fmt.Sprintf("session exited: code %d (%s)", code, sig)
	}
	return fmt.Sprintf("session exited: code %d", code)
}

// signalName maps the common termination signals to their names; "" for others.
func signalName(sig int) string {
	switch sig {
	case 2:
		return "SIGINT"
	case 6:
		return "SIGABRT"
	case 9:
		return "SIGKILL"
	case 11:
		return "SIGSEGV"
	case 15:
		return "SIGTERM"
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
