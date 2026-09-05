# warden — plain agents (lifecycle, rotate, handoff)

Plain agents are the default: independent one-off tasks. Manage them through the
MCP tools (`list_agents`, `get_agent`, `spawn_agent`, `send_to_agent`,
`get_agent_output`, `stop_agent`, `terminate_agent`, `delete_agent`,
`remove_worktree`, `restore_agent`, `adopt_agent`) or the CLI. The per-agent tools take a `ticket` —
the agent's **id** from `list_agents` (prompt-spawned ids look like
`agent-<shortid>`).

## Intent → action (MCP)

| The user wants to… | Do this |
|---|---|
| list / check / triage agents | `list_agents`; summarize by status. Call out `waiting_for_input` (needs them), `errored`/`orphaned`, and any agent with a non-null `backend_recovery` field (automatically switching backends after a provider hard limit — see **Backend recovery** below). Show each agent's `subject` and `workdir`. For a nested fleet view, prefer `GET /api/v1/tree` over joining sessions/pipelines/autopilot client-side. |
| spin up an agent to do X | `spawn_agent {prompt: "X"}` (auto-typed, no repo needed). Only add `type`+`repo` (+`branch`/`pr`/`worktree`) for a managed worktree tied to a repo/ticket. Add `model`, `permission_mode`/`supervised`, `tags`, `role` as needed. |
| give an agent a role / persona | `spawn_agent {..., role: "worker"}` at spawn, or `set_role {ticket, role}` on a running agent (relaunches to re-inject; `general`/empty clears it). `list_roles` returns the catalog. Over MCP, routing follows the role's default tier (no `task`/`tier` params). See **Roles** below. |
| fork agent <id>'s session into a new one | `fork_agent {source: "<id>", prompt?}` — branches the source's recorded conversation into a NEW managed agent (fresh sibling worktree, dirty-tree carry; the source keeps running). **Codex-only** (a non-forking backend like Claude returns a clean "cannot fork"); the source's session id must already be pinned (let it run a turn first). See **Fork** below. |
| what is agent <id> doing | `get_agent` (status, subject, workdir, events) + `get_agent_output` (recent terminal) → report concisely. |
| tell / ask agent <id> to do Y | `send_to_agent` (id as `ticket`, plus `text`). Echo back what you sent. |
| tear down / clean up <id> (full) | `stop_agent` — **the primary teardown verb.** Default = terminate + clear record + remove worktree. Subtractive flags: `keep_record`, `keep_worktree` (`keep_worktree` alone == old `done`); `hard` purges; `pr`+`base` open a PR first; `force`/`delete_adopted_branch` for the worktree. Safe order: PR → terminate → clear record → remove worktree. DESTRUCTIVE (removes the worktree) — **confirm with the user first**. |
| stop / terminate / kill <id> (reversible) | `terminate_agent` — kills tmux + the agent process, keeps record+worktree; reversible via `restore_agent`. Alias for `stop_agent {keep_record:true, keep_worktree:true}`. |
| clear / delete an agent's record | `delete_agent` (id, `hard?`) — archives by default. Alias for `stop_agent {keep_worktree:true}` (record only). |
| remove an agent's worktree | `remove_worktree` (id, `force?`) — DESTRUCTIVE; **confirm with the user first**; terminate the agent first. Alias for `stop_agent {keep_record:true}` (worktree only). |
| restore a lost/orphaned agent | `restore_agent` (id) — only for sessions whose tmux is gone (status `orphaned`); resumes the same conversation. |
| revive an archived record whose tmux is still alive | `recover_agents` (`apply?`) — safety net for the tombstone reaper; bare call reports candidates, `apply:true` re-inserts them (children reconnect automatically via `parent_id`). |
| adopt an existing Claude session | `adopt_agent` (dir?, session_id?, tmux_session?). |

## CLI command map

