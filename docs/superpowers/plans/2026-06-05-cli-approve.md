# CLI `approvals` + `approve` Subcommands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add two CLI commands — `agentctl approvals` (list pending recognized tool-permission prompts) and `agentctl approve <TICKET> <option>` (answer one by option number) — closing the lone gap where approvals were reachable only via web/TUI/curl.

**Architecture:** A new `internal/cli/approvals.go` holding two thin cobra commands plus pure, unit-testable helpers (`formatApprovalsList`, `validateApproval`, `parseOption`). The commands reuse the EXISTING client methods `Client.Approvals(ctx) (enabled, []approval.View, err)` and `Client.Approve(ctx, id, option, fingerprint) error` — no daemon/store/client changes. `approve` fetches the live queue first to obtain the current options fingerprint, so it inherits the daemon's TOCTOU re-verify guard for free (daemon re-captures + re-fingerprints and returns 409 "prompt changed; reopen" if it shifted). Tests follow the repo idiom: pure helpers get table tests (mirroring `messages_test.go` `TestFormatMessage`); cobra wiring stays thin and untested-at-command-level (no CLI integration tests exist in this repo).

**Tech Stack:** Go, cobra, existing `internal/client` + `internal/approval` packages.

**Design constraints (from the approved design):**
- Ship BOTH commands (list + answer) so you never approve blind.
- NO `--all` / "yes to everything" flag — each approval is a deliberate single act (approvals are a human safety gate).
- `approve` validates locally for friendly errors (not found / unrecognized / out of range) but the daemon remains the source of truth (re-verifies on POST).

---

### Task 1: Pure helpers + tests

**Files:**
- Create: `internal/cli/approvals.go`
- Test: `internal/cli/approvals_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/approvals_test.go`:

```go
package cli

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/approval"
)

func recognizedView(id string) approval.View {
	opts := []string{"Yes", "Yes, and always allow access to tmp/ from this project", "No"}
	return approval.View{
		ID:          id,
		Question:    "Do you want to proceed?",
		Options:     opts,
		Fingerprint: approval.Fingerprint(opts),
		Recognized:  true,
	}
}

func TestParseOption(t *testing.T) {
	for _, in := range []string{"1", "2", "10"} {
		if _, err := parseOption(in); err != nil {
			t.Fatalf("parseOption(%q) unexpected err: %v", in, err)
		}
	}
	for _, in := range []string{"0", "-3", "x", "", "1.5"} {
		if _, err := parseOption(in); err == nil {
			t.Fatalf("parseOption(%q): expected error", in)
		}
	}
	if n, _ := parseOption("3"); n != 3 {
		t.Fatalf("parseOption(\"3\")=%d, want 3", n)
	}
}

func TestValidateApproval(t *testing.T) {
	views := []approval.View{
		recognizedView("agent-aaa"),
		{ID: "agent-bbb", Recognized: false},
	}

	// happy path returns the matched view
	v, err := validateApproval(views, "agent-aaa", 1)
	if err != nil || v.ID != "agent-aaa" {
		t.Fatalf("happy path: v=%+v err=%v", v, err)
	}

	// not found
	if _, err := validateApproval(views, "agent-zzz", 1); err == nil ||
		!strings.Contains(err.Error(), "no pending approval") {
		t.Fatalf("not-found: got %v", err)
	}

	// found but unrecognized -> attach hint
	if _, err := validateApproval(views, "agent-bbb", 1); err == nil ||
		!strings.Contains(err.Error(), "attach") {
		t.Fatalf("unrecognized: got %v", err)
	}

	// option out of range (high and low)
	if _, err := validateApproval(views, "agent-aaa", 4); err == nil ||
		!strings.Contains(err.Error(), "out of range") {
		t.Fatalf("high: got %v", err)
	}
	if _, err := validateApproval(views, "agent-aaa", 0); err == nil {
		t.Fatalf("zero: expected error")
	}
}

func TestFormatApprovalsList(t *testing.T) {
	rec := recognizedView("agent-aaa")
	unrec := approval.View{ID: "agent-bbb", Recognized: false}

	// disabled
	if out := formatApprovalsList(false, nil); !strings.Contains(out, "disabled") {
		t.Fatalf("disabled: got %q", out)
	}

	// enabled, empty
	if out := formatApprovalsList(true, nil); !strings.Contains(out, "no pending approvals") {
		t.Fatalf("empty: got %q", out)
	}

	// enabled, only unrecognized -> no pending + footer count
	onlyUnrec := formatApprovalsList(true, []approval.View{unrec})
	if !strings.Contains(onlyUnrec, "no pending approvals") || !strings.Contains(onlyUnrec, "1 other") {
		t.Fatalf("only-unrecognized: got %q", onlyUnrec)
	}

	// enabled, recognized present -> id, question, numbered options, answer hint
	full := formatApprovalsList(true, []approval.View{rec, unrec})
	for _, want := range []string{"agent-aaa", "Do you want to proceed?", "1. Yes", "3. No", "agentctl approve agent-aaa", "1 other"} {
		if !strings.Contains(full, want) {
			t.Fatalf("full: missing %q in %q", want, full)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestParseOption|TestValidateApproval|TestFormatApprovalsList' -v`
Expected: FAIL — `undefined: parseOption`, `undefined: validateApproval`, `undefined: formatApprovalsList`.

- [ ] **Step 3: Write the helpers**

Create `internal/cli/approvals.go` with ONLY the helpers for now (commands added in Task 2):

