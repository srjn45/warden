# agentctl CLI `--json` + CLI-first docs — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `--json` flag to `agentctl ls` and `agentctl status` for machine-readable output, and rework the docs so the CLI is a first-class way to drive the fleet (not just an MCP fallback).

**Architecture:** Both commands already deserialize the daemon's HTTP response into fully JSON-tagged `store.Session` structs (`internal/cli/sessions.go`). `--json` re-marshals those structs via a small pure `printJSON` helper instead of the table/text renderer. No new DTO, no API change.

**Tech Stack:** Go (cobra CLI), `encoding/json`. Docs in Markdown (README.md + skills/agentctl/SKILL.md).

---

## Reference facts (verified)

- `internal/cli/sessions.go` holds `newLsCmd()` (uses `clientFor(cmd).List(ctx)` → `[]store.Session`) and `newStatusCmd()` (uses `clientFor(cmd).Get(ctx, id)` → `store.Session`).
- `store.Session` (`internal/store/types.go:77`) has full `json:"..."` tags on every field, incl. `events`. This is the on-disk + API format — stable.
- `internal/cli` currently has **no test files** (`go test` reports `[no test files]`). The commands talk to the daemon over HTTP, so we test the pure `printJSON` helper directly rather than build HTTP-mock infrastructure.
- Commands are registered in `internal/cli/root.go:19` (`newLsCmd(), newStatusCmd()`).
- README CLI reference: `### \`agentctl ls\`` at line 236, `### \`agentctl status <TICKET>\`` at line 250.
- Skill file: `skills/agentctl/SKILL.md` — currently MCP-first with a short CLI fallback that omits `status` and `restore`.
- Module path: `github.com/srajanpathak/agentctl`.

## File Structure

- Modify: `internal/cli/sessions.go` — add `printJSON` helper; add `--json` flag + branch to `newLsCmd` and `newStatusCmd`.
- Create: `internal/cli/sessions_test.go` — unit tests for `printJSON`.
- Modify: `README.md` — document `--json` under the `ls`/`status` reference entries.
- Modify: `skills/agentctl/SKILL.md` — make the CLI first-class; full intent→command map incl. `--json`, `status`, `restore`; note MCP-blocked reality.

---

### Task 1: `--json` flag for `ls` and `status` (+ unit test)

**Files:**
- Modify: `internal/cli/sessions.go`
- Create: `internal/cli/sessions_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/cli/sessions_test.go` with exactly this content:

```go
package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
)

func TestPrintJSON_EmptySlice(t *testing.T) {
	var buf bytes.Buffer
	if err := printJSON(&buf, []store.Session{}); err != nil {
		t.Fatalf("printJSON returned error: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Fatalf("empty slice: want %q, got %q", "[]", got)
	}
}

func TestPrintJSON_SessionHasFields(t *testing.T) {
	var buf bytes.Buffer
	s := store.Session{ID: "agent-x1", Status: store.Status("working")}
	if err := printJSON(&buf, s); err != nil {
		t.Fatalf("printJSON returned error: %v", err)
	}
	// Valid JSON
	var round map[string]any
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if round["id"] != "agent-x1" {
		t.Fatalf("want id=agent-x1, got %v", round["id"])
	}
	// Indented (two-space)
	if !strings.Contains(buf.String(), "\n  \"id\"") {
		t.Fatalf("expected 2-space indented output, got:\n%s", buf.String())
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (printJSON undefined)**

Run: `go test ./internal/cli/ -run TestPrintJSON -v`
Expected: FAIL — compile error `undefined: printJSON`.

- [ ] **Step 3: Implement `printJSON` and wire the `--json` flag**

In `internal/cli/sessions.go`:

(a) Update the import block to add `encoding/json` and `io`:

```go
import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/store"
)
```

(b) Replace `newLsCmd` with:

```go
func newLsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List all active agent sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			sessions, err := clientFor(cmd).List(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				if sessions == nil {
					sessions = []store.Session{}
				}
				return printJSON(cmd.OutOrStdout(), sessions)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tAGE\tDIR\tSUBJECT")
			for _, s := range sessions {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					s.ID, typeOrPending(s.Type), s.Status, age(s.UpdatedAt), dirName(s.Workdir), s.Subject)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}