| Intent | CLI command |
|---|---|
| list / triage agents | `warden ls` (`--json`; `--watch`/`-w` live-updates over SSE; `--tag <t>` repeatable, AND semantics) |
| full status of one agent | `warden status <id>` (`--json`) |
| recent terminal output | `warden agent tail <id>` (`--lines N`) |
| spawn from a prompt | `warden start "<prompt>"` |
| spawn a managed worktree agent | `warden start <TICKET> --type <TYPE> --repo <repo>` |
| spawn with a role / switch a role | `warden start "<prompt>" --role worker`; `warden agent role set <id> <role>` (relaunches); `warden agent role list`. See **Roles** below |
| route a spawn to a model tier | `warden start … --task <name>` (tier from the task registry) or `--tier tier-1\|tier-2\|tier-3` (`--tier` also a pipeline `tier:`; neither on MCP). A pinned `--backend`/`--model` bypasses it. See **Roles** below |
| send a message to an agent | `warden send <id> "<text>"` |
| full teardown (terminate + clear record + remove worktree) | `warden agent stop <id>` (asks before removing the worktree unless `--yes`; `--keep-record`/`--keep-worktree` subtract steps; `--hard`; `--pr [--base <b>]` opens a PR first) |
| terminate + clear record (keeps worktree) | `warden agent done <id>` (= `warden agent stop <id> --keep-worktree`; `--create-pr` pushes the branch and opens a GitHub PR before terminating, `--base` sets target, default main) |
| remove the worktree | `warden agent remove-worktree <id>` (= `warden agent stop <id> --keep-record`; guarded; `--force` overrides) |
| restore a lost/orphaned agent | `warden agent restore <id>` |
| revive an archived record whose tmux is still alive | `warden agent recover` (dry-run; `--apply` to actually revive, `--json` for scripting) |
| adopt an existing session | `warden agent adopt [--session-id <uuid>] [--dir <path>]` |
| attach interactively | `warden agent attach <id>` |
| completion digest | MCP `digest {ticket}` / `warden agent digest <id>` (`--json`) — files touched, branch, turn count, narrative |
| search the fleet | MCP `search {query, closed?}` / `warden inspect search <query…>` (terms ANDed; `--closed` folds in archive) |
| browse the archive | MCP `history {since?, type?, limit?}` / `warden inspect history` (`--since 24h|7d|2w|date`, `--type`, `--limit`) |
| hand off / delegate work (one verb, three modes) | `/warden agent handoff` — see below. New delegate (default) / `--to <id>` (existing agent) keep you running; `--retire` reaps you into a same-worktree successor. MCP `handoff_agent {prompt, context?, to?|repo/type/… | retire+ticket}` |
| rotate yourself into a fresh agent | `/warden agent handoff --retire` (alias: `/warden agent rotate`) — self-succession, see below; remote: MCP `rotate_agent {ticket, resume_prompt, resume_file?}` or `handoff_agent {retire:true, ticket, prompt}` |
| fork an agent's session into a new agent | `warden agent fork <agent> ["<prompt>"]` (shorthand for `warden start --fork-from <agent>`); MCP `fork_agent {source, prompt?, name?, model?, type?, force?}`. **Codex-only** — see **Fork** below |

## Spawn options worth knowing

- **Model** — `--model` (CLI) / `model` (MCP). Aliases `opus`/`sonnet`/`haiku`/`fable`;
  config default `model_default`; fallback `claude-sonnet-4-6`. Shown in the MODEL
  column, preserved on restore.
- **Role** — `--role` (CLI) / `role` (MCP): attach a built-in persona + default
  flags + a default model tier — *who the agent is*. `general` (default, no
  persona) | `orchestrator` | `planner` | `worker` | `autopilot` | `brain` (legacy
  `implementer`/`auto-merger`/`reviewer` still work, mapped to `worker`). The
  role's default flags fill only fields you leave unset (explicit value wins; tags
  unioned). See **Roles** below.
