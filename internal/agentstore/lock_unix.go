//go:build unix

package agentstore

import (
	"errors"
	"os"
	"syscall"
)

var errLockHeld = errors.New("lock held")

// flockFile is an exclusive, non-blocking advisory lock on a file. flock locks
// belong to the open file description, so a second opener in the same process
// is rejected exactly like one in another process, and the kernel drops the
// lock on Close or process death.
type flockFile struct{ f *os.File }

func acquireFlock(path string) (*flockFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, errLockHeld
		}
		return nil, err
	}
	return &flockFile{f: f}, nil
}

func (l *flockFile) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}
