# Collaboration Groups & Pipeline-as-Substrate — Design Spec

**Date:** 2026-08-26
**Status:** Draft — design review / brainstorming. No implementation yet.

This spec covers two related shifts:

1. **Pipeline demoted from a user-facing noun to the shared execution
   substrate** — one lifecycle engine, three levels of autonomy over it
   (direct/human, orchestrator-delegated, autopilot), with **push-based**
   completion signalling so orchestrators stop burning tokens polling.
2. **Collaboration groups** — a first-class way for one developer's
   per-project orchestrator agents to become *aware of each other* and message
   / delegate across projects, built on the messaging and shared-context
   primitives warden already has.

Scope is the **single developer, single machine, multiple projects** case.
Cluster mode and multi-developer collaboration on a shared project are parked
in §8 (Future scope) — both require warden-hub as transport/directory and are
explicitly out of scope here.

---

## 0. Design principles

1. **One engine, three autonomy levels.** Pipelines, manual subagents, and
   autopilot are not three parallel mechanisms — they are one warden-owned
   lifecycle engine (spawn → run → tear down) driven at three levels of
   control. Demote "pipeline" as a marketed peer of subagents; keep the engine
   as the substrate everything stands on.
2. **Warden owns lifecycle; the orchestrator sleeps.** In both the subagent
   (single job) and pipeline (multiple jobs) cases, warden creates the worker,
   runs it, tears it down, and **wakes** the orchestrator with results. The
   orchestrator must never poll — that is the whole token argument.
3. **Detect state out-of-band; never interrogate on a loop.** Warden learns
   whether a job is done / blocked / stuck from zero-token signals (an explicit
   done-signal from the worker, pane-state classification, a watchdog). Asking
   the agent to self-report is reserved for the one genuinely ambiguous case
   and is done once, not on a poll loop (§2).
4. **Groups make agents discoverable; messaging stays as-is.** A collaboration
   group is an *address book + roster*, not a new communication channel.
   Directed messaging (`send_message` / `read_inbox` / `wait_for_message`) and
   the shared-context blackboard (`ctx_*`) are unchanged; the group just tells
   agents who is present and who to contact for what.
5. **Membership is social; identity is durable.** Because directed messaging is
   keyed by agent-id and independent of group membership, leaving a group is a
   *soft* operation (stop being discoverable / accepting new work) that never
   breaks in-flight replies. Only **termination** orphans work (§5.4).
6. **Git-anchored throughout.** A project is a git remote (§4.1); cross-project
   changes always land as a PR in the *target* repo, made by the *target's*
   own worker on its own worktree. Nobody edits a repo they don't own.

---

## 1. Goal & scope

### In scope

- **Pipeline-as-substrate:** reposition pipeline as warden's lifecycle engine,
  keep a thin direct-to-engine human entry point, and add an
  **orchestrator-delegated** mode where an orchestrator hands warden a work
  plan and blocks on a wake-up instead of polling.
- **Completion / stuck detection** built from an explicit done-signal +
  pane-state classifier + watchdog, with agent self-report demoted to a
  sparing fallback.
- **Collaboration groups** for a single developer: a `join`/`leave` command
  that seats one orchestrator per project in a named group, brokers
  introductions through warden, and persists the roster in a store.
- **The four edge rulings** (§4): project identity, capability/summary,
  storage, leave-vs-terminate.
- **Cockpit UI changes** (§6) that apply the pipeline demotion: TUI control
  pane (conditional Approvals, pipelines nested under their orchestrator,
  Agents/Terminals as nested frames) and a new web **Agents** tab.

### Out of scope

- Any change to the directed-messaging or `ctx_*` wire formats.
- Autopilot's brain/overwatch internals (autopilot remains the opt-in fully
  autonomous level; this spec only positions it relative to the engine).
- **Cluster mode** (one logical dev environment across multiple daemons/
  machines) and **multi-developer project collaboration** (shared live context
  across people) — see §8. Both need the hub.
- A typed cross-project *delegation protocol*. v1 delegation is a directed
  message to a known peer; the peer decides how to fulfil it (§6).

---

## 2. Pipeline-as-substrate & push-based monitoring

### 2.1 The three levels

