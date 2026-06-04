package pipeline

import (
	"errors"
	"testing"
)

func TestStoreCreateGetList(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	p := &Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusPending,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}}
	if err := s.Create(p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create want ErrExists, got %v", err)
	}
	got, err := s.Get("p1")
	if err != nil || got.Name != "p1" || len(got.Jobs) != 1 {
		t.Fatalf("Get: %+v err=%v", got, err)
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get want ErrNotFound, got %v", err)
	}
	all, _ := s.List()
	if len(all) != 1 {
		t.Fatalf("List want 1, got %d", len(all))
	}
}

func TestStoreUpdate(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	s.Create(&Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusPending,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}})
	if err := s.Update("p1", func(p *Pipeline) {
		p.Status = StatusRunning
		p.Job("a").Status = JobDone
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get("p1")
	if got.Status != StatusRunning || got.Job("a").Status != JobDone {
		t.Fatalf("update not persisted: %+v", got)
	}
}

func TestStoreDelete(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	s.Create(&Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusDone,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobDone}}})
	if err := s.Delete("p1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete, Get want ErrNotFound, got %v", err)
	}
	if err := s.Delete("p1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing want ErrNotFound, got %v", err)
	}
}
