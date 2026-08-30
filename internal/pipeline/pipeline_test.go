package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/digest"
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

// TestValidateAcceptsDelegationRoles guards the orchestrator→(planner,worker)
// delegation path: a local planner→worker pipeline (the Phase 4 ergonomics goal)
// must validate cleanly, and the legacy role aliases must keep resolving.
func TestValidateAcceptsDelegationRoles(t *testing.T) {
	p := &Pipeline{Name: "delegate", Repo: "/r", Jobs: []Job{
		{ID: "plan", Prompt: "write the spec", Worktree: "none", Role: "planner"},
		{ID: "build", Prompt: "implement the spec", DependsOn: []string{"plan"}, Worktree: "fresh", Role: "worker"},
	}}
	if err := Validate(p); err != nil {
		t.Fatalf("planner->worker pipeline rejected: %v", err)
	}

	// Every canonical/aliased role (and the empty role) must pass validation.
	for _, r := range []string{"", "general", "orchestrator", "planner", "worker", "reviewer", "implementer", "auto-merger"} {
		p := &Pipeline{Name: "ok", Repo: "/r", Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Role: r}}}
		if err := Validate(p); err != nil {
			t.Fatalf("role %q rejected: %v", r, err)
		}
	}
}

// TestValidateRejectsUnknownRole proves a typo'd role fails fast at create time
// rather than silently degrading to a personaless general agent at spawn time.
func TestValidateRejectsUnknownRole(t *testing.T) {
	p := &Pipeline{Name: "typo", Repo: "/r", Jobs: []Job{
		{ID: "a", Prompt: "x", Worktree: "none", Role: "wroker"},
	}}
	err := Validate(p)
	if err == nil || !strings.Contains(err.Error(), "wroker") || !strings.Contains(err.Error(), "role") {
		t.Fatalf("want invalid-role error mentioning the bad role, got %v", err)
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

func TestJobDigestRoundTrips(t *testing.T) {
	j := Job{ID: "impl", Prompt: "do it", Digest: &digest.Digest{Summary: "did it", Turns: 3}}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Job
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Digest == nil || got.Digest.Summary != "did it" || got.Digest.Turns != 3 {
		t.Fatalf("digest did not round-trip: %+v", got.Digest)
	}
}
