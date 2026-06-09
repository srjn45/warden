# Quick-launch (blank-prompt interactive agent) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an empty prompt mean "open Claude Code interactively in the chosen project directory and wait," across the lifecycle, daemon, TUI, web, and CLI.

**Architecture:** The non-typed spawn mode is redefined by **cwd, not prompt**. `Type == ""` is "free-form mode" (launch `claude` in `Cwd`); within it a non-empty prompt runs autonomously (unchanged) while an empty prompt launches bare `claude` with no prompt argument and no prompt file. All other spawn machinery (tmux session, `--session-id`, `--name`, `pipelineHint` injection, `supervised`) is identical between the two sub-cases. Typed mode (`Type != ""`, managed worktree) is untouched.

**Tech Stack:** Go (lifecycle, daemon, TUI via Bubble Tea, CLI via cobra), TypeScript/React (Astro web), tests via Go `testing`+`testify` and Vitest.

---

## Spec reference

`docs/superpowers/specs/2026-06-08-agentctl-quick-launch-interactive-design.md`

## File Structure

- `internal/lifecycle/lifecycle.go` — `Spawn()` free-form branch (core behavior change).
- `internal/lifecycle/lifecycle_test.go` — new interactive-spawn test; existing prompt-mode test is the regression guard.
- `internal/daemon/lifecycle_routes.go` — `handleSpawn()` validation + classification guard.
- `internal/daemon/lifecycle_routes_test.go` — new route tests; `fakeLife.Spawn` id logic updated to free-form.
- `internal/tui/keys.go` — `updateNewAgent` empty-prompt handling.
- `internal/tui/view.go` (or wherever the new-agent footer hint lives) — hint text.
- `internal/tui/model_test.go` — new empty-prompt spawn test.
- `internal/cli/lifecycle.go` — `newStartCmd` free-form arg handling + new `promptFromArgs` helper.
- `internal/cli/lifecycle_test.go` — `promptFromArgs` unit test.
- `web/src/components/NewAgentModal.tsx` + `web/src/components/QuickSpawn.tsx` — drop prompt-required guard, update placeholders.

---

## Task 1: Lifecycle — empty prompt launches bare claude (interactive)

**Files:**
- Modify: `internal/lifecycle/lifecycle.go:485-545`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/lifecycle/lifecycle_test.go`:

```go
func TestSpawnInteractiveNoPromptLaunchesBareClaude(t *testing.T) {
	t.Setenv("AGENTCTL_NO_PIPELINE_HINT", "") // anchor: hint on, matches production
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "" // intentionally empty: the no-prompt path must not need it
	s, err := l.Spawn(context.Background(), SpawnRequest{Cwd: "/work/project"})
	require.NoError(t, err)

	// Launches from the caller cwd, like prompt-mode.
	require.Equal(t, "/work/project", s.Workdir)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", s.ID, "-e", "AGENTCTL_SESSION_ID=" + s.ID, "-c", "/work/project"})

	// No prompt file is written for an interactive agent.
	require.Equal(t, -1, fr.callIndex("mkdir -p "), "no prompts-dir mkdir for empty prompt")
	for _, c := range fr.Calls {
		require.NotEqual(t, "mkdir", c.Argv[0], "no mkdir of a prompts dir")
		if c.Argv[0] == "sh" {
			require.NotContains(t, c.Argv, `printf '%s' "$1" > "$2"`, "no prompt-file write")
		}
	}

	// The launch carries session-id, name, and the pipeline hint, but NO cat fragment.
	expectedLaunch := claudeLaunch(s.ClaudeSessionID, s.ID, false) + pipelineHint()
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, expectedLaunch, "Enter"})

	// Subject reads cleanly in the agent list instead of being blank.
	require.Equal(t, "interactive", s.Subject)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/lifecycle/ -run TestSpawnInteractiveNoPromptLaunchesBareClaude -v`
Expected: FAIL — current code requires a non-empty prompt path / writes a prompt file / sets a blank subject.

- [ ] **Step 3: Implement the free-form branch**

In `internal/lifecycle/lifecycle.go`, replace the current prompt-mode block. Change the mode local and the subject, and make the prompt file + cat fragment conditional on a non-empty prompt.

Replace line 486:
```go
	promptMode := req.Prompt != "" && req.Type == ""
	if !promptMode {
		req.Type = store.NormalizeType(string(req.Type))
	}
