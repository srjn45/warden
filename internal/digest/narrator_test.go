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

func TestClaudeNarratorSummarizeError(t *testing.T) {
	n := ClaudeNarrator{Run: func(_ context.Context, _ string) (string, error) {
		return "x", errors.New("boom")
	}}
	if _, err := n.Summarize(context.Background(), Facts{}); err == nil {
		t.Fatal("want error propagated so the daemon can degrade to LastMessage")
	}
}
