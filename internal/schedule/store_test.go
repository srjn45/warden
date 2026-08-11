package schedule

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), "schedules.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func mustNew(t *testing.T, name string) *Schedule {
	t.Helper()
	s, err := New(Params{Name: name, Cron: "0 9 * * *", Prompt: "go"}, time.Now())
	if err != nil {
		t.Fatalf("New(%q): %v", name, err)
	}
	return s
}

func TestStoreCreateGetList(t *testing.T) {
	st := newTestStore(t)
	if err := st.Create(mustNew(t, "b")); err != nil {
		t.Fatalf("Create b: %v", err)
	}
	if err := st.Create(mustNew(t, "a")); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	got, err := st.Get("a")
	if err != nil {
		t.Fatalf("Get a: %v", err)
	}
	if got.Name != "a" {
		t.Fatalf("Get a name = %q", got.Name)
	}
	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Fatalf("List not sorted: %+v", list)
	}
}

func TestStoreCreateDuplicate(t *testing.T) {
	st := newTestStore(t)
	if err := st.Create(mustNew(t, "dup")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.Create(mustNew(t, "dup")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create err = %v, want ErrExists", err)
	}
}

func TestStoreGetMissing(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.Get("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestStoreUpdate(t *testing.T) {
	st := newTestStore(t)
	st.Create(mustNew(t, "s")) //nolint:errcheck
	now := time.Now()
	if err := st.Update("s", func(sc *Schedule) { Advance(sc, now, "", nil) }); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := st.Get("s")
	if got.LastRun == nil {
		t.Fatal("Update did not persist LastRun")
	}
	if err := st.Update("ghost", func(*Schedule) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing err = %v, want ErrNotFound", err)
	}
}

func TestStoreDelete(t *testing.T) {
	st := newTestStore(t)
	st.Create(mustNew(t, "s")) //nolint:errcheck
	if err := st.Delete("s"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get("s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete, Get err = %v, want ErrNotFound", err)
	}
	if err := st.Delete("s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing err = %v, want ErrNotFound", err)
	}
}

// Persistence: a second Store over the same path (after the first is closed,
// mirroring a daemon restart) sees what the first wrote.
func TestStorePersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st1, err := NewStore(path)
	if err != nil {
		t.Fatalf("open st1: %v", err)
	}
	st1.Create(mustNew(t, "keep")) //nolint:errcheck
	if err := st1.Close(); err != nil {
		t.Fatalf("close st1: %v", err)
	}

	st2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	got, err := st2.Get("keep")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Name != "keep" {
		t.Fatalf("reopened name = %q", got.Name)
	}
}

// TestStoreLegacyImport seeds a flat legacy schedules.json, then opens a fresh
// ScrivaDB-backed Store over the same path: the legacy entries must be readable,
// the import sentinel must now exist, the legacy JSON must be left in place as a
// read-only backup, and a second NewStore must not re-import or error.
func TestStoreLegacyImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "schedules.json")

	// Seed a legacy flat-JSON map keyed by schedule id, the format the old Store
	// wrote and NewStore now imports once.
	one := mustNew(t, "one")
	two := mustNew(t, "two")
	legacy := map[string]*Schedule{one.ID: one, two.ID: two}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	st, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (import): %v", err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatalf("List after import: %v", err)
	}
	if len(list) != 2 || list[0].ID != "one" || list[1].ID != "two" {
		t.Fatalf("imported entries wrong: %+v", list)
	}

	// The sentinel now marks the ScrivaDB as authoritative.
	sentinel := filepath.Join(dir, importedMarker)
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("import sentinel missing: %v", err)
	}
	// The legacy JSON is left untouched as a read-only backup.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("legacy schedules.json must be preserved: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close st: %v", err)
	}

	// A second open must not re-import (adding a schedule then reopening proves
	// the second NewStore reads the live ScrivaDB, not the stale legacy JSON) and
	// must not error.
	st2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen must not re-import or error: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })
	if err := st2.Create(mustNew(t, "three")); err != nil {
		t.Fatalf("Create after reopen: %v", err)
	}
	list2, err := st2.List()
	if err != nil {
		t.Fatalf("List after reopen: %v", err)
	}
	if len(list2) != 3 {
		t.Fatalf("after reopen want 3 schedules (no re-import clobber), got %d: %+v", len(list2), list2)
	}
}
