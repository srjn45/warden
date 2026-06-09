# agentctl self-rotate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `/agentctl rotate` — a long-running agent writes a handoff, then `agentctl rotate --confirm` spawns a fresh successor in the same worktree and retires the original, bounding context without losing the task.

**Architecture:** Approach B — a thin CLI verb (`internal/cli/rotate.go`) over the **existing** `Client.Get`/`Spawn`/`Terminate` methods, plus a `rotate` section in `skills/agentctl/SKILL.md`. No daemon changes, no new endpoint. The agent (LLM) writes the creative handoff content; the verb does the deterministic, must-not-fail plumbing (config inheritance, spawn-before-reap ordering, the `--confirm` gate). Orchestration is unit-tested against a minimal `rotator` interface that **omits `RemoveWorktree`**, making "never delete the inherited worktree" a compile-time guarantee.

**Tech Stack:** Go, cobra (CLI), testify (tests). Module path `github.com/srajanpathak/agentctl`.

---

## File Structure

- **Create** `internal/cli/rotate.go` — pure helpers (`selfSessionID`, `validateHandoff`, `composeSuccessorPrompt`, `buildSuccessorParams`), the `rotator` interface, the `runRotate` orchestrator, and the `newRotateCmd` cobra command.
- **Create** `internal/cli/rotate_test.go` — unit tests for the pure helpers and `runRotate` (happy path, spawn-error-no-reap, reap-error).
- **Modify** `internal/cli/root.go:21` — register `newRotateCmd()`.
- **Modify** `skills/agentctl/SKILL.md` — add a self-rotation subsection in section 1 plus a CLI-map pointer row.

All exported daemon surface already exists (`client.go`): `Get`, `Spawn(SpawnParams{Prompt,Cwd,Supervised,...})`, `Terminate`. `$AGENTCTL_SESSION_ID` is injected at `tmux new-session` (`lifecycle.go:594`).

---

## Task 1: Pure helpers — successor params & prompt

**Files:**
- Create: `internal/cli/rotate.go`
- Test: `internal/cli/rotate_test.go`

- [ ] **Step 1: Write the failing tests**

```go
package cli

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srajanpathak/agentctl/internal/store"
)

func TestBuildSuccessorParams(t *testing.T) {
	old := &store.Session{Workdir: "/repo/.worktrees/CRD-1", Supervised: true, Repo: "/repo", Worktree: "/repo/.worktrees/CRD-1"}
	p := buildSuccessorParams(old, "do the thing")
	require.Equal(t, "do the thing", p.Prompt)
	require.Equal(t, "/repo/.worktrees/CRD-1", p.Cwd, "successor must launch in the old agent's workdir (the worktree)")
	require.True(t, p.Supervised, "successor inherits supervised mode")
	// Prompt-mode spawn: no Type/Repo/Worktree, so the existing worktree is reused by cwd, not recreated.
	require.Empty(t, p.Type)
	require.Empty(t, p.Repo)
	require.False(t, p.Worktree)
}

func TestComposeSuccessorPrompt(t *testing.T) {
	got := composeSuccessorPrompt("Finish the migration.", "/repo/.agentctl/rotate-handoff.md")
	require.Contains(t, got, "/repo/.agentctl/rotate-handoff.md", "must point successor at the handoff file")
	require.Contains(t, got, "Finish the migration.", "must include the human-reviewed resume prompt")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestBuildSuccessorParams|TestComposeSuccessorPrompt' -v`
Expected: FAIL — `undefined: buildSuccessorParams` / `undefined: composeSuccessorPrompt`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/cli/rotate.go`. **Import only what this task uses** — Go fails on unused imports; later tasks grow the import block:

```go
package cli

import (
	"fmt"

	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
)

// composeSuccessorPrompt builds the successor's initial prompt: it points the
// fresh agent at the handoff notes first, then appends the human-reviewed
// resume prompt.
func composeSuccessorPrompt(resumePrompt, handoffPath string) string {
	return fmt.Sprintf("You are resuming work handed off from a previous agent that is being retired. "+
		"First read the handoff notes at %s for full context, decisions already made, and next steps. "+
		"Then continue the work:\n\n%s", handoffPath, resumePrompt)
}

