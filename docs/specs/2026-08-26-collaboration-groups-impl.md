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
| **U-C — TUI projects-first** | **pipeline · 3 jobs** | C2 projects-frame+rename (←B2) · C3 pipeline-placement (←C2,A3) · C5 open-project+auto-orch (←C2,B2,B3) | **C2←B2; C3←A3; C5←B2,B3** |
| **U-C1 — TUI remove approvals** | **subagent** | — | none |
| **U-C4 — Web agents tab** | **subagent** | — | none |

**Cross-track edges (Track C now depends on B and A):** `C2←B2`,
`C3←{C2,A3}`, `C5←{C2,B2,B3}`. `C1` and `C4` are fully independent. The
needs-input→idle correctness fix rides in **A2** (Track A), not Track C.

### Autopilot launch plan

Autopilot starts everything dep-free at once; the DAGs self-sequence:

- **Launch immediately (parallel):** the **U-B pipeline**, the **U-A pipeline**,
  and subagents **U-C1 / U-C4**.
- **Track C pipeline gates:** `C2` starts after **B2** merges; `C3` after
  **C2 + A3**; `C5` after **C2 + B2 + B3**.
- Within each pipeline, no-dep jobs run in parallel (B1‖B2; A1‖A2‖A3); the rest
  fall in behind their deps.

Shape: two long pipelines (groups, monitoring) plus a Track-C pipeline that
leans on B2/B3/A3, beside two quick independent UI subagents (remove-approvals,
web-agents-tab). The manager stays idle/cheap between worker completions.

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

Projects-first restructure (design §6, revised 2026-08-26). Units: **U-C1**,
**U-C4** (independent subagents); **U-C** pipeline (C2 → C3, C5 — internal +
cross-track deps). See §1 for types/deps.

Cross-track deps: **C2 ← B2** (project-key normalizer); **C3 ← C2, A3**
(pipeline owner link); **C5 ← C2, B2, B3** (project key + orchestrator
spawn/dup). The needs-input→idle correctness fix lives in **Track A (A2/A5)**,
not here.

### C1 — TUI: remove the Approvals section entirely  · *subagent*
- **Goal:** delete `secApprovals` from the control tree — there is no top-level
  Approvals list. Pending approvals surface only via an agent node's existing
  **needs-input** status (the classifier refinement is A2/A5, not this task).
- **Files:** `internal/tui/control_pane.go` (remove the `recognizedApprovals` /
  `secApprovals` build, ~L152–155, and any Approvals-only helpers/rotation refs).
- **Acceptance:** `control_pane_test.go` — no Approvals section/row ever emitted;
  `firstEntityCursor` lands on the first agent under the first project.
- **Out of scope:** the needs-input status semantics (Track A); other sections.

### C2 — TUI: Projects frame + Terminals frame (+ dir→Project rename)  · *pipeline job (U-C), ←B2*
- **Goal:** replace the flat Agents frame with a **Projects** frame that groups
  agents under their project node (project-key from B2's normalizer; multiple
  worktrees of one repo collapse to one node), top-level agents listed directly
  with subagents/pipelines nesting beneath each. Keep Terminals a flat,
  non-project-scoped frame. Both are bordered/titled inner frames inside the
  control frame. Rename the **dir** vocabulary to **Project** across TUI titles,
  headers, hints, help. Rotation: `M-a` cycles Projects, `M-p` retired.
- **Depends on:** **B2** (project-key normalizer).
- **Files:** `internal/tui/control_pane.go`, `internal/tui/boxes.go`,
  `internal/tui/compositor.go` (view/routing + grouping; `item` model unchanged),
  plus hint/help/header strings.
- **Acceptance:** box/compositor tests for the nested Projects/Terminals layout;
  agents grouped by project; two worktrees of one repo → one project node;
  Terminals unchanged; no "dir" wording remains in TUI strings.
- **Out of scope:** pipeline placement (C3); Open Project panel (C5); web.

