//go:build unix

package ownerlock

import (
	"errors"
	"os"
	"syscall"
)

var errHeld = errors.New("lock held")

type file struct{ f *os.File }

// openLocked opens path and takes a non-blocking exclusive flock (per open
// file description, so a second acquire in the same process is also refused).
func openLocked(path string) (*file, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, errHeld
		}
		return nil, err
	}
	return &file{f: f}, nil
}

func (l *file) writeAll(b []byte) error {
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	_, err := l.f.WriteAt(b, 0)
	return err
}

func (l *file) close() error {
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}
