# Agent Self-Identification (Phase 3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Set `AGENTCTL_SESSION_ID=<id>` in every agent's tmux session at creation, so inside an agent the `msg` and `ctx` commands (and the future `pipeline emit`) default to the agent's own identity without needing `--as`.

**Architecture:** All three `tmux new-session` sites (Spawn worktree, Spawn no-worktree, and `resumeInTmux` for Restore + Adopt) already funnel through one helper, `lifecycle.newAgentSession`. We add tmux's `-e VAR=VALUE` flag there, so the agent's pane shell — and the `claude` process plus its `Bash` tool subprocesses launched in it — all inherit the variable. One-line behavior change; the bulk of the work is updating existing tests that assert the exact `new-session` argv.

**Tech Stack:** Go, tmux (`-e` requires tmux ≥ 3.0 — already relied on elsewhere in this codebase, e.g. `paste-buffer -r`). Module: `github.com/srajanpathak/agentctl`.

**Scope note:** Only `AGENTCTL_SESSION_ID` is wired here. The pipeline env vars (`AGENTCTL_PIPELINE_ID`/`AGENTCTL_JOB_ID`) from spec §4 are set only for pipeline jobs and are deferred to Phase 4 (which will extend the spawn path to pass them through). Every `msg`/`ctx` command still accepts explicit `--as <id>` so a human or lead agent can act on another agent's behalf.

---

## File Structure

- **Modify** `internal/lifecycle/lifecycle.go` — add the `-e AGENTCTL_SESSION_ID=<id>` flag to the `tmux new-session` invocation in `newAgentSession` (the single chokepoint for all agent sessions).
- **Modify** `internal/lifecycle/lifecycle_test.go` — add one focused test; update the 14 existing references that assert the `new-session` argv.
- **Modify** `docs/USAGE.md` — drop the "set in a later phase" caveat now that identity is wired.

---

## Task 1: Set AGENTCTL_SESSION_ID on every agent tmux session

**Files:**
- Modify: `internal/lifecycle/lifecycle.go:535` (the `new-session` call in `newAgentSession`)
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Add this focused test to `internal/lifecycle/lifecycle_test.go` (it calls the unexported helper directly — the test file is `package lifecycle`, so this is allowed):

```go
func TestNewAgentSessionSetsSessionIDEnv(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	if err := lc.newAgentSession(context.Background(), "", "agent-xyz", "/work"); err != nil {
		t.Fatalf("newAgentSession: %v", err)
	}
	require.Contains(t, fr.calledArgs(), []string{
		"tmux", "new-session", "-d", "-s", "agent-xyz",
		"-e", "AGENTCTL_SESSION_ID=agent-xyz", "-c", "/work",
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestNewAgentSessionSetsSessionIDEnv`
Expected: FAIL — the actual `new-session` argv has no `-e AGENTCTL_SESSION_ID=...` element, so `require.Contains` fails.

- [ ] **Step 3: Write minimal implementation**

In `internal/lifecycle/lifecycle.go`, find the `new-session` line inside `newAgentSession` (currently):

```go
	if out, err := l.run.Run(ctx, runDir, "tmux", "new-session", "-d", "-s", id, "-c", cwd); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
```

Replace the `Run(...)` call so it sets the session-id env var (tmux `-e`, available in tmux ≥ 3.0; the pane shell, `claude`, and its Bash-tool subprocesses all inherit it):

```go
	// -e sets AGENTCTL_SESSION_ID in the session environment so the agent's own
	// shell tools (e.g. `agentctl msg`/`ctx`) know which agent they are without
	// needing --as. Inherited by the claude process and its Bash subprocesses.
	if out, err := l.run.Run(ctx, runDir, "tmux", "new-session", "-d", "-s", id,
		"-e", "AGENTCTL_SESSION_ID="+id, "-c", cwd); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
```

- [ ] **Step 4: Run the focused test (pass) then the full lifecycle suite (existing tests now fail)**

Run: `go test ./internal/lifecycle/ -run TestNewAgentSessionSetsSessionIDEnv`
Expected: PASS.

Run: `go test ./internal/lifecycle/...`
Expected: FAIL — many existing tests now mismatch because they assert the OLD `new-session` argv (without the `-e` element) or use it as a `FakeRunner` `Responses` key / `callIndex` lookup. Step 5 fixes all of them.

- [ ] **Step 5: Update every existing `new-session` reference**

