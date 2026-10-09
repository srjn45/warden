//go:build !unix

package backendstore

import "os"

type dirLock struct{}

// acquireDirLock is a no-op off unix; scriva's own Repair still refuses a held lock.
func acquireDirLock(string) (*dirLock, error) { return &dirLock{}, nil }

func (*dirLock) release() error { return nil }

func checkAuthority(dir string) error {
	_, err := os.Stat(dir)
	return err
}