```
with:
```go
	freeMode := req.Type == ""
	if !freeMode {
		req.Type = store.NormalizeType(string(req.Type))
	}
```

Replace the `Subject:` field in the `sess := &store.Session{...}` literal (line 503):
```go
		Subject:     firstWords(req.Prompt, 10),
```
with:
```go
		Subject:     spawnSubject(req.Prompt),
```

Replace the whole `if promptMode { … }` block (lines 509-545) with:
```go
	if freeMode {
		// The agent runs in the caller's directory (the "master shell"), which is
		// already trusted by Claude Code — we never create a fresh per-agent dir,
		// which would trigger Claude's per-directory trust/onboarding prompts on
		// every spawn. cwd is required: there is no directory to fall back to.
		if req.Cwd == "" {
			return nil, fmt.Errorf("free-form spawn requires a launch dir (cwd)")
		}
		sess.Workdir = req.Cwd

		// launchPrompt is the trailing claude argument. Empty for an interactive
		// agent (open claude and wait); for an autonomous agent it reads the prompt
		// back from a file via "$(cat …)". Persisting the prompt to a file (keyed by
		// id, in a shared state dir outside the caller's project) keeps the command
		// typed into the pane to a single physical line: a multi-line prompt typed
		// directly would have its embedded newlines register as Enter and submit a
		// half-typed command. The prompt is passed to the writer as an exec argument
		// (never through a shell), so quotes and newlines in it need no escaping.
		launchPrompt := ""
		if req.Prompt != "" {
			if l.PromptsDir == "" {
				return nil, fmt.Errorf("prompt spawn requires a prompts dir")
			}
			if out, err := l.run.Run(ctx, "", "mkdir", "-p", l.PromptsDir); err != nil {
				return nil, fmt.Errorf("mkdir prompts dir: %w: %s", err, out)
			}
			promptFile := filepath.Join(l.PromptsDir, id)
			if out, err := l.run.Run(ctx, "", "sh", "-c", `printf '%s' "$1" > "$2"`, "sh", req.Prompt, promptFile); err != nil {
				return nil, fmt.Errorf("write prompt file: %w: %s", err, out)
			}
			launchPrompt = ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
		}

		if err := l.newAgentSession(ctx, "", id, req.Cwd); err != nil {
			return nil, err
		}
		launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) + pipelineHint() + launchPrompt
		if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
			l.killSession(id) // the session exists but launch failed — don't orphan it
			return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
		}
		return sess, nil
	}
```

Add the `spawnSubject` helper near `firstWords` (search for `func firstWords`):
```go
// spawnSubject is the short list-view label for a spawned agent: the first
// words of its prompt, or "interactive" when there is no prompt (the agent was
// opened to wait for instructions typed into Claude directly).
func spawnSubject(prompt string) string {
	if prompt == "" {
		return "interactive"
	}
	return firstWords(prompt, 10)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/lifecycle/ -run 'TestSpawnInteractiveNoPromptLaunchesBareClaude|TestSpawnPromptModeLaunchesFromCwd' -v`
Expected: PASS for both — the new interactive test and the existing prompt-mode regression guard.

- [ ] **Step 5: Run the full lifecycle package**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/lifecycle/`
Expected: PASS (ok).

- [ ] **Step 6: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): empty prompt launches claude interactively