- **Task / Tier** — `--task` / `--tier` (`warden start` + REST; `--tier` is also a
  pipeline-job field `tier:`, but the Job spec has **no** `task:`; **not** MCP
  `spawn_agent`, which routes by `role`): steer the quota-balanced model router
  — *what the agent is doing*. `--task` names a unit of work from the task registry
  whose tier drives routing (tier-1 `analysis`/`architecture`/`design`/`research`/`spike`,
  tier-2 `code-review`/`development`/`docs`/`pr-review`, tier-3 `debug-ci`/`merge-pr`/`monitor-ci`/`release`);
  `--tier` pins `tier-1`/`tier-2`/`tier-3` directly. Precedence: explicit `--tier` >
  task tier > role default tier > tier-2. A pinned `--backend`/`--model` bypasses the
  router. Distinct from `--type` (worktree policy). See **Roles** below.
- **Backend** — `--backend <id>` (CLI) / `backend` (MCP); default `claude`.
  Accepted ids: `claude` | `aider` | `opencode` | `codex` | `crush` | `goose` | `cursor` | `antigravity`.
  A plain terminal is **not** a backend — spawn one as a session kind via `--kind
  terminal` (CLI) / `kind:"terminal"` (MCP `spawn_agent`); the back-compat alias
  `--backend terminal` still resolves to `kind=terminal`. See the terminal-kind note below.
  **Only `claude` is fully tested and stable**; `codex` and `antigravity` are
  **β beta** (live-verified state, approval, and transcript fidelity, still
  maturing); the rest are 🧪 **experimental / work-in-progress** — functionality
  may be reduced or unverified. Backends differ in capabilities
  and warden degrades gracefully (it never crashes on a missing one).
  `aider` is bring-your-own-model — **you must pass `model`** (e.g.
  `ollama_chat/qwen2.5-coder:3b`); it has **no resume** (rotate/handoff re-spawn
  fresh, `restore` refuses), **no priced spend** (`spend` shows tokens, `savings`
  omits it), and runs an **autonomous `--message` task that exits when done**
  rather than a persistent loop.
  `opencode` is also bring-your-own-model (**pass `model`**, e.g.
  `ollama/qwen2.5-coder:3b`); unlike aider it **DOES resume** (dir-scoped —
  `opencode -c` continues the worktree's last session, so rotate/handoff/restore
  work), has a **structured Tier-A transcript** (real digests, sourced via
  `opencode export`), runs a **persistent TUI loop**, gets **context injection**
  (warden's hints via `AGENTS.md`), but **no priced spend** (tokens-only, BYO model)
  and its interactive approval prompts are **not yet parsed** (warden infers idle
  from staleness).
  `codex` is BYO-provider (configured via `~/.codex/config.toml`; pass `model`
  for `-m`); it **DOES resume** (dir-scoped `codex resume --last`, upgraded to
  exact-id via discover-then-pin), has a **structured Tier-A transcript** (JSONL
  rollout files), accepts an initial prompt on TUI launch (like Claude), has
  **live state + approval detection** and **context injection** (`AGENTS.md`); no
  priced spend.
  `crush` is BYO-model (TUI is config-driven; headless `crush run` accepts model);
  it **DOES resume** (dir-scoped, `crush --continue`), has a **structured Tier-A
  transcript** (SQLite, via `crush session show --json`), gets **context injection**
  (`CRUSH.md`), and the initial prompt is **auto-typed into the TUI after launch**
  (`PromptSeeder`), but its approval prompts are **not yet parsed**; no priced spend.
  `goose` is BYO-provider (set `GOOSE_PROVIDER`/`GOOSE_MODEL` env before
  spawning; no `--model` flag on `goose session`); it **DOES resume**
  (name-deterministic — warden pins its own id as the Goose `--name`, so
  `goose session -r --name <id>` is exact, not dir-scoped guessing), has a
  **structured Tier-A transcript** (SQLite, via `goose session export`) and
  **context injection** (`.goosehints`); approval prompts **not yet parsed**; no
  priced spend.
  `cursor` (`cursor-agent`) is a **hosted plan** (billed to the operator's Cursor
  subscription — no $0-local rig, no priced spend); it **DOES resume** (dir-scoped,
  `--continue`), exposes **rich native permission modes** (`plan`/`ask`/`auto-review`/`force`),
  has **live state + approval/trust detection** and **context injection** (`AGENTS.md`),
  but its interactive transcript is an unreadable SQLite store with no export verb, so
  it is **Tier C — no digests yet**.
  `antigravity` (`agy`) is a **Google-hosted free tier** (quota-capped; no priced spend)
  with a **multi-vendor model menu** (Gemini/Claude/GPT-OSS via env/config); it **DOES
  resume** (dir-scoped, `agy -c`), has a **structured Tier-A transcript** (plaintext
  trajectory JSONL ⇒ real digests), **live state + approval detection** and **context
  injection** (`AGENTS.md`).
  A **terminal is a first-class session kind (`kind=terminal`), not a `--backend`
  value** — spawn it with `--kind terminal` / `spawn_agent {kind:"terminal"}` (the
  legacy `--backend terminal` is a back-compat alias). It is **NOT an AI agent** — it
  opens a plain interactive `$SHELL` (fallback `bash`) in the agent's directory,
  managed with warden's normal worktree/git/tmux lifecycle (attach, `commit`/`push`/`sync`,
  snapshot, teardown, cockpit listing). Every AI feature degrades off (no digests,
  resume, model, priced spend, approval parsing) and the **prompt is ignored** (a
  shell would execute it). Use it for the managed "human seat" beside the fleet — a
  shell parked in a repo/worktree that warden tracks and tears down like any other
  agent; in the cockpit it lists under the **Terminals** section.
  Claude is the full-fidelity default — leave `backend` unset unless the operator
  explicitly asked for another agent.
