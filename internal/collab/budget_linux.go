//go:build linux

package collab

import (
	"os"
	"strconv"
	"strings"
)

// osWatchLimit reads the per-user inotify watch limit, or 0 if it can't be
// determined (in which case the watcher falls back to defaultWatchBudget).
func osWatchLimit() int {
	b, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return n
}