Free-form spawn (no --type) is now keyed on cwd, not prompt. An empty
prompt skips the prompt file and launches bare claude (session-id + name
+ pipeline hint, no prompt arg) so it waits for instructions."
```

---

## Task 2: Daemon — accept empty-prompt free-form spawn

**Files:**
- Modify: `internal/daemon/lifecycle_routes.go:47-59`, `:115-117`
- Test: `internal/daemon/lifecycle_routes_test.go`
- Modify (test fake): `internal/daemon/lifecycle_routes_test.go` `fakeLife.Spawn`

- [ ] **Step 1: Write the failing tests**

Append to `internal/daemon/lifecycle_routes_test.go`:

```go
func TestPostSpawnInteractiveNoPrompt(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	dir := t.TempDir() // cwd must be an existing directory
	body, _ := json.Marshal(SpawnRequest{Cwd: dir})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "empty prompt + cwd is a valid interactive spawn")
	require.Equal(t, dir, fl.spawnedCwd)
	require.Equal(t, "", fl.spawned.Prompt)
}

func TestPostSpawnRejectsEmptyRequest(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{}) // no type, no repo, no cwd
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
```

Also update `fakeLife.Spawn`'s id logic (in the same file) so a free-form spawn yields a stable id. Change:
```go
	promptMode := req.Prompt != "" && req.Type == ""
	id := req.Ticket
	if id == "" {
		if promptMode {
			id = "agent-test"
		} else {
			id = req.Type + "-auto"
		}
	}
	typ := store.Type("")
	if !promptMode {
		typ = store.NormalizeType(req.Type)
	}
```
to:
```go
	freeMode := req.Type == ""
	id := req.Ticket
	if id == "" {
		if freeMode {
			id = "agent-test"
		} else {
			id = req.Type + "-auto"
		}
	}
	typ := store.Type("")
	if !freeMode {
		typ = store.NormalizeType(req.Type)
	}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/daemon/ -run 'TestPostSpawnInteractiveNoPrompt|TestPostSpawnRejectsEmptyRequest' -v`
Expected: `TestPostSpawnInteractiveNoPrompt` FAILS with 400 (current handler requires a prompt or type+repo). `TestPostSpawnRejectsEmptyRequest` passes already (it should remain green).

- [ ] **Step 3: Relax the handler validation and classification guard**

In `internal/daemon/lifecycle_routes.go`, replace lines 47-59:
```go
	promptMode := req.Prompt != "" && req.Type == ""
	if !promptMode {
		if req.Type == "" || req.Repo == "" {
			writeErr(w, http.StatusBadRequest, "provide a prompt, or type and repo")
			return
		}
		// Reject an unknown type rather than silently collapsing it to "other".
		if !store.Type(req.Type).Valid() {
			writeErr(w, http.StatusBadRequest, "unknown type "+req.Type+
				"; valid: development, analysis, spike, pr-review, buildkite-debug, test-run, env-test, other")
			return
		}
	}
```
with:
```go
	freeMode := req.Type == ""
	if !freeMode {
		if req.Repo == "" {
			writeErr(w, http.StatusBadRequest, "typed spawn requires repo")
			return
		}
		// Reject an unknown type rather than silently collapsing it to "other".
		if !store.Type(req.Type).Valid() {
			writeErr(w, http.StatusBadRequest, "unknown type "+req.Type+
				"; valid: development, analysis, spike, pr-review, buildkite-debug, test-run, env-test, other")
			return
		}
	}
```

Replace line 71 (the cwd guard):
```go
	if promptMode && req.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "prompt-mode spawn requires cwd (the directory to launch the agent in)")
		return
	}
```
with:
```go
	if freeMode && req.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "provide a launch dir (cwd; prompt optional), or type and repo")
		return
	}
```

Replace the classification trigger at lines 115-117:
```go
	if promptMode {
		go s.classifyAndUpdate(sess.ID, req.Prompt)
	}
