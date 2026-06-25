//go:build !linux

package collab

// osWatchLimit is Linux-specific (/proc/sys/fs/inotify); other platforms have
// no portable equivalent here, so the watcher uses defaultWatchBudget.
func osWatchLimit() int { return 0 }
