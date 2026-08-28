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

## Collaboration groups — make per-project orchestrators discoverable

MCP `collaborate_group {group, action}`; CLI `warden collaborate group <name> join|leave`.

A collaboration group is a **roster + introduction broker**, not a new channel: it
lets one developer's **per-project orchestrator agents** discover each other and
then message/delegate over the *existing* directed-message bus (`send_message` /
`wait_for_message`). It adds no wire format — the group just says who is present.

```sh
warden collaborate group my-team join     # seat this agent as its project's orchestrator
warden collaborate group my-team leave    # remove this agent's seat (soft)
```

- **join** creates the group if absent, then: enforces **one orchestrator per
  project** (project = the canonical git-remote key, with a `local:` fallback for
  remoteless repos; a duplicate join returns **409** naming the already-seated
  agent, so you message the incumbent instead of erroring); switches the caller to
  the **orchestrator** role; resolves a one-line **project summary** (declared blurb
  in `CLAUDE.md`/`README.md` → else asked once → cached); and **brokers
  introductions both directions** — warden sends each existing member a templated
  descriptor of the joiner and sends the joiner the reciprocal roster. Agents spend
  **zero tokens** on intros; they just receive a compact roster and can address
  peers by id.
- **leave** is **soft/social**: the agent stops being discoverable and accepts no
  *new* inbound delegations, but in-flight replies still route by agent-id (leaving
  never breaks a conversation). Warden notifies peers.
- **terminate** (not leave) is the hard path that orphans work: terminating an
  orchestrator that holds a group seat requires **explicit confirmation** (naming
  the group(s) and any outstanding received delegations) and notifies requesters of
  abandoned work. Prefer `leave` then a graceful teardown.
- **Recovery:** on `recover_agents`, a recovered orchestrator **auto-rejoins** its
  groups and re-announces (the group is durable; membership is live).

**Cross-project delegation (v1)** is just a directed message to a known peer — no
protocol. FE orchestrator → `send_message` to BE orchestrator ("I need `POST /x`");
BE reads its inbox, spawns **its own** worker on **its own** worktree, opens a PR in
the **BE** repo, and replies with the link. Worktree isolation holds — nobody edits
a repo they don't own — and every cross-project change is an auditable PR in the
target repo.

Groups and the TUI **Open Project** panel are distinct but share the
one-orchestrator-per-project invariant: opening a project auto-spawns its single
orchestrator; `join` opts that orchestrator into cross-project *membership*.

## File-conflict detection (don't overwrite a peer)

The daemon watches each active agent's worktree (real-time fsnotify + a `git diff`
poll safety net) and warns — via the inbox, deduplicated — when two agents are
editing the same file. **Before editing a file a peer might also be changing,
check first and coordinate via `send_message` rather than overwriting.**

- MCP `who_is_editing_file {file}` — which agents share that repo-relative file.
- MCP `get_collaboration_status` — the current conflict picture.
- CLI `warden collab conflicts` / `warden collab who-is-editing <file>`.
- Also check `read_inbox` for file-conflict warnings the daemon delivers.

Tunable via `collab.enabled` / `collab.interval` / `collab.hint`. Spawned agents
get a system-prompt hint to do exactly this before editing shared files.

## Branch & CI tracking (read-only, informational)

Opt-in daemon monitor (`branch_track.enabled`, off by default;
`branch_track.interval` default `2m`). Per active agent with a branch it reports
its **GitHub CI status** (`gh run list` in the worktree) and its **standing vs
`origin/main`** (commits behind/ahead, whether merged).

- MCP `get_branch_status`; CLI `warden branches` (`--json`).
- Alerts are **informational, never blocking**: a new CI failure → an inbox note to
  the agent + a desktop notification to the operator; a merged or >10-commits-behind
  branch → an inbox nudge. A 5-min dedup window suppresses repeats. Every subprocess
  **fails open** (missing/unauthenticated `gh`, timeout, non-repo worktree → skip).

## Approvals inbox — answer prompts without attaching

Answer routine agent tool-permission prompts (from supervised agents) without
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