```
with:
```go
	if freeMode && req.Prompt != "" {
		go s.classifyAndUpdate(sess.ID, req.Prompt)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/daemon/`
Expected: PASS (ok) — new tests green and all existing daemon route tests still green.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/lifecycle_routes.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): accept empty-prompt free-form spawn

Validation is keyed on type vs cwd, not prompt: a free-form spawn needs
only a cwd. Skip classification when there is no prompt to classify."
```

---

## Task 3: TUI — Ctrl+S with an empty prompt spawns interactively

**Files:**
- Modify: `internal/tui/keys.go:135-144`
- Modify: the new-agent footer hint (search for the modeNewAgent help text in `internal/tui/view.go`)
- Test: `internal/tui/model_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/model_test.go`:

```go
func TestNewAgentEmptyPromptSpawnsInteractive(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	// Submit immediately, without typing a prompt.
	m, _ = submit(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, f.spawned, "empty prompt now spawns an interactive agent")
	require.Equal(t, "", f.spawned.Prompt)
	require.NotEqual(t, "prompt was empty", m.status)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run TestNewAgentEmptyPromptSpawnsInteractive -v`
Expected: FAIL — `f.spawned` is nil because the current code sets `m.status = "prompt was empty"` and returns without spawning.

- [ ] **Step 3: Remove the empty-prompt rejection**

In `internal/tui/keys.go`, replace the `case tea.KeyCtrlS:` block (lines 135-144):
```go
	case tea.KeyCtrlS:
		prompt := strings.TrimSpace(m.ta.Value())
		m.mode = modeNormal
		m.ta.Blur()
		if prompt == "" {
			m.status = "prompt was empty"
			return m, nil
		}
		m.pendingPrompt, m.pendingDir = prompt, m.targetDir
		return m, spawnCmd(m.api, prompt, m.targetDir, false)
```
with:
```go
	case tea.KeyCtrlS:
		// An empty prompt is intentional: it opens claude in the target dir and
		// waits for the user to type instructions into Claude directly.
		prompt := strings.TrimSpace(m.ta.Value())
		m.mode = modeNormal
		m.ta.Blur()
		m.pendingPrompt, m.pendingDir = prompt, m.targetDir
		return m, spawnCmd(m.api, prompt, m.targetDir, false)
```

- [ ] **Step 4: Update the new-agent footer hint**

Find the modeNewAgent help/footer string in `internal/tui/view.go`:
Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && grep -rn "ctrl+s\|ctrl-s\|tab.*dir\|new agent" internal/tui/view.go`

In the footer line shown while `m.mode == modeNewAgent`, append a clause so the hint reads (adapt to the exact existing wording):
```go
		// e.g. existing: "ctrl+s spawn · tab pick dir · esc cancel"
		// becomes:
		"ctrl+s spawn (blank = open Claude & wait) · tab pick dir · esc cancel"
```
If the exact text differs, keep the existing phrasing and add the `(blank = open Claude & wait)` note next to the ctrl+s hint.

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/`
Expected: PASS (ok) — new test green, existing `TestNewAgentModeFlow` (typed prompt) still green.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/keys.go internal/tui/view.go internal/tui/model_test.go
git commit -m "feat(tui): blank prompt in new-agent opens Claude interactively

Ctrl+S no longer rejects an empty prompt; it spawns an interactive agent
in the target dir. Footer hint updated."
```

---

## Task 4: CLI — `agentctl start --dir <path>` with no prompt

**Files:**
- Modify: `internal/cli/lifecycle.go:23-47`, and command help at `:17-18`
- Test: `internal/cli/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/lifecycle_test.go`:

```go
func TestPromptFromArgs(t *testing.T) {
	require.Equal(t, "fix the bug", promptFromArgs([]string{"fix the bug"}), "single arg is the prompt")
	require.Equal(t, "", promptFromArgs(nil), "no args means an interactive (empty-prompt) spawn")
	require.Equal(t, "", promptFromArgs([]string{}), "no args means an interactive (empty-prompt) spawn")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/cli/ -run TestPromptFromArgs -v`
Expected: FAIL — `promptFromArgs` is undefined.

- [ ] **Step 3: Add the helper and use it in the free-form branch**

In `internal/cli/lifecycle.go`, add the helper above `newStartCmd`:
```go
// promptFromArgs returns the prompt for a free-form (no --type) spawn: the
// single positional argument, or "" when none is given — an empty prompt opens
// claude interactively in the launch dir and waits for instructions.
func promptFromArgs(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return ""
}
```

Replace the prompt-mode block (lines 23-47) so it no longer rejects zero args:
```go
			// Prompt mode: `agentctl start "<prompt>"` with no --type.
			if typ == "" {
				if len(args) != 1 {
					return fmt.Errorf("provide a prompt: agentctl start \"<prompt>\"  (or use --type for a managed worktree)")
				}
				dirFlag, _ := cmd.Flags().GetString("dir")
				dir, err := resolveDir(dirFlag)
				if err != nil {
					return err
				}
				supervised, _ := cmd.Flags().GetBool("supervised")
				force, _ := cmd.Flags().GetBool("force")
				s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Prompt: args[0], Cwd: dir, Supervised: supervised, Force: force})
				if err != nil {
					var cre *client.ErrConfirmationRequired
					if errors.As(err, &cre) {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"⚠ memory pressure: %s\n  re-run with --force to spawn anyway\n", cre.Verdict.Reason)
						return fmt.Errorf("spawn blocked by memory-pressure gate")
					}
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "spawned %s (classifying…) — attach with `agentctl attach %s`\n", s.ID, s.ID)
				return nil
			}
