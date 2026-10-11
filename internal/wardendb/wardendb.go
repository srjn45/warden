// Package wardendb is the sole owner of the shared schema-2 warden-db handle:
// the one ScrivaDB instance at <dataDir>/warden-db that the collection contract
// (docs/specs/2026-10-10-warden-db-collection-contract.md) makes the home of
// every schema-2 collection.
//
// It owns four things and nothing else:
//
//   - the directory: Dir(dataDir) is the only place the handle is rooted;
//   - the process ownership: Open takes an exclusive lock inside that
//     directory, so a second Open — from this process or any other — fails
//     with *LockedError. Exactly one daemon process may own warden-db;
//   - the collection registry: a collection is reachable only after it has
//     been declared with Register, its name must be in the contract catalogue
//     (Catalogue), and it can be declared once;
//   - the close order: Close runs registered closers and collections in
//     reverse registration order, then closes the handle, then releases the
//     lock. Everything handed out fails with ErrClosed afterwards.
//
// Probe is the diagnostic entry point. It never creates the directory, never
// opens the database and never takes ownership, so it is safe to call from a
// CLI while the daemon is running.
//
// The package is additive and inert: nothing opens it unless a caller does so
// explicitly, it reads and writes no schema-1 store, and it is not canonical
// for any data yet. Durability/integrity policy (strict open), verification,
// backup/restore and the operation journal are layered on by later work
// through Option; cross-collection writes go through internal/dbtx on Handle.
package wardendb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"

	"github.com/srjn45/warden/internal/ownerlock"
)

// DirName is the warden-db directory inside the data directory.
const DirName = "warden-db"

// SchemaVersion is the schema generation this handle hosts.
const SchemaVersion = 2

// ErrClosed is returned by every use of a DB, or of anything obtained from
// it, after Close has begun retiring it.
var ErrClosed = errors.New("wardendb: database is closed")

// ErrLocked is wrapped by every *LockedError.
var ErrLocked = errors.New("wardendb: warden-db is owned by another opener")

// LockedError reports that warden-db already has an owner.
type LockedError struct {
	Dir string
	// Owner is the holder's advisory metadata; nil when it is unreadable.
	Owner *ownerlock.Owner
	cause error
}

func (e *LockedError) Error() string {
	if o := e.Owner; o != nil {
		return fmt.Sprintf("wardendb: %s is already owned by %s pid %d; only one process may open warden-db (use the running daemon, or Probe for diagnostics)", e.Dir, o.Kind, o.PID)
	}
	return fmt.Sprintf("wardendb: %s is already owned; only one process may open warden-db (use the running daemon, or Probe for diagnostics)", e.Dir)
}

// Unwrap exposes ErrLocked and the underlying lock error.
func (e *LockedError) Unwrap() []error { return []error{ErrLocked, e.cause} }

// Dir returns the warden-db directory for dataDir.
func Dir(dataDir string) string { return filepath.Join(dataDir, DirName) }

type config struct {
	owner ownerlock.Info
	trace func(step string)
}

// Option configures Open. Options are the extension point for later open
// policy (durability, integrity callbacks, compaction, audit/metrics).
type Option func(*config)

// WithOwner sets the advisory owner metadata recorded in the lock file and
// reported to a rejected second opener. The default kind is the daemon.
func WithOwner(info ownerlock.Info) Option {
	return func(c *config) { c.owner = info }
}

// DB is the owned warden-db handle. It is safe for concurrent use.
type DB struct {
	dir   string
	trace func(string)

	mu      sync.RWMutex
	closing bool
	handle  *scriva.DB // nil once closed
	lock    *ownerlock.Lock
	entries map[string]*entry
	order   []string // collection registration order
	steps   []closeStep

	closeOnce sync.Once
	closeErr  error
}

type closeStep struct {
	label string
	fn    func() error
}

