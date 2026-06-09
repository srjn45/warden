# agentctl Completion Digest Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an on-demand "completion digest" — `agentctl digest <id>`, a web Digest button, and a TUI `d` key — that summarizes what an agent accomplished (narrative + files-touched + branch/turns/status), pulled from the deterministic transcript and the agent's cwd.

**Architecture:** A new pure `internal/digest` package parses the transcript JSONL into deterministic `Facts`, parses `git diff --numstat`, and merges them into a `Digest`. A best-effort LLM narrative comes from a `Narrator` interface (real impl shells `claude -p` via lifecycle's existing plumbing; degrades to the last assistant message on failure). The daemon exposes `GET /sessions/{id}/digest`; CLI, web, and TUI consume it.

**Tech Stack:** Go (chi router, cobra CLI, bubbletea TUI), React + TypeScript + vitest (web). Repo: `github.com/srajanpathak/agentctl`. No origin remote — worktree baseRef=head.

**Design spec:** `docs/superpowers/specs/2026-06-05-agentctl-completion-digest-design.md`

---

## File Structure

**New files:**
- `internal/digest/digest.go` — types (`Facts`, `FileChange`, `Digest`, `LineDelta`) + `Narrator` interface.
- `internal/digest/parse.go` — `ParseTranscript(io.Reader) (Facts, error)` (pure).
- `internal/digest/numstat.go` — `ParseNumstat(string)` + `MergeFiles(...)` (pure).
- `internal/digest/narrator.go` — `ClaudeNarrator` + `NarratorPrompt(Facts) string`.
- `internal/digest/parse_test.go`, `numstat_test.go`, `narrator_test.go`
- `internal/digest/testdata/*.jsonl` — fixture transcripts.
- `internal/daemon/digest_routes.go` — `handleDigest` + route registration.
- `internal/daemon/digest_routes_test.go`
- `internal/cli/digest.go` + `internal/cli/digest_test.go`
- `web/src/lib/digest.ts` + `web/src/lib/digest.test.ts`
- `internal/tui/digest.go` (render) — tests in `internal/tui/digest_test.go`

**Modified files:**
- `internal/lifecycle/lifecycle.go` — export `TranscriptPath`, add `GitBranch`, `GitNumstat`, `RunClaudeP`.
- `internal/daemon/api.go` — extend `Lifecycle` interface (3 methods), add `narrator` field + `SetNarrator`, wire route.
- `internal/daemon/lifecycle_adapter.go` — forward the 3 new methods.
- `internal/daemon/*_test.go` — `fakeLife` stubs for the 3 new interface methods.
- `internal/client/client.go` — `Digest(ctx, id)` method.
- `internal/cli/root.go` — register `newDigestCmd()`.
- `internal/cli/daemon.go` — `srv.SetNarrator(...)`.
- `web/src/lib/types.ts` — `Digest` + `FileChange` types.
- `web/src/lib/api.ts` — `getDigest(id)`.
- `web/src/components/AgentTab.tsx` — Digest button + panel.
- `internal/tui/model.go` — digest model fields + `api` interface method.
- `internal/tui/cmds.go` — `digestMsg` + `digestCmd`.
- `internal/tui/keys.go` — `d` key.
- `internal/tui/view.go` — render digest in detail pane.

---

### Task 1: `internal/digest` types + `ParseTranscript`

**Files:**
- Create: `internal/digest/digest.go`
- Create: `internal/digest/parse.go`
- Create: `internal/digest/testdata/full.jsonl`
- Create: `internal/digest/testdata/toolresult_first.jsonl`
- Create: `internal/digest/testdata/malformed.jsonl`
- Test: `internal/digest/parse_test.go`

- [ ] **Step 1: Create the fixture transcripts**

`internal/digest/testdata/full.jsonl` (each line is one JSON record; copy exactly — note the `Bash`/`Read` tool_use must be ignored, and `a.go` appears twice to test dedup):

```jsonl
{"type":"last-prompt","prompt":"ignore me"}
{"type":"custom-title","title":"x"}
{"type":"attachment","data":"x"}
{"type":"file-history-snapshot","snapshot":{}}
{"type":"user","message":{"role":"user","content":"Implement the foo feature"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Sure, starting now."}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"readonly.go"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"a.go","content":"package a"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"a.go","old_string":"a","new_string":"b"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"MultiEdit","input":{"file_path":"b.go","edits":[]}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"NotebookEdit","input":{"notebook_path":"nb.ipynb","new_source":"x"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"system","subtype":"x"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"All done — added a.go, b.go, nb.ipynb."}]}}
```

`internal/digest/testdata/toolresult_first.jsonl` (first user record is a tool_result, so Task must come from the second user record):

```jsonl
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t0","content":"resume context"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Real task here"}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Write","input":{"file_path":"only.go","content":"x"}}]}}
```

`internal/digest/testdata/malformed.jsonl` (broken + truncated lines are skipped; valid ones still parse):

```jsonl
{"type":"user","message":{"role":"user","content":"Fix the bug"}}
{not valid json at all
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"fix.go"
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed it."}]}}
```

- [ ] **Step 2: Write the failing test**

`internal/digest/parse_test.go`:

```go
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
	if f.Turns != 7 {
		t.Errorf("Turns = %d, want 7 (every assistant record)", f.Turns)
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
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/digest/...`
Expected: FAIL — `undefined: ParseTranscript` / `Facts`.

- [ ] **Step 4: Write `internal/digest/digest.go`**

```go
// Package digest builds an on-demand completion digest for an agent: a
// deterministic set of facts parsed from the Claude Code transcript, merged
// with git change stats, and enriched with a best-effort LLM narrative.
package digest

import "context"

// Facts are the deterministic signals parsed from a transcript. Pure output of
// ParseTranscript — no filesystem, no subprocess.
type Facts struct {
	EditedFiles []string // unique Write/Edit/MultiEdit/NotebookEdit targets, first-seen order
	Turns       int      // count of assistant records
	Task        string   // first real user prompt
	LastMessage string   // last assistant text (deterministic summary fallback)
}

// FileChange is one entry in a digest's file list.
type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`   // git --numstat added lines; 0 when cwd isn't a repo
	Removed int    `json:"removed"` // git --numstat removed lines
	Edited  bool   `json:"edited"`  // appeared as an edit-tool target in the transcript
}

