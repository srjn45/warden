//go:build !unix

package ownerlock

import (
	"errors"
	"os"
)

var errHeld = errors.New("lock held")

type file struct{ f *os.File }

// openLocked provides no exclusion off unix (warden ships for linux/darwin).
func openLocked(path string) (*file, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
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

func (l *file) close() error { return l.f.Close() }