**The transformation is mechanical and exact:** in every reference to an agent `new-session` command, insert the env element immediately after `-s <id>` and before `-c <cwd>`:
- In an **argv slice** `[]string{..., "-s", ID, "-c", CWD}` → insert `"-e", "AGENTCTL_SESSION_ID="+ID,` before `"-c"` (where `ID` is the literal or expression already present, e.g. `"PROJ-350"` or `s.ID`).
- In a **string** `"tmux new-session -d -s ID -c CWD"` → insert ` -e AGENTCTL_SESSION_ID=ID` before ` -c` (preserving that line's existing `+`/spacing style around any `s.ID`).

Apply these exact find → replace edits in `internal/lifecycle/lifecycle_test.go` (some appear more than once — replace all occurrences):

**Slice forms:**

1. Find (2 occurrences):
```go
[]string{"tmux", "new-session", "-d", "-s", "PROJ-350", "-c", "/repo/.worktrees/PROJ-350"}
```
Replace:
```go
[]string{"tmux", "new-session", "-d", "-s", "PROJ-350", "-e", "AGENTCTL_SESSION_ID=PROJ-350", "-c", "/repo/.worktrees/PROJ-350"}
```

2. Find (1 occurrence):
```go
[]string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/repo"}
```
Replace:
```go
[]string{"tmux", "new-session", "-d", "-s", s.ID, "-e", "AGENTCTL_SESSION_ID=" + s.ID, "-c", "/repo"}
```

3. Find (2 occurrences):
```go
[]string{"tmux", "new-session", "-d", "-s", s.ID, "-c", "/work/project"}
```
Replace:
```go
[]string{"tmux", "new-session", "-d", "-s", s.ID, "-e", "AGENTCTL_SESSION_ID=" + s.ID, "-c", "/work/project"}
```

4. Find (1 occurrence):
```go
[]string{"tmux", "new-session", "-d", "-s", "agent-r1", "-c", workdir}
```
Replace:
```go
[]string{"tmux", "new-session", "-d", "-s", "agent-r1", "-e", "AGENTCTL_SESSION_ID=agent-r1", "-c", workdir}
```

5. Find (1 occurrence):
```go
[]string{"tmux", "new-session", "-d", "-s", "agent-a1", "-c", workdir}
```
Replace:
```go
[]string{"tmux", "new-session", "-d", "-s", "agent-a1", "-e", "AGENTCTL_SESSION_ID=agent-a1", "-c", workdir}
```

**String forms** (note the differing `+` spacing between sites — match each exactly):

6. Find (2 occurrences — both `Responses` map keys):
```go
"tmux new-session -d -s PROJ-350 -c /repo/.worktrees/PROJ-350"
```
Replace:
```go
"tmux new-session -d -s PROJ-350 -e AGENTCTL_SESSION_ID=PROJ-350 -c /repo/.worktrees/PROJ-350"
```

7. Find (1 occurrence — a `callIndex` arg, no spaces around `+`):
```go
"tmux new-session -d -s "+s.ID+" -c /repo"
```
Replace:
```go
"tmux new-session -d -s "+s.ID+" -e AGENTCTL_SESSION_ID="+s.ID+" -c /repo"
```

8. Find (1 occurrence — `callIndex`, no spaces around `+`):
```go
"tmux new-session -d -s "+s.ID+" -c /work/project"
```
Replace:
```go
"tmux new-session -d -s "+s.ID+" -e AGENTCTL_SESSION_ID="+s.ID+" -c /work/project"
```

9. Find (2 occurrences — one `callIndex`, one `Responses` key):
```go
"tmux new-session -d -s ag1 -c /cwd"
```
Replace:
```go
"tmux new-session -d -s ag1 -e AGENTCTL_SESSION_ID=ag1 -c /cwd"
```

10. Find (1 occurrence — a `callIndex`, WITH spaces around `+`):
```go
"tmux new-session -d -s " + s.ID + " -c /repo"
```
Replace:
```go
"tmux new-session -d -s " + s.ID + " -e AGENTCTL_SESSION_ID=" + s.ID + " -c /repo"
```

After applying all of the above, double-check nothing was missed:

Run: `grep -n 'new-session -d -s' internal/lifecycle/lifecycle_test.go`
Expected: EVERY line printed contains `AGENTCTL_SESSION_ID=` (any line without it is a missed site — fix it). The only `new-session` lines without the env var should be comments/prose (e.g. "new-session fails ...", "mouse on must follow new-session").

- [ ] **Step 6: Run the full lifecycle suite to verify it passes**

Run: `go test ./internal/lifecycle/...`
Expected: PASS (all tests, including the new `TestNewAgentSessionSetsSessionIDEnv` and every updated assertion).

Then confirm nothing else broke:
Run: `go build ./... && go test ./...`
Expected: PASS across all packages.

- [ ] **Step 7: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): set AGENTCTL_SESSION_ID env on every agent tmux session"
```

---

## Task 2: Update docs + full verification

**Files:**
- Modify: `docs/USAGE.md`

- [ ] **Step 1: Update the identity caveat**

Read `docs/USAGE.md` and find the "Directed messages" section's identity sentence (added in Phase 2), which currently reads approximately:

> Identity defaults to `$AGENTCTL_SESSION_ID` (set per agent in a later phase); until then pass `--as <agent-id>`.

Replace that sentence with (matching the surrounding prose style):

```markdown
Identity defaults to `$AGENTCTL_SESSION_ID`, which agentctl sets on every agent's
tmux session automatically — so inside an agent, `msg` and `ctx` commands just
work without flags. Pass `--as <agent-id>` only to act as a different agent (e.g.
a human operator or a lead agent answering on another's behalf).
```

If the "Shared context" section has a similar "later phase" caveat about `$AGENTCTL_SESSION_ID`, update it the same way; otherwise leave it (its existing wording is already correct).

- [ ] **Step 2: Run the full suite**

Run: `go build ./... && go test ./... && make lint`
Expected: PASS across all packages; lint (go vet) clean. If anything fails, do NOT commit — report it.

- [ ] **Step 3: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: agent identity is now auto-set via AGENTCTL_SESSION_ID"
```

---

## Verification checklist (after all tasks)

- [ ] `go build ./...` clean.
- [ ] `go test ./...` green; `grep -n 'new-session -d -s' internal/lifecycle/lifecycle_test.go` shows `AGENTCTL_SESSION_ID=` on every command line (none missed).
- [ ] `make lint` clean.
- [ ] Manual smoke (rebuild + restart daemon: `./scripts/reinstall.sh`):
  - Spawn an agent: `agentctl start "say hi"` (note its id, e.g. `agent-XXXX`).
  - Confirm the env var is set in its tmux session:
    `tmux show-environment -t agent-XXXX AGENTCTL_SESSION_ID` → prints `AGENTCTL_SESSION_ID=agent-XXXX`.
  - From inside that agent (or by sending it the command), `agentctl ctx set scratch.note hi` then `agentctl ctx list` should attribute the write to `agent-XXXX` (not `human`) — proving the agent's Bash tool inherited the id.
  - Restored/adopted sessions get the var too (same `newAgentSession` path).
```
