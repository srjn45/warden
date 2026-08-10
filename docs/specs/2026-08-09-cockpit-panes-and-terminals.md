# Cockpit Panes & First-class Terminals — Design Spec

**Date:** 2026-08-09
**Status:** Draft — design review.

This spec renames the three TUI cockpit panes to describe their purpose,
promotes **terminals** to a first-class entity (removing `terminal` as an agent
*backend*), and reworks how you open and switch between agents and terminals. The
control pane becomes a four-section navigator (Approvals · Pipelines · Agents ·
Terminals) feeding two viewports; a dedicated terminal viewport replaces the old
dual-role master/shell pane and the `--repl` TUI flavor.

---

## 0. Design principles

1. **Entities feed viewports.** The control pane lists *entities* (approvals,
   pipelines, agents, terminals). The other two panes are *viewports*: one shows
   the currently-focused **agent**, one shows the currently-focused **terminal**.
   A pane's job is singular — this is what dissolves the old master pane's
   shell/REPL/toggle triple-duty.
2. **Reclassify, don't rebuild.** A terminal is mechanically almost identical to
   an agent session (a tracked tmux pane running a process). Terminals reuse the
   existing session lifecycle/recovery/persistence via a `Kind` discriminator —
   they are *not* a new store or subsystem. "Terminal" simply stops being an
   agent *backend*.
3. **Don't restrict spawning; render honestly.** The `dir > agent > subagent`
   tree invariant is achieved by a *rendering* rule (same-project children nest;
   cross-dir children surface under their own dir with a lineage backlink), not
   by constraining what a user/orchestrator may spawn. Cross-dir orchestration
   has first-class homes already: **pipelines** and **peer messaging**.
4. **The daemon owns state; clients are thin.** Terminals, like agents, live in
   the session store and are exposed over the same API so TUI, web, and Android
   behave identically.

---

## 1. Goal & scope

### In scope

- **Rename** the three cockpit panes: `list → control`, `master → terminal`,
  `detail → agent` (internal names, internal CLI flags, doc comments, health
  checks, user-facing headers, web-cockpit mirror).
- **Terminals as a first-class entity:** a `Kind` (`agent`|`terminal`)
  discriminator on the session; `terminal` **removed from the backend registry**;
  a "new terminal" path distinct from "new agent".
- **Control-pane tree** restructured into four fixed sections: Approvals,
  Pipelines, Agents (dir-grouped, subagents nested n-deep), Terminals.
- **A default terminal** in the launch cwd, shown in the terminal pane at
  startup; removal of the old master shell, the `M-t` shell-toggle machinery, and
  the **`--repl` TUI flavor** (the standalone `wd repl` command is unaffected).
- **Open routing:** agents open in the agent pane, terminals in the terminal
  pane. `t` in the control pane creates/focuses a terminal in the opened agent's
  dir (or `~`), via a `(c)reate / (f)ocus` choice.
- **Terminal naming** that is stable + informative: `<index>. <repo>:<rel>/ (<branch>)`.
- **Rotation hotkeys** (global, Alt-based): `M-t` terminals, `M-a` all agents,
  `M-p` pipeline agents (pipeline > agents order).
- Backend-registry removal ripple: `GET /api/v1/backends`, CLI `--backend` help,
  docs/skill, and **coordination with the Android app** (wd-app) whose picker
  consumes `/backends`.

### Out of scope (so the first build stays reviewable)

- A separate terminal *store* or terminal-specific API surface beyond the `Kind`
  filter (terminals ride the session endpoints).
- Making terminals participate in cost/savings/state/approvals (they never do —
  they are filtered out of those surfaces; see §3).
- Multiplexing more than one agent and one terminal *visible* at once (still a
  fixed 3-pane layout; rotation swaps which entity each viewport shows).
- Full web-cockpit feature parity in the *same* PR as the TUI (scoped as its own
  stage, §12 — but explicitly owned, not dropped).

---

## 2. Panes — rename & roles

| Today (name) | New name | Role | tmux position |
|---|---|---|---|
| `list` (`listPaneModel`) | **control** | Navigator/picker: Approvals · Pipelines · Agents · Terminals + overlays | top-left |
| `master` (`masterID`) | **terminal** | Viewport for the currently-focused *terminal* | bottom-left |
| `detail` (`detailID`) | **agent** | Viewport for the currently-focused *agent* | full-height right |

