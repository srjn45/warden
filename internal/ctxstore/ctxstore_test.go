package ctxstore

import (
	"errors"
	"testing"
)

func TestSetThenGet(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Set("global.greeting", "hello", "agent-A"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e, err := s.Get("global.greeting")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if e.Value != "hello" || e.UpdatedBy != "agent-A" || e.Key != "global.greeting" {
		t.Fatalf("got %+v", e)
	}
	if e.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt not set")
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSetEmptyKeyRejected(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Set("", "v", "by"); !errors.Is(err, ErrBadKey) {
		t.Fatalf("want ErrBadKey, got %v", err)
	}
}

func TestSetOverwrites(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("k", "v1", "a")
	s.Set("k", "v2", "b")
	e, _ := s.Get("k")
	if e.Value != "v2" || e.UpdatedBy != "b" {
		t.Fatalf("overwrite failed: %+v", e)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, _ := New(dir)
	s1.Set("k", "v", "a")
	s2, err := New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	e, err := s2.Get("k")
	if err != nil || e.Value != "v" {
		t.Fatalf("not persisted: %+v err=%v", e, err)
	}
}

func TestListByPrefixSorted(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("pipeline.p1.b.output", "B", "b")
	s.Set("pipeline.p1.a.output", "A", "a")
	s.Set("global.x", "X", "x")

	all, err := s.List("")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3, got %d", len(all))
	}
	// sorted by key: global.x, pipeline.p1.a.output, pipeline.p1.b.output
	if all[0].Key != "global.x" || all[1].Key != "pipeline.p1.a.output" {
		t.Fatalf("not sorted: %+v", all)
	}

	pref, _ := s.List("pipeline.p1.")
	if len(pref) != 2 {
		t.Fatalf("prefix want 2, got %d", len(pref))
	}
}

func TestListEmptyStoreReturnsEmptySlice(t *testing.T) {
	s, _ := New(t.TempDir())
	got, err := s.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", got)
	}
}

func TestDel(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Set("k", "v", "a")
	if err := s.Del("k"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("still present after Del")
	}
	if err := s.Del("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Del missing want ErrNotFound, got %v", err)
	}
}
