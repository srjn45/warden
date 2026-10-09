//go:build unix

package backendstore

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type dirLock struct{ f *os.File }

// acquireDirLock takes the engine's own LOCK file exclusively and without
// blocking, so recovery cannot run beside a live opener (daemon or CLI).
func acquireDirLock(dir string) (*dirLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("%w (%s)", ErrOwned, dir)
		}
		return nil, err
	}
	return &dirLock{f: f}, nil
}

func (l *dirLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}

// checkAuthority requires the registry directory to be owned by the caller (or root).
func checkAuthority(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if uid := os.Geteuid(); uid != 0 && int(st.Uid) != uid {
			return fmt.Errorf("permission denied: %s is owned by uid %d, not the current user (uid %d)", dir, st.Uid, uid)
		}
	}
	return nil
}
