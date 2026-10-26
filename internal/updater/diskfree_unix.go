package updater

import "syscall"

// diskFree measures bytes available to unprivileged writers on dir's filesystem.
func diskFree(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
