package digest

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNarratorPromptIncludesFacts(t *testing.T) {
	p := NarratorPrompt(Facts{
		Task:        "Build the thing",
		EditedFiles: []string{"a.go", "b.go"},
		LastMessage: "Done building.",
	})
	for _, want := range []string{"Build the thing", "a.go", "b.go", "Done building."} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q in:\n%s", want, p)
		}
	}
}

func TestClaudeNarratorSummarize(t *testing.T) {
	n := ClaudeNarrator{Run: func(_ context.Context, arg string) (string, error) {
		if !strings.Contains(arg, "Build the thing") {
			t.Errorf("Run got arg without task: %q", arg)
		}
		return "  Implemented the thing across two files.\n", nil
	}}
	got, err := n.Summarize(context.Background(), Facts{Task: "Build the thing"})
	if err != nil {
		t.Fatalf("Summarize err: %v", err)
	}
	if got != "Implemented the thing across two files." {
		t.Errorf("got %q, want trimmed single line", got)
	}
}

func TestStripPreamble(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"this-is-task + based-on-colon (live leak)",
			"This is a summarization task. Based on the agent's last message: The agent read the handoff doc and got synced.",
			"The agent read the handoff doc and got synced.",
		},
		{
			"this-is-just-task + no-skill (live leak)",
			"This is just a summarization task. No skill applies. The agent read the handoff doc and got synced on the work.",
			"The agent read the handoff doc and got synced on the work.",
		},
		{
			"here is the summary colon",
			"Here is the summary: Refactored the parser and added tests.",
			"Refactored the parser and added tests.",
		},
		{
			"instruction-is-clear colon + restated instruction (live leak)",
			"The instruction is clear: produce a 1-2 sentence plain summary of what the agent did. The agent read a handoff doc and synced itself.",
			"The agent read a handoff doc and synced itself.",
		},
		{
			"no preamble — untouched",
			"Refactored the parser and added tests.",
			"Refactored the parser and added tests.",
		},
		{
			"legit sentence mentioning a task is kept",
			"Completed the migration task and updated the docs.",
			"Completed the migration task and updated the docs.",
		},
		{
			"real summary mentioning a status summary is kept",
			"The agent gave a one-paragraph status summary covering the design, then asked the user a question.",
			"The agent gave a one-paragraph status summary covering the design, then asked the user a question.",
		},
		{
			"strip-only result would be empty — fall back to original",
			"This is a summarization task.",
			"This is a summarization task.",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripPreamble(c.in); got != c.want {
				t.Errorf("stripPreamble(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestClaudeNarratorSummarizeStripsPreamble(t *testing.T) {
	n := ClaudeNarrator{Run: func(_ context.Context, _ string) (string, error) {
		return "This is a summarization task. Based on the agent's last message: It fixed the bug.\n", nil
	}}
	got, err := n.Summarize(context.Background(), Facts{})
	if err != nil {
		t.Fatalf("Summarize err: %v", err)
	}
	if got != "It fixed the bug." {
		t.Errorf("Summarize = %q, want preamble stripped", got)
	}
}

func TestClaudeNarratorSummarizeError(t *testing.T) {
	n := ClaudeNarrator{Run: func(_ context.Context, _ string) (string, error) {
		return "x", errors.New("boom")
	}}
	if _, err := n.Summarize(context.Background(), Facts{}); err == nil {
		t.Fatal("want error propagated so the daemon can degrade to LastMessage")
	}
}
