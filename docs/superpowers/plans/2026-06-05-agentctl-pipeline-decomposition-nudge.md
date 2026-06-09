# agentctl Pipeline-Decomposition Nudge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make "decompose large/long-running work into a pipeline of short-lived, bounded-context stages" a standing recommendation — in the agentctl skill rubric and via a system-prompt advisory injected into every freshly-spawned plain agent.

**Architecture:** Two independent changes. (A) Docs-only edits to `skills/agentctl/SKILL.md` (a size/longevity axis in the rubric, a "Don't" bullet, broadened activation triggers). (B) A small Go change in `internal/lifecycle/lifecycle.go`: a guidance constant plus an env-gated `pipelineHint()` helper that returns a `--append-system-prompt '<guidance>'` fragment, concatenated onto the launch string at the two `Spawn` call sites only. `SpawnJob` (pipeline jobs) and the resume path are untouched, so they are excluded by construction.

**Tech Stack:** Go (stdlib `os`, `testify/require`), Markdown skill file. Tests via `go test ./internal/lifecycle/...`.

**Spec:** `docs/superpowers/specs/2026-06-05-agentctl-pipeline-decomposition-nudge-design.md`

---

## File Structure

- **Modify** `skills/agentctl/SKILL.md` — rubric "Second axis" paragraph, "Don't" bullet, frontmatter `description:` triggers. (Task 1)
- **Modify** `internal/lifecycle/lifecycle.go` — add `pipelineHintGuidance` const + `pipelineHint()` helper (near `claudeLaunch`, ~line 56); concatenate `pipelineHint()` at the two `Spawn` call sites (lines 467, 497). (Tasks 2–3)
- **Modify** `internal/lifecycle/lifecycle_test.go` — new unit tests for `pipelineHint`; new Spawn-inclusion and SpawnJob-exclusion tests; update 4 existing exact-match assertions (lines ~97, ~182, ~432, ~480). (Tasks 2–3)

No new files. No web/TUI/MCP/daemon changes.

---

### Task 1: Skill rubric — size/longevity axis, Don't bullet, triggers

**Files:**
- Modify: `skills/agentctl/SKILL.md` (lines ~3, ~24–36)

Docs-only; no tests. Apply the three edits, verify with grep, commit.

- [ ] **Step 1: Add the "Second axis" paragraph after the litmus test**

In the `## Choosing the tool (read this first)` section, replace:

```markdown
**Litmus test: does any step need to wait for another step's result — its output
or its code — before it can start?** No → plain agent(s). Yes → pipeline.

| Use… | When | How |
```

with:

```markdown
**Litmus test: does any step need to wait for another step's result — its output
or its code — before it can start?** No → plain agent(s). Yes → pipeline.

**Second axis — size & longevity:** would one agent accumulate a large or
long-lived context (a multi-phase task, a long unattended run, anything likely to
approach the context limit and auto-compact)? If yes, **decompose it into a
pipeline of bounded stages** — even when the steps are sequential and one agent
could do them. Each stage gets a fresh, small context and is **torn down on
completion**, returning memory to the OS and avoiding the compaction spikes a
long-lived large-context agent causes.

| Use… | When | How |
```

- [ ] **Step 2: Add the "Don't" bullet**

Replace:

```markdown
**Don't:** use a pipeline for a single task (needless overhead — use a plain agent);
use plain agents + manual relay for a clear dependency chain (that's exactly what a
pipeline automates); hand-roll `ctx`/`msg` coordination that a pipeline already
gives you.
```

with:

```markdown
**Don't:** use a pipeline for a single task (needless overhead — use a plain agent);
use plain agents + manual relay for a clear dependency chain (that's exactly what a
pipeline automates); hand-roll `ctx`/`msg` coordination that a pipeline already
gives you; run a big multi-phase task as one long-lived plain agent — decompose it
into pipeline stages so each agent stays small and closes when its phase finishes.
```

- [ ] **Step 3: Broaden the frontmatter `description:` triggers**

On the `description:` line (line 3), find this substring:

```
"create/run/show/cancel/delete a pipeline", "run these steps in order", "multi-stage or dependent agent work", an analyze→implement→review chain;
```

