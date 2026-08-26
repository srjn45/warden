# Implementation Plan — Collaboration Groups, Pipeline-as-Substrate & Cockpit UI

**Companion to:** `2026-08-26-collaboration-groups.md` (design)
**Date:** 2026-08-26
**Status:** Ready to execute (autopilot-driven)

This breaks the design into **small, bounded-context units** so it can be handed
to **autopilot**: each worker gets one self-contained brief, keeps a small
context, and spends no tokens rediscovering the design. Every unit is tagged
**subagent** (one job) or **pipeline** (a warden-managed job-DAG).

---

## 0. Execution model — autopilot-driven, bounded context

This plan is built to be **handed to autopilot**. Autopilot is the manager: it
reads the unit list (§1), spawns a worker per unit/job, gates each PR on CI +
`make verify-fast`, tears the worker down, and moves on. Its **brain** resolves
blocking prompts; **overwatch** nudges it if it stalls. Human touchpoints per
feature are two: kickoff and the release-tag confirmation (§5).

Two rules keep this token-cheap — the whole reason for small tasks:

1. **Bounded briefs.** A worker is handed **only its own stage brief** (§§2–4)
   plus the two spec docs — never the whole plan. Each brief is self-contained
   (goal · file surface · acceptance · out-of-scope), so context stays small.
2. **Warden owns lifecycle, not a live agent.** Multi-step chains are expressed
   as **pipelines** so *warden* — not a token-burning orchestrator — spawns each
   job's fresh worker and carries results forward. Nobody holds the whole chain
   in one context.

### Subagent vs. pipeline — the decision rule

- **Subagent (one job):** a single bounded task, independent of the others, done
  in one PR-sized context. Warden spawns it, it completes, warden wakes the
  manager. Used for the isolated UI edits (C1, C2, C4).
- **Pipeline (multi-job DAG):** a whole feature or an ordered chain whose steps
  each deserve a **fresh** context. Warden runs the DAG; no-dep jobs run in
  parallel; each job is a short-lived worker. Used for the two backend features
  (groups, monitoring) and the two-step UI demotion (C3).

(This mirrors the design itself: **a subagent is a one-job pipeline.**)

---

## 1. Execution units

| Unit | Type | Jobs (→ = intra-unit dep) | Cross-track gate |
|------|------|---------------------------|------------------|
| **U-B — Collaboration groups** | **pipeline · 8 jobs** | B1 store · B2 git-key · B3 join/leave (←B1,B2) · B4 intros (←B3) · B5 summary (←B4) · B6 leave/terminate (←B3) · B7 recover-rejoin (←B3) · B8 CLI+MCP+docs (←B4,B5,B6) | none |
| **U-A — Pipeline monitoring** | **pipeline · 5 jobs** | A1 done-signal · A2 stuck-detect · A3 owner-link · A4 delegated-push (←A3) · A5 idle-self-report (←A2) | none |
| **U-C3 — TUI pipeline demotion** | **pipeline · 2 jobs** | C3a conditional-section+render-rule · C3b delegated-nesting (←C3a) | **C3b needs A3** |
| **U-C1 — TUI conditional approvals** | **subagent** | — | none |
| **U-C2 — TUI nested frames** | **subagent** | — | none |
| **U-C4 — Web agents tab** | **subagent** | — | none |

**The only cross-track edge:** `C3b` (delegated-pipeline nesting) needs `A3`
(owner link) merged first. Everything else is independent.

### Autopilot launch plan

Autopilot starts almost everything at once; the DAGs self-sequence:

- **Launch immediately (parallel):** the **U-B pipeline**, the **U-A pipeline**,
  subagents **U-C1 / U-C2 / U-C4**, and pipeline job **C3a**.
- **Gate:** start **C3b** only after **A3** merges.
- Within each pipeline, no-dep jobs run in parallel (B1‖B2; A1‖A2‖A3); the rest
  fall in behind their deps.