Layout is unchanged (fixed three panes); only the *identity and contents* of the
two viewports change. The rename touches:

- Internal: `listPaneModel`, `masterID`/`detailID`, `buildCockpit`,
  `healthyCockpit` pane-shape assertions, `@warden_shell_pane` (removed, §5).
- Internal CLI flags (warden-spawned only, no external callers — safe to rename):
  `--pane=list → --pane=control`, `--detail-pane → --agent-pane`, plus a new
  `--terminal-pane` (§6).
- User-facing: pane headers, footer key hints.
- The **web cockpit** (`warden-web-cockpit`) mirror.

---

## 3. Terminals as a first-class entity (reclassify)

### Data model

Add a discriminator to `store.Session` (`internal/store/types.go`):

```go
type SessionKind string

const (
    KindAgent    SessionKind = "agent"    // default; empty string == agent (back-compat)
    KindTerminal SessionKind = "terminal"
)

// Session gains:
Kind SessionKind `json:"kind,omitempty"` // empty ⇒ agent (no migration needed)
```

A terminal session:
- runs `${SHELL:-bash}` (what the old `terminal` backend's `LaunchCmd` did),
- has `Backend == ""` (it is **not** a backend anymore),
- never seeds a prompt, never parses approvals, has no transcript/state/cost.

### `terminal` leaves the backend registry

- Delete `internal/agentbackend/backends/terminal.go`'s registration; the id
  `terminal` no longer appears in `agentbackend.IDs()`, the `--backend` picker,
  or `GET /api/v1/backends`. (See §9 for the app coordination this requires.)
- The shell-launch knowledge it held moves to the terminal-spawn path (a tiny
  helper: terminals always run `${SHELL:-bash}`, non-`exec` so the lifecycle's
  exit-capture still fires on `exit`).

> **Sequencing note (2026-08-09, during build).** The backend removal is
> **deferred out of stage 2 into stage 6** — see the amended §12. Reason:
> `agentbackend.Detect()` enumerates the in-memory registry (`IDs()`) and the
> daemon feeds that into `backendstore.Reconcile` (`cli/daemon.go`), which is
> what `GET /api/v1/backends` serves. So deleting the `terminal` backend is
> **inseparable from the `/backends` contract change** — the very thing §9 says
> to coordinate with wd-app and ship atomically with `kind=terminal` support and
> the capability flag. Stage 2 therefore stays **purely additive** (the `Kind`
> discriminator + the AI-surface filters below), leaving the terminal backend
> registered and working; stage 6 removes it from *both* registries
> (`agentbackend` + `backendstore`), flips the `/backends` test, adds the
> `kind` create/list filter, and advertises the capability flag as one
> coordinated unit. No release is cut between stages, so the app never sees a
> half-changed contract.

### Filtering AI-centric surfaces (the honest cost of reclassify)

Everywhere that aggregates or reasons about *agents* must exclude
`Kind==KindTerminal`. Known sites to gate with `WHERE kind != terminal`:

- listing/metrics: `get_metrics`, `savings`, `spend`, `insights`, `digest`
- state/approvals: state detection, approvals inbox, auto-approve
- naming/summarize/classify offload (terminals have no transcript)

A terminal **is** still a real tracked tmux session, so it participates in:
recovery (`recover_agents`), attach (`a`), persistence, and the collaboration
daemon's file-conflict awareness (a shell can edit files too).

---

## 4. Control-pane tree

Four fixed top-level sections, in order:

```
Approvals            (n pending)          ← was an overlay; now a persistent section
Pipelines
  <pipeline>
    <agent> …
Agents
  <dir>
    <agent>
      <subagent> …                        ← n-level, same-project lineage
Terminals
  1. warden:site/ (main)
  2. warden-android-app:app/ (feature/x)
```

- **Approvals** moves from today's overlay to a persistent, collapsible section
  showing the pending count and expanding to the items.
- **Agents** keeps today's dir-grouping and `parent_id` nesting, combined by the
  rule in §4.1.
- **Terminals** is new; naming in §7.

### 4.1 Subagent nesting rule (the resolved design question)

Two groupings compete: **directory/project** (`dir > agent`) and **orchestration
lineage** (`child.parent_id`: `agent > subagent`). They only coexist cleanly when
lineage stays within a project. Note warden already gives each agent its **own
isolated worktree**, so "same dir" means *same project root*, not same path.

Resolution — a **render rule, not a spawn restriction**:

- A child in the **same project** as its parent → nests under it: `dir > agent >
  subagent`, n-deep.
- A child in a **different project** → surfaces under *its own* dir as a normal
  agent, with a lightweight `↳ from <parent>` backlink so lineage is still
  visible and the dir grouping stays truthful.
- **Idiomatic cross-dir work** is a **peer agent + messaging** or a **pipeline**
  (pipelines are the natural home for ordered, cross-project orchestration) —
  not cross-dir nesting. This matches how warden is already used (independent
  top-level agents in different repos coordinating over the inbox).

No spawn-time constraint is added; an orchestrator may still spawn anywhere.

---

## 5. Default terminal; retire master pane, `M-t`, and `--repl` TUI

- **Startup:** create one `KindTerminal` session in the launch cwd, list it under
  Terminals, and open it in the **terminal** pane. This replaces the old master
  default shell.
- **Removed:** the master pane's dual role, `shellToggleScript`, the
  `@warden_shell_pane` user-option, and the `M-t` swap binding — their only job
  ("get a scratch shell") is now `t` / `M-t`-rotate over real terminal entities.
- **`--repl` TUI flavor removed:** delete `o.useRepl` and the TUI `--repl`
  option/path that ran `wd repl` in the master pane. **The standalone `wd repl`
  command is unchanged** — only the cockpit flavor goes away.

---

## 6. Open routing & pane targeting

Today the agent (detail) pane is the single hardcoded open target; opening is
`respawn-pane -k -t <pane> "env -u TMUX tmux attach -t <session>"`. We generalize
to two targets:

- Thread **both** pane ids into the control pane: `--agent-pane=<id>`
  (renamed from `--detail-pane`) and `--terminal-pane=<id>` (new), wired in
  `buildCockpit` the same right-to-left capture used today.
- The Enter handler branches on the selected entity's kind:
  - **Agent** (`Kind==agent`, live) → respawn into the **agent** pane
    (unchanged behavior).
  - **Terminal** (`Kind==terminal`, live) → respawn into the **terminal** pane.
  - Non-live agents → stored-detail render in the **agent** pane (unchanged).
- The control pane tracks **what is currently open in the agent pane**
  (`openedAgent`) so `t` (§6.1) can read its dir. This is new state; today the
  respawn is fire-and-forget.
- Focus: after opening/rotating a **terminal**, `select-pane` onto the terminal
  pane (terminals are interactive — you want to type). Opening/rotating an
  **agent** keeps focus in control (watch-mode), matching today.

### 6.1 `t` — terminal in the opened agent's dir

`t` in the control pane opens a terminal whose cwd is:

- the dir of the agent currently open in the **agent** pane, else
- `~` if the agent pane is empty.

It prompts a small inline choice: **`(c)reate`** a fresh terminal in that dir, or
**`(f)ocus`** an existing terminal already in that dir. If none exists in that
dir, `f` falls back to create (or is greyed out).

---

## 7. Terminal naming

Format: **`<index>. <repo>:<rel>/ (<branch>)`** — e.g. `2. warden:site/ (main)`.

- **`<index>`** — a stable per-cockpit ordinal assigned once at create; it never
  changes as cwd changes, anchoring the `M-t` rotation and giving a fixed handle.
- **`<repo>:<rel>/`** — repo-relative path: `<repo-basename>:<path-from-repo-root>`.
  Disambiguates the "two `src/`" collision that plain basename hits; empty rel
  renders as the repo root (`warden:/` or just `warden`).
- **`(<branch>)`** — current git branch of the terminal's cwd (worktree-aware).

Mechanics (poll on the existing refresh tick — no shell hooks):
- cwd from tmux `#{pane_current_path}` (updates live on `cd`),
- repo root / branch via git on that cwd (`rev-parse --show-toplevel`,
  `--abbrev-ref HEAD`); branch changes rarely (a `git checkout`) but is polled
  too rather than left stale,
- outside any git repo: fall back to `<index>. <abs-or-~-abbreviated path>`.

Names update live in the Terminals section as the shell `cd`s.

*(Alternatives considered and folded in as future options: a user-pinned label
overriding the auto name; swapping in `#{pane_current_command}` when a non-shell
command is in the foreground, e.g. `2. ▶ npm run dev`. Not in the first build.)*

---

## 8. Switching — global Alt rotation

Each key rotates a **viewport** (not just the tree cursor), globally (works from
any pane, so you can flip while typing). Alt is chosen over Ctrl deliberately:
`C-a`/`C-p`/`C-t` are essential readline keys (start-of-line, prev-history,
transpose) and binding them at the tmux root would clobber them inside every
shell and AI CLI. Alt-letter combos don't collide and are already the codebase's
idiom for global bindings.

| Key | Rotates | Viewport | Set (order) |
|---|---|---|---|
| `M-t` | terminals | terminal pane | all live terminals (by index) |
| `M-a` | agents | agent pane | all live agents |
| `M-p` | pipeline agents | agent pane | agents in pipelines, ordered **pipeline > agents** |

- `M-t` is exactly the binding freed by removing the old shell-toggle — no new
  key territory.
- Rotation respawns the target pane to the next entity's attach (same mechanism
  as open); the previously-shown entity keeps running in its own session.
