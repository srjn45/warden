package schedule

import (
	"errors"
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
	if err := st.Update("s", func(sc *Schedule) { Advance(sc, now, nil) }); err != nil {
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

// Persistence: a second Store over the same file sees what the first wrote.
func TestStorePersistsAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	st1, _ := NewStore(path)
	st1.Create(mustNew(t, "keep")) //nolint:errcheck

	st2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := st2.Get("keep")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.Name != "keep" {
		t.Fatalf("reopened name = %q", got.Name)
	}
}