// Open takes exclusive ownership of <dataDir>/warden-db (creating the
// directory if needed) and opens its ScrivaDB handle. A live owner yields
// *LockedError. The caller must Close the returned DB.
func Open(dataDir string, opts ...Option) (*DB, error) {
	if dataDir == "" {
		return nil, errors.New("wardendb: empty data directory")
	}
	cfg := config{owner: ownerlock.Info{Kind: ownerlock.KindDaemon}}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	dir := Dir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wardendb: create %s: %w", dir, err)
	}
	lock, err := ownerlock.Acquire(dir, cfg.owner)
	if err != nil {
		var owned *ownerlock.OwnedError
		if errors.As(err, &owned) {
			return nil, &LockedError{Dir: dir, Owner: owned.Owner, cause: err}
		}
		return nil, fmt.Errorf("wardendb: lock %s: %w", dir, err)
	}
	handle, err := scriva.Open(dir)
	if err != nil {
		_ = lock.Release()
		// The engine's own directory lock is the backstop where the
		// ownership lock offers no exclusion.
		if errors.Is(err, engine.ErrDatabaseLocked) {
			return nil, &LockedError{Dir: dir, cause: err}
		}
		return nil, fmt.Errorf("wardendb: open %s: %w", dir, err)
	}
	return &DB{
		dir:     dir,
		trace:   cfg.trace,
		handle:  handle,
		lock:    lock,
		entries: map[string]*entry{},
	}, nil
}

// Dir returns the directory this handle owns.
func (db *DB) Dir() string { return db.dir }

// Handle returns the owned ScrivaDB handle, for the transaction boundary
// (internal/dbtx). The caller must not Close it and must not retain it past
// Close; it fails with ErrClosed once Close has begun.
func (db *DB) Handle() (*scriva.DB, error) {
	if db == nil {
		return nil, ErrClosed
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.closing || db.handle == nil {
		return nil, ErrClosed
	}
	return db.handle, nil
}

// OnClose registers fn to run during Close, before the handle is closed.
// Closers and collections share one stack and are retired in reverse
// registration order, so a closer may still use every collection registered
// before it. fn must not call Close.
func (db *DB) OnClose(name string, fn func() error) error {
	if fn == nil {
		return errors.New("wardendb: nil closer")
	}
	if db == nil {
		return ErrClosed
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closing {
		return ErrClosed
	}
	db.steps = append(db.steps, closeStep{label: "closer:" + name, fn: fn})
	return nil
}

// Close retires the database in a fixed order: registered closers and
// collections in reverse registration order, then the ScrivaDB handle, then
// the ownership lock. Every step runs even if an earlier one fails; the
// failures are joined. Close is idempotent: later calls wait for the first to
// finish and return its result.
func (db *DB) Close() error {
	if db == nil {
		return nil
	}
	db.closeOnce.Do(func() {
		db.mu.Lock()
		db.closing = true
		steps := db.steps
		db.steps = nil
		db.mu.Unlock()

		var errs []error
		for i := len(steps) - 1; i >= 0; i-- {
			db.traceStep(steps[i].label)
			if err := steps[i].fn(); err != nil {
				errs = append(errs, fmt.Errorf("wardendb: %s: %w", steps[i].label, err))
			}
		}

		db.mu.Lock()
		handle := db.handle
		db.handle = nil
		db.mu.Unlock()
		db.traceStep("handle")
		if err := handle.Close(); err != nil {
			errs = append(errs, fmt.Errorf("wardendb: close handle: %w", err))
		}

		db.traceStep("lock")
		if err := db.lock.Release(); err != nil {
			errs = append(errs, fmt.Errorf("wardendb: release lock: %w", err))
		}
		db.closeErr = errors.Join(errs...)
	})
	return db.closeErr
}

func (db *DB) traceStep(step string) {
	if db.trace != nil {
		db.trace(step)
	}
}

// Status is what Probe reports about a warden-db directory.
type Status struct {
	Dir    string
	Exists bool
	// Owned reports a live owner in another process. Owner is its advisory
	// metadata and may be nil when unreadable.
	Owned bool
	Owner *ownerlock.Owner
}

// Probe is the diagnostic, read-only entry point: it reports whether
// warden-db exists under dataDir and whether another process owns it. It
// never creates the directory, never opens the database, never mutates it and
// never retains the lock, so it is safe alongside a running daemon. A handle
// owned by the calling process is reported as not Owned.
func Probe(dataDir string) (Status, error) {
	st := Status{Dir: Dir(dataDir)}
	fi, err := os.Stat(st.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if !fi.IsDir() {
		return st, fmt.Errorf("wardendb: %s is not a directory", st.Dir)
	}
	st.Exists = true
	owned, err := ownerlock.Probe(st.Dir)
	if err != nil {
		return st, err
	}
	if owned != nil {
		st.Owned, st.Owner = true, owned.Owner
	}
	return st, nil
}
