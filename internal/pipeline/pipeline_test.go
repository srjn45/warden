package pipeline

import (
	"strings"
	"testing"
)

func valid() *Pipeline {
	return &Pipeline{
		Name: "refactor-auth", Repo: "/repo",
		Jobs: []Job{
			{ID: "analyze", Prompt: "look", Worktree: "none"},
			{ID: "impl", Prompt: "do", DependsOn: []string{"analyze"}, Worktree: "fresh"},
			{ID: "review", Prompt: "merge", DependsOn: []string{"impl"}, Worktree: "from:impl"},
		},
	}
}

func TestValidateOK(t *testing.T) {
	if err := Validate(valid()); err != nil {
		t.Fatalf("valid pipeline rejected: %v", err)
	}
}

func TestValidateRejectsUnknownDep(t *testing.T) {
	p := valid()
	p.Jobs[1].DependsOn = []string{"ghost"}
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want unknown-dep error, got %v", err)
	}
}

func TestValidateRejectsCycle(t *testing.T) {
	p := &Pipeline{Name: "p", Repo: "/r", Jobs: []Job{
		{ID: "a", Prompt: "x", DependsOn: []string{"b"}, Worktree: "none"},
		{ID: "b", Prompt: "x", DependsOn: []string{"a"}, Worktree: "none"},
	}}
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestValidateRejectsUnknownFromRef(t *testing.T) {
	p := valid()
	p.Jobs[2].Worktree = "from:ghost"
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want unknown from-ref error, got %v", err)
	}
}

func TestValidateRejectsBadIDs(t *testing.T) {
	if err := Validate(&Pipeline{Name: "bad/name", Repo: "/r", Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none"}}}); err == nil {
		t.Fatalf("want bad pipeline name error")
	}
	if err := Validate(&Pipeline{Name: "ok", Repo: "/r", Jobs: []Job{{ID: "a/b", Prompt: "x", Worktree: "none"}}}); err == nil {
		t.Fatalf("want bad job id error")
	}
	if err := Validate(&Pipeline{Name: "ok", Repo: "/r", Jobs: []Job{{ID: "a", Prompt: "", Worktree: "none"}}}); err == nil {
		t.Fatalf("want empty-prompt error")
	}
}

func TestParseWorktree(t *testing.T) {
	mode, from := ParseWorktree("from:impl")
	if mode != "from" || from != "impl" {
		t.Fatalf("got %q %q", mode, from)
	}
	if m, _ := ParseWorktree("fresh"); m != "fresh" {
		t.Fatalf("fresh")
	}
}

func TestJobLookup(t *testing.T) {
	p := valid()
	if p.Job("impl") == nil || p.Job("nope") != nil {
		t.Fatalf("Job() lookup wrong")
	}
}