Shape: two long pipelines (groups, monitoring) running beside three quick UI
subagents plus one two-step UI pipeline with a single gated job. The manager
stays idle/cheap between worker completions.

### Per-job worker brief (what autopilot hands each worker)

> You are implementing **<stage id>** from
> `docs/specs/2026-08-26-collaboration-groups-impl.md`. Read **only** that
> stage's brief below, plus the design `2026-08-26-collaboration-groups.md` for
> context. Work in your isolated worktree; stay inside the listed file surface;
> meet the acceptance gates; respect out-of-scope. Keep the tree green
> (`make verify-fast`). Signal completion with `wd job done --summary '…'` when
> your PR is up and CI is green.

---

## 2. Track C — Cockpit UI

Units: **U-C1**, **U-C2** (subagents); **U-C3** (2-job pipeline: C3a, C3b);
**U-C4** (subagent). See §1 for types/deps.

### C1 — TUI: hide Approvals when zero  · *subagent*
- **Goal:** `secApprovals` header/section emitted only when
  `len(recognizedApprovals) > 0`.
- **Files:** `internal/tui/control_pane.go` (the section-build around the
  `recognizedApprovals(m.approvals)` / `secApprovals` append, ~L152–155).
- **Acceptance:** `control_pane_test.go` — no Approvals row when approvals empty;
  count row + expansion unchanged when non-empty; `firstEntityCursor` still
  lands on the first entity.
- **Out of scope:** any other section.

### C2 — TUI: Agents & Terminals nested frames  · *subagent*
- **Goal:** wrap Agents rows and Terminals rows in two bordered/titled inner
  frames inside the control frame; each scrolls/collapses independently.
- **Files:** `internal/tui/boxes.go`, `internal/tui/compositor.go`,
  `internal/tui/control_pane.go` (view/routing only; the `item` model is
  unchanged — rows route into the owning frame).
- **Acceptance:** box/compositor tests for the nested layout; Agents/Terminals in
  separate frames; Approvals (when present) a banner above them.
- **Out of scope:** pipeline nesting (C3); web.

### C3a — TUI: conditional human-Pipelines section + render rule  · *pipeline job (U-C3)*
- **Goal:** stop emitting `secPipelines` as an always-present peer. Render a
  **conditional** top-level `Pipelines` banner holding **only orchestrator-less
  (human) pipelines**, shown only when ≥1 (same rule as Approvals). Establish the
  agent-tree render rule so a pipeline *can* appear as a node with job children
  (`(deps: …)` annotated) — even if no delegated pipelines exist yet.
- **Files:** `internal/tui/control_pane.go` (`secPipelines` build ~L163–165, agent
  tree build ~L170–172, `pipelineAgents()` ~L301), `internal/tui/pipeline_view.go`.
- **Acceptance:** Pipelines section hidden at zero human pipelines; a human
  pipeline appears top-level; render rule renders a pipeline node + job children
  with deps; collapse-completed + cancel affordance preserved.
- **Out of scope:** the delegated (owner-linked) nesting — that is C3b.

### C3b — TUI: nest delegated pipelines under their orchestrator  · *pipeline job (U-C3), gated on A3*
- **Goal:** render a **delegated** pipeline (one carrying an owner link from A3)
  nested under its owning orchestrator in the Agents tree, as a sibling of that
  orchestrator's subagents.
- **Depends on:** C3a (render rule) **and A3** (owner link on the pipeline
  record).
- **Files:** `internal/tui/control_pane.go` (agent-tree grouping).
- **Acceptance:** a delegated pipeline nests under its orchestrator with job
  children; a human pipeline still renders top-level (C3a) — the two coexist.
- **Out of scope:** creating delegated pipelines (A4).

### C4 — Web: new Agents tab  · *subagent*
- **Goal:** add an `agents` fixed tab mirroring the Terminals tab's master-detail
  (agent list left, focused agent right).