```

(c) Replace `newStatusCmd` with:

```go
func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status <TICKET>",
		Short: "Show full status for one session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := clientFor(cmd).Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
				return printJSON(cmd.OutOrStdout(), s)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "id:       %s\ntype:     %s\nticket:   %s\nstatus:   %s\nrepo:     %s\nworkdir:  %s\nworktree: %s\nbranch:   %s\npr:       %s\nsubject:  %s\nclaude:   %s\nupdated:  %s\n",
				s.ID, typeOrPending(s.Type), s.Ticket, s.Status, s.Repo, s.Workdir, s.Worktree, s.Branch, s.PR, s.Subject, s.ClaudeSessionID, s.UpdatedAt.Format(time.RFC3339))
			fmt.Fprintln(out, "events:")
			for _, e := range s.Events {
				fmt.Fprintf(out, "  %s  %-14s %s\n", e.TS.Format("15:04:05"), e.Type, e.Detail)
			}
			return nil
		},
	}
	cmd.Flags().Bool("json", false, "output as JSON")
	return cmd
}
```

(d) Add the `printJSON` helper at the end of the file:

```go
// printJSON writes v as indented JSON followed by a newline. Used by the
// --json flag so agents/scripts can parse agentctl output reliably.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
```

- [ ] **Step 4: Run the unit tests to verify they pass**

Run: `go test ./internal/cli/ -run TestPrintJSON -v`
Expected: PASS (both `TestPrintJSON_EmptySlice` and `TestPrintJSON_SessionHasFields`).

- [ ] **Step 5: Build and verify the whole suite still passes**

Run: `go build ./... && go test ./...`
Expected: build succeeds; all packages `ok` (no regressions). `internal/cli` now shows `ok` instead of `[no test files]`.

- [ ] **Step 6: Live verification against the running daemon**

The daemon is installed and running on `127.0.0.1:8765`. Build a local binary and exercise the flag:

Run: `go build -o /tmp/agentctl-json ./cmd/agentctl && /tmp/agentctl-json ls --json`
Expected: valid JSON. With no agents, output is exactly `[]`.

Run: `/tmp/agentctl-json ls --json | python3 -m json.tool >/dev/null && echo "ls --json is valid JSON"`
Expected: `ls --json is valid JSON`.

(If any agents exist, also spot-check `/tmp/agentctl-json status <id> --json | python3 -m json.tool >/dev/null && echo ok`. If none exist, skip — the empty-`ls` check plus the unit test cover the rendering.)

Run: `rm -f /tmp/agentctl-json`

- [ ] **Step 7: Confirm default output is unchanged**

Run: `go run ./cmd/agentctl ls`
Expected: the normal table header `ID  TYPE  STATUS  AGE  DIR  SUBJECT` (no `--json` → unchanged behavior).

- [ ] **Step 8: Commit**

```bash
git add internal/cli/sessions.go internal/cli/sessions_test.go
git status --short
git commit -m "feat(cli): --json output for ls and status"
```
Before committing, confirm `git status --short` shows exactly the two files (modified `sessions.go`, new `sessions_test.go`). If anything else appears, STOP and report.

---

### Task 2: CLI-first documentation

**Files:**
- Modify: `README.md`
- Modify: `skills/agentctl/SKILL.md`

- [ ] **Step 1: Add `--json` to the README `ls` reference**

In `README.md`, find the `### \`agentctl ls\`` section (around line 236). After the existing example code block and the `DIR`/`SUBJECT` explanation paragraph (line 248), add:

```markdown

Use `--json` for machine-readable output (a JSON array of full session objects; an empty fleet prints `[]`). Useful for scripts and for Claude driving the CLI:

```sh
agentctl ls --json
```
```

(Note the nested fence: in the file this becomes a normal prose line followed by a ```sh code block. Do not include the outer ```markdown wrapper.)

- [ ] **Step 2: Add `--json` to the README `status` reference**

In `README.md`, find `### \`agentctl status <TICKET>\`` (around line 250). After its existing `agentctl status PROJ-350` example block, add:

```markdown

Add `--json` to emit the full session as a single JSON object (including the `events` array):

```sh
agentctl status PROJ-350 --json
```
```