- `M-a` and `M-p` both drive the **agent** pane, differing only in the traversal
  set. After `M-t`, focus moves to the terminal pane (§6).

---

## 9. Backend-registry removal ripple + app coordination

Removing `terminal` from the registry is a **contract change** on
`GET /api/v1/backends` (see `internal/daemon/strict_lifecycle.go` `ListBackends`
and memory `backends-list-endpoint`). Consumers:

- **CLI `--backend` help** and any static backend lists in docs/skill drop
  `terminal`.
- **The Android app (wd-app)** fetches `/backends` for its create-agent picker.
  Since "terminal" becomes a *separate action* (new terminal ≠ new agent), the
  app will want a distinct "New terminal" affordance rather than a backend row.
  **Coordinate before landing the registry removal** so their picker isn't
  surprised (this agent owns daemon+site; wd-app owns the app).
- **Interaction with the backend-registry spec** (`2026-08-06-backend-registry.md`,
  shipped): that spec lists `terminal` as a backend row shown "for completeness"
  and force-installed. This spec supersedes that: `terminal` leaves the registry
  entirely, so the backendstore/Backends screens drop the `terminal` row. Update
  that surface accordingly.

If terminals need to be listed/created over the API, they ride the existing
**session** endpoints filtered by `Kind` (a `kind=terminal` create flag / list
filter) — **not** a new endpoint.