| Level | Who authors the plan | Who owns lifecycle | Token cost while running | Blocking prompts |
|-------|----------------------|--------------------|--------------------------|------------------|
| **Direct** (human) | developer, via `create_pipeline` | warden | none | fall to human |
| **Delegated** (orchestrator) | a live orchestrator, at runtime | warden | none while asleep; wakes at checkpoints | fall to human |
| **Autopilot** | autopilot manager | warden | brain/overwatch as designed | self-resolved |

The only structural difference between a "subagent" and a "pipeline" is
**single job vs. multiple jobs**. In both, warden creates the worker(s), runs
them, tears them down, and wakes the orchestrator with results. "Subagent" is
just a one-job pipeline; do not model it as a separate subsystem.

### 2.2 Keep a direct-to-engine human path

Demoting pipeline as a *noun* must not delete the engine's human entry point.
There is a real case — a nightly multi-step job, a CI-like sequence — where a
developer wants "run these steps, no agent babysitting, no tokens" with no
orchestrator in the loop. `create_pipeline` / `start_pipeline` remain as the
direct path; they simply stop being marketed as a peer of subagents.

### 2.3 Delegated monitoring — the token unlock

The delegated mode is the new capability. An orchestrator hands warden a work
plan with explicit **callback points** and then blocks:

- A **pure DAG** has zero callbacks — warden runs the whole thing and wakes the
  orchestrator once, at the end, with the aggregate result.
- An **adaptive plan** ("if tests fail, try approach B") declares callbacks at
  the decision points where the agent's judgment is genuinely required. Warden
  owns everything mechanical between callbacks.

The orchestrator blocks on `wait_for_message` (the primitive already exists)
and spends tokens only when woken at a real decision point. Mechanical steps
cost the orchestrator nothing.

> **Interface being designed:** the split between *mechanical steps warden owns*
> and *decision points that need the agent*. Everything mechanical is free;
> only decision points cost a turn.

### 2.4 Completion / stuck detection

Interrogating the worker on a loop ("reply with `{status,...}` JSON") would
just move the polling cost from the orchestrator to the worker — every poll is
a full model turn, it pollutes the working transcript, and a genuinely stuck
agent won't answer reliably anyway. So warden detects state from zero-token
signals and reserves asking for the one ambiguous case:

1. **Explicit done-signal (primary, deterministic).** Workers are launched with
   an instruction to declare completion — e.g. run `wd job done --summary '…'`
   or emit a sentinel line `<<WARDEN_DONE>>{json}`. Warden watches for it. This
   yields **status *and* summary in one shot**, no extra turn. This is the
   backbone; it mirrors how structured-output subagents declare their result.
2. **Pane-state classifier (for what the agent can't self-report).** Reuse the
   existing pane-reading machinery (the same used by auto-approve, trust-prompt,
   and rate-limit handling) to continuously classify the pane as `working` /
   `idle-at-prompt` / `blocked-on-approval` / `exited`. `blocked` and `exited`
   are unambiguous and free.
3. **Wall-clock watchdog (for true hangs/loops).** No state change for *N*
   minutes → flag `possibly-stuck`, notify human/orchestrator. Optional
   output-hash-repeat detection for loops later.
4. **Agent self-report — sparing fallback only.** Exactly one case is
   genuinely ambiguous: an interactive agent that returns to its input prompt
   after finishing looks identical to one waiting for a human answer. **Only
   there** does warden ask, and it asks **once, on the transition to idle**,
   using the `{status, details, summary}` contract — never on a poll loop.

Blocking prompts that warden can't (or shouldn't) auto-answer fall to the
human, consistent with principle 2. Full autonomy is the autopilot opt-in.

---

## 3. Collaboration groups — overview

A collaboration group is a named set of **per-project orchestrator agents**
belonging to a single developer, on one machine, made mutually discoverable so
they can message and delegate across projects (BE↔FE, a project and a
dependency it needs changed, etc.).

The group is deliberately thin: it is a **roster + an introduction broker**. It
adds no new messaging channel — peers talk over the existing directed-message
bus once they know each other exists.

### 3.1 Command surface

```
wd collaborate group <group-name> join
wd collaborate group <group-name> leave
```

(MCP mirror: a `collaborate_group` tool taking `{group, action}` — CLI/MCP
parity per repo convention.)

- **join:** add the calling agent to `<group-name>`, creating the group if it
  does not exist. On join warden:
  1. Enforces **one orchestrator per project** (§4.1); a duplicate join fails
     and returns the already-seated agent.
  2. Switches the joining agent to the **orchestrator** role (`set_role`).
  3. Resolves the agent's **project summary** (§4.2) and writes the roster
     entry.
  4. **Brokers introductions both directions** (§3.2).