and replace it with (adds size/longevity trigger phrases):

```
"create/run/show/cancel/delete a pipeline", "run these steps in order", "multi-stage or dependent agent work", an analyze→implement→review chain; "this is a big/multi-phase/long-running task", "this will take a while", "break this down into stages";
```

- [ ] **Step 4: Verify the edits landed**

Run:
```bash
grep -n "Second axis" skills/agentctl/SKILL.md
grep -n "one long-lived plain agent" skills/agentctl/SKILL.md
grep -n "break this down into stages" skills/agentctl/SKILL.md
```
Expected: one match line for each.

- [ ] **Step 5: Commit**

```bash
git add skills/agentctl/SKILL.md
git commit -m "docs(skill): recommend decomposing large tasks into pipelines"
```

---

### Task 2: `pipelineHint()` helper + guidance constant (TDD)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (add after `claudeLaunch`, ~line 56)
- Test: `internal/lifecycle/lifecycle_test.go` (new `TestPipelineHint`)

- [ ] **Step 1: Write the failing test**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestPipelineHint(t *testing.T) {
	t.Run("on by default", func(t *testing.T) {
		t.Setenv("AGENTCTL_NO_PIPELINE_HINT", "") // empty == unset for our check
		got := pipelineHint()
		require.Contains(t, got, "--append-system-prompt")
		require.Contains(t, got, "agentctl pipeline")
		require.True(t, strings.HasPrefix(got, " "), "leading space so it concatenates onto claudeLaunch output")
		require.NotContains(t, got, "\n", "must stay a single typed line")
	})
	t.Run("opt-out via env", func(t *testing.T) {
		t.Setenv("AGENTCTL_NO_PIPELINE_HINT", "1")
		require.Equal(t, "", pipelineHint())
	})
}
```

(If `strings` is not already imported in the test file, add it — it is used widely elsewhere; verify with `grep -n '"strings"' internal/lifecycle/lifecycle_test.go`.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestPipelineHint -v`
Expected: FAIL — `undefined: pipelineHint`.

- [ ] **Step 3: Implement the helper + constant**

In `internal/lifecycle/lifecycle.go`, immediately after `claudeLaunch` (the function ending at line 56), add:

```go
// pipelineHintGuidance is appended to a freshly spawned plain agent's system
// prompt so that, handed a large/multi-phase task, the agent recommends
// decomposing it into an agentctl pipeline of short-lived stages (which keeps
// each agent's context bounded and returns its memory to the OS on teardown)
// before proceeding. Worded conditionally so small tasks trigger no advisory.
// No apostrophes — keeps the single-quoted shell form (shellQuoteArg) clean.
const pipelineHintGuidance = "You were launched as a standalone agentctl agent. " +
	"If this task is large or spans multiple distinct phases (for example analyze, " +
	"implement, test, review) such that you would accumulate a very large context, " +
	"briefly recommend up front that it be split into an agentctl pipeline of smaller " +
	"stages (each a short-lived agent with a fresh, bounded context), then proceed with " +
	"the task as a single agent unless told otherwise."

// pipelineHint returns the claude flag fragment that injects pipelineHintGuidance
// as a system-prompt addendum, or "" when AGENTCTL_NO_PIPELINE_HINT is set (any
// non-empty value, per the repo's env convention). The leading space lets callers
// concatenate it directly onto a claudeLaunch string. Applied only by Spawn (plain
// agents); SpawnJob (pipeline jobs, already decomposed) and resume omit it.
func pipelineHint() string {
	if os.Getenv("AGENTCTL_NO_PIPELINE_HINT") != "" {
		return ""
	}
	return " --append-system-prompt " + shellQuoteArg(pipelineHintGuidance)
}
```