### C3 — TUI: remove Pipelines section + place pipelines in the Projects frame  · *pipeline job (U-C), ←C2, A3*
- **Goal:** delete `secPipelines` (no top-level Pipelines home). Render every
  pipeline **inside the Projects frame**: a **delegated** pipeline (owner link
  from A3) nests under its owning orchestrator; a **human/orchestrator-less**
  pipeline renders directly under its **project node** (sibling to that
  project's agents). Pipeline node shows job children, each `(deps: …)`
  annotated; collapse-completed + cancel affordance preserved.
- **Depends on:** **C2** (Projects frame) **and A3** (owner link on pipeline).
- **Files:** `internal/tui/control_pane.go` (`secPipelines` build ~L163–165,
  project-tree grouping, `pipelineAgents()` ~L301), `internal/tui/pipeline_view.go`.
- **Acceptance:** no top-level Pipelines section; a delegated pipeline nests under
  its orchestrator; a human pipeline renders under its project node; job children
  + deps render; collapse/cancel preserved.
- **Out of scope:** creating delegated pipelines (A4).

### C5 — TUI: Open Project panel (`o`) + auto-spawn orchestrator  · *pipeline job (U-C), ←C2, B2, B3*
- **Goal:** repurpose `o` to take over the whole control/project pane with an
  **Open Project** panel: (1) a persisted **recent-projects** list; (2) **open
  local** via a directory navigator (reuse existing dir nav); (3) **open via
  git** — clone into `~/.warden/workspace/<project>` (disambiguate name
  collisions). Opening any project **auto-spawns its orchestrator**, enforcing
  one-per-project via B3 (existing ⇒ focus, not error). `Esc` returns to the
  Projects view.
- **Depends on:** **C2** (the pane it takes over), **B2** (project key), **B3**
  (orchestrator spawn + dup→focus).
- **Files:** new `internal/tui/open_project.go` (panel + navigator + clone),
  `internal/tui/control_pane.go` (`o` binding routes to the panel), a lean
  `internal/projectstore` (ScrivaDB, recent list) or a group-store extension.
- **Acceptance:** `o` opens the panel; recent list persists across restart; local
  open registers + spawns orchestrator; git open clones into the workspace then
  spawns; re-opening a live project focuses its orchestrator (no dup).
- **Out of scope:** group membership (`collaborate group`, B8); web.

### C4 — Web: new Agents tab  · *subagent*
- **Goal:** add an `agents` fixed tab mirroring the Terminals tab's master-detail
  (agent list left, focused agent right). Web stays flat/tab-based this wave —
  the Projects model is TUI-only (design §6.3).
- **Files:** `web/src/lib/tabs.ts` (add `'agents'` to `FIXED_TABS`),
  `web/src/lib/router.ts`, new `web/src/components/AgentsTab.tsx` (list reuses the
  `kind`-filtered agents list from `web/src/lib/kind.ts`; detail reuses
  `web/src/components/AgentTab.tsx`), `web/src/styles/app.css`.
- **Acceptance:** `tabs.test.ts` asserts `FIXED_TABS` includes `'agents'`;
  selecting an agent opens it on the right.
- **Out of scope:** the web `pipelines` tab — it **stays as-is** in this wave;
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
  wall-clock watchdog → `possibly-stuck` notification. **Correctness fix (drives
  the TUI, since C1 removes the Approvals section):** an agent that has *finished
  its task and sits at an empty prompt* must classify as **idle/done**, NOT
  `needs-input`/`blocked` — today it wrongly reports `needs-input` when simply
  done. `needs-input` must mean a genuine pending prompt only.
- **Files:** the pane-state classifier (locate the auto-approve/rate-limit pane
  reader in `internal/lifecycle` or `internal/daemon`), the status field the TUI
  renders as `needs-input`, watchdog wiring.
- **Acceptance:** blocked/exited detected free; watchdog fires + notifies on a
  no-state-change hang; a done-but-idle agent reports idle/done, never
  `needs-input`.
- **Out of scope:** ambiguous-idle self-report (A5); delegation.

### A3 — Pipeline → owning-orchestrator link
- **Goal:** add an owner/parent link on the pipeline record so a delegated
  pipeline knows its orchestrator — unblocks C3 nesting and A4's push target.
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
  = minor; **projects-first-cockpit** (U-C: C1/C2/C3/C5 — projects frame, pipeline
  placement, Open Project panel) = minor; **web-agents-tab** (C4) = small patch.
- **Docs** — `README.md`, `docs/FEATURES.md` (+ root matrix), `docs/USAGE.md`,
  the two spec docs, website (`site/` guide + generated `reference/cli.md`).
  Projects-first: document the Project vocabulary, the Open Project panel (recent
  / local / git-clone-into-`~/.warden/workspace`), and that opening auto-spawns
  the orchestrator.
- **Skill** — `skills/warden/`: join/leave groups; delegated monitoring (delegate
  + `wait_for_message` instead of polling); Open Project + one-orchestrator-per-
  project.

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

*Projects-first TUI (added 2026-08-26):*

5. **Projects replace Agents as the first-class TUI grouping** — "dir" is renamed
   **Project**; the control frame holds a **Projects** frame (agents grouped by
   project-key) + an unchanged **Terminals** frame. The **Approvals and Pipelines
   top-level sections are removed** (not made conditional). Pipelines render only
   inside the Projects frame (delegated → under owner; human → under project).
6. **Approvals surface via `needs-input`, not a section** — no new TUI mechanism.
   The correctness fix (a done-but-idle agent must show idle/done, never
   `needs-input`) is implemented in **A2** (pane classifier), not Track C.
7. **Open Project (`o`) auto-spawns the project orchestrator** — `o` is a
   full-pane panel (recent / open-local / open-via-git-clone-into
   `~/.warden/workspace/<project>`); opening a project spawns its single
   orchestrator (one-per-project via B3; existing ⇒ focus). Web stays flat/tab-
   based this wave (design §6.3).
