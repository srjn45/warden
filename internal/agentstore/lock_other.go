//go:build !unix

package agentstore

import (
	"errors"
	"os"
)

var errLockHeld = errors.New("lock held")

// flockFile is a no-op on non-unix builds (warden ships for linux/darwin only);
// it provides no exclusion.
type flockFile struct{ f *os.File }

func acquireFlock(path string) (*flockFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &flockFile{f: f}, nil
}

func (l *flockFile) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
