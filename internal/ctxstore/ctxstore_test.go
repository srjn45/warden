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