- [ ] **Step 3: Verify README edits**

Run: `grep -n "agentctl ls --json\|agentctl status PROJ-350 --json" README.md`
Expected: two matching lines.
Run: `grep -c '^```' README.md`
Expected: an **even** number (fences balanced).

- [ ] **Step 4: Rework `skills/agentctl/SKILL.md` to make the CLI first-class**

Read `skills/agentctl/SKILL.md` fully first. Then make these changes, preserving the existing frontmatter and overall structure:

(a) In the **Preconditions / tool-availability** area, change the framing so the CLI is a co-equal primary path, not just a fallback. Replace the existing "If the `agentctl` MCP tools are not available … fall back to the CLI" sentence with wording to the effect of:

> Two equivalent ways to drive the fleet: the **agentctl MCP tools** (when registered in this session) and the **`agentctl` CLI** (always available wherever the binary is installed). They wrap the same daemon REST API — use whichever is available. **Note:** MCP registration may be blocked by enterprise policy (`claude mcp add` is locked down on some machines); when the MCP tools are absent, use the CLI — no capability is lost.

(b) Add a **CLI command map** section (a table) that mirrors the existing MCP "Intent → action" table, with the exact CLI command for each intent. Use this content:

```markdown
## CLI command map (when not using MCP tools)

| Intent | CLI command |
|---|---|
| list / triage agents | `agentctl ls` (add `--json` for machine-readable output) |
| full status of one agent | `agentctl status <id>` (add `--json` for the full object incl. events) |
| recent terminal output | `agentctl tail <id>` |
| spawn from a prompt | `agentctl start "<prompt>"` |
| spawn a managed worktree agent | `agentctl start <TICKET> --type <TYPE> --repo <repo>` |
| send a message to an agent | `agentctl send <id> "<text>"` |
| terminate / clean up | `agentctl done <id>` (guarded; `--force` to override the git guard) |
| restore a lost/orphaned agent | `agentctl restore <id>` |
| attach interactively | `agentctl attach <id>` |

Prefer `--json` on `ls`/`status` when you need to parse the result programmatically — the table/text views are for humans and may change.
```

(c) If the existing terse "fall back to the CLI: `agentctl ls`, `agentctl start …`" bullet remains elsewhere, ensure it no longer omits `status` and `restore` (either remove it in favor of the new table, or update it to point at the table).

Keep all existing guardrails (cleanup confirmation, never bulk-terminate, daemon-unreachable handling, restore-is-resume-only) intact.

- [ ] **Step 5: Verify SKILL.md edits**

Run: `grep -n "CLI command map\|--json\|enterprise policy\|agentctl restore" skills/agentctl/SKILL.md`
Expected: matches for the new section heading, `--json`, the enterprise-policy note, and `agentctl restore`.
Run: `grep -c '^```' skills/agentctl/SKILL.md`
Expected: an **even** number (fences balanced), or `0` if the file uses no code fences.

- [ ] **Step 6: Commit**

```bash
git add README.md skills/agentctl/SKILL.md
git status --short
git commit -m "docs: document --json; make CLI a first-class path in the agentctl skill"
```
Before committing, confirm `git status --short` shows exactly `README.md` and `skills/agentctl/SKILL.md`. If anything else appears, STOP and report.

---

## Final verification

- [ ] `go build ./... && go test ./...` all green (incl. new `internal/cli` tests).
- [ ] `agentctl ls --json` emits valid JSON (`[]` when empty); `agentctl ls` (no flag) unchanged.
- [ ] README documents `--json` for both `ls` and `status`; fences balanced.
- [ ] SKILL.md presents the CLI as first-class with a full command map incl. `--json`/`status`/`restore` and the enterprise-policy note; guardrails intact.
- [ ] Each task committed separately; `git status` clean.

## Notes for the implementer

- The installed skill at `~/.claude/skills/agentctl` is a symlink into this repo's `skills/agentctl`, so editing `skills/agentctl/SKILL.md` here updates the live skill automatically — no copy step needed.
- Do NOT add `--json` to `tail` (raw passthrough), `start`, `send`, `done`, or `restore` — out of scope for this change.
- Work on a dedicated branch in an isolated git worktree off `master` (the main checkout may be shared with other sessions).