---

## 10. Web-cockpit parity

The web cockpit mirrors the TUI layout and must gain: the four-section control
tree (incl. Terminals), a terminal viewport, `t`/create-or-focus, and rotation
equivalents. The web surface has its own layout primitives (not tmux panes), so
this is scoped as its own stage (§12) — owned, not assumed free.

---

## 11. Edge cases

- **Last terminal / always ≥1:** the cockpit always keeps at least the default
  terminal; closing the last one recreates a default in the launch cwd.
- **Terminal `exit`:** removes it from the Terminals list (terminals are
  ephemeral), unless it is the last one (§ above). An explicit close key (`x`) on
  a selected terminal does the same.
- **Terminal proliferation:** `t` with `(f)ocus` and rotation keep the list
  navigable; index-based naming keeps handles stable.
- **cwd/branch unavailable:** naming falls back to an abbreviated path (§7).
- **Cross-dir subagent:** rendered under its own dir with `↳ from <parent>` (§4.1).
- **Recovery:** terminals recover like agents (own tmux session); a recovered
  terminal re-derives its name from live `pane_current_path`.
- **Attach (`a`) unaffected:** full-screen `switch-client` attach works for a
  terminal exactly as for an agent, independent of pane routing.

---

## 12. Build stages (proposed warden pipeline)

Short-lived, bounded-context stages; mostly sequential with parallel client
stages at the end.

1. **Pane rename** — `control`/`terminal`/`agent` across `internal/tui`
   (`compositor.go`, `list_pane.go`, `healthyCockpit`), internal CLI flags
   (`cli/tui.go`), headers/hints. Pure mechanical; lands first as the shared
   vocabulary. (No behavior change.)
2. **Terminal reclassify (additive)** — `Kind` (`agent`|`terminal`) on
   `store.Session` + `IsTerminal()`; `Kind` filters excluding terminals from the
   AI-centric surfaces (§3). Purely additive: the `terminal` backend stays
   registered (its removal is inseparable from the `/backends` contract, so it
   moves to stage 6 — see §3 sequencing note). No behavior change until a
   terminal-kind session actually exists (stage 4).
