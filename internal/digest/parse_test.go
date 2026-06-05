package digest

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func parseFixture(t *testing.T, name string) Facts {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	facts, err := ParseTranscript(f)
	if err != nil {
		t.Fatalf("ParseTranscript(%s): %v", name, err)
	}
	return facts
}

func TestParseTranscriptFull(t *testing.T) {
	f := parseFixture(t, "full.jsonl")

	wantFiles := []string{"a.go", "b.go", "nb.ipynb"}
	if !reflect.DeepEqual(f.EditedFiles, wantFiles) {
		t.Errorf("EditedFiles = %v, want %v (deduped, first-seen order; Read/Bash excluded)", f.EditedFiles, wantFiles)
	}
	if f.Turns != 8 {
		t.Errorf("Turns = %d, want 8 (every assistant record)", f.Turns)
	}
	if f.Task != "Implement the foo feature" {
		t.Errorf("Task = %q, want first user prompt", f.Task)
	}
	if !strings.Contains(f.LastMessage, "All done") {
		t.Errorf("LastMessage = %q, want the final assistant TEXT (not the Bash tool_use record)", f.LastMessage)
	}
}

func TestParseTranscriptTaskSkipsToolResult(t *testing.T) {
	f := parseFixture(t, "toolresult_first.jsonl")
	if f.Task != "Real task here" {
		t.Errorf("Task = %q, want the first real prompt (tool_result-only user record skipped)", f.Task)
	}
	if !reflect.DeepEqual(f.EditedFiles, []string{"only.go"}) {
		t.Errorf("EditedFiles = %v, want [only.go]", f.EditedFiles)
	}
}

func TestParseTranscriptMalformedTolerated(t *testing.T) {
	f := parseFixture(t, "malformed.jsonl")
	if f.Task != "Fix the bug" {
		t.Errorf("Task = %q, want 'Fix the bug' despite malformed lines", f.Task)
	}
	if f.LastMessage != "Fixed it." {
		t.Errorf("LastMessage = %q, want 'Fixed it.'", f.LastMessage)
	}
	// The truncated Edit line is invalid JSON -> skipped -> no files.
	if len(f.EditedFiles) != 0 {
		t.Errorf("EditedFiles = %v, want empty (truncated edit line skipped)", f.EditedFiles)
	}
}

func TestParseTranscriptEmpty(t *testing.T) {
	f, err := ParseTranscript(strings.NewReader(""))
	if err != nil {
		t.Fatalf("empty reader should not error: %v", err)
	}
	if f.Turns != 0 || len(f.EditedFiles) != 0 || f.Task != "" || f.LastMessage != "" {
		t.Errorf("empty reader -> zero Facts, got %+v", f)
	}
}