// buildSuccessorParams clones the retiring agent's launch configuration so the
// successor lands in the identical environment — same working directory (which,
// for a worktree-backed agent, IS the worktree dir) and the same supervised
// flag. It is a prompt-mode spawn (no Type/Repo/Worktree), so the successor
// reuses the existing worktree by cwd rather than creating a new one.
func buildSuccessorParams(old *store.Session, prompt string) client.SpawnParams {
	return client.SpawnParams{
		Prompt:     prompt,
		Cwd:        old.Workdir,
		Supervised: old.Supervised,
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestBuildSuccessorParams|TestComposeSuccessorPrompt' -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/rotate.go internal/cli/rotate_test.go
git commit -m "feat(rotate): successor params + prompt helpers"
```

---

## Task 2: Pure helpers — self id & handoff validation

**Files:**
- Modify: `internal/cli/rotate.go`
- Test: `internal/cli/rotate_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/rotate_test.go` (add `"os"`, `"path/filepath"` to its imports):

```go
func TestSelfSessionID(t *testing.T) {
	t.Setenv("AGENTCTL_SESSION_ID", "agent-abc123")
	id, err := selfSessionID()
	require.NoError(t, err)
	require.Equal(t, "agent-abc123", id)

	t.Setenv("AGENTCTL_SESSION_ID", "")
	_, err = selfSessionID()
	require.Error(t, err, "must error when not run inside an agent session")
}

func TestValidateHandoff(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "nope.md")
	require.Error(t, validateHandoff(missing), "missing file is an error")

	empty := filepath.Join(dir, "empty.md")
	require.NoError(t, os.WriteFile(empty, nil, 0o644))
	require.Error(t, validateHandoff(empty), "empty file is an error")

	good := filepath.Join(dir, "good.md")
	require.NoError(t, os.WriteFile(good, []byte("notes"), 0o644))
	require.NoError(t, validateHandoff(good))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestSelfSessionID|TestValidateHandoff' -v`
Expected: FAIL — `undefined: selfSessionID` / `undefined: validateHandoff`.

- [ ] **Step 3: Write minimal implementation**

Add `"os"` to `rotate.go`'s import block, then add:

```go
// selfSessionID returns the current agent's own id from the environment that
// every agentctl-spawned tmux session carries (AGENTCTL_SESSION_ID, set at
// `tmux new-session`). rotate is only meaningful from inside an agent.
func selfSessionID() (string, error) {
	id := os.Getenv("AGENTCTL_SESSION_ID")
	if id == "" {
		return "", fmt.Errorf("rotate must be run inside an agentctl agent session (AGENTCTL_SESSION_ID is unset)")
	}
	return id, nil
}

// validateHandoff fails when the handoff file is missing or empty — caught
// before anything irreversible (spawn/reap) happens.
func validateHandoff(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("handoff file %q: %w", path, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("handoff file %q is empty", path)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestSelfSessionID|TestValidateHandoff' -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/rotate.go internal/cli/rotate_test.go
git commit -m "feat(rotate): self-id resolution + handoff validation"
```

---

## Task 3: Orchestration — `rotator` interface & `runRotate`

**Files:**
- Modify: `internal/cli/rotate.go`
- Test: `internal/cli/rotate_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/rotate_test.go` (add `"context"` and `"errors"` to imports; `client` is already needed — add `"github.com/srajanpathak/agentctl/internal/client"`):

```go
type fakeRotator struct {
	getSession  *store.Session
	getErr      error
	spawnResult *store.Session
	spawnErr    error
	terminateErr error
	calls       []string
	spawnParams client.SpawnParams
}

func (f *fakeRotator) Get(ctx context.Context, id string) (*store.Session, error) {
	f.calls = append(f.calls, "get:"+id)
	return f.getSession, f.getErr
}
func (f *fakeRotator) Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error) {
	f.calls = append(f.calls, "spawn")
	f.spawnParams = p
	return f.spawnResult, f.spawnErr
}
func (f *fakeRotator) Terminate(ctx context.Context, id string) error {
	f.calls = append(f.calls, "terminate:"+id)
	return f.terminateErr
}

func TestRunRotateHappyPath(t *testing.T) {
	f := &fakeRotator{
		getSession:  &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x", Supervised: true},
		spawnResult: &store.Session{ID: "agent-new", Workdir: "/repo/.worktrees/x"},
	}
	// onSpawned closes over f so its call interleaves into the same ordered log.
	succ, err := runRotate(context.Background(), f, "agent-old", "resume prompt",
		func(s *store.Session) { f.calls = append(f.calls, "print:"+s.ID) })
	require.NoError(t, err)
	require.Equal(t, "agent-new", succ.ID)
	require.Equal(t,
		[]string{"get:agent-old", "spawn", "print:agent-new", "terminate:agent-old"},
		f.calls,
		"summary must print AFTER spawn but BEFORE reap — the reap kills this very process in self-rotation")
	require.Equal(t, "/repo/.worktrees/x", f.spawnParams.Cwd)
	require.True(t, f.spawnParams.Supervised)
}

func TestRunRotateSpawnErrorDoesNotReap(t *testing.T) {
	f := &fakeRotator{
		getSession: &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x"},
		spawnErr:   errors.New("spawn boom"),
	}
	printed := false
	succ, err := runRotate(context.Background(), f, "agent-old", "resume prompt",
		func(s *store.Session) { printed = true })
	require.Error(t, err)
	require.Nil(t, succ, "no successor on spawn failure")
	require.False(t, printed, "must not print a success summary when spawn fails")
	require.Equal(t, []string{"get:agent-old", "spawn"}, f.calls, "must NOT terminate the old agent when spawn fails")
}

func TestRunRotateReapErrorStillReturnsSuccessor(t *testing.T) {
	f := &fakeRotator{
		getSession:   &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x"},
		spawnResult:  &store.Session{ID: "agent-new"},
		terminateErr: errors.New("terminate boom"),
	}
	succ, err := runRotate(context.Background(), f, "agent-old", "resume prompt", func(s *store.Session) {})
	require.Error(t, err, "reap failure is surfaced")
	require.NotNil(t, succ, "successor is already live, so it is returned")
	require.Equal(t, "agent-new", succ.ID)
}

func TestRunRotateToleratesNilCallback(t *testing.T) {
	f := &fakeRotator{
		getSession:  &store.Session{ID: "agent-old", Workdir: "/repo/.worktrees/x"},
		spawnResult: &store.Session{ID: "agent-new"},
	}
	_, err := runRotate(context.Background(), f, "agent-old", "resume prompt", nil)
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestRunRotate' -v`
Expected: FAIL — `undefined: runRotate` (and `rotator` interface unused).

- [ ] **Step 3: Write minimal implementation**

Add `"context"` to `rotate.go`'s import block, then add:

```go
// rotator is the minimal daemon surface rotate needs. It deliberately OMITS
// RemoveWorktree: the successor inherits the live worktree by cwd, so rotate
// must never remove it — leaving the method off the interface makes that
// invariant a compile-time guarantee, not just a test.
type rotator interface {
	Get(ctx context.Context, id string) (*store.Session, error)
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	Terminate(ctx context.Context, id string) error
}

// the real client must satisfy the interface.
var _ rotator = (*client.Client)(nil)

// runRotate performs the irreversible half: spawn the successor in the retiring
// agent's environment, then reap the old agent. Ordering is spawn-before-reap
// and fail-safe — if Spawn fails, the old agent is NOT terminated (returns a nil
// successor), so no work is stranded. If the successor spawns but the reap
// fails, it returns the live successor AND a non-nil error so the caller can
// warn that the old agent may still be running.
//
// onSpawned (if non-nil) is invoked with the live successor AFTER the spawn
// succeeds but BEFORE the reap. This ordering is load-bearing for self-rotation:
// Terminate kills the very tmux session this process runs in, SIGKILLing the
// rotate process, so any user-facing summary must be emitted in onSpawned —
// printing it after runRotate returns would never be seen.
func runRotate(ctx context.Context, r rotator, selfID, successorPrompt string, onSpawned func(*store.Session)) (*store.Session, error) {
	old, err := r.Get(ctx, selfID)
	if err != nil {
		return nil, fmt.Errorf("look up self %q: %w", selfID, err)
	}
	successor, err := r.Spawn(ctx, buildSuccessorParams(old, successorPrompt))
	if err != nil {
		return nil, fmt.Errorf("spawn successor (old agent left running): %w", err)
	}
	if onSpawned != nil {
		onSpawned(successor)
	}
	if err := r.Terminate(ctx, selfID); err != nil {
		return successor, fmt.Errorf("successor %s spawned, but reaping old agent %s failed: %w", successor.ID, selfID, err)
	}
	return successor, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestRunRotate' -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/rotate.go internal/cli/rotate_test.go
git commit -m "feat(rotate): runRotate orchestration (spawn-before-reap, fail-safe)"
```

---

## Task 4: Cobra command & registration

**Files:**
- Modify: `internal/cli/rotate.go`
- Modify: `internal/cli/root.go:21`

- [ ] **Step 1: Add the command constructor**

Add `"github.com/spf13/cobra"` to `rotate.go`'s import block, then add:

```go
func newRotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Hand this agent's work to a fresh successor in the same workspace, then retire it",
		Long: "Run inside an agent session. Phase 1 is driven by the /agentctl skill " +
			"(the agent writes a handoff file + resume prompt and shows you). On your " +
			"go-ahead, run with --confirm to spawn the successor and reap this agent.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			confirm, _ := cmd.Flags().GetBool("confirm")
			if !confirm {
				return fmt.Errorf("rotate is irreversible; re-run with --confirm once you've reviewed the handoff")
			}
			resumeFile, _ := cmd.Flags().GetString("resume-file")
			resumePrompt, _ := cmd.Flags().GetString("resume-prompt")
			if resumeFile == "" || resumePrompt == "" {
				return fmt.Errorf("--resume-file and --resume-prompt are both required with --confirm")
			}
			selfID, err := selfSessionID()
			if err != nil {
				return err
			}
			if err := validateHandoff(resumeFile); err != nil {
				return err
			}
			prompt := composeSuccessorPrompt(resumePrompt, resumeFile)
			out := cmd.OutOrStdout()
			// Summary is printed in onSpawned — BEFORE the reap — because the reap
			// kills this process in self-rotation. See runRotate's doc comment.
			onSpawned := func(successor *store.Session) {
				fmt.Fprintf(out, "rotated: successor %s spawned in %s\n", successor.ID, successor.Workdir)
				fmt.Fprintf(out, "  handoff notes: %s\n", resumeFile)
				fmt.Fprintf(out, "  old agent %s retiring; its transcript + the handoff file remain on disk for recovery\n", selfID)
				fmt.Fprintf(out, "  attach to the successor: agentctl attach %s\n", successor.ID)
			}
			successor, err := runRotate(cmd.Context(), clientFor(cmd), selfID, prompt, onSpawned)
			if successor == nil {
				return err // get/spawn failed; nothing irreversible happened, summary not printed
			}
			// Reaching here with a non-nil err means the reap failed — which means the
			// session was NOT killed, so this process is still alive to warn.
			if err != nil {
				fmt.Fprintf(out, "  WARNING: %v — verify and remove manually with: agentctl done %s\n", err, selfID)
			}
			return nil
		},
	}
	cmd.Flags().Bool("confirm", false, "actually spawn the successor and retire this agent (required)")
	cmd.Flags().String("resume-file", "", "path to the handoff notes file the successor should read")
	cmd.Flags().String("resume-prompt", "", "the successor's initial task prompt")
	return cmd
}
```

- [ ] **Step 2: Register the command**

In `internal/cli/root.go`, line 21 currently reads:

```go
	root.AddCommand(newApprovalsCmd(), newApproveCmd())
```

Change it to:

```go
	root.AddCommand(newApprovalsCmd(), newApproveCmd(), newRotateCmd())
```

- [ ] **Step 3: Verify it compiles and the command is wired**

Run: `go build ./... && go run . rotate 2>&1 | head -1`
Expected: build succeeds; the run prints the gate error `rotate is irreversible; re-run with --confirm once you've reviewed the handoff` (proves the command is registered and the `--confirm` gate fires).

- [ ] **Step 4: Verify the full package test still passes**

Run: `go test ./internal/cli/ -v`
Expected: PASS (all rotate tests + existing cli tests).

- [ ] **Step 5: Commit**

```bash
git add internal/cli/rotate.go internal/cli/root.go
git commit -m "feat(rotate): cobra command + register agentctl rotate"
```

---

## Task 5: Skill guidance

**Files:**
- Modify: `skills/agentctl/SKILL.md`

- [ ] **Step 1: Add the CLI-map pointer row**

In `skills/agentctl/SKILL.md`, the CLI command map row at line 94 reads:

```markdown
| attach interactively | `agentctl attach <id>` |
```

Add immediately after it:

```markdown
| rotate yourself into a fresh agent (free your context) | `/agentctl rotate` — see "Rotating a long-running agent" below (self only) |
```

- [ ] **Step 2: Add the self-rotation subsection**

In `skills/agentctl/SKILL.md`, immediately before the `---` separator at line 96 (after the CLI command map), insert:

```markdown
## Rotating a long-running agent into a fresh one (self-rotation)

When **you yourself** are a long-running agent whose context has grown large and the
user runs `/agentctl rotate`, hand your work to a fresh successor in the same
workspace, then retire yourself. This bounds context and returns memory to the OS
without losing the task. **Self only** — you rotate the agent you are running in
(your id is in `$AGENTCTL_SESSION_ID`); there is no remote rotate.

Two phases, with a human review gate between them:

1. **Prepare (you do this directly — you have your own context).**
   - Write a **handoff file** in your working directory (e.g.
     `./.agentctl/rotate-handoff.md`) capturing what a fresh agent needs to
     *continue*: the goal, current working-tree state (branch, committed vs.
     uncommitted), key decisions and approaches already ruled out, precise next
     steps, and pointers to the relevant files.
   - Compose a one-paragraph **resume prompt** — the successor's initial task.
   - Show the user the handoff file path and the resume prompt, and **stop**. Let
     them edit the file and confirm before you go further.

2. **Commit (only after the user says go):**

   ```sh
   agentctl rotate --confirm \
     --resume-file ./.agentctl/rotate-handoff.md \
     --resume-prompt "<the resume prompt>"
   ```

   This spawns the successor in your exact working directory (same worktree, same
   supervised mode), prints the new agent id, then retires you. Nothing
   irreversible happens without `--confirm`.

Do **not** spawn the successor or terminate yourself by hand — `agentctl rotate`
inherits your launch config and orders spawn-before-reap safely (a failed spawn
leaves you running, so no work is stranded).
```

- [ ] **Step 3: Verify the doc reads correctly**

Run: `grep -n "rotate" skills/agentctl/SKILL.md`
Expected: shows the new CLI-map row and the new subsection heading.

- [ ] **Step 4: Commit**

```bash
git add skills/agentctl/SKILL.md
git commit -m "docs(rotate): skill guidance for /agentctl rotate self-rotation"
```

---

## Task 6: Full verification & install

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: PASS across all packages (no regressions).

- [ ] **Step 2: Run vet / build**

Run: `go vet ./... && go build ./...`
Expected: no errors.

- [ ] **Step 3: Rebuild & reinstall the CLI**

Run: `make install` (or the repo's documented install target).
Expected: the `agentctl` on PATH now includes the `rotate` subcommand. **No daemon restart is required** — rotate adds no daemon code; it only calls existing endpoints.

- [ ] **Step 4: Manual live smoke (LEFT FOR USER — needs a real agent)**

Inside a real supervised agent running in a worktree with some uncommitted changes:
1. Write a small handoff file.
2. `agentctl rotate --confirm --resume-file <path> --resume-prompt "continue"`.
3. Confirm: a new agent id is printed; `agentctl ls` shows the successor in the **same workdir**; the uncommitted changes are still present in that worktree; the old agent is gone; the worktree was **not** deleted.

---

## Notes for the implementer

- This feature touches **no daemon code** — do not add endpoints. If you find yourself editing `internal/daemon` or `internal/lifecycle`, stop; the design is pure CLI orchestration over existing `Client` methods.
- The `rotator` interface intentionally omits `RemoveWorktree`. Do not add it. That omission is the enforcement of the "never delete the inherited worktree" invariant.
- Keep `RunE` thin; all branching logic worth testing lives in `runRotate` and the pure helpers.
