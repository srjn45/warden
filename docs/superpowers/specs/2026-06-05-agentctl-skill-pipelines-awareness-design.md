# agentctl Skill — Pipelines Awareness & the Plain-Agent-vs-Pipeline Rubric

**Date:** 2026-06-05
**Status:** Approved design, pre-implementation

## 1. Problem

The packaged `agentctl` Claude Code skill (`skills/agentctl/SKILL.md`) is the
mechanism by which a Claude session learns what agentctl can do — it auto-loads
when a request matches its triggers, installed via `make install-skill`. But it
was written before any of the pipeline / inter-agent-communication work: it has
**zero** mention of pipelines, shared context (`ctx`), or directed messages
(`msg`). So a Claude session today literally cannot know those capabilities
exist, and has no guidance on **when to use a plain agent vs a pipeline** — which
matters because the user will author pipelines by *asking a Claude session*, not
by writing YAML by hand.

## 2. Goal

Make Claude (a) aware of agentctl's full current capability surface and (b)
able to choose the right tool for a delegation. Two artifacts:

- **A: rewrite `skills/agentctl/SKILL.md`** — add the decision rubric, a
  "how to author a pipeline" section, and the `ctx`/`msg` primitives; keep the
  existing plain-agent content (reframed as tier 1).
- **B: a ~3-line nudge to `~/.claude/output-styles/orchestrator.md`** — so
  delegate-mode actively reaches for pipelines on multi-stage work.

**Scope decisions (confirmed):** skill-only — Claude drives pipelines via the
`agentctl pipeline …` **CLI** (Bash), not new MCP tools (the CLI is universal and
sidesteps enterprise-MCP lockout). No Go code changes.

## 3. The decision rubric (the core content)

A 3-tier model fronted by one litmus test:

> **Litmus test: "Does any step need to wait for another step's result (its
> output or its code) before it can start?"** — No → plain agent(s). Yes → pipeline.

1. **Plain agent** (`agentctl start "…"` / `spawn_agent`) — the default. One
   self-contained task. Several *independent* agents at once are still just
   multiple plain spawns, **not** a pipeline. Human stays in the loop, relaying.
2. **Pipeline** (`agentctl pipeline create -f … && … start`) — structured
   dependency between stages, where you'd otherwise babysit "wait for A, then
   start B with A's result": sequential handoff (analyze→implement→review),
   fan-out→fan-in (parallel work → a synthesis/merge step), code flowing
   downstream (later job builds on an earlier job's branch), or anything you want
   to run **unattended** (the daemon drives it; flags `needs_attention` on stall).
3. **`ctx` / `msg`** — ad-hoc coordination between otherwise-independent agents
   (a shared scratchpad, or one agent asking another a question), when you want
   light cross-talk but not a full DAG.

**Anti-patterns the skill calls out:**
- A pipeline for a *single* task → needless overhead; use a plain agent.
- Plain agents + manual relay for a *clear dependency chain* → that's exactly
  what a pipeline automates.
- Hand-rolling coordination with `ctx`/`msg` to rebuild what a pipeline already
  does → just use a pipeline.

## 4. Artifact A — `skills/agentctl/SKILL.md` structure

Frontmatter `description` (triggers) gains pipeline/ctx/msg intents so the skill
loads for them: e.g. "create/run a pipeline", "run these steps in order",
"multi-stage / dependent agent work", "have agents share data / ask each other".

Sections (existing plain-agent material is kept, reframed under the rubric):

1. **What agentctl is** — daemon + per-task agents, **plus** pipelines (DAGs of
   agent jobs), shared context, and directed messages.
2. **Choosing the tool** — the §3 rubric: litmus test, 3 tiers, anti-patterns.
   Placed near the top so it's read before acting.
3. **Plain agents** (tier 1) — the existing intent→action table, CLI command
   map, and guardrails, substantively unchanged.
4. **Pipelines** (tier 2):
   - Lifecycle: `pipeline create -f <spec.yaml>` → `pipeline start <name>` →
     monitor with `pipeline show <name>` (shows per-job status, branch, emitted
     output) → `pipeline cancel`/`pipeline delete`.
   - **YAML schema:** `name`, `repo`, and `jobs:` each with `id`, `prompt`,
     optional `depends_on: [ids]`, `worktree: none|fresh|from:<job>` (default
     `none`), optional `handoff` (one-line "what to hand downstream"), optional
     `supervised`, optional `type`.
   - **Worktree modes:** `none` = runs in repo root, touches no code
     (analysis); `fresh` = new branch off repo head (writes code); `from:<job>`
     = new branch based on that upstream job's branch (builds on its commits; the
     fan-in agent runs `git merge` itself).
   - **Authoring rule (critical):** write each job's `prompt` as a plain task
     description plus, when relevant, a `handoff` line. **Do NOT put `agentctl
     pipeline emit` instructions in the prompt** — the daemon auto-appends the
     emit footer; upstream outputs are auto-injected into downstream prompts.
   - Driving/recovery: `pipeline emit "<text>"` (an agent publishes its handoff;
     the operator/lead can emit on a job's behalf), `pipeline edit-job` (a
     *pending* job's prompt/handoff), `pipeline retry <job>` (re-run a failed /
     needs-attention job, reopening skipped descendants).
   - **Worked example:** the analyze→implement→review dev chain
     (`analyze` worktree:none → `implement` depends:[analyze] worktree:fresh →
     `review` depends:[implement] worktree:from:implement), shown as the YAML
     spec + the create/start/show command sequence.
5. **Shared context & messages** (tier 3):
   - `ctx set/get/list/del` — a namespaced KV blackboard agents read/write.
   - `msg send/inbox/wait` — per-agent inbox; `wait` blocks cheaply (one Bash
     call); a working agent is never interrupted (woken only when idle/waiting).
   - Framed as ad-hoc coordination between independent agents.
6. **Guardrails** — existing ones, plus: cancel a pipeline before deleting it
   (delete refuses while jobs are live); `from:` chaining means a later job
   inherits an earlier job's branch; don't hand-roll coordination a pipeline
   provides.

Target length ~150 lines (from ~79), kept scannable via tables and one short
example per capability. Remains a single skill — no split.

## 5. Artifact B — orchestrator output-style nudge

Edit step 2 ("Delegate") of `~/.claude/output-styles/orchestrator.md` to read
approximately: *"Delegate. Spawn a **plain agent** for a self-contained task; set
up a **pipeline** (`agentctl pipeline create -f …`) when the work has **dependent
stages** — one step needs another's result — per the agentctl skill's rubric."*
~3 lines. This file is **user-local** (`~/.claude/`, not version-controlled in
the repo); the edit is applied in place. (Optionally a repo-tracked copy could be
added later for reproducibility — out of scope here.)

## 6. Verification

Skills are not unit-testable. Verification is **accuracy**:
- Every command, flag, and YAML field in the rewritten skill matches the real
  `agentctl` surface — checked against `agentctl --help` / `agentctl pipeline
  --help` / `agentctl ctx --help` / `agentctl msg --help` and this design.
- Frontmatter is valid and the triggers cover the new intents.
- The orchestrator nudge reads cleanly and points at the skill's rubric.
- A light manual sanity check: a fresh reading of the skill makes the
  plain-vs-pipeline choice obvious for a couple of example requests.

## 7. Non-goals

- No new MCP tools (pipelines stay CLI-driven for Claude).
- No Go/daemon changes.
- No new skill files (single `agentctl` skill).
- Not adding a repo-tracked copy of the orchestrator output style (it stays
  user-local; can be revisited).