// Digest is the wire payload returned by GET /sessions/{id}/digest and consumed
// by the CLI, web, and TUI.
type Digest struct {
	Summary string       `json:"summary"` // LLM narrative, or LastMessage on fallback
	Files   []FileChange `json:"files"`
	Branch  string       `json:"branch"` // "" when cwd isn't a git repo
	Turns   int          `json:"turns"`
	Status  string       `json:"status"` // current agentctl status, set by the daemon
	Task    string       `json:"task"`
}

// LineDelta is the +/- pair for one file from git --numstat.
type LineDelta struct {
	Added, Removed int
}

// Narrator turns deterministic facts into a short natural-language summary. The
// real impl shells `claude -p`; the daemon degrades to Facts.LastMessage on error.
type Narrator interface {
	Summarize(ctx context.Context, f Facts) (string, error)
}
```

- [ ] **Step 5: Write `internal/digest/parse.go`**

```go
package digest

import (
	"bufio"
	"encoding/json"
	"io"
)

// record is the minimal shape we read from each JSONL line. Most transcript
// records (last-prompt, attachment, system, file-history-snapshot, …) are not
// conversation turns and are skipped via the top-level Type discriminator.
type record struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"` // string OR []block
	} `json:"message"`
}

type block struct {
	Type  string          `json:"type"`  // "text" | "tool_use" | "tool_result"
	Text  string          `json:"text"`  // for type=="text"
	Name  string          `json:"name"`  // for type=="tool_use"
	Input json.RawMessage `json:"input"` // for type=="tool_use"
}

type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
}

var editTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}

// ParseTranscript reads a Claude Code transcript JSONL stream and returns
// deterministic Facts. Malformed lines are skipped (not fatal); only a reader
// error is returned.
func ParseTranscript(r io.Reader) (Facts, error) {
	var f Facts
	seen := map[string]bool{}

	sc := bufio.NewScanner(r)
	// Transcript lines (esp. tool_result payloads) can be very long; raise the
	// scanner's per-line cap well above the 64K default.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // malformed line — skip
		}
		switch rec.Type {
		case "assistant":
			f.Turns++
			text, files := assistantParts(rec.Message.Content)
			for _, p := range files {
				if p != "" && !seen[p] {
					seen[p] = true
					f.EditedFiles = append(f.EditedFiles, p)
				}
			}
			if text != "" {
				f.LastMessage = text // keep the last assistant text seen
			}
		case "user":
			if f.Task != "" {
				continue
			}
			if t := userPrompt(rec.Message.Content); t != "" {
				f.Task = t
			}
		}
	}
	if err := sc.Err(); err != nil {
		return f, err
	}
	return f, nil
}

// assistantParts returns the concatenated text and any edit-tool file targets in
// an assistant message. content is either a JSON string or a list of blocks.
func assistantParts(content json.RawMessage) (text string, files []string) {
	if s, ok := asString(content); ok {
		return s, nil
	}
	var blocks []block
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", nil
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text += b.Text
		case "tool_use":
			if editTools[b.Name] {
				var in toolInput
				_ = json.Unmarshal(b.Input, &in)
				p := in.FilePath
				if p == "" {
					p = in.NotebookPath
				}
				files = append(files, p)
			}
		}
	}
	return text, files
}

// userPrompt returns the prompt text of a user message, or "" if the record is
// only tool_result blocks (not an actual prompt).
func userPrompt(content json.RawMessage) string {
	if s, ok := asString(content); ok {
		return s
	}
	var blocks []block
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var text string
	for _, b := range blocks {
		if b.Type == "text" {
			text += b.Text
		}
	}
	return text
}

// asString reports whether raw is a JSON string and returns its value.
func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
```

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/digest/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/digest/digest.go internal/digest/parse.go internal/digest/parse_test.go internal/digest/testdata
git commit -m "feat(digest): pure transcript parser -> Facts"
```

---

### Task 2: numstat parser + merge

**Files:**
- Create: `internal/digest/numstat.go`
- Test: `internal/digest/numstat_test.go`

- [ ] **Step 1: Write the failing test**

`internal/digest/numstat_test.go`:

```go
package digest

import (
	"reflect"
	"testing"
)

func TestParseNumstat(t *testing.T) {
	in := "3\t1\ta.go\n0\t5\tb.go\n-\t-\timg.png\nbogus line\n"
	got := ParseNumstat(in)
	want := map[string]LineDelta{
		"a.go":    {Added: 3, Removed: 1},
		"b.go":    {Added: 0, Removed: 5},
		"img.png": {Added: 0, Removed: 0}, // binary file: "-" -> 0, still present
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseNumstat = %v, want %v", got, want)
	}
}

func TestMergeFilesUnion(t *testing.T) {
	// a.go edited+changed; reverted.go edited but reverted (no numstat row);
	// sideeffect.go changed by git only (e.g. a formatter), never an edit target.
	edited := []string{"a.go", "reverted.go"}
	stats := map[string]LineDelta{
		"a.go":         {Added: 3, Removed: 1},
		"sideeffect.go": {Added: 2, Removed: 0},
	}
	got := MergeFiles(edited, stats)
	want := []FileChange{
		{Path: "a.go", Added: 3, Removed: 1, Edited: true},
		{Path: "reverted.go", Added: 0, Removed: 0, Edited: true},
		{Path: "sideeffect.go", Added: 2, Removed: 0, Edited: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeFiles = %+v, want %+v", got, want)
	}
}

func TestMergeFilesNoGit(t *testing.T) {
	got := MergeFiles([]string{"x.go", "y.go"}, nil)
	want := []FileChange{
		{Path: "x.go", Edited: true},
		{Path: "y.go", Edited: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeFiles(no git) = %+v, want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/digest/ -run 'Numstat|MergeFiles'`
Expected: FAIL — `undefined: ParseNumstat`.

- [ ] **Step 3: Write `internal/digest/numstat.go`**