- **Permission mode** — `--permission-mode <acceptEdits|auto|bypassPermissions|default|dontAsk|plan>`
  (legacy `--supervised` = `acceptEdits`). Global default `default_permission_mode`
  (defaults `auto`). Change at runtime: MCP `set_permission_mode {ticket, mode}` /
  `warden agent permission-mode set <id> <mode>`.
- **Force-compact** — when an agent hits the critical context threshold while
  **still working**, warden can interrupt it (Escape), `/compact` once it idles,
  then send a resume prompt — destructive, so it's off by default. Global default
  `token_force_compact`; per-agent override: MCP `set_force_compact {ticket, state}`
  (`on|off|inherit`) / `warden agent compact set <id> on|off|inherit`. The resume
  message is `token_compact_resume_prompt`.
- **Presets** — `warden project preset save <name> [spawn flags]` persists
  `--type`/`--model`/`--permission-mode`/`--auto-restart`/`--worktree`/`--in-repo`;
  `warden project preset list`; `warden start --preset <name>` (explicit flags still
  override). CLI-only.
- **Prompt templates** — `warden project prompt-template save <name> --prompt "…{{VAR}}…"`
  (alias `pt`) persists a reusable, variabled prompt *body* (where presets store
  *flags*); variables are auto-derived from the `{{VAR}}` placeholders.
  `warden project prompt-template list`; `warden start --prompt-template <name> --set
  FILE=foo.go --set X=y` fills it in as the spawn prompt (every declared variable
  must be supplied). A positional prompt still wins; free-form only (no `--type`).
  CLI-only.
- **Library** — `warden project library list` (alias `lib`) is one umbrella that browses
  saved spawn presets, saved prompt templates, AND the built-in pipeline templates
  in labeled sections; `warden project preset save <name> [spawn flags]` delegates
  to `preset save` and `warden project prompt-template save <name> --prompt "…"` delegates
  to `prompt-template save`. Also over MCP as `library_list` (returns `{presets,
  prompt_templates, templates}`). Presets/templates surfaces are otherwise unchanged.
- **Tags** — `warden start --tags backend,urgent` (lowercased, deduped). Part of
  the search haystack; filter with `warden ls --tag …`.
- **Task types (`--type`)** — `development`/`code`/`docs`/`website`/`debug-ci`/`tests`
  get a fresh-branch worktree (isolated in `.worktrees/<id>`; `--in-repo` shares the
  repo). `pr-review` checks out a PR branch (needs `--pr`/`--branch`).
  `analysis`/`spike` run in the repo unless `--worktree`. `other` = no worktree.