- **leave:** remove the calling agent from the group's roster and notify peers.
  Soft semantics (§5.4).

### 3.2 Introductions are brokered by warden (not by the agents)

To keep this token-cheap, warden — not the agents — performs the introductions:

- On join, warden sends each existing member a templated message: *"Agent
  `<name>` (`<id>`) joined group `<group>`; it orchestrates project `<project>`
  — `<summary>`. Contact it for changes to that project."*
- Warden sends the joining agent the reciprocal roster: the same template for
  every existing member.

Agents therefore spend **no tokens** generating or exchanging introduction
prose; they receive a compact, uniform descriptor and can immediately address
peers. The roster is also readable on demand (it lives in `ctx_*` / the group
store, §4.3).

---

## 4. The four edges

### 4.1 Project identity = the git remote URL

A "project" is keyed by its **canonical git remote URL** (GitHub or GitLab),
not the local path. Consequences:

- Two worktrees of the same repo are the **same project** and cannot both hold
  a seat — this is what makes "one orchestrator per project" well-defined.
- A duplicate `join` (same normalized remote already seated) **fails** and
  returns the seated agent's id/name in the error, so the caller can message
  the incumbent instead.
- Remote URLs are normalized before comparison (scheme/host/`.git` suffix/
  trailing slash/casing) so `git@github.com:org/repo.git` and
  `https://github.com/org/repo` resolve to one key.
- An agent with no remote (local-only repo) **may** join using a **`local:`
  fallback key** derived from its path/root (settled 2026-08-26). The key is
  tagged `local:` because it is **not** portable across machines — the future
  hub/cluster work must treat local keys distinctly from remote ones.

### 4.2 Capability & project summary

The orchestrator's capability is generic — *"orchestrates work for project
X."* What makes a peer **targetable** is the human-readable *what-for*, so each
roster entry carries a one-line project summary. Resolution order (cheapest
first):

1. **Declared blurb** — a project-config field, or a `## Summary` / first
   paragraph of `CLAUDE.md` / `README.md`. Zero tokens.
2. **Agent-generated once** — if nothing is declared, warden asks the joining
   agent for a single line. The agent already has project context loaded, so
   this is one cheap, **one-time** turn.
3. **Cached** on the group/project record so re-joins and daemon restarts never
   regenerate it.

Human-declared wins when present; the agent fills the gap once; cached forever
after.

### 4.3 Storage

Groups persist in a ScrivaDB store (a small `collaboration`/`groupstore`, or a
reuse of the `ctx_*` blackboard under a `group/<name>` namespace — decide at
implementation).

- The record holds **only** the lean roster: per member `{name, id, project
  (remote key), summary, joined_at}`.
- **Never** put transcripts or message logs in the group record — that is what
  previously tripped the oversized-record `ReadAt` (>64 KB) and index-corruption
  failure modes. Messages stay in the existing inbox store; the group record
  stays small and mostly static.
- The **group is durable** (survives daemon restarts); **membership is live**
  (tied to a running agent). On `recover_agents`, a recovered orchestrator
  **auto-rejoins** its groups and re-announces, so groups don't rot after every
  restart.

### 4.4 Leave vs. terminate

The key realization: **directed messaging is by agent-id and independent of
group membership.** So the two operations are cleanly separable.

**Leave — soft / social (default, safe by construction):**
- The agent stops being *discoverable* and stops accepting *new* delegations.
- It does **not** disappear as a message target:
  - Delegations it *sent* → peers' replies still route to its inbox on
    completion. Nothing breaks.
  - Delegations it *received* and is mid-way on → it **drains** them (finishes
    committed work). Leave means "no new inbound," not "abandon in-flight."
- Warden notifies the group: *"`<name>` left `<group>` — send no new requests;
  in-flight work still completes."*

**Terminate — hard (this is the case that orphans work):**
- Tearing down an orchestrator that still has *received* delegations open
  orphans them. As part of teardown, warden checks the agent's group
  memberships and notifies each requester: *"your delegate `<name>` was
  terminated; request `<ref>` is abandoned."*
- Because this is destructive to peers' in-flight work, **terminating an
  orchestrator that is a member of any group requires explicit friction** — a
  confirmation prompt that names the group(s) and any outstanding delegations
  that would be abandoned. (Applies to `terminate_agent` when the target holds
  a group seat.)

