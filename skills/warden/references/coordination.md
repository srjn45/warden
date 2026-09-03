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