(`os` and `shellQuoteArg` are already in this file.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/lifecycle/ -run TestPipelineHint -v`
Expected: PASS (both subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): add env-gated pipelineHint system-prompt fragment"
```

---

### Task 3: Inject the hint into `Spawn`; update + add integration tests

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (lines 467, 497)
- Test: `internal/lifecycle/lifecycle_test.go` (lines ~97, ~182, ~432, ~480; new inclusion/exclusion tests)

- [ ] **Step 1: Write the failing tests (inclusion + exclusion)**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestSpawnInjectsPipelineHint(t *testing.T) {
	t.Setenv("AGENTCTL_NO_PIPELINE_HINT", "") // force on
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: "fix the auth bug", Cwd: "/work/project"})
	require.NoError(t, err)

	promptFile := "/state/prompts/" + s.ID
	want := claudeLaunch(s.ClaudeSessionID, s.ID, false) + pipelineHint() + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, want, "Enter"})
	require.Contains(t, want, "--append-system-prompt")
}

func TestSpawnRespectsPipelineHintOptOut(t *testing.T) {
	t.Setenv("AGENTCTL_NO_PIPELINE_HINT", "1")
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: "fix the auth bug", Cwd: "/work/project"})
	require.NoError(t, err)

	for _, argv := range fr.calledArgs() {
		for _, a := range argv {
			require.NotContains(t, a, "--append-system-prompt", "opt-out env must suppress the hint")
		}
	}
}

func TestSpawnJobOmitsPipelineHint(t *testing.T) {
	t.Setenv("AGENTCTL_NO_PIPELINE_HINT", "") // even with the hint ON, pipeline jobs must not get it
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	_, err := l.SpawnJob(context.Background(), JobSpawnRequest{
		PipelineID: "p", JobID: "a", Repo: "/repo", Prompt: "do the thing",
	})
	require.NoError(t, err)

	for _, argv := range fr.calledArgs() {
		for _, a := range argv {
			require.NotContains(t, a, "--append-system-prompt", "pipeline jobs are already decomposed")
		}
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/lifecycle/ -run 'TestSpawnInjectsPipelineHint|TestSpawnRespectsPipelineHintOptOut|TestSpawnJobOmitsPipelineHint' -v`
Expected: `TestSpawnInjectsPipelineHint` FAILS (production launch has no hint yet); the other two PASS (no hint exists yet). This confirms the inclusion test is meaningful before the change.

- [ ] **Step 3: Inject the hint at both Spawn call sites**

In `internal/lifecycle/lifecycle.go`, line 467, replace:

```go
		launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
```

with:

```go
		launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) + pipelineHint() + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
```

Then at line 497, replace:

```go
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, claudeLaunch(sess.ClaudeSessionID, id, req.Supervised), "Enter"); err != nil {
```

with:

```go
	launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) + pipelineHint()
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
```

(Do NOT touch line 888 in `SpawnJob` — that is the exclusion guarantee.)

- [ ] **Step 4: Update the 4 existing exact-match assertions**

These tests build the expected launch from `claudeLaunch(...)` and must now include `pipelineHint()` (they run with the env unset, so the hint is on — production and expected both call `pipelineHint()`, so they stay in lockstep).

In `internal/lifecycle/lifecycle_test.go` line ~97, replace:

```go
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "PROJ-350", claudeLaunch(s.ClaudeSessionID, "PROJ-350", false), "Enter"})
```

with:

```go
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "PROJ-350", claudeLaunch(s.ClaudeSessionID, "PROJ-350", false) + pipelineHint(), "Enter"})
```

At line ~182, replace:

```go
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, claudeLaunch(s.ClaudeSessionID, s.ID, false), "Enter"})
```

with:

```go
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, claudeLaunch(s.ClaudeSessionID, s.ID, false) + pipelineHint(), "Enter"})
```

At line ~432 (in `TestSpawnPromptModeLaunchesFromCwd`), replace:

```go
	launch := claudeLaunch(s.ClaudeSessionID, s.ID, false) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
```

with:

```go
	launch := claudeLaunch(s.ClaudeSessionID, s.ID, false) + pipelineHint() + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
```

At line ~480 (in `TestSpawnPromptModeMultilinePromptIsFileBacked`), make the identical replacement:

```go
	launch := claudeLaunch(s.ClaudeSessionID, s.ID, false) + pipelineHint() + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
