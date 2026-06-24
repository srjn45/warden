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
	if !strings.Contains(out, "warden pipeline emit") || !strings.Contains(out, "job `impl`") || !strings.Contains(out, "pipeline `refactor-auth`") {
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
	if !strings.Contains(out, "warden pipeline emit") {
		t.Fatalf("footer always present")
	}
}

func TestComposePromptNotesFailedUpstream(t *testing.T) {
	// A failure-handler must be told its upstream failed; the failed job has no
	// handoff output of its own to inject.
	p := &Pipeline{ID: "p", Name: "p", Repo: "/r", Jobs: []Job{
		{ID: "build", Prompt: "build", Worktree: "none", Status: JobFailed},
		{ID: "notify", Prompt: "alert", DependsOn: []string{"build"}, Worktree: "none", RunIf: "failure"},
	}}
	out := ComposePrompt(p, p.Job("notify"))
	if !strings.Contains(out, "job `build` FAILED") {
		t.Fatalf("failure-handler should be told its upstream failed:\n%s", out)
	}
}

func TestComposePromptInstructsCommitBeforeEmit(t *testing.T) {
	p := &Pipeline{ID: "p", Name: "p", Jobs: []Job{{ID: "a", Prompt: "do x", Worktree: "fresh"}}}
	out := ComposePrompt(p, p.Job("a"))
	// The pipeline chains a job's branch downstream by commit, so the footer must
	// tell the agent to commit its work before emitting.
	if !strings.Contains(strings.ToLower(out), "commit") {
		t.Fatalf("footer must instruct the agent to commit before emitting:\n%s", out)
	}
}