```
with:
```go
			// Free-form mode: `agentctl start "<prompt>" [--dir]` (autonomous) or
			// `agentctl start --dir <path>` with no prompt (interactive: opens
			// claude in the dir and waits). No --type.
			if typ == "" {
				prompt := promptFromArgs(args)
				dirFlag, _ := cmd.Flags().GetString("dir")
				dir, err := resolveDir(dirFlag)
				if err != nil {
					return err
				}
				supervised, _ := cmd.Flags().GetBool("supervised")
				force, _ := cmd.Flags().GetBool("force")
				s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Prompt: prompt, Cwd: dir, Supervised: supervised, Force: force})
				if err != nil {
					var cre *client.ErrConfirmationRequired
					if errors.As(err, &cre) {
						fmt.Fprintf(cmd.ErrOrStderr(),
							"⚠ memory pressure: %s\n  re-run with --force to spawn anyway\n", cre.Verdict.Reason)
						return fmt.Errorf("spawn blocked by memory-pressure gate")
					}
					return err
				}
				verb := "spawned %s (classifying…)"
				if prompt == "" {
					verb = "opened interactive agent %s"
				}
				fmt.Fprintf(cmd.OutOrStdout(), verb+" — attach with `agentctl attach %s`\n", s.ID, s.ID)
				return nil
			}
```

Update the command `Short`/`Use` (lines 17-18) to mention the interactive form:
```go
		Use:   "start [TICKET|\"<prompt>\"] [--type <TYPE>] [--dir <PATH>]",
		Short: "Spawn an agent — `start \"<prompt>\"` (auto-typed), `start --dir <path>` (interactive: open Claude & wait), or `start TICKET --type <TYPE>` (managed worktree)",
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/cli/`
Expected: PASS (ok) — `TestPromptFromArgs` green, existing CLI tests still green.

- [ ] **Step 5: Build the whole module**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go build ./...`
Expected: no output (success).

- [ ] **Step 6: Commit**

```bash
git add internal/cli/lifecycle.go internal/cli/lifecycle_test.go
git commit -m "feat(cli): agentctl start --dir with no prompt opens interactive agent

A free-form start with zero positional args is an interactive spawn
(empty prompt) launched in the resolved --dir."
```

---

## Task 5: Web — allow a blank prompt in the new-agent forms

**Files:**
- Modify: `web/src/components/NewAgentModal.tsx:18`, `:39-48`
- Modify: `web/src/components/QuickSpawn.tsx:16`, `:32-38`

No React component-test harness exists (web tests are Vitest lib-only). This task is verified by the TypeScript build (typecheck) + the existing Vitest suite staying green + a manual browser smoke. The client/api layer already accepts an empty `prompt` (covered by `web/src/lib/api.test.ts`), so no api change is needed.

- [ ] **Step 1: Drop the prompt-required guard in NewAgentModal**

In `web/src/components/NewAgentModal.tsx`, in `doSpawn`, delete line 18:
```tsx
    if (!prompt.trim()) { setErr('a prompt is required'); return; }
```
(Keep the `if (!dir) …` line directly below it.)

