package pipeline

import "testing"

const sampleYAML = `
name: refactor-auth
repo: /repo
jobs:
  - id: analyze
    prompt: "look at auth"
    worktree: none
  - id: impl
    prompt: "do it"
    depends_on: [analyze]
    worktree: fresh
    handoff: "the branch name"
`

func TestParseSpec(t *testing.T) {
	p, err := ParseSpec([]byte(sampleYAML))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if p.ID != "refactor-auth" || p.Name != "refactor-auth" || p.Repo != "/repo" {
		t.Fatalf("header wrong: %+v", p)
	}
	if p.Status != StatusPending {
		t.Fatalf("new pipeline should be pending, got %s", p.Status)
	}
	if len(p.Jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(p.Jobs))
	}
	a := p.Job("analyze")
	if a.Status != JobPending || a.Type != "development" {
		t.Fatalf("defaults wrong: %+v", a)
	}
	if p.Job("impl").Handoff != "the branch name" {
		t.Fatalf("handoff not parsed")
	}
}

func TestParseSpecDefaultsWorktreeNone(t *testing.T) {
	p, err := ParseSpec([]byte("name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if p.Job("a").Worktree != "none" {
		t.Fatalf("blank worktree should default to none, got %q", p.Job("a").Worktree)
	}
}

func TestParseSpecInvalidRejected(t *testing.T) {
	if _, err := ParseSpec([]byte("name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    depends_on: [ghost]\n")); err == nil {
		t.Fatalf("expected validation error for unknown dep")
	}
	if _, err := ParseSpec([]byte("not: valid: yaml: [")); err == nil {
		t.Fatalf("expected yaml parse error")
	}
}
