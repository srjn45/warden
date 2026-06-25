package collab

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/srjn45/warden/internal/store"
)

// fakeFSWatch records Add/Remove calls and lets a test drive the event/error
// channels, so the watcher's bookkeeping can be tested without real inotify.
type fakeFSWatch struct {
	mu      sync.Mutex
	added   map[string]int
	removed map[string]int
	events  chan fsnotify.Event
	errs    chan error
}

func newFakeFSWatch() *fakeFSWatch {
	return &fakeFSWatch{
		added:   map[string]int{},
		removed: map[string]int{},
		events:  make(chan fsnotify.Event, 32),
		errs:    make(chan error, 4),
	}
}

func (f *fakeFSWatch) Add(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added[name]++
	return nil
}

func (f *fakeFSWatch) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[name]++
	return nil
}

func (f *fakeFSWatch) Close() error                  { return nil }
func (f *fakeFSWatch) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeFSWatch) Errors() <-chan error          { return f.errs }
func (f *fakeFSWatch) addCount() int                 { f.mu.Lock(); defer f.mu.Unlock(); return len(f.added) }
func (f *fakeFSWatch) removeCount() int              { f.mu.Lock(); defer f.mu.Unlock(); return len(f.removed) }
func (f *fakeFSWatch) wasAdded(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.added[name]
	return ok
}

// mkTree creates root with the given relative subdirectories and returns root.
func mkTree(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	return root
}

func TestNewWatcherWithDefaultsBudget(t *testing.T) {
	w := newWatcherWith(newFakeFSWatch(), 0)
	if w.budget != defaultWatchBudget {
		t.Fatalf("budget = %d, want default %d", w.budget, defaultWatchBudget)
	}
}

func TestReconcileAddsThenRemovesRoot(t *testing.T) {
	root := mkTree(t, "internal/auth", "web/src", ".git/objects")
	fake := newFakeFSWatch()
	w := newWatcherWith(fake, 1024)

	w.reconcile([]string{root})
	// root + internal + internal/auth + web + web/src = 5 dirs; .git is skipped.
	if got := w.watchedDirs(); got != 5 {
		t.Fatalf("watched dirs = %d, want 5", got)
	}
	if fake.wasAdded(filepath.Join(root, ".git")) {
		t.Fatalf(".git must not be watched")
	}

	w.reconcile(nil)
	if got := w.watchedDirs(); got != 0 {
		t.Fatalf("after removal watched dirs = %d, want 0", got)
	}
	if fake.removeCount() != 5 {
		t.Fatalf("removed dirs = %d, want 5", fake.removeCount())
	}
}

func TestReconcileIsIdempotentForUnchangedRoots(t *testing.T) {
	root := mkTree(t, "a")
	fake := newFakeFSWatch()
	w := newWatcherWith(fake, 1024)

	w.reconcile([]string{root})
	first := fake.addCount()
	w.reconcile([]string{root}) // same set → no new Adds
	if fake.addCount() != first {
		t.Fatalf("reconcile re-added watches for an unchanged root: %d → %d", first, fake.addCount())
	}
}

func TestReconcileRespectsBudget(t *testing.T) {
	root := mkTree(t, "a", "b", "c", "d", "e")
	fake := newFakeFSWatch()
	w := newWatcherWith(fake, 3) // room for only 3 dirs total

	w.reconcile([]string{root})
	if got := w.watchedDirs(); got != 3 {
		t.Fatalf("watched dirs = %d, want capped at budget 3", got)
	}
}

func TestNoteCreateWatchesNewDirUnderRoot(t *testing.T) {
	root := mkTree(t, "x")
	fake := newFakeFSWatch()
	w := newWatcherWith(fake, 1024)
	w.reconcile([]string{root})
	before := w.watchedDirs()

	newDir := filepath.Join(root, "x", "y")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	w.noteCreate(newDir)
	if w.watchedDirs() != before+1 {
		t.Fatalf("noteCreate did not watch the new dir (count %d, want %d)", w.watchedDirs(), before+1)
	}

	// A regular file and a path outside any root must be ignored.
	f := filepath.Join(root, "x", "f.go")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.noteCreate(f)
	w.noteCreate("/some/other/place")
	if w.watchedDirs() != before+1 {
		t.Fatalf("noteCreate should ignore files and out-of-root paths (count %d)", w.watchedDirs())
	}
}

func TestWatchLoopDebouncesEventBurstIntoOneScan(t *testing.T) {
	sessions := []*store.Session{
		{ID: "a", Worktree: "/wt/a", Status: store.StatusWorking},
		{ID: "b", Worktree: "/wt/b", Status: store.StatusWorking},
	}
	diffs := map[string][]string{"/wt/a": {"auth.go"}, "/wt/b": {"auth.go"}}
	m := newTestMonitor(t, sessions, diffs)
	fake := newFakeFSWatch()
	m.watch = newWatcherWith(fake, 1024)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.watchLoop(ctx)

	// A burst of edits should coalesce into a single rescan, and dedup keeps it
	// to one warning per agent — not one per event.
	for i := 0; i < 5; i++ {
		fake.events <- fsnotify.Event{Name: "/wt/a/auth.go", Op: fsnotify.Write}
	}
	waitForMessageCount(t, m, "a", 1)

	if msgs, _ := m.mbox.Messages("a"); len(msgs) != 1 {
		t.Fatalf("want exactly 1 warning from a burst, got %d", len(msgs))
	}
}

func TestRealWatcherTriggersScanOnEdit(t *testing.T) {
	wtA, wtB := t.TempDir(), t.TempDir()
	sessions := []*store.Session{
		{ID: "a", Worktree: wtA, Status: store.StatusWorking},
		{ID: "b", Worktree: wtB, Status: store.StatusWorking},
	}
	// Stub the diff so the test needs no real git repo; the watcher, reconcile,
	// debounce, and tick all run for real.
	diffs := map[string][]string{wtA: {"auth.go"}, wtB: {"auth.go"}}
	m := newTestMonitor(t, sessions, diffs)
	w, err := newWatcher()
	if err != nil {
		t.Skipf("fsnotify unavailable: %v", err)
	}
	m.watch = w
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.reconcileWatches(ctx)
	go m.watchLoop(ctx)

	// A real filesystem edit must drive a conflict scan within a tick or two.
	if err := os.WriteFile(filepath.Join(wtA, "auth.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForMessageCount(t, m, "a", 1)
}

// waitForMessageCount polls until agent id has at least n messages or times out.
func waitForMessageCount(t *testing.T, m *Monitor, id string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if msgs, err := m.mbox.Messages(id); err == nil && len(msgs) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %d message(s) to %s", n, id)
}
