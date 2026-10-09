//go:build unix

package agentstore

import (
	"fmt"
	"os"
	"syscall"
)

// CheckRepairAuthority verifies the caller may repair dataDir: the directory
// must exist and be owned by the calling user (or the caller is root). Repair
// is offline-only, so filesystem authority is the permission boundary.
func CheckRepairAuthority(dataDir string) error {
	info, err := os.Stat(dataDir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if uid := os.Geteuid(); uid != 0 && int(st.Uid) != uid {
		return fmt.Errorf("permission denied: data directory %s is owned by uid %d, not the current user (uid %d)", dataDir, st.Uid, uid)
	}
	return nil
}
