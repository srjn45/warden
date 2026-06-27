# warden — coordinating agents (context, messages, conflicts, branches, approvals)

For independent agents that occasionally need to share data, talk, avoid stepping
on each other, or be unblocked. Prefer these warden primitives over hand-rolled
files or out-of-band coordination. (For a *dependency chain*, use a pipeline
instead — see pipelines.md.)

## Shared context — a namespaced KV blackboard

MCP `ctx_set` / `ctx_get` / `ctx_list` (+ `ctx_append`, `ctx_cas`); CLI `warden ctx
set|get|list|del`.

```sh
warden ctx set <key> <value>     # or: --file <path> / --stdin
warden ctx get <key>
warden ctx list [<prefix>]
warden ctx del <key>
```

- Keys are dot-namespaced (`global.*`, `agent.<id>.*`, `pipeline.<id>.*`). Writes
  are attributed to `$WARDEN_SESSION_ID` (set per agent) or `--as <id>`.
- `ctx_append` appends to a key (build a running log without read-modify-write).
- `ctx_cas` is compare-and-set — for a contended key, set only if the current value
  matches the expected one (a lightweight lock / coordination primitive).
- Pipeline outputs land here automatically at `pipeline.<id>.<job>.output`.

## Directed messages — a durable per-agent inbox

MCP `send_message` / `read_inbox` / `wait_for_message`; CLI `warden msg
send|inbox|wait`.

```sh
warden msg send <agent-id> "<message>"            # delivers; wakes it only if idle/waiting
warden msg inbox [--unread]                       # read my messages (marks read)
warden msg wait [--from <id>] [--timeout <sec>]   # block until a message, then print it
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
- CLI `warden collab conflicts` / `warden collab who-is-editing <file>`.
- Also check `read_inbox` for file-conflict warnings the daemon delivers.

Tunable via `collab_enabled` / `collab_interval` / `collab_hint`. Spawned agents
get a system-prompt hint to do exactly this before editing shared files.

## Branch & CI tracking (read-only, informational)

Opt-in daemon monitor (`branch_track_enabled`, off by default;
`branch_track_interval` default `2m`). Per active agent with a branch it reports
its **GitHub CI status** (`gh run list` in the worktree) and its **standing vs
`origin/main`** (commits behind/ahead, whether merged).

- MCP `get_branch_status`; CLI `warden branches` (`--json`).
- Alerts are **informational, never blocking**: a new CI failure → an inbox note to
  the agent + a desktop notification to the operator; a merged or >10-commits-behind
  branch → an inbox nudge. A 5-min dedup window suppresses repeats. Every subprocess
  **fails open** (missing/unauthenticated `gh`, timeout, non-repo worktree → skip).

## Approvals inbox — answer prompts without attaching

Answer routine Claude tool-permission prompts (from supervised agents) without
attaching. Config-gated by `approvals` (on by default).

- MCP `list_approvals` → pending prompts with numbered options; `approve {id, n}`
  answers one.
- CLI `warden approvals`; `warden approve <id> <n>`.
- A TOCTOU re-capture + fingerprint re-verify guards each answer; unrecognized
  prompts fall back to attach.

**Auto-approve** (off by default): auto-answers recognized yes/no prompts. Two layers:

- **Per-agent toggle** — opt one agent in even when the global policy is off:
  MCP `set_auto_approve {ticket, enabled}` / `warden auto-approve <id> on|off`.
- **Rule policy** — an allow/deny engine: a prompt is answered only when it matches
  an allow rule, matches no deny rule, and isn't on warden's built-in destructive
  deny-list (always wins). Rules match by `tool`, a case-insensitive glob
  (`pattern`), a Go `regex`, and/or `paths`; per-agent overrides (keyed by agent
  name/id) replace the default for that agent. Manage with MCP
  `set_auto_approve_policy {action: show|allow|deny|clear|enable|disable, agent?,
  tool?, pattern?, regex?, paths?}` / `warden auto-approve rules|allow|deny|clear|enable|disable`.
  Changes are live (no restart) and persisted to config.

With **no rules** an enabled policy is the simple legacy toggle (approve every
recognized, non-destructive prompt — `auto_approve: true` still works). Skips
multi-select/text-entry/unrecognized (falls back to manual); never retries on
failure; logs every attempt.
