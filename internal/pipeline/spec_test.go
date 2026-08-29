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
	if len(p.Jobs) != 3 {
		t.Fatalf("want 3 jobs, got %d", len(p.Jobs))
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
	if p.Job("a").Worktree != "pipeline" {
		t.Fatalf("blank worktree should default to none, got %q", p.Job("a").Worktree)
	}
}

func TestParseSpecDefaultsRunIfSuccess(t *testing.T) {
	p, err := ParseSpec([]byte("name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n  - id: b\n    prompt: y\n    depends_on: [a]\n    run_if: failure\n"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if got := p.Job("a").RunIf; got != "success" {
		t.Fatalf("blank run_if should default to success, got %q", got)
	}
	if got := p.Job("b").RunIf; got != "failure" {
		t.Fatalf("explicit run_if not parsed, got %q", got)
	}
}

func TestParseSpecInvalidRunIfRejected(t *testing.T) {
	if _, err := ParseSpec([]byte("name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    run_if: maybe\n")); err == nil {
		t.Fatalf("expected validation error for invalid run_if")
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

func TestParseSpecRoleTierBackendModel(t *testing.T) {
	spec := `
name: tiered-pipeline
repo: /repo
jobs:
  - id: plan
    prompt: "plan architecture"
    role: orchestrator
    tier: tier-1
    backend: claude
    model: opus
    worktree: none
  - id: impl
    prompt: "implement feature"
    role: worker
    tier: tier-2
    backend: codex
    model: gpt-5.6-terra
    depends_on: [plan]
    worktree: fresh
  - id: test
    prompt: "run tests"
    role: worker
    tier: tier-3
    backend: cursor
    model: composer-2.5-fast
    depends_on: [impl]
    worktree: from:impl
`
	p, err := ParseSpec([]byte(spec))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if len(p.Jobs) != 4 {
		t.Fatalf("want 4 jobs, got %d", len(p.Jobs))
	}

	plan := p.Job("plan")
	if plan.Role != "orchestrator" || plan.Tier != "tier-1" || plan.Backend != "claude" || plan.Model != "opus" {
		t.Fatalf("plan job fields wrong: %+v", plan)
	}

	impl := p.Job("impl")
	if impl.Role != "worker" || impl.Tier != "tier-2" || impl.Backend != "codex" || impl.Model != "gpt-5.6-terra" {
		t.Fatalf("impl job fields wrong: %+v", impl)
	}

	test := p.Job("test")
	if test.Role != "worker" || test.Tier != "tier-3" || test.Backend != "cursor" || test.Model != "composer-2.5-fast" {
		t.Fatalf("test job fields wrong: %+v", test)
	}
}

func TestParseSpecTierValidation(t *testing.T) {
	validTiers := []string{"tier-1", "tier-2", "tier-3"}
	for _, tier := range validTiers {
		spec := "name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    tier: " + tier + "\n"
		p, err := ParseSpec([]byte(spec))
		if err != nil {
			t.Fatalf("valid tier %q rejected: %v", tier, err)
		}
		if p.Job("a").Tier != tier {
			t.Fatalf("tier %q not set, got %q", tier, p.Job("a").Tier)
		}
	}

	invalidTiers := []string{"tier-4", "tier-0", "fast", "Tier-1", "invalid"}
	for _, tier := range invalidTiers {
		spec := "name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    tier: " + tier + "\n"
		if _, err := ParseSpec([]byte(spec)); err == nil {
			t.Fatalf("expected error for invalid tier %q", tier)
		}
	}
}