Design them as two distinct paths: leave is graceful and reply-safe; the hard
orphaning lives with terminate, where it belongs and where the confirmation
gate sits.

---

## 5. Cross-project delegation (v1)

No delegation *protocol* in v1. The group is an **address book**; delegation is
a directed message to a known peer:

> FE orchestrator → `send_message` to BE orchestrator: *"I need `POST /x`."*
> BE orchestrator reads its inbox, decides, spawns **its own** worker on **its
> own** worktree, opens a PR in the **BE** repo, and replies with the PR link.

This composes with the git-first invariant:

- Worktree isolation stays intact — nobody edits a repo they don't own.
- Cost is attributed to the right project (the target's worker spends the
  tokens).
- Every cross-project change is auditable as a PR in the target repo.

If freeform messaging later proves too loose, a typed delegation request
(status, acceptance, completion callback) can be layered on top — but not
before it's needed.

---

## 6. UI changes — TUI first, then web

Applies the §2 pipeline-as-substrate demotion to the cockpit. This builds on
the 2026-08-09 cockpit spec (`control`/`agent`/`terminal` panes; the
four-section control tree). **TUI is the priority surface** (it's the one used
most); web follows once the TUI shape is validated.

### 6.1 TUI — control pane (`internal/tui/control_pane.go`, `boxes.go`, `compositor.go`)

Four changes.

**(a) Approvals: conditional, not always-present.**
Today `secApprovals` is emitted as a permanent header. Change: emit it **only
when `len(recognizedApprovals) > 0`**. Zero pending ⇒ the section vanishes
entirely (no header, no count row), decluttering the common case. Cursor-home
(`firstEntityCursor`) already skips section headers, so nothing else moves.

**(b) Pipelines: demoted — delegated ones nest under their owner; only
orchestrator-less ones keep a conditional top-level section.**
`secPipelines` stops being an always-present peer of Agents. A **delegated**
pipeline renders as a **node in the Agents tree, under the orchestrator that
owns it**, a sibling of that orchestrator's subagents:

```
Agents
  <dir>
    orch-agent
      subagent
      <pipeline name>
        job-1
        job-2  (deps: job-1)
```

- The pipeline node shows its name; its **jobs** nest under it, each annotated
  `(deps: …)` when it has dependencies — the DAG is legible inline.
- This unifies with §2: a **subagent is a one-job pipeline**. A subagent shows
  as a leaf; a pipeline shows as a named node with job children. Both are
  warden-owned work hanging off the orchestrator.
- **Ownership / where a pipeline lives:**
  - A **delegated** pipeline nests under the orchestrator that created it
    (a `parent_id`-style link pipeline → owning agent) — inside the Agents
    tree, as shown above.
  - A **human-created** pipeline (direct-to-engine, no orchestrator, §2.2) has
    no owning agent, so it keeps a **minimal top-level `Pipelines` section** —
    but that section is **conditional, exactly like Approvals: shown only when
    there is at least one human-owned pipeline to display** (zero ⇒ no header,
    no section). Delegated pipelines never appear here (they're in the Agents
    tree), so on a fleet with only delegated pipelines the section stays hidden.
  - So `secPipelines` survives, but demoted to a *conditional home for
    orchestrator-less pipelines only* — not the always-present peer of Agents
    it is today.
- Job rows keep today's collapse-completed-by-default behaviour and the cancel
  affordance (currently `secPipelines`-scoped) — reattached to the pipeline
  node. `pipelineAgents()` still resolves the same live sessions; only their
  render position changes.

**(c) Agents & Terminals as nested frames inside the control frame.**
Today the control pane is one box holding flat sections. Introduce two **inner
frames** — an **Agents frame** and a **Terminals frame** — each bordered/titled
like the outer control frame (reuse `boxes.go`), so the control frame becomes a
container:

```
┌ Control ──────────────────┐
│ (Approvals — only if >0)   │
│ (Pipelines — only if >0)   │   ← human-owned pipelines only (§6.1b)
│ ┌ Agents ──────────────┐   │
│ │ <dir> / orch / …      │   │
│ └──────────────────────┘   │
│ ┌ Terminals ───────────┐   │
│ │ 1. warden:site/ …     │   │
│ └──────────────────────┘   │
└───────────────────────────┘
```

