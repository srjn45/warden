//go:build unix

package store

import (
	"os"
	"syscall"
)

// storeLock is an advisory exclusive lock on a lock file in the data directory,
// held for the lifetime of the writable FileStore. It is enforced with
// syscall.Flock (BSD flock), which the kernel releases automatically when the
// holding process dies or the descriptor is closed — so a crashed daemon never
// leaves a permanently stuck store, and no PID-liveness heuristic is needed.
type storeLock struct {
	f *os.File
}

// acquireStoreLock opens (creating if needed) the lock file at path and takes a
// non-blocking exclusive lock. A conflicting lock held by another live process
// returns ErrStoreOwned; any other failure is returned as-is. The lock file
// content is diagnostic only and is NEVER trusted as the authority — the flock
// is the authority.
func acquireStoreLock(path string) (*storeLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, ErrStoreOwned
		}
		return nil, err
	}
	return &storeLock{f: f}, nil
}

// release drops the advisory lock and closes the descriptor. Safe on a nil lock
// (a store opened before locking was wired, or a double close).
func (l *storeLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}
