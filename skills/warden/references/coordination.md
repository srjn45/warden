# warden — coordinating agents (context, messages, conflicts, branches, approvals)

For independent agents that occasionally need to share data, talk, avoid stepping
on each other, or be unblocked. Prefer these warden primitives over hand-rolled
files or out-of-band coordination. (For a *dependency chain*, use a pipeline
instead — see pipelines.md.)

## Shared context — a namespaced KV blackboard

MCP `ctx_set` / `ctx_get` / `ctx_list` (+ `ctx_append`, `ctx_cas`); CLI `warden context
set|get|list|del`.

```sh
warden context set <key> <value>     # or: --file <path> / --stdin
warden context get <key>
warden context list [<prefix>]
warden context delete <key>
```

- Keys are dot-namespaced (`global.*`, `agent.<id>.*`, `pipeline.<id>.*`). Writes
  are attributed to `$WARDEN_SESSION_ID` (set per agent) or `--as <id>`.
- `ctx_append` appends to a key (build a running log without read-modify-write).
- `ctx_cas` is compare-and-set — for a contended key, set only if the current value
  matches the expected one (a lightweight lock / coordination primitive).
- Pipeline outputs land here automatically at `pipeline.<id>.<job>.output`.

## Directed messages — a durable per-agent inbox

MCP `send_message` / `read_inbox` / `wait_for_message`; CLI `warden message
send|inbox|wait`.

```sh
warden message send <agent-id> "<message>"            # delivers; wakes it only if idle/waiting
warden message inbox [--unread]                       # read my messages (marks read)
warden message wait [--from <id>] [--timeout <sec>]   # block until a message, then print it
```

A *working* agent is never interrupted (woken only when idle/waiting). `msg wait` /
`wait_for_message` blocks in the daemon, so an agent awaits a reply in a single call
with no busy-loop. Identity defaults to `$WARDEN_SESSION_ID`; override with `--as
<id>`.

## File-conflict detection (don't overwrite a peer)

The daemon watches each active agent's worktree (real-time fsnotify + a `git diff`
poll safety net) and warns — via the inbox, deduplicated — when two agents are
editing the same file. **Before editing a file a peer might also be changing,
check first and coordinate via `send_message` rather than overwriting.**

- MCP `who_is_editing_file {file}` — which agents share that repo-relative file.
- MCP `get_collaboration_status` — the current conflict picture.
- CLI `warden workspace conflicts` / `warden workspace who-is-editing <file>`.
- Also check `read_inbox` for file-conflict warnings the daemon delivers.

Tunable via `collab.enabled` / `collab.interval` / `collab.git_reconcile_interval` / `collab.hint`. Spawned agents
get a system-prompt hint to do exactly this before editing shared files.

## Branch & CI tracking (read-only, informational)

Opt-in daemon monitor (`branch_track.enabled`, off by default;
`branch_track.interval` default `2m`). Per active agent with a branch it reports
its **GitHub CI status** (`gh run list` in the worktree) and its **standing vs
`origin/main`** (commits behind/ahead, whether merged).

- MCP `get_branch_status`; CLI `warden workspace branches` (`--json`).
- Alerts are **informational, never blocking**: a new CI failure → an inbox note to
  the agent + a desktop notification to the operator; a merged or >10-commits-behind
  branch → an inbox nudge. A 5-min dedup window suppresses repeats. Every subprocess
  **fails open** (missing/unauthenticated `gh`, timeout, non-repo worktree → skip).

## Approvals inbox — answer prompts without attaching

Answer routine agent tool-permission prompts (from supervised agents) without
attaching. Config-gated by `approvals` (on by default).

- MCP `list_approvals` → pending prompts with numbered options; `approve {id, n}`
  answers one.
- CLI `warden approval list`; `warden approval answer <id> <n>`.
- A TOCTOU re-capture + fingerprint re-verify guards each answer; unrecognized
  prompts fall back to attach.

**Workspace-trust prompts are not your job.** The "do you trust this folder?" prompt
Claude/Codex/Antigravity raise in a fresh worktree is answered by the daemon
(`trust_workspace: true`, default; Cursor is launched with `--trust`), regardless of
auto-approve. Do not attach to answer it; if an agent still sits on it, the operator
set `trust_workspace: false` — answer it from the approvals inbox.

**Unknown prompts are recognized for you.** When an AI CLI rewords a prompt and no
parser matches it, the daemon has Fast-Brain read the stalled menu
(`recognize_prompts: true`, default), verifies the reading against the pane and then
treats it like any parsed prompt: `waiting_for_input`, listed in `warden approval
list`, subject to the same auto-approve policy. A `prompt_recognized` event marks it.
If an agent sits on a menu and is still not `waiting_for_input` after ~20s, attach.
Learned shapes are inspectable: `warden approval known list` (MCP `list_known_prompts`); drop a bad one with `warden approval known forget <id>` (MCP `forget_known_prompt`).

**Auto-approve** (off by default): auto-answers recognized yes/no prompts. Two layers:

- **Per-agent toggle** — opt one agent in even when the global policy is off:
  MCP `set_auto_approve {ticket, enabled}` / `warden approval auto set <id> on|off`.
- **Rule policy** — an allow/deny engine: a prompt is answered only when it matches
  an allow rule, matches no deny rule, and isn't on warden's built-in destructive
  deny-list (always wins). Rules match by `tool`, a case-insensitive glob
  (`pattern`), a Go `regex`, and/or `paths`; per-agent overrides (keyed by agent
  name/id) replace the default for that agent. Manage with MCP
  `set_auto_approve_policy {action: show|allow|deny|clear|enable|disable, agent?,
  tool?, pattern?, regex?, paths?}` / `warden approval auto rules|allow|deny|clear|enable|disable`.
  Changes are live (no restart) and persisted to config.

With **no rules** an enabled policy is the simple legacy toggle (approve every
recognized, non-destructive prompt — `auto_approve: true` still works). Skips
multi-select/text-entry/unrecognized (falls back to manual); never retries on
failure; logs every attempt.

**Fast-Brain arbiter** (opt-in, `auto_approve.use_fast_brain: true`, default
false): a third layer for prompts the static rules can't answer. Tool
permissions go to a ≤1.5s fast-tier model call, strategic questions to a ≤10s
thinking-tier call; applied only at confidence ≥ 0.8, otherwise it fails open to
a human/brain. The destructive deny-list and circuit breaker run first.

**Circuit breaker** (always on): when the *identical* prompt keeps re-appearing
after being approved (the agent re-runs a failing command and re-asks), warden
stops auto-approving it after `max_repeats` consecutive identical approvals
(default 10), records an `approval_loop` anomaly event, notifies the operator,
and leaves the prompt for a human — the agent then shows as `waiting_for_input`.
Configure via `auto_approve.max_repeats` (0 = default, negative = off; per-agent
overrides inherit the default when unset). A different prompt, or ~10 quiet
minutes, resets the run. If you see the "auto-approve halted" anomaly on an
agent, read its output — the underlying command is failing (e.g. expired
credentials) and needs a human fix, not another approval.

## Crash triage drafts — local-only until the user approves

When an agent crashes, Fast-Brain triage may stage a sanitized GitHub-issue
draft at `~/.warden/crashes/<id>.json` (secrets redacted, home paths
normalized). Agents must treat these drafts as **local-only**: never submit
one to GitHub, run `gh issue create`, or open an issue URL on the user's
behalf — submission needs the user's explicit approval. When a
`bug_draft_staged` event appears, tell the user to run `warden bug-report <id>`
(or press `B` in the cockpit); never answer its `y/N` prompt yourself.