- **Files:** `web/src/lib/tabs.ts` (add `'agents'` to `FIXED_TABS`),
  `web/src/lib/router.ts`, new `web/src/components/AgentsTab.tsx` (list reuses the
  `kind`-filtered agents list from `web/src/lib/kind.ts`; detail reuses
  `web/src/components/AgentTab.tsx`), `web/src/styles/app.css`.
- **Acceptance:** `tabs.test.ts` asserts `FIXED_TABS` includes `'agents'`;
  selecting an agent opens it on the right.
- **Out of scope:** the web `pipelines` tab — it **stays as-is** in v1 (§6.1);
  do not touch it.

---

## 3. Track B — Collaboration groups  · *pipeline · 8 jobs (U-B)*

One warden-managed pipeline. Jobs B1/B2 run in parallel; the rest sequence per
the §1 deps. Each job is a fresh short-lived worker.

### B1 — Group store + model
- **Goal:** new `internal/groupstore` (ScrivaDB, mirroring
  `internal/schedule/store.go` / `internal/backendstore/store.go`). Record =
  `{name, members:[{agent_id, project_key, summary, joined_at}]}` — **lean only**,
  never transcripts/logs (avoids >64KB `ReadAt` / index-corruption).
- **Files:** `internal/groupstore/store.go` + `_test.go`; register in the daemon
  store set (as `schedule`/`backendstore` are wired in `cli/daemon.go`).
- **Acceptance:** CRUD + persistence-across-restart test; record stays small.
- **Out of scope:** API/CLI/MCP.

### B2 — Project identity helper (git-remote key)
- **Goal:** normalize a repo's canonical git remote URL → a stable project key
  (scheme/host/`.git`/trailing-slash/case), so two worktrees of one repo map to
  one key.
- **Files:** a helper near the existing git plumbing
  (`internal/daemon/strict_git.go` reads remotes; add normalization) + tests.
- **Acceptance:** the design §4.1 pairs collapse correctly; a local-only repo
  (no remote) → a **`local:` fallback key** from its path/root (per §6.4), tagged
  so the future hub can treat it distinctly — **not** rejected.
- **Out of scope:** everything else.

### B3 — join/leave core (daemon + API, openapi-first)  · *←B1,B2*
- **Goal:** `POST /api/v1/collaborate/groups/{name}/join` + `/leave`. Join:
  create group if absent, enforce **one orchestrator per project** via B2 key
  (dup → `409` returning the seated agent), switch caller to `orchestrator` role
  (reuse `internal/cli/role.go` / daemon role handler). Leave: remove the seat.
- **Files:** `internal/daemon/apidocs/openapi.yaml` (+ `make generate`; not a
  streaming route → no `oapi/config.yaml` exclude), daemon handler,
  `internal/groupstore`, `internal/client/client.go`.
- **Acceptance:** join seats+creates+flips role; dup join 409 w/ incumbent; leave
  removes seat.
- **Out of scope:** introductions, summary, CLI/MCP, notifications.

### B4 — Warden-brokered introductions  · *←B3*
- **Goal:** on join, warden sends each existing member a templated intro of the
  joiner and sends the joiner the reciprocal roster — reusing the directed-message
  path (`internal/mcp/tools_extra.go` `send_message` / daemon inbox). Zero agent
  tokens.
- **Files:** daemon join handler; messaging/inbox path.
- **Acceptance:** N existing members each get one intro; joiner gets N. (Summary
  may be a placeholder until B5.)
- **Out of scope:** summary resolution.

### B5 — Project summary resolution  · *←B4*
- **Goal:** resolve each member's one-line summary — declared blurb (config field,
  or `## Summary` / first paragraph of `CLAUDE.md`/`README.md`) → else ask the
  joining agent once → **cache** on the record.
