package agentstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/srjn45/warden/internal/store"
)

const (
	agentsLockName = ".agents-store.lock"
	legacyLockName = ".sessions-store.lock"
)

// OwnershipError is returned when another handle or process already owns the
// agent store. It wraps store.ErrStoreOwned (errors.Is) and carries the
// canonical data dir and a safe next step.
type OwnershipError struct {
	Dir  string // canonical data directory
	Lock string // lock file that is held
}

func (e *OwnershipError) Error() string {
	return fmt.Sprintf("agent store %s is owned by another warden process (lock %s); %s",
		e.Dir, e.Lock, e.NextStep())
}

func (e *OwnershipError) Unwrap() error { return store.ErrStoreOwned }

// NextStep is the safe operator action. It never suggests deleting files.
func (e *OwnershipError) NextStep() string {
	return "stop the running warden daemon (including one on another port) that uses this data directory, then retry; do not delete the lock file"
}

// canonicalDir resolves dir to an absolute, symlink-free path so relative,
// absolute and symlink aliases all name the same store.
func canonicalDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// acquireOwnership takes the exclusive agent-store lock for dir.
func acquireOwnership(dir string) (*flockFile, string, error) {
	canon, err := canonicalDir(dir)
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(canon, agentsLockName)
	l, err := acquireFlock(path)
	if errors.Is(err, errLockHeld) {
		return nil, canon, &OwnershipError{Dir: canon, Lock: path}
	}
	if err != nil {
		return nil, canon, err
	}
	return l, canon, nil
}

// acquireLegacyRead guards the importer's read of sessions-db: when the legacy
// session store is live-owned the import must not read it mid-write. Only
// needed while an import marker is missing and sessions-db exists.
func acquireLegacyRead(dir string) (*flockFile, error) {
	_, err := os.Stat(filepath.Join(dir, "sessions-db"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	need := false
	for _, m := range []string{importedMarker, closedImportedMarker} {
		if _, err := os.Stat(filepath.Join(dir, m)); errors.Is(err, os.ErrNotExist) {
			need = true
		} else if err != nil {
			return nil, err
		}
	}
	if !need {
		return nil, nil
	}
	path := filepath.Join(dir, legacyLockName)
	l, err := acquireFlock(path)
	if errors.Is(err, errLockHeld) {
		return nil, &OwnershipError{Dir: dir, Lock: path}
	}
	return l, err
}