```go
package digest

import (
	"sort"
	"strconv"
	"strings"
)

// ParseNumstat parses `git diff --numstat` output into a path -> LineDelta map.
// Rows are "added\tremoved\tpath"; binary files use "-" which maps to 0. Rows
// that don't have three tab fields are skipped.
func ParseNumstat(out string) map[string]LineDelta {
	res := map[string]LineDelta{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			continue
		}
		res[fields[2]] = LineDelta{Added: atoiDash(fields[0]), Removed: atoiDash(fields[1])}
	}
	return res
}

func atoiDash(s string) int {
	if s == "-" {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// MergeFiles unions transcript-edited files (authoritative for WHICH files,
// kept in first-seen order) with git-changed files (annotated +/-). Edited files
// come first in transcript order; git-only files follow, sorted for determinism.
func MergeFiles(edited []string, stats map[string]LineDelta) []FileChange {
	editedSet := map[string]bool{}
	var out []FileChange
	for _, p := range edited {
		editedSet[p] = true
		d := stats[p] // zero value when absent (reverted / non-repo)
		out = append(out, FileChange{Path: p, Added: d.Added, Removed: d.Removed, Edited: true})
	}
	var gitOnly []string
	for p := range stats {
		if !editedSet[p] {
			gitOnly = append(gitOnly, p)
		}
	}
	sort.Strings(gitOnly)
	for _, p := range gitOnly {
		d := stats[p]
		out = append(out, FileChange{Path: p, Added: d.Added, Removed: d.Removed, Edited: false})
	}
	return out
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/digest/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/digest/numstat.go internal/digest/numstat_test.go
git commit -m "feat(digest): numstat parse + transcript∪git merge"
```

---

### Task 3: `ClaudeNarrator` + prompt builder

**Files:**
- Create: `internal/digest/narrator.go`
- Test: `internal/digest/narrator_test.go`

- [ ] **Step 1: Write the failing test**

`internal/digest/narrator_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/digest/ -run Narrator`
Expected: FAIL — `undefined: NarratorPrompt` / `ClaudeNarrator`.

- [ ] **Step 3: Write `internal/digest/narrator.go`**

```go
package digest

import (
	"context"
	"fmt"
	"strings"
)

// ClaudeNarrator is the real Narrator: it shells `claude -p` through the Run
// func (wired to lifecycle's bounded claude -p plumbing). Run is the only seam,
// so tests inject a canned function and stay offline.
type ClaudeNarrator struct {
	Run func(ctx context.Context, arg string) (string, error)
}

// Summarize asks the model for a 1–2 sentence "what this agent did" line.
func (n ClaudeNarrator) Summarize(ctx context.Context, f Facts) (string, error) {
	out, err := n.Run(ctx, NarratorPrompt(f))
	if err != nil {
		return "", err
	}
	return cleanLine(out), nil
}

// NarratorPrompt builds the compact prompt from deterministic facts.
func NarratorPrompt(f Facts) string {
	var b strings.Builder
	b.WriteString("You are summarizing what a coding agent accomplished. ")
	b.WriteString("In 1-2 sentences, plainly describe what it did. Reply with ONLY the summary.\n\n")
	if f.Task != "" {
		fmt.Fprintf(&b, "Task: %s\n", f.Task)
	}
	if len(f.EditedFiles) > 0 {
		fmt.Fprintf(&b, "Files edited: %s\n", strings.Join(f.EditedFiles, ", "))
	}
	if f.LastMessage != "" {
		fmt.Fprintf(&b, "Agent's last message: %s\n", f.LastMessage)
	}
	return b.String()
}

// cleanLine collapses a model reply to a trimmed single paragraph.
func cleanLine(out string) string {
	s := strings.TrimSpace(out)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/digest/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/digest/narrator.go internal/digest/narrator_test.go
git commit -m "feat(digest): ClaudeNarrator + prompt builder"
```

---

### Task 4: lifecycle — export transcript path, git helpers, claude -p

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/digest_helpers_test.go` (new)

These thin exports let the daemon resolve the transcript and read git without
duplicating lifecycle's path-encoding and subprocess plumbing.

- [ ] **Step 1: Write the failing test**

`internal/lifecycle/digest_helpers_test.go`:

```go
package lifecycle

import (
	"context"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
)

func TestGitBranchAndNumstat(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"git rev-parse --abbrev-ref HEAD": {Out: "feature/x\n"},
		"git diff --numstat":              {Out: "1\t2\tf.go\n"},
	}}
	l := New(fr)
	if b := l.GitBranch(context.Background(), "/repo"); b != "feature/x" {
		t.Errorf("GitBranch = %q, want feature/x (trimmed)", b)
	}
	if ns := l.GitNumstat(context.Background(), "/repo"); ns != "1\t2\tf.go\n" {
		t.Errorf("GitNumstat = %q", ns)
	}
}

func TestGitBranchErrorEmpty(t *testing.T) {
	fr := &FakeRunner{FailIf: func(argv []string) error { return context.Canceled }}
	l := New(fr)
	if b := l.GitBranch(context.Background(), "/notrepo"); b != "" {
		t.Errorf("GitBranch on error = %q, want empty", b)
	}
}

func TestTranscriptPathExportedWrapper(t *testing.T) {
	// No ProjectsDir set -> lookup disabled -> "".
	l := New(&FakeRunner{})
	if p := l.TranscriptPath(&store.Session{ClaudeSessionID: "abc"}); p != "" {
		t.Errorf("TranscriptPath with no ProjectsDir = %q, want empty", p)
	}
}