3. **Control tree restructure** — four fixed, collapsible sections (Approvals ·
   Pipelines · Agents · Terminals) as selectable header rows in `internal/tui`
   (`list.go` section items + `noDirGroup`, `control_pane.go` `items()`/collapse/
   Enter, the §4.1 cross-project render rule with a `↳ from <parent>` backlink,
   Terminals split out of the Agents tree via `splitByKind`, and the §7
   `terminalDisplayName` formatter). Approvals moves from an overlay to a section
   that expands to one selectable row per prompt (Enter opens the overlay focused
   there). A fresh cockpit auto-focuses the first entity rather than a header.
   (Depends 1–2.) *Sequencing note (built 2026-08-09):* the §7 name **formatter**
   and rendering land here, but the **live-poll wiring** (deriving cwd from tmux
   `#{pane_current_path}` and branch via git on the refresh tick) belongs with the
   first real terminal-kind session, which isn't created until stage 4 — there is
   nothing to poll before then. Stage 3 renders terminal rows from the session's
   stored `Workdir`/`Repo`/`Branch`; stage 4 attaches the live poll.
4. **Panes & routing** — default terminal at startup; retire master/`M-t`/`--repl`
   TUI (§5); `--terminal-pane` + Enter routing + `openedAgent` tracking + `t`
   create/focus (§6). (Depends 1–3.) *Sequencing note (built 2026-08-09):* terminals
   are created via the **existing `terminal` backend** — a one-line bridge in
   `lifecycle.Spawn` sets `Kind=terminal` when `backend=="terminal"`, so **no
   `/backends` or spawn-request contract changes here** (the explicit `kind` create
   field stays in stage 6, which removes the terminal backend). The control pane
   ensures ≥1 terminal on the first session list (adopt an existing live terminal,
   else spawn one in the launch cwd) and opens it in the terminal pane without
   stealing focus; Enter/`t`/create *do* focus it. The §7 **live cwd/branch poll**
   (deferred from stage 3) is wired here: `terminalInfoCmd` reads each terminal's
   `#{pane_current_path}` + git root/branch on the refresh tick and feeds
   `terminalItems`. The **tmux-native cockpit has no terminal pane**, so
   `--terminal-pane` is empty there and terminal features (Enter-on-terminal, `t`,
   the default terminal) degrade to a status hint. The `repl` config setting and the
   standalone `wd repl` command are untouched — only the cockpit `--repl` flavor is
   gone. Global `M-t`/`M-a`/`M-p` rotation is **not** bound yet (stage 5); `M-t` is
   simply freed here.
5. **Rotation** — `M-t`/`M-a`/`M-p` global bindings + viewport rotation (§8).
   (Depends 4.) *Sequencing note (built 2026-08-09):* the three keys are bound in
   `buildCockpit` as tmux root bindings (`bind-key -n`) that **forward themselves to
   the control pane** (`send-keys -t <controlPane> M-t`), so rotation works from any
   pane while typing; the control pane owns the state and respawns the target pane.
   `M-t` cycles the terminal pane over live terminals (creation order, grabs focus);
   `M-a` cycles the agent pane over all live agents; `M-p` cycles the agent pane over
   pipeline agents (pipeline > job order), both keeping control focus (§6 watch-mode).
   New model state `openedAgent` (id) anchors the agent cycle — set on every Enter-open
   and rotate; `openedTerminal` (added stage 4) anchors the terminal cycle. `nextInCycle`
   wraps and treats an absent current as "start at the first". The **tmux-native cockpit
   does not bind these** (it leaves the user's own Alt keys intact — noted as a gap, not
   a regression); the model still handles the keys defensively, so `M-t` there degrades
   to the "no terminal pane" hint. Alt (not Ctrl) is used so shell readline keys
   (C-a/C-p/C-t) are never clobbered.