## Roles

A **role** attaches a named, persistent system-prompt **persona** to an agent
plus a set of default spawn flags. Every agent has exactly one role; the default
`general` injects no persona (a plain agent). The set is a **fixed built-in
catalog** — no user-defined roles. Browse it with `warden agent role list` / MCP
`list_roles`.

| Role | Persona | Default flags | Default tier |
|---|---|---|---|
| `general` | *(none — plain agent)* | — | tier-2 |
| `orchestrator` | coordinates a fleet of warden agents; plans + delegates, doesn't write feature code unless trivial | `permission_mode=auto` | tier-1 |
| `planner` | research/analysis/planning only — specs, RFCs, design docs; must not edit code | `permission_mode=plan` | tier-1 |
| `worker` | owns one task end-to-end (implement, self-review, PR, drive green, merge) and reports status back to its coordinator | `type=development`, `permission_mode=auto`, `auto_approve=on` | tier-2 |
| `autopilot` | long-lived headless **manager** of a whole autopilot run — decomposes, spawns workers/brains, gates + lands into the integration branch | `permission_mode=bypassPermissions`, `auto_approve=on` | tier-1 |
| `brain` | on-demand **decision resolver** — unblocks a stuck agent or makes an ad-hoc design/arch call, no human interaction | `permission_mode=auto`, `auto_approve=on` | tier-2 |

`autopilot`, `worker`, and `brain` form autopilot's manager → worker → brain
topology. **Legacy aliases:** `reviewer`, `implementer`, and `auto-merger` are no
longer first-class roles — that work is now a **task** (`pr-review`/`development`/`merge-pr`)
— but all three names still resolve to `worker`. The **default tier** feeds the
quota-balanced model router unless a `--task` or `--tier` overrides it.

Drive it:

- **At spawn** — `warden start "<prompt>" --role worker` / `spawn_agent {role:"worker"}`.
  The role's default flags fill only fields you left unset (**explicit value >
  role default > global default**; tags unioned, `auto_approve` OR-ed).
- **On a running agent** — `warden agent role set <id> <role>` / `set_role {ticket, role}`.
  This **relaunches** the agent so the new persona re-injects (its in-flight turn
  is discarded, unlike `set-permission-mode`); `general`/empty clears the persona.
- **UIs** — TUI new-agent `ctrl+r` picker; web **+ New agent** Role dropdown.

Only the role **name** is persisted (`Session.Role`; empty ⇒ `general`); the
persona re-resolves from the registry at every (re)launch, so nothing
persona-shaped is stored and resuming after a switch re-injects automatically.

## Spawn gate

`worktree.spawn_gate` (default on, `worktree.spawn_gate_max_agents` = 5) warns before spawning when
many agents are already live. `--force` (or MCP `force:true`) spawns anyway. The
same gate governs new-delegate `handoff`.

## Parentage & sub-trees

When **you** (an agent) call `spawn_agent`, warden records you as the spawned
agent's **parent** (`parent_id` on its session — stamped from your
`WARDEN_SESSION_ID`; operator/CLI spawns have none). The TUI uses this to nest
the agents you spawn **under you** as a collapsible sub-tree, so the operator can
see which agents are yours vs. theirs. Implications for how you should work:

- Nothing changes in how you call `spawn_agent` — parentage is recorded
  automatically. You don't (and can't) set `parent_id` yourself.
- The operator may **delete you while your children are still running**. You
  won't be removed outright; you become a *terminated tombstone* (a header-only
  row) and your children keep running, so they are never orphaned. The daemon
  removes the tombstone once your whole sub-tree finishes.
- Prefer `spawn_agent` (not the built-in Task/Agent subagent tool) for work the
  operator should see and manage — sub-tree nesting only applies to warden
  agents you spawn.

## Handoff is one concept

`handoff` covers **every** way of passing work to another agent. Pick the mode by
who runs the work next and whether **you** survive:

| mode | flag | successor lives in | does it reap you? |
| --- | --- | --- | --- |
| new delegate | _(default)_ | its own fresh worktree | no — you keep running |
| existing agent | `--to <id>` | that agent's worktree | no — you keep running |
| retire self | `--retire` | **your** worktree (cwd) | **yes** — you are retired |

`--retire` is the **self-succession** mode (formerly the separate `rotate`
command). `warden agent rotate` is now a thin alias for `warden agent handoff --retire` — same
flags, same behavior. `--retire` and `--to` are mutually exclusive (one reaps you,
the other never does).

## Retiring a long-running agent into a fresh one (`handoff --retire`)

When **you yourself** are a long-running agent whose context has grown large and
the user runs `/warden agent rotate` (or asks you to retire/rotate), hand your work to a
fresh successor in the same workspace, then retire yourself. This bounds context
and returns memory to the OS without losing the task. From **inside** the agent
use `/warden agent handoff --retire` (or its `/warden agent rotate` alias) — it reads
`$WARDEN_SESSION_ID`. An **orchestrator** can retire any agent remotely via MCP
`handoff_agent {retire:true, ticket, prompt, resume_file?}` (or the `rotate_agent`
alias) — same semantics (successor inherits the worktree + permission mode; old
agent reaped after the successor spawns).

Two phases, with a human review gate between them:

1. **Prepare (you do this directly — you have your own context).**
   - Write a **handoff file** to a unique per-agent path so concurrent rotations
     never collide — `${TMPDIR:-/tmp}/warden-rotate-handoff-$WARDEN_SESSION_ID.md`.
     Capture what a fresh agent needs to *continue*: the goal, current working-tree
     state (branch, committed vs. uncommitted), key decisions and approaches ruled
     out, precise next steps, and pointers to the relevant files.
   - Compose a one-paragraph **resume prompt** — the successor's initial task.
   - Show the user the handoff file path and the resume prompt, and **stop** for
     review before going further.

2. **Commit (only after the user says go):**

   ```sh
   HANDOFF="${TMPDIR:-/tmp}/warden-rotate-handoff-$WARDEN_SESSION_ID.md"
   warden agent handoff --retire --confirm \
     --resume-file "$HANDOFF" \
     --resume-prompt "<the resume prompt>"
   # `warden agent rotate --confirm …` is an exact alias if you prefer the short verb.
   ```

   This spawns the successor in your exact working directory (same worktree, same
   supervised mode), prints the new agent id, then retires you. The successor
   deletes the temp handoff file once read. Nothing irreversible happens without
   `--confirm`.

Do **not** spawn the successor or terminate yourself by hand — `warden agent handoff
--retire` inherits your launch config and orders spawn-before-reap safely (a
failed spawn leaves you running, so no work is stranded).

## Delegating a sub-task to another agent (`handoff` default / `--to`)

These are the **cross-agent** handoff modes — you hand a slice of work to a
**different** agent and **you keep running** (contrast `--retire` above, which
reaps you). Use them to fork off an independent sub-task or to brief an
already-running agent.

- **New delegate (default):** spawns a fresh agent with **its own isolated
  worktree** off the repo. Best for a sub-task that can proceed in parallel.
- **Existing agent (`--to <id>`):** delivers the handoff into a running agent's
  inbox (waking it if idle). Best for briefing an agent already on a related task.

Same two-phase, human-reviewed shape as the retire mode:

1. **Prepare (you do this directly).** Write a handoff file to a unique per-agent
   temp path (`${TMPDIR:-/tmp}/warden-handoff-$WARDEN_SESSION_ID.md`). The
   recipient runs in a *different* worktree, so write **self-contained** context —
   its content is inlined into the recipient's prompt/message, not read by path.
   Compose a one-paragraph resume prompt. Show both and **stop** for review.

2. **Deliver (after the user says go):**

   ```sh
   HANDOFF="${TMPDIR:-/tmp}/warden-handoff-$WARDEN_SESSION_ID.md"
   # New delegate (its own worktree off your repo):
   warden agent handoff --resume-file "$HANDOFF" --resume-prompt "<task>"
   # …or hand to an existing agent:
   warden agent handoff --to <agent-id> --resume-file "$HANDOFF" --resume-prompt "<task>"
   ```

   New mode prints the delegate's id (`warden agent attach <id>` to watch it); `--to`
   mode confirms delivery and whether the recipient was woken. **You are never
   retired** — this is delegation, not succession. If new-mode spawn is blocked by
   the memory-pressure gate, add `--force` to spawn anyway.

