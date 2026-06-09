# agentctl quick-launch (blank-prompt interactive agent)

**Date:** 2026-06-08
**Status:** Approved — ready for implementation plan

## Problem

Spawning an agent today always requires an initial prompt. The user writes the
task up front and the agent runs autonomously. But the prompt-entry box on the
TUI and web is a plain textarea — it does not replicate Claude Code's own
interactive affordances (skill-name autofill, file completion, etc.). When the
user simply wants to *start working in a project*, typing the whole instruction
into a degraded box is friction.

The user wants a quick-launch: pick a project directory, open Claude Code in a
fresh tmux session in that directory, and **wait** — the user then types their
instructions into Claude's own native UI (by attaching), getting full Claude
Code UX.

## Key insight

Almost all of the plumbing already exists. agentctl's **prompt-mode** spawn
(`Type == ""`, `Cwd` set) already launches `claude` in a chosen directory inside
a fresh tmux session, with `/fs/dirs` directory pickers in both the TUI (`n`) and
web (NewAgentModal / QuickSpawn). The only thing blocking quick-launch is that
every surface **requires a non-empty prompt** and the lifecycle always passes the
prompt as Claude's first CLI argument (running it autonomously).

So this is not a new subsystem — it is **letting an empty prompt mean "open
Claude interactively and wait."**

## Design decisions (confirmed with user)

1. **UX shape:** Blank prompt = interactive. Reuse the existing new-agent forms;
   leaving the prompt empty produces an interactive agent. No separate
   button/key/verb. (One flow, minimal new surface.)
2. **System prompt:** Keep injection. Interactive agents still get the
   `--append-system-prompt` pipeline hint, identical to autonomous spawns, so
   behavior is consistent across all agentctl agents.
3. **Surfaces:** TUI, Web, CLI. MCP `spawn_agent` is **not** modified (it already
   accepts an empty prompt in its schema and inherits the relaxed daemon
   validation for free — acceptable, not a documented capability).

## Core semantic change

The non-typed spawn mode is redefined by **cwd, not prompt**:

- **Free-form mode** = `Type == ""` → launches `claude` in `Cwd` (cwd required).
  - **prompt non-empty** → autonomous, exactly as today (prompt written to the
    prompts-dir file and cat'd as Claude's first argument).
  - **prompt empty** → **interactive**: launch bare `claude` (no prompt argument,
    no prompt file) and let it sit at its own prompt. The user drives it natively
    by attaching — TUI detail pane or the web `tmux attach` terminal.
- **Typed mode** = `Type != ""` → unchanged (managed worktree).

Everything else is identical between the two free-form sub-cases: tmux session
creation (`newAgentSession`), `--session-id` pinning, `--name` label,
`pipelineHint()` system-prompt injection, and the `supervised` flag.

## Changes by layer

### 1. `internal/lifecycle/lifecycle.go` — `Spawn()` (~485–545)

- Rename the local `promptMode := req.Prompt != "" && req.Type == ""` to
  `freeMode := req.Type == ""` (semantics now cwd-driven, not prompt-driven).
- When `req.Prompt == ""`:
  - Skip the `mkdir` of `PromptsDir` and the prompt-file write entirely.
  - Build `launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) +
    pipelineHint()` — **no** trailing `"$(cat …)"` fragment.
  - When non-empty: unchanged (prompt file + cat fragment).
- `Subject`: when the prompt is empty, set a placeholder `"interactive"` instead
  of `firstWords("")` (which would be blank) so the agent list reads cleanly.
- The `Cwd == ""` and `PromptsDir == ""` guards: keep the cwd guard (still
  required for free-form). The `PromptsDir` guard only needs to fire on the
  non-empty-prompt path (no prompt file is written when the prompt is empty).

### 2. `internal/daemon/lifecycle_routes.go` — `handleSpawn()` (~47–73)

- Rename `promptMode := req.Prompt != "" && req.Type == ""` to
  `freeMode := req.Type == ""`.
- Validation:
  - Free-form (`freeMode`) requires `Cwd` (already enforced at ~71). Drop the
    implicit "prompt required" coupling.
  - Typed branch (`!freeMode`) still requires `Repo` and a valid `Type`.
  - Update the error string from `"provide a prompt, or type and repo"` to
    `"provide a launch dir (prompt optional), or type and repo"`.
- Classification guard: `if freeMode && req.Prompt != "" { go
  s.classifyAndUpdate(sess.ID, req.Prompt) }` — there is nothing to classify for
  an empty prompt.

### 3. TUI — `internal/tui/keys.go` `updateNewAgent` (~121–149)

- Remove the `if prompt == "" { m.status = "prompt was empty"; return }`
  rejection on `Ctrl+S`. An empty prompt now spawns an interactive agent. The
  launch directory still comes from `m.targetDir` (must be non-empty, as today).
- Update the new-agent modal footer hint to note that a blank prompt opens Claude
  and waits.

### 4. Web — `web/src/components/NewAgentModal.tsx` + `QuickSpawn.tsx`

- Drop the `if (!prompt.trim())` guard in `doSpawn`. Keep the directory
  requirement (`if (!dir) …`).
- Submit button enabled on directory alone (no longer gated on prompt).
- Prompt textarea placeholder → "Leave blank to open Claude interactively and
  type instructions yourself."

### 5. CLI — `internal/cli/lifecycle.go` `newStartCmd`

- Allow `agentctl start --dir <path>` (or default cwd) with no prompt argument
  and no `--type` → interactive free-form spawn. Relax the validation that
  currently requires a non-empty prompt for prompt-mode; `--dir` (or the default
  current directory) is the only requirement for free-form.

## Out of scope

- MCP `spawn_agent` tool — unchanged. It inherits the relaxed daemon validation
  but the capability is not documented in its schema/description.
- No new HTTP endpoint, no new client method, no new CLI verb.

## Edge cases & notes

- **Monitoring:** an idle interactive agent will read as `waiting_for_input` —
  the correct, first-class state, not "stuck." No code change; verify in the live
  smoke test.
- **Daemon rebuild:** validation + lifecycle are daemon-side, so this requires
  `make release` / `make install` + daemon restart. The web bundle is embedded in
  the binary, so the npm build is part of `make release`.

## Testing

- **`lifecycle` unit test:** empty-prompt free-form spawn → asserts the launch
  command has **no** `cat` fragment, **does** carry `--session-id`, `--name`, and
  the `pipelineHint` `--append-system-prompt`, and that **no** prompt file is
  written. A companion test confirms the non-empty-prompt path is unchanged
  (still writes the file + cat fragment).
- **`daemon` route test:** `{cwd: <existing dir>}` with empty prompt and empty
  type → `201 Created` (was `400`). `{}` with nothing → `400`. Typed spawn
  unchanged.
- **TUI:** `updateNewAgent` empty-prompt `Ctrl+S` issues a spawn command (no
  "prompt was empty" status).
- **Web:** form submits with a blank prompt and a chosen directory.
- **Live smoke (manual, by user):** quick-launch from web + TUI → attach →
  confirm Claude is sitting interactively and accepts typed instructions
  natively; confirm the agent shows `waiting_for_input` while idle.
