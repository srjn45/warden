---
title: CLI command reference
description: Every warden command and its flags, generated from warden --help.
---

> Generated from `warden --help` on 2026-06-26; regenerate when commands change.

All commands accept `--addr` to point at a non-default daemon (overrides the `addr` config setting) and `--config` to point at a non-default config file (default `~/.warden/config.yaml`). `<TICKET>` is the agent ID — a Jira key for managed agents, or an `agent-xxxx` ID for prompt-spawned ones.

## warden

```text
spawn, monitor, and tear down Claude Code agent sessions.
Run `warden` with no arguments to open the cockpit TUI. Alias: wd.

Usage:
  warden [flags]
  warden [command]

Available Commands:
  adopt               Register the Claude session in this directory (resume it under tmux, or register the current tmux session live)
  approvals           List pending tool-permission prompts waiting for an answer
  approve             Answer a pending tool-permission prompt by option number
  attach              Attach to the agent's tmux session
  audit               Inspect the append-only action audit trail
  auto-approve        Toggle per-agent auto-approve, or manage the auto-approve rule policy
  branches            Per-agent CI status and standing vs origin/main (opt-in monitor)
  check               Run the project's configured checks and report only failures
  collab              Inter-agent collaboration: see which agents are editing the same files
  commit              Stage and commit the worktree (warden rails + hooks + bookkeeping)
  completion          Generate shell completion scripts
  config              Show the resolved configuration (and its file path)
  ctx                 Read and write the shared context (a namespaced key/value store agents share)
  daemon              Run the warden hub (HTTP API + poller; the single writer to the file store)
  delete              Clear an agent's stored record (archives by default; --hard to purge)
  digest              Summarize what an agent accomplished (files, branch, turns, narrative)
  doctor              Run preflight checks (required binaries, daemon, data dir)
  done                Terminate an agent and clear its record (does NOT remove the worktree)
  export              Serialize agent session metadata to JSON on stdout
  force-compact       Override force-compact for one agent (interrupt → /compact → resume)
  handoff             Hand off work: delegate to a new/existing agent (--to), or retire self into a successor (--retire)
  history             Browse archived (closed) agents, newest first
  import              Insert agent session metadata from a JSON dump on stdin
  insights            Mine agent history for patterns and parallelization wins
  library             Browse saved spawn presets, prompt templates, and pipeline templates in one place
  llm                 Local-LLM helpers for the REPL (wd repl)
  ls                  List all active agent sessions
  mcp                 Run the MCP stdio server so an orchestrator Claude can manage agents
  msg                 Send and receive directed messages between agents
  pipeline            Define and run DAG pipelines of agent jobs
  plugin              Inspect the plugin registry (custom task types + lifecycle hooks)
  preset              Save and list named spawn configs (replay with `warden start --preset <name>`)
  prompt-template     Save and list reusable, variabled prompt templates (fill with `warden start --prompt-template <name> --set VAR=value`)
  prune               Reclaim orphaned warden worktrees under .worktrees (always asks; --force overrides guards)
  push                Push the current branch to origin (warden rails + bookkeeping)
  remove-worktree     Remove an agent's git worktree + branch (always asks; --force overrides guards)
  repl                Interactive REPL for agents, pipelines, and the git/check lifecycle (local LLM + `/` commands)
  restore             Recreate and resume a lost/orphaned agent (claude --resume)
  rotate              Retire this agent and hand its work to a fresh successor in the same workspace (alias for `handoff --retire`)
  savings             Show the token reductions warden's lifecycle features have earned
  schedule            Fire an agent or pipeline on a cron/at timer (opt-in)
  search              Full-text search agents by subject, prompt, type, name, branch, or pane text
  send                Type a message into an agent's claude session and press Enter
  set-permission-mode Set the permission mode for an agent
  setup               Install missing dependencies (tmux, git, claude; optional gh, ollama)
  spend               Show measured Claude spend in dollars, per agent / repo / day
  snapshot            Checkpoint a worktree + transcript and roll back later
  start               Spawn an agent — `start "<prompt>"` (auto-typed), `start --dir <path>` (interactive: open Claude & wait), or `start TICKET --type <TYPE>` (managed worktree)
  stats               Show warden's resource footprint (per-agent memory/CPU, system pressure, daemon stats)
  stop                Tear down an agent — the single umbrella verb (default: terminate + clear record + remove worktree)
  status              Show full status for one session
  sync                Fetch and rebase the current branch onto its base (warden conflict detect)
  tail                Print the recent output of an agent's claude session
  terminate           Stop an agent: kill its tmux+claude session (keeps the record and worktree)
  token               Manage the daemon's remote-access bearer token
  tui                 Live terminal cockpit for agents
  tutorial            Run the first-run guided walkthrough
  worktree            Inspect and reclaim warden's git worktrees: list and prune, in one place

Flags:
      --addr string     daemon address (overrides the addr config setting)
      --config string   config file path (default ~/.warden/config.yaml)
  -h, --help            help for warden
  -v, --version         version for warden
```