Update the prompt textarea label + placeholder (lines 39 and 44):
```tsx
        <label>What should this agent do? <span className="muted">(leave blank to open Claude and type instructions yourself)</span>
          <textarea
            rows={6}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            placeholder="Leave blank to open Claude interactively, or describe a task to run autonomously…"
            autoFocus
            onKeyDown={(e) => { if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) doSpawn(false); }}
          />
        </label>
```
The submit button is already gated on `!dir` only (line 64), so no change there.

- [ ] **Step 2: Drop the prompt-required guard in QuickSpawn**

In `web/src/components/QuickSpawn.tsx`, in `submit`, delete line 16:
```tsx
    if (!prompt.trim()) { setErr('a prompt is required'); return; }
```
Update the textarea placeholder (line 36):
```tsx
        placeholder="Describe a task, or leave blank to open Claude and type instructions yourself (⌘/Ctrl+Enter to launch)"
```
The button is already gated on `!dir` only (line 45), so no change there.

- [ ] **Step 3: Typecheck-build the web bundle**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl/web && npm run build`
Expected: build succeeds (Astro build runs `tsc`/type-checking; no type errors).

- [ ] **Step 4: Run the web unit tests**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl/web && npm test`
Expected: PASS — all existing Vitest suites green (no regression in `api.test.ts` etc.).

- [ ] **Step 5: Commit**

```bash
git add web/src/components/NewAgentModal.tsx web/src/components/QuickSpawn.tsx
git commit -m "feat(web): allow blank prompt to open an interactive agent

Both the new-agent modal and the Overview QuickSpawn now submit on a
chosen dir alone; a blank prompt opens Claude interactively."
```

---

## Task 6: Full verification + release

**Files:** none (build/verify only).

- [ ] **Step 1: Run the whole Go test suite**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./...`
Expected: PASS (ok) across all packages. (If unrelated heavy tmux/daemon packages time out under machine contention, re-run the touched packages — `./internal/lifecycle/ ./internal/daemon/ ./internal/tui/ ./internal/cli/` — in isolation and confirm green.)

- [ ] **Step 2: Build the release binary (embeds the web bundle)**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && make release`
Expected: build succeeds; the binary embeds the rebuilt `web/dist`.

- [ ] **Step 3: Hand off to the user for install + live smoke**

Print this checklist for the user (validation and daemon restart are theirs to run):

```
Reinstall + restart the daemon (validation/lifecycle changes are daemon-side):
  make install   # or the repo's reinstall script
  # restart the running daemon so the new validation + web bundle take effect

Live smoke:
  1. Web Overview QuickSpawn: pick a dir, leave the prompt blank, Launch →
     a new agent appears (subject "interactive").
  2. Attach (web tmux terminal or `agentctl attach <id>`) → Claude is sitting
     at its own prompt; type instructions and confirm native UX (skill autofill).
  3. While idle, confirm the agent shows `waiting_for_input` (not "stuck").
  4. TUI: `n`, Ctrl+S without typing → interactive agent spawns.
  5. CLI: `agentctl start --dir <path>` (no prompt) → "opened interactive agent …".
```

---

## Self-review notes

- **Spec coverage:** lifecycle empty-prompt launch (Task 1), daemon validation + classification guard (Task 2), TUI (Task 3), CLI (Task 4), web (Task 5), rebuild/smoke (Task 6) — every spec section maps to a task. MCP is explicitly out of scope per the spec.
- **Type consistency:** `freeMode` is the consistent mode name in both lifecycle (Task 1) and daemon (Task 2); `spawnSubject` (Task 1) and `promptFromArgs` (Task 4) are each defined before use. The lifecycle test references `claudeLaunch`, `pipelineHint`, `shellQuoteArg`, `fr.calledArgs()`, `fr.callIndex()`, `FakeRunner.Calls` — all existing symbols.
- **Placeholders:** none — every code step shows the full replacement.
- **Web testing honesty:** no component-test harness is added (YAGNI); the web task is verified by typecheck-build + existing Vitest + manual smoke, stated explicitly.
