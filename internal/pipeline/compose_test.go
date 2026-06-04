package pipeline

import (
	"strings"
	"testing"
)

func TestComposePromptWithUpstreamAndFooter(t *testing.T) {
	p := &Pipeline{ID: "refactor-auth", Name: "refactor-auth", Repo: "/r", Jobs: []Job{
		{ID: "analyze", Prompt: "look", Worktree: "none", Status: JobDone, Output: "found X", Branch: ""},
		{ID: "impl", Prompt: "do the work", DependsOn: []string{"analyze"}, Worktree: "fresh", Handoff: "the branch name"},
	}}
	out := ComposePrompt(p, p.Job("impl"))

	if !strings.Contains(out, "Upstream output — job `analyze`") || !strings.Contains(out, "found X") {
		t.Fatalf("missing upstream block:\n%s", out)
	}
	if !strings.Contains(out, "do the work") {
		t.Fatalf("missing job prompt")
	}
	if !strings.Contains(out, "agentctl pipeline emit") || !strings.Contains(out, "job `impl`") || !strings.Contains(out, "pipeline `refactor-auth`") {
		t.Fatalf("missing/incorrect footer:\n%s", out)
	}
	if !strings.Contains(out, "the branch name") {
		t.Fatalf("handoff hint not included")
	}
}

func TestComposePromptNoDepsNoUpstreamBlock(t *testing.T) {
	p := &Pipeline{ID: "p", Name: "p", Jobs: []Job{{ID: "a", Prompt: "start", Worktree: "none"}}}
	out := ComposePrompt(p, p.Job("a"))
	if strings.Contains(out, "Upstream output") {
		t.Fatalf("should have no upstream block:\n%s", out)
	}
	if !strings.Contains(out, "agentctl pipeline emit") {
		t.Fatalf("footer always present")
	}
}