- **Files:** join handler + summary resolver; `groupstore` cached `summary`.
- **Acceptance:** precedence (declared beats generated); cache hit on rejoin.
- **Out of scope:** none major.

### B6 — Leave-vs-terminate semantics  · *←B3*
- **Goal:** leave = **soft** (notify peers "no new inbound"; in-flight replies
  still route by agent-id). Terminate of a grouped orchestrator = **hard**:
  confirmation/friction gate naming the group(s) + outstanding received-
  delegations, and notify requesters of abandoned work.
- **Files:** leave handler; `terminate_agent` path (`internal/mcp/tools_extra.go`
  + daemon lifecycle) — check membership, gate, emit abandonment notices.
- **Acceptance:** leave notifies peers + replies still deliver; terminating a
  grouped orchestrator requires confirm + emits notices.
- **Out of scope:** deep delegation-state tracking (v1 minimal).

### B7 — Recover auto-rejoin  · *←B3*
- **Goal:** on `recover_agents`, a recovered orchestrator auto-rejoins its groups
  and re-announces (group durable; membership live).
- **Files:** recovery path (daemon/lifecycle) + `groupstore`.
- **Acceptance:** restart test — memberships restored + re-announced.
- **Out of scope:** none.

### B8 — CLI + MCP surface + docs  · *←B4,B5,B6*
- **Goal:** `wd collaborate group <name> join|leave` (cobra), MCP
  `collaborate_group` ({group, action}), CLI/MCP parity. Run `make gendocs`.
- **Files:** `internal/cli/`, `internal/mcp/tools_extra.go`,
  `internal/client/client.go`, generated `reference/cli.md`.
- **Acceptance:** parity test; `make gendocs-check` green.
- **Out of scope:** none.

---

## 4. Track A — Pipeline-as-substrate & monitoring  · *pipeline · 5 jobs (U-A)*

One warden-managed pipeline. A1/A2/A3 run in parallel; A4←A3, A5←A2.

### A1 — Worker completion done-signal
- **Goal:** launch workers with a done-signal contract — **both** a `wd job done
  --summary '…'` subcommand (primary, deterministic) **and** a sentinel line
  `<<WARDEN_DONE>>{json}` (backstop for backends that can't run a command
  mid-task) the lifecycle watches. Warden captures status **and** summary in one
  shot, no extra turn. (Reuse/extend `emit_pipeline_output` if it fits.)
- **Files:** `internal/cli/` (new/extended `job done`), `internal/pipeline/
  pipeline.go` + `store.go`, `internal/lifecycle` (signal capture).
- **Acceptance:** a worker emitting the signal marks its job complete with summary;
  no interrogation turn spent.
- **Out of scope:** stuck detection, delegation.

### A2 — Stuck detection (pane classifier + watchdog)
- **Goal:** reuse the existing pane-reading machinery (behind auto-approve /
  trust-prompt / rate-limit handling) to classify each worker pane as `working` /
  `idle-at-prompt` / `blocked-on-approval` / `exited` (zero-token); add a
  wall-clock watchdog → `possibly-stuck` notification.
- **Files:** the pane-state classifier (locate the auto-approve/rate-limit pane
  reader in `internal/lifecycle` or `internal/daemon`), watchdog wiring.
- **Acceptance:** blocked/exited detected free; watchdog fires + notifies on a
  no-state-change hang.
- **Out of scope:** ambiguous-idle self-report (A5); delegation.

### A3 — Pipeline → owning-orchestrator link
- **Goal:** add an owner/parent link on the pipeline record so a delegated
  pipeline knows its orchestrator — unblocks C3b nesting and A4's push target.
  (Mirror how `ParentID` was threaded for agents.)
- **Files:** `internal/pipeline/pipeline.go` + `store.go`; `create_pipeline` API
  (`openapi.yaml` + `make generate`) + MCP (`internal/mcp`).
- **Acceptance:** `create_pipeline` with an owner records it; TUI can group by it.
- **Out of scope:** push/wait mechanics (A4).