```go
package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/approval"
)

// parseOption parses a 1-based option argument; rejects non-integers and < 1.
func parseOption(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("option must be a positive integer, got %q", s)
	}
	return n, nil
}

// validateApproval finds the approval for id in the live queue and checks the
// option is answerable. Returns friendly errors the daemon would otherwise only
// surface after a round-trip. The daemon still re-verifies on POST.
func validateApproval(views []approval.View, id string, option int) (approval.View, error) {
	for _, v := range views {
		if v.ID != id {
			continue
		}
		if !v.Recognized {
			return approval.View{}, fmt.Errorf("prompt for %s is not a recognized menu — attach with: agentctl attach %s", id, id)
		}
		if option < 1 || option > len(v.Options) {
			return approval.View{}, fmt.Errorf("option %d out of range (1-%d)", option, len(v.Options))
		}
		return v, nil
	}
	return approval.View{}, fmt.Errorf("no pending approval for %s (run: agentctl approvals)", id)
}

// formatApprovalsList renders the queue. Recognized prompts are shown with their
// numbered options and an answer hint; unrecognized waiting agents are summarized
// in a footer (they must be attached, not answered here).
func formatApprovalsList(enabled bool, views []approval.View) string {
	if !enabled {
		return "approvals disabled (set AGENTCTL_APPROVALS=on)\n"
	}
	var b strings.Builder
	recognized, unrecognized := 0, 0
	for _, v := range views {
		if v.Recognized {
			recognized++
			b.WriteString(formatApproval(v))
		} else {
			unrecognized++
		}
	}
	out := b.String()
	if recognized == 0 {
		out = "(no pending approvals)\n"
	}
	if unrecognized > 0 {
		out += fmt.Sprintf("(%d other waiting agent(s) need attaching — not answerable here)\n", unrecognized)
	}
	return out
}

// formatApproval renders one recognized prompt block.
func formatApproval(v approval.View) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", v.ID, v.Question)
	for i, opt := range v.Options {
		fmt.Fprintf(&b, "  %d. %s\n", i+1, opt)
	}
	fmt.Fprintf(&b, "  answer: agentctl approve %s <n>\n", v.ID)
	return b.String()
}

var _ = cobra.Command{} // keep cobra imported for Task 2; remove if Task 2 lands in same change
```

NOTE for implementer: the `var _ = cobra.Command{}` line is a scaffold so the file compiles before Task 2 wires the commands. DELETE it in Task 2 when the real commands (which use cobra) are added.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestParseOption|TestValidateApproval|TestFormatApprovalsList' -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/approvals.go internal/cli/approvals_test.go docs/superpowers/plans/2026-06-05-cli-approve.md
git commit -m "feat(cli): approvals list/answer pure helpers + tests"
```

---

### Task 2: Wire the `approvals` and `approve` cobra commands

**Files:**
- Modify: `internal/cli/approvals.go` (add the two command constructors; remove the Task 1 scaffold line)
- Modify: `internal/cli/root.go:18-24` (register the commands)

- [ ] **Step 1: Add the command constructors**

In `internal/cli/approvals.go`, DELETE the `var _ = cobra.Command{}` scaffold line and append:

```go
func newApprovalsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approvals",
		Short: "List pending tool-permission prompts waiting for an answer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			enabled, views, err := clientFor(cmd).Approvals(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), formatApprovalsList(enabled, views))
			return nil
		},
	}
}

func newApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <TICKET> <option>",
		Short: "Answer a pending tool-permission prompt by option number",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			option, err := parseOption(args[1])
			if err != nil {
				return err
			}
			c := clientFor(cmd)
			enabled, views, err := c.Approvals(cmd.Context())
			if err != nil {
				return err
			}
			if !enabled {
				return fmt.Errorf("approvals disabled (set AGENTCTL_APPROVALS=on)")
			}
			v, err := validateApproval(views, id, option)
			if err != nil {
				return err
			}
			if err := c.Approve(cmd.Context(), id, option, v.Fingerprint); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s → %d. %s\n", id, option, v.Options[option-1])
			return nil
		},
	}
}
```

- [ ] **Step 2: Register the commands in root.go**

In `internal/cli/root.go`, add a line near the other `AddCommand` calls (e.g., right after line 20 `root.AddCommand(newSendCmd(), newTailCmd())`):

```go
	root.AddCommand(newApprovalsCmd(), newApproveCmd())
```

- [ ] **Step 3: Verify build, vet, and existing tests still pass**

Run: `go build ./... && go vet ./... && go test ./internal/cli/ -v`
Expected: builds clean, vet clean, all cli tests PASS (the Task 1 trio plus pre-existing ones). Confirm `approvals.go` has NO leftover `var _ = cobra.Command{}` line.

- [ ] **Step 4: Smoke the help output (sanity, no daemon needed)**

Run: `go run ./cmd/agentctl approve --help && go run ./cmd/agentctl approvals --help`
Expected: both print usage (Use/Short as written); `approve` shows `<TICKET> <option>`.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/approvals.go internal/cli/root.go
git commit -m "feat(cli): wire approvals list + approve answer commands"
```

---

## Self-Review

- **Spec coverage:** list command (`approvals`) ✓ Task 1+2; answer command (`approve <id> <n>`) ✓ Task 2; fingerprint reuse via pre-fetch ✓ (`approve` calls `Approvals` then passes `v.Fingerprint`); no `--all` flag ✓ (none added); friendly local validation ✓ `validateApproval`.
- **Type consistency:** `approval.View` fields used (`ID`, `Question`, `Options`, `Fingerprint`, `Recognized`) match `internal/approval/approval.go:129-136`. Client signatures match `client.go:211,224`. `clientFor` from `common.go`. Helper names identical across Task 1 (defined/tested) and Task 2 (called): `parseOption`, `validateApproval`, `formatApprovalsList`, `formatApproval`.
- **Placeholder scan:** none — all code is complete; the one scaffold line is explicitly called out for deletion in Task 2.