## warden start

```text
Spawn an agent — `start "<prompt>"` (auto-typed), `start --dir <path>` (interactive: open Claude & wait), or `start TICKET --type <TYPE>` (managed worktree)

Usage:
  warden start [TICKET|"<prompt>"] [--type <TYPE>] [--dir <PATH>] [--backend <ID>] [flags]

Flags:
      --auto-restart             auto-resume this agent if it crashes (errored), capped at a few attempts
      --backend string           agent backend to drive: claude (default), aider, or opencode. Backends differ in capabilities — aider & opencode are bring-your-own-model (pass --model) with tokens-only spend; aider has no resume and runs an autonomous task that exits when done; opencode has a structured (Tier A) transcript and DOES resume the worktree's last session
      --branch string            new branch (development) or checkout target (pr-review)
      --dir string               directory to launch the agent from (default: current directory)
      --force                    spawn even when the memory-pressure gate warns
  -h, --help                     help for start
      --in-repo                  write-agent opt-out: run in the shared repo instead of an isolated worktree (ignored for pr-review)
      --model string             claude model: opus, sonnet, haiku, fable, or full model ID (default: the model_default config setting)
      --name string              optional human-friendly name (max 32 chars, alphanumeric + hyphens/underscores)
      --permission-mode string   permission mode: acceptEdits|auto|bypassPermissions|default|dontAsk|plan (default: from config or 'auto')
      --pr string                PR number/url (pr-review)
      --preset string            load saved spawn defaults from a named preset (see `warden preset`); explicit flags override
      --prompt-template string   fill a saved prompt template (see `warden prompt-template`) as the spawn prompt; a positional prompt still wins
      --repo string              repo path (default: current directory)
      --set stringArray          supply a prompt-template variable as VAR=value (repeatable, e.g. --set FILE=foo.go --set X=y)
      --supervised               alias for --permission-mode acceptEdits (kept for backwards compatibility)
      --tags strings             comma-separated labels for grouping/filtering (searchable; filter with `warden ls --tag`)
      --type string              task type: development|analysis|spike|pr-review|code|docs|website|debug-ci|tests|other
      --worktree                 create a scratch worktree for analysis/spike
```

## warden ls

```text
List all active agent sessions

Usage:
  warden ls [flags]

Flags:
  -h, --help          help for ls
      --json          output as JSON
      --tag strings   only show agents carrying every given tag (repeatable or comma-separated)
  -w, --watch         live-update the list on every agent state change (Ctrl+C to exit)
```

## warden status

```text
Show full status for one session

Usage:
  warden status <TICKET> [flags]

Flags:
  -h, --help   help for status
      --json   output as JSON
```

## warden attach

```text
Attach to the agent's tmux session

Usage:
  warden attach <TICKET> [flags]
```

## warden send

```text
Type a message into an agent's claude session and press Enter

Usage:
  warden send <TICKET> <message...> [flags]
```

## warden tail

```text
Print the recent output of an agent's claude session

Usage:
  warden tail <TICKET> [flags]

Flags:
  -h, --help        help for tail
      --lines int   number of pane lines to capture (default 200)
```

## warden digest

```text
Summarize what an agent accomplished (files, branch, turns, narrative)

Usage:
  warden digest <TICKET> [flags]

Flags:
  -h, --help   help for digest
      --json   output as JSON
```

## warden rotate

`warden rotate` is a thin alias for `warden handoff --retire` — the unified handoff verb's self-succession mode. Both run the identical code path.