## Fork — branch an agent's session into a new agent (`warden agent fork` / `fork_agent`)

Fork is the **third** way to put an agent's work into a new agent, and the only one
that **keeps the conversation**. Where handoff/rotate carry the *task* but drop the
*conversation*, a fork **branches the source's recorded session sideways**: it
continues the source agent's conversation/reasoning (the backend's session rollout)
in a divergent timeline as its own managed agent — a fresh sibling worktree off the
source's branch HEAD, seeded with the source's uncommitted **tracked** changes
(dirty-tree carry), with its own tmux session warden monitors and tears down. **The
source agent keeps running, untouched.**

| verb | what moves | the source |
| --- | --- | --- |
| **fork** | the **whole conversation**, branched into a new timeline | keeps running |
| **snapshot** restore | rewinds **one** timeline to a checkpoint | the same agent |
| **rotate** / **handoff** | the **task only** (fresh conversation) | retired (`--retire`) or kept (delegate/`--to`) |

Drive it from either surface (it's a managed spawn, so it has **MCP + CLI parity** —
unlike the CLI-only `wd git review` / `wd backend model` superpowers):

```sh
warden agent fork agent-7                  # fork agent-7, continue its conversation
warden agent fork agent-7 "now try X"      # fork and seed a divergent first prompt
```

MCP: `fork_agent {source: "<id>", prompt?, name?, model?, type?, permission_mode?, force?}`
— a thin wrapper over `spawn_agent` with `fork_from` set (no new endpoint).

Worth knowing:

- **Codex-only today.** Fork is gated by the backend's native session fork
  (`agentbackend.SessionForker`, which **Codex** implements). A backend without one
  (e.g. Claude) returns a clean "cannot fork" — don't treat that as an error to work
  around; it's the "add on top, never restrict" rule.
- **Pin the session first.** The source's backend session id must already exist — if
  the source hasn't run a turn yet, fork says so; let it run, then retry.
- **Tracked changes only.** The dirty-tree carry seeds the source's **tracked**
  uncommitted changes; untracked / `.gitignore`'d build artifacts are not carried.
- **Inherits repo + backend** (resolved daemon-side); `--type` defaults to
  `development` (a fork needs its own worktree). If the memory-pressure gate warns,
  add `--force` / `force:true`.


## Backend recovery

When an agent hits a confirmed provider hard limit, the **reactive backend recovery coordinator** automatically tries eligible subscription backends from the backend registry — no operator action needed. Watch for a non-null `backend_recovery` field on sessions from `list_agents` / `get_agent`.

**Recovery phases:**

| `backend_recovery.phase` | Meaning |
|---|---|
| `refreshing_usage` | Coordinator reading usage windows from all backends |
| `switching` | Hot-swapping to the selected candidate |
| `stabilizing` | Candidate running; waiting for the stabilization window to elapse |
| `waiting_for_capacity` | All candidates exhausted; retry timer armed |

**Key fields:**
- `backend_recovery.current` — `{backend_id, model_id}` of the candidate being tried
- `backend_recovery.attempts` — ordered list of all tried candidates and their outcomes
- `backend_recovery.next_retry_at` — when the coordinator will retry (while waiting)
- `backend_recovery` null — no recovery active (normal operation or recovery complete)

**When surfacing recovery to the user:**
- Treat `waiting_for_capacity` like `rate_limited` — the agent needs capacity, not intervention.
- If the user wants to override: `switch_agent` / `stop_agent` automatically supersedes recovery; `send_to_agent` also works once the new backend is running.
- You do NOT need to manually manage recovery timers; the daemon owns them.

SSE subscribers get a notification on every phase transition — `list_agents` or `get_agent` after an SSE event gives the current state.