6. **Backend removal + API/app coordination** — remove the `terminal` backend
   from **both** registries (`agentbackend` init + `backendstore` special-casing)
   and the terminal-spawn helper for `${SHELL:-bash}`; drop `terminal` from
   `GET /api/v1/backends` (flip its test); add the `kind` create/list filter on
   the session endpoints; **advertise an explicit capability flag** for
   `kind=terminal` support (wd-app's chosen skew switch, with terminal-absent-
   from-`/backends` as the fallback); MCP parity if needed; ping + align with
   wd-app (§9). Bundled as one atomic contract unit. (Depends 2.)
   *Sequencing note (built 2026-08-10):* the `terminal` backend adapter moved from
   `internal/agentbackend/backends/terminal.go` (registered) to an **unregistered**
   internal adapter `internal/agentbackend/terminal.go`, reached only via
   `agentbackend.TerminalBackend()` — so `terminal` vanishes from `IDs()`/`Get()`/
   `Detect()`/`GET /api/v1/backends` while the launch path reuses the exact same
   degraded machinery (zero launch-behavior change). lifecycle resolves it through a
   new `launchBackend(sess)` seam that keys on `sess.IsTerminal()`, and a
   `kind=terminal` spawn is forced free-form (a stray Type can't route it onto the
   worktree/AI path). The `kind` field rides the existing spawn body
   (oapi.SpawnRequest → daemon.SpawnRequest → lifecycle.SpawnRequest → client.SpawnParams;
   `store.Session` is `x-go-type`, so `kind` already serialized on every row — the
   openapi Session edit is doc-only). The list filter is `GET /api/v1/sessions?kind=`.
   The capability flag is **`terminal-sessions`**, served by a new
   `GET /api/v1/capabilities` (`CapabilitiesResponse{capabilities:[]}`) — no version
   plumbed (the warden version is conveyed to wd-app directly + is in the release
   tag). backendstore prunes a stale pre-stage-6 `terminal` row on every Reconcile
   (a new `Store.Delete`). back-compat: `backend=terminal` is still accepted as an
   alias for `kind=terminal`. MCP `spawn_agent` gains a `kind` arg (terminal dropped
   from its backend enum); CLI gains `--kind` (terminal dropped from `--backend`
   help; `make gendocs` run). Prose docs (README/FEATURES/USAGE/site/skill) are the
   stage-8 sweep; this stage ships code + contract + generated `cli.md`.
7. **Web cockpit** — parity (§10). (Depends 2–5.) — parallel with 6.
8. **Docs & DoD** — README, `docs/FEATURES.md` + root `FEATURES.md`,
   `docs/USAGE.md`, site guides + `reference/cli.md` (generated), skill; tag +
   release (§13).

---

## 13. Definition-of-Done checklist (per CLAUDE.md)

- **Tag & release:** one tag for the feature (minor bump — sizable, user-facing
  cockpit change); confirm with maintainer before pushing the `v*` tag.
- **Docs:** `README.md`; `docs/FEATURES.md` + root `FEATURES.md` matrix (drop
  `terminal` from the backend list, add Terminals as a cockpit concept);
  `docs/USAGE.md`; website `guides/` + `concepts/` (panes) + `reference/cli.md`
  (via `make gendocs`); `skills/warden/` (pane names, `t`, rotation keys,
  terminal-vs-agent guidance).
- **CLI help/manual:** update any command help referencing `--repl` TUI flavor,
  `--backend` (drop `terminal`), and the internal pane flags; `make gendocs` +
  commit; `make gendocs-check` stays green.

---

## Appendix — key source anchors (as of 2026-08-09)

- Cockpit build & panes: `internal/tui/compositor.go` — `buildCockpit`
  (~L97–170), `masterPaneCmd`/`listPaneCmd`/`detailPlaceholderCmd` (~L30–62),
  `shellToggleScript` (~L72–84, to remove), `healthyCockpit` (~L228–251).
- Control-pane model & open flow: `internal/tui/list_pane.go` — `listPaneModel`
  (~L27,32), Enter case (~L828–850), `cockpitDetailCmd` (~L1290–1304),
  `respawnDetailArgs`/`openInDetailCmd` (~L1271–1281), `openJobDetailCmd`/
  `openAgentDetailCmd`, `switchClientCmd` (~L930–936, 1253–1265),
  `m.detailPane`/`selected()` (~L32,102,130).
- Item/session data: `internal/tui/list.go` — `liveStatus` (~L207),
  `item.session` (~L164–165), `buildItems`/`flatSessions` (~L215+).
- Session type (add `Kind`): `internal/store/types.go` (`Backend` ~L148).
- CLI pane flags: `internal/cli/tui.go` (`--detail-pane` parse ~L16,44;
  `RunListPane` ~L29).
- Terminal backend (to remove): `internal/agentbackend/backends/terminal.go`.
- Backends endpoint (contract change): `internal/daemon/strict_lifecycle.go`
  `ListBackends`; spec interaction: `docs/specs/2026-08-06-backend-registry.md`.