```text
Run inside an agent session. Phase 1 is driven by the /warden skill (the agent writes a handoff file + resume prompt and shows you). On your go-ahead, run with --confirm to spawn the successor and reap this agent.

This is a thin alias for `warden handoff --retire` — the unified handoff verb's self-succession mode. Both run the identical code path.

Usage:
  warden rotate [flags]

Flags:
      --confirm                actually spawn the successor and retire this agent (required for retire)
  -h, --help                   help for rotate
      --resume-file string     path to the handoff notes file the successor reads (use a unique per-agent path, e.g. $TMPDIR/warden-rotate-handoff-$WARDEN_SESSION_ID.md, so concurrent rotations don't clobber each other)
      --resume-prompt string   the successor's initial task prompt
```

## warden approvals

```text
List pending tool-permission prompts waiting for an answer

Usage:
  warden approvals [flags]
```

## warden approve

```text
Answer a pending tool-permission prompt by option number

Usage:
  warden approve <TICKET> <option> [flags]
```

## warden doctor

```text
Run preflight checks (required binaries, daemon, data dir)

Usage:
  warden doctor [flags]
```

## warden setup

```text
Install missing dependencies (tmux, git, claude; optional gh, ollama)

Verify the current install with the same checks as `warden doctor`, then
install whatever is missing. setup is idempotent: it only touches deps that
are not already on PATH.

For each missing dependency it prints the exact install command and prompts
before running it (use --yes for non-interactive/automation). Required deps
(tmux, git, claude) are offered first, then optional ones (gh, ollama).

Package managers: Homebrew on macOS (never auto-bootstrapped — if brew is
missing setup prints the instruction and skips brew installs), and apt, dnf,
or pacman on Linux (auto-detected). Claude Code and Ollama use their official
installers. After installing, setup re-runs the checks and prints the report.

setup is CLI-only by design (it installs host packages) and is not exposed
over MCP or the daemon.

Usage:
  warden setup [flags]

Flags:
  -h, --help   help for setup
      --yes    install all missing dependencies without prompting (non-interactive)
```

## warden llm suggest

```text
Suggest local LLM models for warden's REPL (wd repl), ranked against
this machine's memory.

warden auto-detects two figures: total memory (GPU VRAM, Apple unified memory, or
system RAM — whichever bounds a usable model) and average free memory (sampled a
few times to smooth out spikes). Each candidate is then marked:

  fits now           runnable right now within free memory
  free memory first  fits the machine, but you'd need to close apps first
  too large          won't fit this machine

Models are scored by suitability for the conductor role — reliable tool/function
calling, not coding or raw size. The recommendation (★) is the best-scoring model
that runs comfortably now while leaving headroom for your real workload (Docker,
DBs, IDE, Claude sessions, the warden daemon). warden only ever recommends — you
set `local_llm.model` yourself.

Usage:
  warden llm suggest [flags]

Flags:
      --free-gb float    override detected free memory (GB)
  -h, --help             help for suggest
      --json             output as JSON
      --samples int      free-memory samples to average (default 5)
      --total-gb float   override detected total memory (GB)
```

## warden stats

```text
Show warden's resource footprint (per-agent memory/CPU, system pressure, daemon stats)

Usage:
  warden stats [flags]

Flags:
  -h, --help    help for stats
      --json    output as JSON
      --watch   redraw every 3s until interrupted
```

## warden cost

```text
One umbrella over warden's two financial views:
  • SPEND   — the REAL dollars agents billed Claude, measured from their transcripts (`wd cost spend`)
  • SAVINGS — the tokens, and the dollars they represent, warden kept OUT of context (`wd cost savings`)

`wd cost` with no subcommand prints a combined at-a-glance summary of both axes.
`wd cost spend` and `wd cost savings` are the same commands as the top-level
`wd spend` and `wd savings`, which remain available and unchanged. Both views are
gated by the `savings` config setting. Resource footprint — memory/CPU/pressure —
is a different axis; see `wd stats`.

Usage:
  warden cost [flags]
  warden cost [command]

Available Commands:
  savings     Show the token reductions warden's lifecycle features have earned
  spend       Show measured Claude spend in dollars, per agent / repo / day
```

Purely additive: it reuses the existing spend rollup and savings ledger and their render logic, so the top-level `warden spend` / `warden savings` keep working unchanged. `warden cost spend` / `warden cost savings` accept every flag the standalone commands do (`--by`, `--json`, `--benchmark`, `--since`, `--audit`, `--calibrate`).

## warden savings

