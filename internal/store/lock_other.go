//go:build !unix

package store

import "os"

// storeLock is a best-effort no-op on non-unix platforms. warden ships only for
// linux and darwin (both unix, both flock-backed); this stub exists solely so
// the package still compiles under `go build` on other GOOS values. It provides
// no cross-process exclusion.
type storeLock struct {
	f *os.File
}

// acquireStoreLock creates the lock file (so its presence is still visible) but
// takes no OS lock. It never returns ErrStoreOwned — the real exclusion
// guarantee holds only on the unix build.
func acquireStoreLock(path string) (*storeLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return &storeLock{f: f}, nil
}

func (l *storeLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
