package collab

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

const (
	// watchBudgetFraction caps how many inotify watches the monitor will hold,
	// as a fraction of the OS per-user limit, leaving headroom for editors,
	// language servers, and other watchers sharing the same machine.
	watchBudgetFraction = 0.8
	// defaultWatchBudget is used when the OS watch limit can't be read.
	defaultWatchBudget = 8192
)

// errBudgetExhausted stops a worktree walk once the watch budget is spent.
var errBudgetExhausted = errors.New("collab: watch budget exhausted")

// fsWatch is the slice of *fsnotify.Watcher the watcher needs; tests supply a
// fake so the reconcile/budget bookkeeping can be exercised without real inotify.
type fsWatch interface {
	Add(name string) error
	Remove(name string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

// fsnotifyWatch adapts *fsnotify.Watcher to fsWatch (its channels are fields,
// not methods).
type fsnotifyWatch struct{ *fsnotify.Watcher }

func (w fsnotifyWatch) Events() <-chan fsnotify.Event { return w.Watcher.Events }
func (w fsnotifyWatch) Errors() <-chan error          { return w.Watcher.Errors }

// watcher maintains inotify watches over a changing set of worktree roots,
// recursively watching each tree's directories within a watch budget. inotify
// is not recursive, so each directory is watched individually and new
// directories are picked up from their create events (see noteCreate).
type watcher struct {
	fsw    fsWatch
	budget int

	mu    sync.Mutex
	roots map[string]map[string]struct{} // worktree root -> set of watched dirs
	count int                            // total watched dirs across all roots
}

// newWatcher constructs a watcher over a real fsnotify backend, sized to the
// host's inotify budget.
func newWatcher() (*watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return newWatcherWith(fsnotifyWatch{fsw}, watchBudget()), nil
}

// newWatcherWith builds a watcher over an arbitrary backend and budget (tests).
func newWatcherWith(fsw fsWatch, budget int) *watcher {
	if budget <= 0 {
		budget = defaultWatchBudget
	}
	return &watcher{fsw: fsw, budget: budget, roots: map[string]map[string]struct{}{}}
}

// watchBudget returns the number of inotify watches the monitor may hold:
// watchBudgetFraction of the OS per-user limit, or a default if unknown.
func watchBudget() int {
	if max := osWatchLimit(); max > 0 {
		return int(float64(max) * watchBudgetFraction)
	}
	return defaultWatchBudget
}

func (w *watcher) Close() error { return w.fsw.Close() }

// reconcile makes the watched root set exactly `want`: it drops watches for
// departed worktrees and recursively adds watches for new ones. warden has no
// session-termination event bus, so this is driven off the poll loop's view of
// the active-session set (mirroring poller-based cleanup elsewhere).
func (w *watcher) reconcile(want []string) {
	wantSet := make(map[string]struct{}, len(want))
	for _, r := range want {
		if r != "" {
			wantSet[r] = struct{}{}
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for root, dirs := range w.roots {
		if _, ok := wantSet[root]; ok {
			continue
		}
		for d := range dirs {
			_ = w.fsw.Remove(d)
			w.count--
		}
		delete(w.roots, root)
	}
	for r := range wantSet {
		if _, ok := w.roots[r]; ok {
			continue
		}
		w.addRootLocked(r)
	}
}

// addRootLocked recursively watches every directory under root (skipping .git),
// stopping early if the watch budget is exhausted — the remaining tree is still
// covered by the poll loop. Caller holds w.mu.
func (w *watcher) addRootLocked(root string) {
	dirs := map[string]struct{}{}
	w.roots[root] = dirs
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip it, keep walking the rest
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		if w.count >= w.budget {
			return errBudgetExhausted
		}
		if err := w.fsw.Add(path); err == nil {
			dirs[path] = struct{}{}
			w.count++
		}
		return nil
	})
	if errors.Is(err, errBudgetExhausted) {
		slog.Warn("collab: inotify watch budget reached; remaining dirs covered by polling only", "budget", w.budget)
	}
}

// noteCreate watches a newly created directory so edits beneath it are seen
// without waiting for a full reconcile. Non-directories, already-watched paths,
// and paths outside any known root are ignored.
func (w *watcher) noteCreate(path string) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.count >= w.budget {
		return
	}
	for root, dirs := range w.roots {
		if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
			continue
		}
		if _, ok := dirs[path]; ok {
			return
		}
		if err := w.fsw.Add(path); err == nil {
			dirs[path] = struct{}{}
			w.count++
		}
		return
	}
}

// watchedDirs reports the total number of directories currently watched.
func (w *watcher) watchedDirs() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}