```

Note: `TestSpawnPromptModeMultilinePromptIsFileBacked` also asserts `require.NotContains(t, launch, "\n", …)`. The guidance is a single line with no `\n`, so this assertion still holds — do not change it.

- [ ] **Step 5: Run the full lifecycle test suite**

Run: `go test ./internal/lifecycle/... -v`
Expected: PASS — including the 3 new tests and the 4 updated assertions.

- [ ] **Step 6: Run the whole repo test suite (no regressions)**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 7: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): inject pipeline-decomposition hint into plain spawns"
```

---

### Task 4: Build + manual live smoke

**Files:** none (verification only — no commit).

This verifies the real `claude` binary accepts `--append-system-prompt` and that the advisory + opt-out behave end-to-end. Requires rebuilding and restarting the running daemon (it predates this change).

- [ ] **Step 1: Build and reinstall**

Run: `make release && make install` (or the repo's documented build — confirm with `grep -n '^install:\|^release:' Makefile`).
Expected: build succeeds; `agentctl` binary updated on PATH.

- [ ] **Step 2: Restart the daemon**

Run (adjust to how the daemon is managed — launchd or foreground):
```bash
launchctl list | grep -i agentctl   # if launchd-managed, unload/load to restart
# otherwise: stop the running `agentctl daemon` and start a fresh one
```
Expected: daemon running on the new binary.

- [ ] **Step 3: Spawn a plain agent and confirm the flag is present**

Spawn a plain agent (CLI): `agentctl start "say hello" --cwd "$PWD"` (or via MCP `spawn_agent`).
Attach to its tmux session and inspect the launched command line, or check the agent's transcript. Confirm the launch included `--append-system-prompt` with the guidance text.
Expected: the flag and guidance are present; the agent runs normally.

- [ ] **Step 4: Confirm the advisory fires on a large task and not a small one**

Spawn an agent with an obviously multi-phase prompt (e.g. "analyze the auth module, implement MFA, add tests, and review it"). Confirm it briefly recommends splitting into a pipeline, then proceeds.
Spawn an agent with a trivial prompt (e.g. "print the current date"). Confirm it does NOT nag about pipelines.
Expected: advisory on the large task, silence on the small one.

- [ ] **Step 5: Confirm the opt-out**

Restart the daemon with `AGENTCTL_NO_PIPELINE_HINT=1` in its environment, spawn a plain agent, and confirm the launch has NO `--append-system-prompt` flag.
Expected: no hint injected.

- [ ] **Step 6: Confirm pipeline jobs are unaffected**

Create + start a small pipeline; confirm the job agents' launch commands do NOT contain `--append-system-prompt`.
Expected: pipeline jobs never get the hint.

---

## Self-Review

**Spec coverage:**
- Component A (rubric second axis, Don't bullet, broadened triggers) → Task 1. ✓
- Component B (guidance constant, env-gated helper, threaded into Spawn only, non-blocking) → Tasks 2–3. ✓ (Refinement vs spec: the helper is *concatenated at the Spawn call sites* rather than passed as a new `claudeLaunch` parameter — same outcome, less churn, `claudeLaunch` stays a pure session/name builder and `SpawnJob`/resume are excluded by simply not calling it.)
- Component C (helper env on/off + quoting; claudeLaunch/Spawn includes flag; SpawnJob excludes) → Task 2 (`TestPipelineHint`) + Task 3 (`TestSpawnInjectsPipelineHint`, `TestSpawnRespectsPipelineHintOptOut`, `TestSpawnJobOmitsPipelineHint`). ✓
- Rollout (rebuild/restart daemon), risks (`--append-system-prompt` support, noise) → Task 4. ✓

**Placeholder scan:** No TBD/TODO; every code/test step shows complete code and exact commands. ✓

**Type consistency:** `pipelineHint()` (no args, returns string) and `pipelineHintGuidance` (const) are used identically in Tasks 2 and 3. `JobSpawnRequest` fields (`PipelineID`, `JobID`, `Repo`, `Prompt`) match the struct at lifecycle.go:811. `SpawnRequest{Prompt, Cwd}` and `New(fr)`/`l.PromptsDir` match existing test usage. ✓