```text
Show the token reductions warden's lifecycle features have earned — a real,
append-only ledger of output kept out of agents' context windows. Gated by the
`savings` config setting (default on).

Usage:
  warden savings [flags]

Flags:
      --benchmark        headline A/B proof (without-vs-with warden, % cut, $ saved, trend sparkline)
      --since string     only count savings since this window (24h, 7d, 2w) or a date
      --json             output the structured summary as JSON
      --audit            print retained raw-vs-kept provenance samples (requires savings_samples)
      --calibrate        measure this workload's bytes/token vs Claude count_tokens (needs ANTHROPIC_API_KEY)
      --calibrate-max int  cap the number of paid count_tokens calls a --calibrate run makes
```

Two axes are reported separately and never blended: the **context** axis (how much leaner context stayed, % and $) and the **offload** axis (Claude work moved off entirely onto the local LLM, $). Each figure states its basis — `CALIBRATED` or the 4-bytes/token `HEURISTIC`.

## warden spend

```text
Show measured Claude spend in dollars, per agent / repo / day — the REAL billed
input/output tokens warden read from agents' transcripts, priced per model. The
cost side of the savings ledger. Gated by the `savings` config setting.

Usage:
  warden spend [flags]

Flags:
      --by string    show only one rollup: agent, repo, or day (default: all three)
      --json         output the structured rollup as JSON
```

The headline names the **daily** and **weekly** totals the budget gate enforces
(`tokens.budget_gate` / `tokens.budget_daily_usd` / `tokens.budget_weekly_usd`): with the gate on, a
spawn that would push measured spend over a cap warns first (re-run with `--force`),
mirroring the memory-pressure spawn gate. `warden ls` also gains a **COST** column.

## warden branches

```text
Per-agent CI status and standing vs origin/main (opt-in monitor). Reports each
active agent's latest GitHub CI result (gh run list in its worktree) and whether
its branch is ahead/behind/merged relative to origin/main.

Usage:
  warden branches [flags]

Flags:
      --json   output as JSON
```

Enable the background monitor with `branch_track_enabled` (tune `branch_track_interval`). Alerts are **non-blocking**: an inbox note (+ desktop ping) on a new CI failure, an inbox nudge on a merged or far-behind branch. Also at `GET /api/v1/collab/branches` and the `get_branch_status` MCP tool.

## warden insights

```text
Mine agent history for recurring patterns, slow/failure-prone work, and
parallelization opportunities — a deterministic report, optionally narrated by the
local LLM (`local_llm.enabled`). Gated by `insights` (default on).

Usage:
  warden insights [flags]

Flags:
      --json   output as JSON
```

## warden snapshot

```text
Checkpoint an agent's worktree changes + session transcript and roll back later.
Gated by `snapshots` (default on).

Usage:
  warden snapshot [command]

Available Commands:
  create    Capture a checkpoint (worktree stash + transcript)  [name] [-m msg]
  list      List checkpoints  [name] [--all]
  restore   Re-apply a checkpoint onto its recorded worktree  <id> [--force]
```

Restore refuses a dirty/conflicting tree rather than clobbering, and a failed apply leaves the snapshot intact. Also the `snapshot_create`/`snapshot_list`/`snapshot_restore` MCP tools.

## warden schedule

```text
Fire an agent or a pipeline on the daemon's own cron/at timer — no external crontab.
Opt-in: set `scheduler_enabled: true` and keep the daemon running (schedules only
fire while it is up). Missed runs are not backfilled.

Usage:
  warden schedule [command]

Available Commands:
  create    Create a schedule  <name> (--cron "0 9 * * *" | --at 2026-06-27T09:00) (--prompt … | --pipeline spec.yaml)
  list      List schedules with kind, mode, spec, enabled, next run, last error
  delete    Remove a schedule  <id>
```

`list_schedules` exposes the same read-only view over MCP.

## warden plugin

```text
Inspect the plugin registry — external executables that extend warden with custom
agent task types and lifecycle hooks over a versioned JSON-over-stdio protocol.
Default off (set `plugins: true`, since plugins run external code).

Usage:
  warden plugin [command]

Available Commands:
  list   Show registered plugins: paths, custom task types, subscribed hook events, config errors
```

## warden tutorial

```text
Run the first-run guided walkthrough of the core loop (spawn → watch → commit →
tear down). Until taken or skipped, warden prints a one-line stderr nudge.

Usage:
  warden tutorial [flags]

Flags:
      --skip    mark the tutorial complete without running it
      --reset   clear the completion marker so the tour and hint return
```

## warden pipeline