- Each inner frame scrolls independently and can collapse.
- Approvals and the human-pipelines section, when non-empty, are thin
  **conditional** banners above the Agents frame (neither is an agent or a
  terminal). Both follow the same "hide when zero" rule.
- This is a compositor/`boxes.go` change; the `item` model is unchanged — rows
  are routed into the frame that owns them.

**(d) Rotation hotkeys** (`M-a` agents, `M-t` terminals, `M-p` pipeline agents)
keep their behaviour; only the render position of pipeline rows changes.

### 6.2 Web — new Agents tab

Web fixed tabs today: `['cockpit','pipelines','terminals','metrics','archive','others']`
(`web/src/lib/tabs.ts`, `web/src/lib/router.ts`). Add an **`agents`** tab
mirroring the Terminals tab's master-detail layout:

- **Left:** list of live agents — reuse the `kind`-filtered agents list that
  already feeds cockpit (the Terminals tab consumes the terminal-kind list;
  this consumes the agent-kind list, per `web/src/lib/kind.ts`).
- **Right:** the focused agent's interactive view — reuse the existing
  `web/src/components/AgentTab.tsx` (already the focused single-agent terminal
  view).
- Selecting an agent on the left opens it on the right, exactly like Terminals.

**Open consistency point — the web `pipelines` tab.** To fully mirror the TUI
demotion, pipelines should stop being a peer tab and render nested under agents
too. This spec's web scope is **only** adding the Agents tab; the pipelines-tab
fate (leave it, or fold pipeline rows into the Agents tab as nested nodes like
the TUI) is a deliberate follow-up decision — flagged, not decided here.

---

## 7. Naming for future-proofing

Even though v1 is single-daemon, name group entities so the hub version is
"same bus, remote transport" rather than a rewrite:

- Address peers by a stable name that does **not** assume a single daemon —
  e.g. `group/<name>/<project-key>` rather than a bare local agent index.
- Keep the roster descriptor (name, id, project remote, summary) transport-
  agnostic so it serializes unchanged over the hub later.

This is the only concession the local design makes to the future; everything
else in §§2–5 is fully local.

---

## 8. Future scope (needs warden-hub — NOT in this spec)

Parked deliberately; listed so the local design doesn't foreclose them.

1. **Cluster mode.** Multiple daemons across machines presented as one logical
   dev environment; the developer issues commands without knowing which machine
   runs them. This is **federation with a unified control plane**, not a
   k8s-style scheduler — work is *pinned to data* (the repo checkout lives on
   one machine and repos are too big to shuffle per command). The hub routes
   each command to the daemon holding the relevant checkout, and provides a
   merged view + health/failover.
2. **Multi-developer collaboration on one project.** Several people, each on
   their own machine/cluster, sharing **live context** on a project. The hard
   part is not transport but **multi-writer context sync** — model it as an
   append-only **event log** (each collaborator emits context events, everyone
   folds them), not CRDT, unless proven necessary — plus **authz and cost
   attribution** (who may delegate onto whose machine, whose tokens pay, whose
   approval gates it). Sort authz before the sync plumbing; it shapes the data
   model.

Both reuse the two primitives this spec establishes locally — a **directory +
addressing** layer and a **message + context bus** — widened to a remote
transport. Building them well locally is the prerequisite.

---

## 9. Definition-of-Done checklist (for the eventual implementation)

Per repo `CLAUDE.md`, when this is built each item must be walked:

- **Tag & release** — one tag per feature (minor for the group feature, patch
  for incremental pieces); confirm before pushing the `v*` tag.
- **Docs** — `README.md`, `docs/FEATURES.md` (+ root matrix), `docs/USAGE.md`,
  this spec, and the website (`site/` — a `guides/` page for collaboration
  groups + a `reference/cli.md` entry, the latter regenerated via
  `make gendocs`, never hand-edited).
- **CLI help** — cobra `Use`/`Short`/`Long` for `wd collaborate group …`, then
  `make gendocs` + commit.
- **Skill** — update `skills/warden/` so agents know how to join/leave groups
  and how delegated monitoring changes their orchestration pattern.
- **MCP/CLI parity** — a `collaborate_group` MCP tool alongside the CLI command.
- **UI** — TUI cockpit (`internal/tui/`: control-pane frames, conditional
  Approvals, pipeline nesting) and the web app (`web/src/`: new Agents tab,
  plus the pipelines-tab consistency decision from §6.2). Keep the TUI and web
  cockpit behaving identically per the daemon-owns-state principle.
