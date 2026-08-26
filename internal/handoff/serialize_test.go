package handoff

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fullHandoff() Handoff {
	return Handoff{
		SessionID:        "agent-abc123",
		Backend:          "claude",
		Model:            "opus",
		SuccessorBackend: "antigravity",
		SuccessorModel:   "gemini-3-pro",
		Reason:           "context_fill",
		GeneratedAt:      "2026-08-26T12:00:00Z",
		Goal:             "Implement the CSV export feature.",
		Decisions:        []string{"Use a streaming parser.", "Buffer instead of mmap."},
		ModifiedFiles:    []string{"internal/export/csv.go", "internal/export/csv_test.go"},
		NextStep:         "Wire up the --format flag.",
		GitDiff:          "internal/export/csv.go\t42\t0\tinternal/export/csv.go",
		SystemContext:    "Hot-swap: claude (opus) → antigravity (gemini-3-pro)\nBranch: feat/csv",
	}
}

// TestMarkdownContainsAllSections: every required section is rendered with its data.
func TestMarkdownContainsAllSections(t *testing.T) {
	md := fullHandoff().Markdown()
	for _, want := range []string{
		"# Warden Session Handoff — agent-abc123",
		"## Goal",
		"Implement the CSV export feature.",
		"## Decisions Log",
		"- Use a streaming parser.",
		"- Buffer instead of mmap.",
		"## Modified Files",
		"- `internal/export/csv.go`",
		"### Working-tree diff (numstat)",
		"## Immediate Next Step",
		"Wire up the --format flag.",
		"## System Context",
		"Branch: feat/csv",
		"**Retired backend:** claude (opus)",
		"**Successor backend:** antigravity (gemini-3-pro)",
		"**Swap reason:** context_fill",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n---\n%s", want, md)
		}
	}
}

// TestMarkdownPlaceholdersForEmptyFields: an empty handoff still renders every
// section, with explicit placeholders rather than blank gaps.
func TestMarkdownPlaceholdersForEmptyFields(t *testing.T) {
	md := Handoff{SessionID: "x"}.Markdown()
	for _, want := range []string{
		"## Goal",
		"_No explicit goal was recorded",
		"## Decisions Log",
		"_No notable decisions were recorded._",
		"## Modified Files",
		"_No file modifications were recorded",
		"## Immediate Next Step",
		"_No explicit next step was recorded",
		"## System Context",
		"_No additional system context was provided._",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("empty-handoff markdown missing %q\n---\n%s", want, md)
		}
	}
	// No git-diff block when there is no diff.
	if strings.Contains(md, "Working-tree diff") {
		t.Errorf("empty handoff should not render a diff block")
	}
}

func TestFilenameAndPath(t *testing.T) {
	if got := Filename("agent-abc"); got != "handoff-agent-abc.md" {
		t.Fatalf("Filename = %q", got)
	}
	// Path separators in an id can never escape .warden.
	if got := Filename("../../etc/passwd"); strings.ContainsAny(got, "/\\") {
		t.Fatalf("Filename %q leaks a path separator", got)
	}
	if got := Path("/repo/wt", "agent-abc"); got != filepath.Join("/repo/wt", ".warden", "handoff-agent-abc.md") {
		t.Fatalf("Path = %q", got)
	}
}

func TestFilenameSanitizesToStableName(t *testing.T) {
	if got := Filename("!!!"); got != "handoff-session.md" {
		t.Fatalf("Filename(%q) = %q, want the stable fallback", "!!!", got)
	}
}

// TestWriteCreatesFileInWardenDir: Write lands the markdown at
// <worktree>/.warden/handoff-<id>.md and returns that path.
func TestWriteCreatesFileInWardenDir(t *testing.T) {
	dir := t.TempDir()
	h := fullHandoff()
	path, err := Write(dir, h)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := filepath.Join(dir, ".warden", "handoff-agent-abc123.md")
	if path != want {
		t.Fatalf("Write path = %q, want %q", path, want)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(body), "Implement the CSV export feature.") {
		t.Fatalf("written file missing content:\n%s", body)
	}
}

func TestWriteRejectsEmptyInputs(t *testing.T) {
	if _, err := Write("", fullHandoff()); err == nil {
		t.Errorf("Write with empty worktree should error")
	}
	if _, err := Write(t.TempDir(), Handoff{}); err == nil {
		t.Errorf("Write with empty session id should error")
	}
}