### A4 — Delegated monitoring (push/wake)  · *←A3*
- **Goal:** an orchestrator delegates a plan with explicit **callback points**;
  warden runs the mechanical steps and **wakes** the orchestrator via a directed
  message at each callback + on completion; the orchestrator blocks on
  `wait_for_message`, spending tokens only at decision points. Pure DAG = zero
  callbacks (one wake at the end).
- **Files:** `internal/pipeline` engine, messaging/inbox, MCP — **extend
  `create_pipeline`** with optional callback points + owner subscription
  (openapi-first; no separate `delegate` tool, per §6.2).
- **Acceptance:** the orchestrator receives wakes on job completion / decision
  points **without polling**; no wake between callbacks.
- **Out of scope:** autopilot internals (autopilot stays the separate fully-
  autonomous level).

### A5 — Ambiguous-idle self-report fallback  · *←A2*
- **Goal:** the one case pane-state can't disambiguate — an interactive agent idle
  at its prompt (done vs. waiting-for-human). On the **transition** to idle,
  warden asks **once** for `{status, details, summary}` — never on a loop.
- **Files:** pane watcher + a single status-query path.
- **Acceptance:** exactly one query fires on the idle transition; no repeated
  polling.
- **Out of scope:** none.

---

## 5. Cross-cutting conventions (every job)

- **API is spec-first:** edit `internal/daemon/apidocs/openapi.yaml` then
  `make generate`; never hand-write handlers/DTOs. (No streaming routes here.)
- **CLI reference is generated:** after any cobra `Use`/`Short`/`Long`/flag
  change, run `make gendocs` + commit; CI (`make gendocs-check`) gates it.
- **The rail has no bypass:** one broken tree file blocks all commits/pushes; keep
  each job green (`make verify-fast`).
- **MCP/CLI parity:** any new capability gets both a CLI command and an MCP tool.
- **Completion signalling:** each worker ends with `wd job done --summary '…'`
  (A1) so warden closes the job without interrogating it.

### Definition-of-Done — per feature (repo `CLAUDE.md`)
Walk when each *feature* (not each job) completes:
- **Tag & release** — one tag per feature; confirm before pushing `v*`. Suggested:
  **collaboration-groups** (U-B) = minor; **pipeline-substrate+monitoring** (U-A)
  = minor; UI jobs fold into whichever feature they serve (C1–C3 with the pipeline
  feature; C4 a small patch).
- **Docs** — `README.md`, `docs/FEATURES.md` (+ root matrix), `docs/USAGE.md`,
  the two spec docs, website (`site/` guide + generated `reference/cli.md`).
- **Skill** — `skills/warden/`: join/leave groups; delegated monitoring (delegate
  + `wait_for_message` instead of polling).

---

## 6. Resolved decisions (settled before kickoff, 2026-08-26)

1. **Web `pipelines` tab** — **keep it as-is for v1.** C4 only *adds* the Agents
   tab; the standalone Pipelines tab is untouched. Full web parity (folding
   pipelines under agents) is revisited in a later pass once the TUI shape is
   validated.
2. **Delegated-monitoring surface** (A4) — **extend `create_pipeline`** with
   optional callback points + owner subscription; **no** separate `delegate`
   tool. Delegation is "a pipeline that wakes its owner," reusing the engine and
   openapi surface.
3. **Done-signal** (A1) — **both.** `wd job done --summary` is the primary
   deterministic signal; a `<<WARDEN_DONE>>{json}` sentinel line is the backstop
   for backends that can't run a command mid-task.
4. **Local-only repos in groups** — **allowed with a local fallback key.** A repo
   with no remote joins using a local path/root key. Design §4.1 is updated
   accordingly. **Caveat for the future hub:** a local key is not
   portable/cross-machine — B2 must tag fallback keys as `local:` so the
   hub/cluster work can treat them distinctly later.