```text
Define and run DAG pipelines of agent jobs

Usage:
  warden pipeline [command]

Available Commands:
  cancel         Cancel a pipeline (terminates running jobs)
  create         Create a pipeline from a YAML spec or a built-in template
  delete         Delete a pipeline's record (must not have live jobs — cancel first)
  edit-job       Edit a pending job's prompt and/or handoff
  emit           Publish this job's handoff (run from inside a pipeline job)
  list           List pipelines
  list-templates List the built-in pipeline templates and their placeholders
  pause          Pause a running pipeline (in-flight jobs finish; no new jobs spawn)
  resume         Resume a paused pipeline (spawns jobs that became ready while paused)
  retry          Re-run a failed or needs-attention job (reopens skipped descendants)
  show           Show a pipeline's jobs and their status
  start          Start a pipeline (spawns jobs with no dependencies)
  validate       Validate a pipeline YAML spec without creating it
```

## warden ctx

```text
Read and write the shared context (a namespaced key/value store agents share)

Usage:
  warden ctx [command]

Available Commands:
  del         Delete a context key
  get         Print the value at a context key
  list        List context keys (optionally filtered by prefix)
  set         Set a context key (value inline, or --file / --stdin)
```

## warden msg

```text
Send and receive directed messages between agents

Usage:
  warden msg [command]

Available Commands:
  inbox       Show this agent's messages (marks them read)
  send        Send a message to an agent (wakes it if it's idle/waiting)
  wait        Block until a message arrives (or timeout), then print it

Flags:
      --as string   act as this agent id (defaults to $WARDEN_SESSION_ID)
```

## Git & check lifecycle

First-class verbs that wrap the git/check round-trips an agent would otherwise run by hand, on **warden rails**: protected branches (`main`/`master`) are refused, hooks run, and each action is linked to the agent. All four take `--json`.

```text
warden commit [-m "<msg>"]   Stage + commit the whole worktree on its branch. Omit -m and warden
                             writes the message (local model from the diff, else a conventional
                             message from the changed paths).
warden push                  Push the current branch to origin (sets upstream). Refuses main/master.
warden sync [--base main]    Fetch + rebase the current branch onto origin/<base>. Refuses a dirty
                             tree; on conflict, leaves the rebase in progress and reports only the
                             conflicting files.
warden check [name]          Run the checks declared in .warden/check.yml and return a pass/fail
                             summary with output for FAILING checks only. No name = all; <name>
                             runs one (test/lint/build/…). Exits non-zero on failure.
```

These are also MCP tools (`commit`, `push`, `sync`, `check`). See the [Lifecycle & rails](/warden/guides/lifecycle-and-rails/) guide for the boundary-enforcement hooks that steer agents onto them.

## warden collab

```text
Inter-agent collaboration: see which agents are editing the same files

Usage:
  warden collab [command]

Available Commands:
  conflicts        List files currently being edited by more than one agent
  who-is-editing   Show which agents are editing a specific file
```

## warden search

```text
Search across active agents' searchable text (name, id, ticket, type, subject, prompt, branch,
last pane excerpt). Multiple terms are AND-ed. Pass --closed to also search archived agents.

Usage:
  warden search <QUERY...> [flags]

Flags:
      --closed   also search archived (closed) agents
      --json     output as JSON
```

## warden history

```text
Browse archived (closed) agents, newest first.

Usage:
  warden history [flags]

Flags:
      --json           output as JSON
      --limit int      cap the number of results (0 = no cap)
      --since string   only agents updated since this window (24h, 7d, 2w) or date
      --type string    filter by task type (development, pr-review, analysis, …)
```

## warden export / import

```text
warden export [--all] > backup.json   Dump active (and with --all, archived) records as a JSON
                                      envelope on stdout. Metadata only — worktrees, branches, and
                                      tmux sessions are NOT serialized.
warden import [--merge] < backup.json Insert records from a `warden export` envelope. Idempotent by
                                      id (existing ids skipped); --merge overwrites on collision.
```

## warden audit

```text
Read the daemon's append-only audit trail (~/.warden/audit.jsonl) — who did what, when, to which
object. Read directly from disk, so it works even while the daemon is down.

Usage:
  warden audit log [flags]   Show recent audited actions, newest last
```

## warden preset

```text
Save and list named spawn configs (replay with `warden start --preset <name>`).

Usage:
  warden preset [command]

Available Commands:
  list   List saved presets and their defaults
  save   Save the given spawn flags as a named preset
```

## warden prompt-template