func TestRunClaudePExported(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{"claude -p hello": {Out: "world"}}}
	l := New(fr)
	out, err := l.RunClaudeP(context.Background(), "hello")
	if err != nil || out != "world" {
		t.Fatalf("RunClaudeP = (%q,%v), want (world,nil)", out, err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/lifecycle/ -run 'Git|Transcript|RunClaudeP'`
Expected: FAIL — `undefined: (*Lifecycle).GitBranch` etc.

- [ ] **Step 3: Add the exported methods to `internal/lifecycle/lifecycle.go`**

Add after the existing `transcriptPath` method (around line 317). `strings` is already imported.

```go
// TranscriptPath is the exported accessor the daemon uses to resolve an agent's
// transcript file (see transcriptPath). Returns "" when unresolved/disabled.
func (l *Lifecycle) TranscriptPath(sess *store.Session) string {
	return l.transcriptPath(sess)
}

// GitBranch returns the current branch name for dir, or "" on any error /
// non-repo. Best-effort: used only to annotate a digest.
func (l *Lifecycle) GitBranch(ctx context.Context, dir string) string {
	out, err := l.run.Run(ctx, dir, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// GitNumstat returns raw `git diff --numstat` output for dir, or "" on error.
func (l *Lifecycle) GitNumstat(ctx context.Context, dir string) string {
	out, err := l.run.Run(ctx, dir, "git", "diff", "--numstat")
	if err != nil {
		return ""
	}
	return out
}

// RunClaudeP exposes the bounded headless `claude -p` runner (the same plumbing
// Classify/Summarize use) so the digest Narrator can reuse it.
func (l *Lifecycle) RunClaudeP(ctx context.Context, arg string) (string, error) {
	return l.runClaudeP(ctx, arg)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/lifecycle/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/digest_helpers_test.go
git commit -m "feat(lifecycle): export TranscriptPath + GitBranch/GitNumstat/RunClaudeP for digest"
```

---

### Task 5: daemon Lifecycle interface + adapter + narrator field

**Files:**
- Modify: `internal/daemon/api.go` (interface + Server field + setter)
- Modify: `internal/daemon/lifecycle_adapter.go` (forward 3 methods)
- Modify: `internal/daemon/lifecycle_routes_test.go` (fakeLife stubs — find where `fakeLife` is defined; add stubs there)

- [ ] **Step 1: Extend the `Lifecycle` interface in `internal/daemon/api.go`**

Add these methods inside the `Lifecycle interface` block (after `SpawnJob`, before the closing brace) and add the `"io"`-free `store` usage (store already imported). Add `"github.com/srajanpathak/agentctl/internal/digest"` to the import block.

```go
	// TranscriptPath resolves the agent's transcript file ("" when none).
	TranscriptPath(sess *store.Session) string
	// GitBranch / GitNumstat read git state in dir (best-effort, "" on error).
	GitBranch(ctx context.Context, dir string) string
	GitNumstat(ctx context.Context, dir string) string
```

Add a `narrator` field to the `Server` struct (after `exec *Executor`):

```go
	// narrator produces the digest's LLM summary (nil ⇒ degrade to LastMessage).
	narrator digest.Narrator
```

Add a setter near `SetExecutor`:

```go
// SetNarrator wires the digest narrator after construction (optional; nil ⇒ the
// digest summary degrades to the agent's last transcript message).
func (s *Server) SetNarrator(n digest.Narrator) { s.narrator = n }
```

- [ ] **Step 2: Forward the new methods in `internal/daemon/lifecycle_adapter.go`**

```go
func (a *lifecycleAdapter) TranscriptPath(sess *store.Session) string {
	return a.lc.TranscriptPath(sess)
}

func (a *lifecycleAdapter) GitBranch(ctx context.Context, dir string) string {
	return a.lc.GitBranch(ctx, dir)
}

func (a *lifecycleAdapter) GitNumstat(ctx context.Context, dir string) string {
	return a.lc.GitNumstat(ctx, dir)
}
```

- [ ] **Step 3: Add stubs to `fakeLife` so existing tests still compile**

Find the `fakeLife` struct definition: `grep -rn "fakeLife" internal/daemon/*_test.go | head`. Add these methods next to its other methods (in whichever `_test.go` defines them). They return zero values — the digest handler test (Task 6) uses a REAL adapter, not fakeLife.

```go
func (f *fakeLife) TranscriptPath(sess *store.Session) string          { return "" }
func (f *fakeLife) GitBranch(ctx context.Context, dir string) string   { return "" }
func (f *fakeLife) GitNumstat(ctx context.Context, dir string) string  { return "" }
```

(If `fakeLife` is split across files or there are multiple fakes implementing `Lifecycle`, add the three stubs to each. `go build ./internal/daemon/...` will name any missing one.)

- [ ] **Step 4: Verify it compiles**

Run: `go build ./internal/daemon/... && go test ./internal/daemon/... -run TestGetOutput`
Expected: PASS (existing tests unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/daemon/*_test.go
git commit -m "feat(daemon): Lifecycle digest methods + Server.narrator seam"
```

---

### Task 6: daemon `GET /sessions/{id}/digest` handler

**Files:**
- Create: `internal/daemon/digest_routes.go`
- Modify: `internal/daemon/api.go` (`router()` — register route)
- Test: `internal/daemon/digest_routes_test.go`

- [ ] **Step 1: Register the route in `router()` (api.go)**

Add after the `/sessions/{id}/approve` line:

```go
	r.Get("/sessions/{id}/digest", s.handleDigest)
```

- [ ] **Step 2: Write the failing test**

`internal/daemon/digest_routes_test.go`. This builds a REAL lifecycle adapter (so real git + transcript-path resolution run) over temp dirs, plus a fake Narrator on the Server.

```go
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/store"
)

type fakeNarrator struct {
	out string
	err error
}

func (n fakeNarrator) Summarize(_ context.Context, _ digest.Facts) (string, error) {
	return n.out, n.err
}

// digestEnv wires a Server with a real lifecycle adapter pointed at a temp
// transcript root + a temp git repo as the session workdir.
func digestEnv(t *testing.T, transcript string, narrator digest.Narrator) (*httptest.Server, string) {
	t.Helper()
	projects := t.TempDir()
	work := t.TempDir()

	// init a real git repo with one committed + one unstaged change.
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "feature/digest")
	runGit("config", "user.email", "t@t")
	runGit("config", "user.name", "t")
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n"), 0o644)
	runGit("add", "a.go")
	runGit("commit", "-m", "init")
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n// edit\n"), 0o644)

	// Place the transcript where TranscriptPath will find it: <projects>/<glob>/<id>.jsonl.
	encDir := filepath.Join(projects, "encoded")
	os.MkdirAll(encDir, 0o755)
	claudeID := "11111111-1111-1111-1111-111111111111"
	if transcript != "" {
		os.WriteFile(filepath.Join(encDir, claudeID+".jsonl"), []byte(transcript), 0o644)
	}

	lc := lifecycle.New(lifecycle.ExecRunner{})
	lc.ProjectsDir = projects
	fs := newFakeStore()
	fs.data["agent-d1"] = &store.Session{
		ID: "agent-d1", Workdir: work, ClaudeSessionID: claudeID, Status: store.Status("working"),
	}
	srv := &Server{store: fs, life: NewLifecycleAdapter(lc, fs), narrator: narrator}
	return httptest.NewServer(srv.router()), claudeID
}

const digestTranscript = `{"type":"user","message":{"role":"user","content":"Edit a.go"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"a.go","old_string":"x","new_string":"y"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Edited a.go."}]}}
`

func getDigest(t *testing.T, ts *httptest.Server, id string) (*digest.Digest, int) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/sessions/" + id + "/digest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var d digest.Digest
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &d, resp.StatusCode
}

func TestDigestHappyPath(t *testing.T) {
	ts, _ := digestEnv(t, digestTranscript, fakeNarrator{out: "Edited a.go to add a comment."})
	defer ts.Close()
	d, code := getDigest(t, ts, "agent-d1")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if d.Summary != "Edited a.go to add a comment." {
		t.Errorf("Summary = %q (want narrator output)", d.Summary)
	}
	if d.Branch != "feature/digest" {
		t.Errorf("Branch = %q, want feature/digest", d.Branch)
	}
	if d.Turns != 2 || d.Task != "Edit a.go" || d.Status != "working" {
		t.Errorf("facts wrong: %+v", d)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "a.go" || !d.Files[0].Edited || d.Files[0].Added == 0 {
		t.Errorf("Files = %+v, want a.go edited with +lines", d.Files)
	}
}

func TestDigestNarratorFailureDegrades(t *testing.T) {
	ts, _ := digestEnv(t, digestTranscript, fakeNarrator{err: errors.New("claude down")})
	defer ts.Close()
	d, _ := getDigest(t, ts, "agent-d1")
	if d.Summary != "Edited a.go." {
		t.Errorf("Summary = %q, want fallback to LastMessage", d.Summary)
	}
}

func TestDigestMissingTranscript(t *testing.T) {
	ts, _ := digestEnv(t, "", fakeNarrator{out: "unused"})
	defer ts.Close()
	d, code := getDigest(t, ts, "agent-d1")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if d.Summary != "no transcript available" || d.Status != "working" {
		t.Errorf("missing-transcript digest = %+v", d)
	}
}

func TestDigestUnknownSession(t *testing.T) {
	ts, _ := digestEnv(t, digestTranscript, fakeNarrator{out: "x"})
	defer ts.Close()
	_, code := getDigest(t, ts, "nope")
	if code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", code)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/daemon/ -run TestDigest`
Expected: FAIL — `s.handleDigest` undefined.

- [ ] **Step 4: Write `internal/daemon/digest_routes.go`**

```go
package daemon

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/store"
)

// handleDigest builds an on-demand completion digest for one agent: deterministic
// transcript facts ∪ git change stats, enriched with a best-effort LLM summary
// that degrades to the last assistant message on any narrator failure.
func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	d := digest.Digest{Status: string(sess.Status)}

	path := s.life.TranscriptPath(sess)
	if path == "" {
		d.Summary = "no transcript available"
		writeJSON(w, http.StatusOK, d)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		d.Summary = "no transcript available"
		writeJSON(w, http.StatusOK, d)
		return
	}
	defer f.Close()

	facts, _ := digest.ParseTranscript(f) // malformed lines tolerated inside
	stats := digest.ParseNumstat(s.life.GitNumstat(r.Context(), sess.Workdir))

	d.Files = digest.MergeFiles(facts.EditedFiles, stats)
	d.Branch = s.life.GitBranch(r.Context(), sess.Workdir)
	d.Turns = facts.Turns
	d.Task = facts.Task

	// Deterministic fallback first; the narrator only enriches.
	d.Summary = facts.LastMessage
	if s.narrator != nil {
		if out, err := s.narrator.Summarize(r.Context(), facts); err == nil && strings.TrimSpace(out) != "" {
			d.Summary = out
		}
	}
	writeJSON(w, http.StatusOK, d)
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/daemon/ -run TestDigest`
Expected: PASS (all four cases).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/digest_routes.go internal/daemon/api.go internal/daemon/digest_routes_test.go
git commit -m "feat(daemon): GET /sessions/{id}/digest handler"
```

---

### Task 7: client `Digest` method

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/digest_test.go` (new)

- [ ] **Step 1: Write the failing test**

`internal/client/digest_test.go`:

```go
package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientDigest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/agent-1/digest" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"summary":"did stuff","branch":"main","turns":3,"status":"idle","files":[{"path":"a.go","added":2,"removed":1,"edited":true}]}`))
	}))
	defer ts.Close()

	d, err := New(ts.URL).Digest(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if d.Summary != "did stuff" || d.Branch != "main" || d.Turns != 3 || d.Status != "idle" {
		t.Errorf("digest = %+v", d)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "a.go" || d.Files[0].Added != 2 {
		t.Errorf("files = %+v", d.Files)
	}
}

func TestClientDigestNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"session not found"}`))
	}))
	defer ts.Close()
	_, err := New(ts.URL).Digest(context.Background(), "x")
	var se *StatusError
	if err == nil || !errorAs(err, &se) || se.Code != 404 {
		t.Fatalf("want 404 StatusError, got %v", err)
	}
}
```

If `errorAs` isn't already a helper in the package, replace its use with the standard library: add `import "errors"` and use `errors.As(err, &se)` directly.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/client/ -run TestClientDigest`
Expected: FAIL — `(*Client).Digest` undefined.

- [ ] **Step 3: Add the method to `internal/client/client.go`**

Add `"github.com/srajanpathak/agentctl/internal/digest"` to imports, then:

```go
// Digest fetches an agent's completion digest. Uses longTimeout because the
// daemon's narrator (claude -p) dominates latency.
func (c *Client) Digest(ctx context.Context, id string) (*digest.Digest, error) {
	var d digest.Digest
	if err := c.doT(ctx, longTimeout, http.MethodGet, "/sessions/"+id+"/digest", nil, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/client/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/digest_test.go
git commit -m "feat(client): Digest(id) method"
```

---

### Task 8: CLI `agentctl digest <id>`

**Files:**
- Create: `internal/cli/digest.go`
- Modify: `internal/cli/root.go` (register)
- Test: `internal/cli/digest_test.go`

- [ ] **Step 1: Write the failing test**

`internal/cli/digest_test.go`:

```go
package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/digest"
)

func sampleDigest() *digest.Digest {
	return &digest.Digest{
		Summary: "Refactored the parser and added tests.",
		Branch:  "feature/x",
		Turns:   12,
		Status:  "idle",
		Task:    "Refactor parser",
		Files: []digest.FileChange{
			{Path: "parse.go", Added: 40, Removed: 12, Edited: true},
			{Path: "fmt.go", Added: 2, Removed: 0, Edited: false},
		},
	}
}

func TestFormatDigestHuman(t *testing.T) {
	out := formatDigest(sampleDigest())
	for _, want := range []string{
		"Refactored the parser", "feature/x", "idle", "12",
		"parse.go", "+40", "-12", "fmt.go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatDigestNoFiles(t *testing.T) {
	out := formatDigest(&digest.Digest{Summary: "Nothing changed yet.", Status: "working"})
	if !strings.Contains(out, "no files") {
		t.Errorf("want a 'no files' line, got:\n%s", out)
	}
}

func TestDigestJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := printJSON(&buf, sampleDigest()); err != nil {
		t.Fatal(err)
	}
	var back digest.Digest
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	if back.Branch != "feature/x" || len(back.Files) != 2 {
		t.Errorf("round-trip lost data: %+v", back)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run Digest`
Expected: FAIL — `formatDigest` undefined.

- [ ] **Step 3: Write `internal/cli/digest.go`**

```go
package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/srajanpathak/agentctl/internal/digest"
)

func newDigestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "digest <TICKET>",
		Short: "Summarize what an agent accomplished (files, branch, turns, narrative)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := clientFor(cmd).Digest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), d)
			}
			fmt.Fprint(cmd.OutOrStdout(), formatDigest(d))
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}

// formatDigest renders the human layout: summary paragraph, file table with
// +/- columns, then branch / turns / status.
func formatDigest(d *digest.Digest) string {
	var b strings.Builder
	if d.Summary != "" {
		b.WriteString(d.Summary)
		b.WriteString("\n\n")
	}
	if len(d.Files) == 0 {
		b.WriteString("files: (no files touched)\n")
	} else {
		b.WriteString("files:\n")
		for _, f := range d.Files {
			mark := " "
			if f.Edited {
				mark = "*"
			}
			fmt.Fprintf(&b, "  %s %-40s +%-4d -%-4d\n", mark, f.Path, f.Added, f.Removed)
		}
	}
	branch := d.Branch
	if branch == "" {
		branch = "—"
	}
	fmt.Fprintf(&b, "\nbranch: %s   turns: %d   status: %s\n", branch, d.Turns, d.Status)
	return b.String()
}
```

- [ ] **Step 4: Register the command in `internal/cli/root.go`**

Add `newDigestCmd()` to an existing `root.AddCommand(...)` call, e.g. next to `newStatusCmd()`:

```go
	root.AddCommand(newLsCmd(), newStatusCmd(), newDigestCmd())
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/cli/... && go run ./cmd/agentctl digest --help`
Expected: tests PASS; `--help` shows the digest command.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/digest.go internal/cli/root.go internal/cli/digest_test.go
git commit -m "feat(cli): agentctl digest <id> (human + --json)"
```

---

### Task 9: wire the real Narrator in the daemon bootstrap

**Files:**
- Modify: `internal/cli/daemon.go`

- [ ] **Step 1: Wire `SetNarrator` after `NewServer`**

In `internal/cli/daemon.go`, after `srv := daemon.NewServer(...)` (line ~66), add:

```go
				srv.SetNarrator(digest.ClaudeNarrator{Run: lc.RunClaudeP})
```

Add the import `"github.com/srajanpathak/agentctl/internal/digest"`.

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: builds clean.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/daemon.go
git commit -m "feat(daemon): wire ClaudeNarrator into digest endpoint"
```

---

### Task 10: web — Digest button + panel

**Files:**
- Create: `web/src/lib/digest.ts`
- Test: `web/src/lib/digest.test.ts`
- Modify: `web/src/lib/types.ts`
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/components/AgentTab.tsx`

- [ ] **Step 1: Add types to `web/src/lib/types.ts`**

```typescript
export interface FileChange {
  path: string;
  added: number;
  removed: number;
  edited: boolean;
}

export interface Digest {
  summary: string;
  files: FileChange[] | null;
  branch: string;
  turns: number;
  status: string;
  task: string;
}
```

- [ ] **Step 2: Write the failing test for the pure formatter**

`web/src/lib/digest.test.ts`:

```typescript
import { describe, it, expect } from 'vitest';
import { fileLabel, hasFiles } from './digest';
import type { Digest } from './types';

const base: Digest = {
  summary: 's', files: [], branch: 'main', turns: 1, status: 'idle', task: 't',
};

describe('digest formatting', () => {
  it('formats a file with +/- and edit marker', () => {
    expect(fileLabel({ path: 'a.go', added: 3, removed: 1, edited: true }))
      .toBe('* a.go  +3 -1');
  });
  it('uses a space marker for non-edited (git-only) files', () => {
    expect(fileLabel({ path: 'b.go', added: 0, removed: 2, edited: false }))
      .toBe('  b.go  +0 -2');
  });
  it('hasFiles is false for null or empty', () => {
    expect(hasFiles({ ...base, files: null })).toBe(false);
    expect(hasFiles({ ...base, files: [] })).toBe(false);
    expect(hasFiles({ ...base, files: [{ path: 'x', added: 0, removed: 0, edited: true }] })).toBe(true);
  });
});
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd web && npx vitest run src/lib/digest.test.ts`
Expected: FAIL — cannot resolve `./digest`.

- [ ] **Step 4: Write `web/src/lib/digest.ts`**

```typescript
import type { Digest, FileChange } from './types';

export function fileLabel(f: FileChange): string {
  const mark = f.edited ? '*' : ' ';
  return `${mark} ${f.path}  +${f.added} -${f.removed}`;
}

export function hasFiles(d: Digest): boolean {
  return !!d.files && d.files.length > 0;
}
```

- [ ] **Step 5: Add `getDigest` to `web/src/lib/api.ts`**

Mirror an existing GET helper (e.g. the one that calls `/sessions/{id}/output`). Add:

```typescript
import type { Digest } from './types';

export async function getDigest(id: string): Promise<Digest> {
  const res = await fetch(`/sessions/${id}/digest`);
  return parse<Digest>(res);
}
```

(Use whatever `parse<T>`/base-URL convention the existing helpers in this file use — match them exactly; the snippet above assumes the existing `parse<T>(res)` helper.)

- [ ] **Step 6: Add the Digest button + panel to `web/src/components/AgentTab.tsx`**

Add state + handler mirroring `TerminateControls` (busy/err pattern) and render a panel:

```tsx
import { useState } from 'react';
import { getDigest } from '../lib/api';
import { fileLabel, hasFiles } from '../lib/digest';
import type { Digest } from '../lib/types';

// inside the AgentTab component:
const [digest, setDigest] = useState<Digest | null>(null);
const [digestBusy, setDigestBusy] = useState(false);
const [digestErr, setDigestErr] = useState<string | null>(null);

async function loadDigest() {
  setDigestBusy(true); setDigestErr(null);
  try {
    setDigest(await getDigest(session.id));
  } catch (e) {
    setDigestErr(e instanceof Error ? e.message : String(e));
  } finally {
    setDigestBusy(false);
  }
}
```

Render near the details toggle:

```tsx
<button className="details-toggle" onClick={loadDigest} disabled={digestBusy}>
  {digestBusy ? '⏳ Generating digest…' : '✦ Digest'}
</button>
{digestErr && <div className="error">{digestErr}</div>}
{digest && (
  <div className="digest-panel">
    <p className="digest-summary">{digest.summary}</p>
    <pre className="digest-files">
      {hasFiles(digest)
        ? digest.files!.map(fileLabel).join('\n')
        : '(no files touched)'}
    </pre>
    <div className="digest-meta">
      branch: {digest.branch || '—'} · turns: {digest.turns} · status: {digest.status}
    </div>
  </div>
)}
```

(Adjust prop name `session` to match AgentTab's actual prop. Keep class names consistent with the file's existing styling conventions.)

- [ ] **Step 7: Run web tests + typecheck/build**

Run: `cd web && npx vitest run && npm run build`
Expected: tests PASS; build succeeds (TypeScript compiles).

- [ ] **Step 8: Commit**

```bash
git add web/src/lib/digest.ts web/src/lib/digest.test.ts web/src/lib/types.ts web/src/lib/api.ts web/src/components/AgentTab.tsx
git commit -m "feat(web): Digest button + panel"
```

---

### Task 11: TUI `d` key → digest in detail pane

**Files:**
- Create: `internal/tui/digest.go` (render)
- Test: `internal/tui/digest_test.go`
- Modify: `internal/tui/model.go` (api method + model fields + Update case)
- Modify: `internal/tui/cmds.go` (`digestMsg` + `digestCmd`)
- Modify: `internal/tui/keys.go` (`d` key)
- Modify: `internal/tui/view.go` (detail-pane render)

- [ ] **Step 1: Write the failing test for the pure render**

`internal/tui/digest_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/digest"
)

func TestRenderDigestLoading(t *testing.T) {
	out := renderDigest(nil, true, nil, 60)
	if !strings.Contains(out, "generating") {
		t.Errorf("loading state should say generating, got %q", out)
	}
}

func TestRenderDigestError(t *testing.T) {
	out := renderDigest(nil, false, errSample("boom"), 60)
	if !strings.Contains(out, "boom") {
		t.Errorf("error state should show error, got %q", out)
	}
}

func TestRenderDigestContent(t *testing.T) {
	d := &digest.Digest{
		Summary: "Did the work.",
		Branch:  "feat/x",
		Turns:   5,
		Status:  "idle",
		Files:   []digest.FileChange{{Path: "a.go", Added: 3, Removed: 1, Edited: true}},
	}
	out := renderDigest(d, false, nil, 60)
	for _, want := range []string{"Did the work.", "a.go", "+3", "-1", "feat/x", "idle", "5"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func errSample(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tui/ -run RenderDigest`
Expected: FAIL — `renderDigest` undefined.

- [ ] **Step 3: Write `internal/tui/digest.go`**

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/digest"
)

// renderDigest renders the digest detail body: a loading placeholder, an error,
// or the summary + file list + meta line. Pure — width is the inner pane width.
func renderDigest(d *digest.Digest, loading bool, err error, width int) string {
	if loading {
		return stMuted.Render("generating digest…")
	}
	if err != nil {
		return stError.Render("digest failed: " + err.Error())
	}
	if d == nil {
		return stMuted.Render("press d to generate a digest")
	}
	var b strings.Builder
	if d.Summary != "" {
		b.WriteString(d.Summary)
		b.WriteString("\n\n")
	}
	if len(d.Files) == 0 {
		b.WriteString(stMuted.Render("(no files touched)"))
	} else {
		for _, f := range d.Files {
			mark := " "
			if f.Edited {
				mark = "*"
			}
			fmt.Fprintf(&b, "%s %s  +%d -%d\n", mark, f.Path, f.Added, f.Removed)
		}
	}
	branch := d.Branch
	if branch == "" {
		branch = "—"
	}
	fmt.Fprintf(&b, "\n%s", stMuted.Render(fmt.Sprintf("branch: %s · turns: %d · status: %s", branch, d.Turns, d.Status)))
	return b.String()
}
```

(If `stMuted` / `stError` style names differ, grep `internal/tui/*.go` for the actual style var names and use those.)

- [ ] **Step 4: Run the render test to verify it passes**

Run: `go test ./internal/tui/ -run RenderDigest`
Expected: PASS.

- [ ] **Step 5: Add the api method + model fields + Update case (model.go)**

Add to the `api` interface:

```go
	Digest(ctx context.Context, id string) (*digest.Digest, error)
```

Add the import `"github.com/srajanpathak/agentctl/internal/digest"`.

Add model fields (in the `Model` struct):

```go
	digest        *digest.Digest
	digestFor     string // session id the current digest/loading belongs to
	digestLoading bool
	digestErr     error
```

Add an `Update` case for `digestMsg` (alongside the other msg cases):

```go
	case digestMsg:
		m.digestLoading = false
		m.digestErr = msg.err
		if msg.err == nil {
			m.digest = msg.digest
			m.digestFor = msg.id
		}
		return m, nil
```

- [ ] **Step 6: Add `digestMsg` + `digestCmd` (cmds.go)**

```go
type digestMsg struct {
	id     string
	digest *digest.Digest
	err    error
}

func digestCmd(a api, id string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong() // narrator (claude -p) dominates latency
		defer cancel()
		d, err := a.Digest(ctx, id)
		return digestMsg{id: id, digest: d, err: err}
	}
}
```

Add the import `"github.com/srajanpathak/agentctl/internal/digest"` to cmds.go.

- [ ] **Step 7: Wire the `d` key (keys.go)**

In the normal-mode key switch, add a case (only when a session is selected):

```go
	case "d":
		if id := m.selectedID(); id != "" {
			m.digestLoading = true
			m.digestErr = nil
			m.digestFor = id
			return m, digestCmd(m.api, id)
		}
		return m, nil
```

- [ ] **Step 8: Render the digest in the detail pane (view.go)**

In the `default:` branch of the detail `switch` (the session-detail case), show the digest when it belongs to the selected agent; otherwise show normal detail:

```go
		default:
			detailTitle = m.selectedID()
			if detailTitle == "" {
				detailTitle = "—"
			}
			if id := m.selectedID(); id != "" && id == m.digestFor && (m.digestLoading || m.digestErr != nil || m.digest != nil) {
				detailTitle = "Digest — " + id
				detailBody = renderDigest(m.digest, m.digestLoading, m.digestErr, detailOuter-2)
			} else {
				detailBody = renderDetail(m.selected(), m.vp, m.outputFocused, detailOuter-2)
			}
```

- [ ] **Step 9: Add `d` to the help text (view.go `helpText()`)**

Add a line:

```go
		"  d            generate completion digest for the selected agent\n" +
```

- [ ] **Step 10: Update the TUI api fake in tests**

Find the fake api used in TUI tests (`grep -rn "func.*Digest\|fakeAPI\|stubAPI" internal/tui/*_test.go` and the type that implements `api`). Add a stub:

```go
func (f *fakeAPI) Digest(ctx context.Context, id string) (*digest.Digest, error) {
	return f.digest, f.digestErr // add these fields to the fake, default nil
}
```

(Match the actual fake's name/fields; add `digest *digest.Digest` and `digestErr error` fields to it, plus the digest import.)

- [ ] **Step 11: Run all TUI tests + build**

Run: `go test ./internal/tui/... && go build ./...`
Expected: PASS / clean build.

- [ ] **Step 12: Commit**

```bash
git add internal/tui/
git commit -m "feat(tui): d key generates a completion digest into the detail pane"
```

---

### Task 12: full verification + rebuild

**Files:** none (verification only).

- [ ] **Step 1: Format + vet + full test suite**

Run:
```bash
gofmt -l internal/ cmd/ && go vet ./... && go test ./...
```
Expected: `gofmt -l` prints nothing; vet clean; all tests PASS.

- [ ] **Step 2: Web build + tests**

Run: `cd web && npm run build && npx vitest run`
Expected: build succeeds, all web tests PASS. (The Go daemon embeds `web/dist`, so a fresh build is required before reinstall.)

- [ ] **Step 3: Rebuild + reinstall + restart the daemon (manual / user)**

The running daemon predates this feature and embeds the old `web/dist`. To serve the new web UI and enable the endpoint:
```bash
make release && make install   # or the repo's documented build/install target
```
Then restart the daemon (it's a launchd agent). Note in the PR that a manual browser + TUI smoke test is left for the user, mirroring prior feature merges.

- [ ] **Step 4: Manual smoke (document, leave for user)**

- `agentctl digest <id>` against a real agent → human layout + `--json`.
- Web Digest button → spinner → summary + file table.
- TUI `d` → "generating digest…" → digest renders in the detail pane.

---

## Self-Review (completed during planning)

**Spec coverage:**
- `internal/digest` pure parser (Facts: files/turns/task/last-msg, all 4 edit tools, dedup, malformed tolerance, tool_result-skip, string-vs-block content) → Task 1. ✓
- numstat parse + union/annotation merge (edited-then-reverted, git-only non-Edited) → Task 2. ✓
- Narrator interface + real `claude -p` impl reusing lifecycle plumbing + test impl → Tasks 3, 4 (RunClaudeP), 9 (wire). ✓
- Daemon `GET /sessions/{id}/digest`: transcriptPath resolve, missing-transcript Status-only digest, 404 unknown id, narrator-failure degradation → Tasks 5, 6. ✓
- CLI human + `--json` → Task 8. ✓
- Web button + spinner + panel → Task 10. ✓
- TUI `d` async fetch into detail pane → Task 11. ✓
- Error-handling table (no transcript, claude fails, non-repo cwd, malformed lines, unknown id) → covered across Tasks 1/2/6. ✓
- NOT in v1 (MCP tool, caching, auto-trigger) → intentionally absent; clean seams (`SetNarrator`, pure digest package) preserved. ✓

**Type consistency:** `digest.Digest`/`FileChange`/`Facts`/`LineDelta`/`Narrator` names + JSON tags are identical across daemon, client, CLI, and TUI (all import the one `internal/digest` package). Web `Digest`/`FileChange` field names match the Go json tags (`summary`, `files`, `branch`, `turns`, `status`, `task`, `path`, `added`, `removed`, `edited`). `bgLong()`/`bg()` and `stMuted`/`stError` are flagged as grep-and-confirm in the steps that use them.

**Placeholder scan:** No TBD/TODO; every code step shows complete code. The two "match the existing convention" notes (web `parse<T>`, TUI fake/style names) point at concrete grep targets rather than hand-waving.