```text
Save and list reusable, variabled prompt templates (fill with `warden start
--prompt-template <name> --set VAR=value`).

Usage:
  warden prompt-template [command]

Aliases:
  prompt-template, prompt-templates, pt

Available Commands:
  list   List saved prompt templates and their variables
  save   Save a named prompt template with {{VAR}} placeholders
```

## warden library

```text
Browse saved spawn presets, prompt templates, and pipeline templates in one place.
One umbrella over warden's reusable launch configs: spawn PRESETS (named `warden
start` defaults), prompt TEMPLATES (variabled prompt bodies), and the built-in
pipeline TEMPLATES (read-only). Purely additive — the `preset`, `prompt-template`,
and `pipeline list-templates` commands keep working unchanged.

Usage:
  warden library [command]

Aliases:
  library, lib

Available Commands:
  list          List saved spawn presets, prompt templates, and built-in pipeline templates
  save-preset   Save the given spawn flags as a named preset (same as `warden preset save`)
  save-prompt   Save a variabled prompt template (same as `warden prompt-template save`)
```

## warden token

```text
Manage the daemon's remote-access bearer token (for non-loopback access).

Usage:
  warden token [command]

Available Commands:
  generate   Generate a random bearer token for remote access
  rotate     Generate a new token, persist it, and restart the daemon
  show       Print the current token (for pasting into a remote client)
```

See the [Remote access](/warden/guides/remote-access/) guide.

## warden handoff

```text
Hand a structured context package off to another agent. Phase 1 (writing the handoff file + resume
prompt) is driven by the /warden skill; this verb performs the delivery. Three modes:

  • default — spawn a fresh delegate in its own isolated worktree for a sub-task; the source agent keeps running.
  • --to <id> — deliver the handoff into an already-running agent's inbox (waking it); the source agent keeps running.
  • --retire — spawn a successor in THIS agent's SAME worktree, then reap the calling agent (self-succession). Requires --confirm. This is what the `rotate` alias runs.

--retire and --to are mutually exclusive: retire reaps the caller, --to never does.

Usage:
  warden handoff [flags]

Flags:
      --retire                 self-succession: spawn a successor in THIS agent's worktree and reap the calling agent (mutually exclusive with --to; requires --confirm). Equivalent to 'warden rotate'
      --confirm                with --retire, actually spawn the successor and retire this agent (required)
      --to string              deliver to this existing agent id instead of spawning a new one
      --resume-file string     path to the handoff notes file whose content is delivered to the recipient (with --retire, the path the successor reads in place)
      --resume-prompt string   the recipient's task prompt
      --as string              act as this agent id for provenance (defaults to $WARDEN_SESSION_ID, else 'human')
      --type string            task type for a new delegate (default "development")
      --branch / --name / --repo / --force   options for a new delegate (ignored with --to)
```

## warden repl

```text
Interactive conductor for agents, pipelines, and the git/check lifecycle (local LLM + `/` commands).
Aliases: interactive, i.

A real line editor (arrow keys, persisted history, reverse-search, a live `/`
menu that filters as you type, Tab completion) that closes with Ctrl-D. Drive it
with deterministic `/` commands (no model:
/agents, /spawn <prompt>, /tell <id> <text>, … — type /help) or with natural
language (planned by the local LLM, each call confirmed).

Guided argument forms: when a `/` command needs more than you typed, warden
collects the arguments interactively — a numbered pick-list for fields with a
known set (model, permission_mode, type, yes/no), free text for the rest. A
command auto-opens the form when a required argument is missing (bare /spawn);
add a trailing + to fill every field (/spawn+ <prompt>). With a local model
present each field opens with a suggested value you can accept with Enter, type
over, or clear with "-".

`!cmd` runs a command in your own $SHELL. Starts without `local_llm.enabled` — only the
natural-language half needs it.

Usage:
  warden repl [flags]
```

See [Interactive mode](/warden/multi-agent/repl/) for the full `/`-command table.

## warden auto-approve / force-compact / set-permission-mode

```text
warden auto-approve <agent-id> <on|off>      Per-agent toggle: opt one agent into auto-approval
                                             even when the global policy is off.

# Rule policy (allow/deny engine; effective immediately, persisted to config):
warden auto-approve rules                     Show the live policy (default rules + per-agent overrides).
warden auto-approve enable|disable [--agent]  Flip the master switch (or a per-agent override's).
warden auto-approve allow [flags]             Append an allow rule.
warden auto-approve deny  [flags]             Append a deny rule.
warden auto-approve clear [--agent <name>]    Clear the default rules (or a per-agent override).
  Flags for allow/deny:
    --tool <name>      exact tool name to match (e.g. Read, Bash)
    --pattern <glob>   case-insensitive glob/substring over the action + question
    --regex <re>       Go regular expression over the action + question
    --paths <a,b>      path globs against the action target (comma-separated)
    --agent <name|id>  scope the rule to a per-agent override

warden force-compact <agent-id> <on|off|inherit>
                                             Per-agent override for force-compact: when on, warden
                                             interrupts the agent if it goes context-critical while
                                             still working, /compacts once it idles, then resumes it
                                             (destructive: discards the in-flight turn). inherit clears
                                             the override (global default: token_force_compact config).
warden set-permission-mode <agent-id> <mode> Set an agent's permission mode
                                             (acceptEdits|auto|bypassPermissions|default|dontAsk|plan).
```

With no rules configured, an enabled policy keeps the simple legacy behavior
(approve every recognized, non-destructive prompt). warden's built-in
destructive deny-list always wins, so no rule can auto-approve a destructive
action.

## warden worktree

The umbrella for warden's worktree operations — **list** and **prune**. Bare
`warden worktree` prints the list (same as `warden worktree list`).

```text
warden worktree                   List warden worktrees (same as `worktree list`)
warden worktree list              List warden worktrees under .worktrees, joined to active/archived records (alias: ls)
warden worktree prune [--force]   Reclaim orphaned warden worktrees under .worktrees (always asks)
```

`warden prune` remains available, unchanged, as a top-level alias for
`warden worktree prune` (same flags, prompts, and output):

```text
warden prune [--force]            Alias for `warden worktree prune`
```

Pruning whole orphaned worktrees is distinct from `warden remove-worktree`,
which tears down one specific agent's worktree.

## warden config

```text
Print the live, resolved configuration values warden is using, grouped by area, with the config
file path at the top. Edit the file by hand to change settings.

Usage:
  warden config [flags]
  warden config [command]

Available Commands:
  init   Create the config file (or migrate it, adding any missing keys)
  path   Print the resolved config file path
```

## Other commands

A handful of lifecycle commands share the simple `<TICKET>` shape:

```text
warden adopt [--session-id <uuid>] [--dir <path>]   Register an existing Claude session
warden stop <TICKET> [flags]                         Umbrella teardown (default: terminate + clear record + remove worktree)
warden terminate <TICKET>                           Stop an agent (keeps record + worktree)
warden restore <TICKET>                             Recreate/resume a lost/orphaned agent
warden done <TICKET> [--hard]                        Terminate + clear record (worktree kept)
warden delete <TICKET> [--hard]                      Clear the stored record only
warden remove-worktree <TICKET> [--force]            Remove the git worktree + branch (destructive)
warden daemon [--addr ADDR]                          Run the hub (HTTP API + poller)
warden mcp [--addr ADDR]                             Run the MCP stdio server
```

### `warden stop` — the umbrella teardown verb

`warden stop <TICKET>` is the single umbrella for all four teardown verbs.
By default it does a **full teardown**: terminate the session, clear (archive)
the record, **and** remove the git worktree + branch (asking for confirmation
first, unless `--yes`). Subtractive flags keep parts; `--pr` opens a GitHub PR
first while the agent is intact. Safe order: PR → terminate → clear record →
remove worktree.

```text
--keep-record            do not clear the stored record
--keep-worktree          do not remove the worktree (this + default == `done`)
--hard                   purge the record instead of archiving
--pr [--base <branch>]   open a GitHub PR first (pushes the branch)
--yes                    skip the worktree-removal confirmation prompt
--force                  override the alive/uncommitted/unpushed worktree guards
--delete-adopted-branch  also delete a branch warden did not create
```

The four older verbs are kept as thin aliases (behavior unchanged):

| old verb | equivalent |
|---|---|
| `wd terminate <T>` | `wd stop <T> --keep-record --keep-worktree` |
| `wd delete <T> [--hard]` | `wd stop <T> --keep-worktree` (record only) |
| `wd remove-worktree <T>` | `wd stop <T> --keep-record` (worktree only) |
| `wd done <T> [--hard\|--create-pr]` | `wd stop <T> --keep-worktree [--hard\|--pr]` |
| `wd stop <T>` | terminate + clear record + remove worktree |
