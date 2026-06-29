# Codex Fork Superpower (Step 6, PR-B2) — Design

**Status:** 📐 Planning & design. Refines §4.2 (the `SessionForker` sketch) and
§4.4 (the daemon-crossing criteria) of the parent step-6 spec
[`2026-06-29-t1-superpowers-design.md`](./2026-06-29-t1-superpowers-design.md).
The parent doc shipped the codex **review** pilot (PR-A) CLI-local and explicitly
**deferred fork** as the one candidate that "crosses the daemon boundary and must go
spec-first if/when it's built" (parent §4.1, §4.4, §6 PR-B). This is that pass.
**Date:** 2026-06-29
**Scope:** codex `fork` ONLY — the $0-local pilot. Other backends' fork-equivalents
are out of scope and re-confirmed non-applicable (§9).

> This is a **planning & design** document. **No production Go code is changed by
> this spec.** It resolves *how* a codex fork crosses (or avoids) the daemon
> boundary with the **minimal, additive, spec-first** change, refines the
> `SessionForker` interface into a concrete signature, and proposes the PR
> breakdown. Building it is a later impl pass.

> **Guiding principle, unchanged:** *warden adds capability ON TOP of an agent; it
> never strips one.* A fork superpower is an **additive** optional, type-asserted
> interface; **Claude untouched and regression-locked**; the daemon delta is the
> smallest possible spec-first field, not a new endpoint.

---

## 1. What `codex fork` actually is (verified live, codex-cli 0.142.3)

Run on the $0 rig (`/home/srjn45/.local/bin/codex`, `codex-cli 0.142.3`):

```
$ codex fork --help
Fork a previous interactive session (picker by default; use --last to fork the most recent)
Usage: codex fork [OPTIONS] [SESSION_ID] [PROMPT]
Arguments:
  [SESSION_ID]  Conversation/session id (UUID). When provided, forks this session.
                If omitted, use --last to pick the most recent recorded session
  [PROMPT]      Optional user prompt to start the session
Options:
  --last   Fork the most recent session without showing the picker
  --all    Show all sessions (disables cwd filtering and shows CWD column)
  -m, --model <MODEL>            -s, --sandbox <SANDBOX_MODE>   -a, --ask-for-approval <POLICY>
  -C, --cd <DIR>                 -c, --config <key=value>       --enable/--disable <FEATURE>
```

**Findings (these resolve the brief's assumptions):**

1. **Fork forks the CONVERSATION, not the working tree.** `codex fork` branches a
   recorded **session rollout** (the JSONL reasoning/transcript under
   `~/.codex/sessions/<Y>/<M>/<D>/`) into a **new, divergent session** — it replays
   the prior conversation context into a fresh session id and lets you continue down
   a different path while the original is preserved. It does **not** snapshot,
   restore, or otherwise touch files on disk. The working-tree state the fork "sees"
   is simply whatever is in the cwd it is launched from.

2. **It is a real subcommand, not "resume into a new id" by accident.** It is a
   first-class verb distinct from `codex resume` (which continues the *same* session,
   cwd-scoped via `--last`). Fork explicitly *diverges*: original stays intact, fork
   gets a new id. So the brief's "is fork effectively resume-into-a-new-id?" question
   resolves to: **no — it is a native conversation-branch**, which is exactly why it
   is a superpower warden lacks (snapshot is linear; rotate/handoff drop the
   conversation — see §6).

3. **It is an interactive TUI launcher, not a non-interactive `exec` sub-form.**
   Unlike `codex exec review` (the structured review path), there is no
   `codex exec fork`. Fork opens the interactive TUI. **This is fine for warden:** a
   managed codex agent already runs the TUI inside a tmux pane, so a fork is driven by
   the exact same pane machinery as a normal codex launch (§4).

4. **Full launcher flag surface — identical to `resume`/launch.** Verified: `codex
   fork` accepts `-m/--model`, `-s/--sandbox {read-only|workspace-write|
   danger-full-access}`, `-a/--ask-for-approval`, `-C/--cd`, `-c`. This means warden's
   existing `codexSandbox()` mode mapping and `-m` model handling apply to a fork
   **byte-for-byte** the same as `LaunchCmd`/`ResumeCmd`. No new flag vocabulary.

5. **Inputs it needs:** a **source session UUID** (the `SESSION_ID` positional) — or
   `--last` — plus an optional starting **PROMPT** (trailing positional, same shape
   as the launch positional codex already takes). `--last` is **cwd-scoped**; an
   explicit `SESSION_ID` forks that session **regardless of cwd** (cwd filtering only
   governs the picker / `--last`). This last point is load-bearing (§4.3, §5).

6. **The fork mints a NEW rollout** under `~/.codex/sessions/...` whose
   `session_meta.session_id` is new and whose `session_meta.cwd` is the launch cwd —
   i.e. it behaves, for warden's transcript/discover-then-pin machinery, **exactly
   like a fresh codex session** (§5).

**Could not verify on the rig (interactive-only; flagged for impl):**
- The exact *latency* before the forked session's first rollout line is flushed to
  disk (affects how many poller ticks discover-then-pin needs). Fresh-session
  behavior suggests it is prompt, but confirm with a live capture.
- That `codex fork <uuid>` launched in a **different** cwd than the source session's
  recorded cwd accepts the explicit id without complaint (the help says an explicit
  id forks "this session" unconditionally; confirm in a tmux pane).

---

## 2. The core question: is fork even worth surfacing CLI-local? — **No.**

The parent spec's two already-shipped superpowers (`wd review`, `wd models`) are
**CLI-local**: read-only, one-shot, exec-in-the-worktree-and-print, **no daemon
change** (parent §4.1). The brief asks whether fork has an equivalent CLI-local
slice that could ship as a "PR-0".

**It does not, and that is the central finding.** A `wd fork <agent>` that merely
shelled out to `codex fork <uuid>` in the current terminal would just drop the
operator into a **codex TUI that warden does not track** — no warden agent record, no
tmux session warden monitors, no worktree warden owns, no state/approval polling, no
teardown. That adds **nothing** over the operator typing `codex fork` themselves.
The orchestrator's analysis is correct: **a codex fork is only valuable when surfaced
as a warden-MANAGED agent** — its own tmux session + worktree, with warden
tracking/monitoring/tearing it down. Review and models are read-only probes that
*return a value*; a fork *starts a long-lived agent*, and a long-lived agent that
warden doesn't manage is an anti-feature.

**Therefore fork is intrinsically daemon-crossing.** There is no useful CLI-local
PR-0. (This is the opposite conclusion from review/models, and the right one.)

---

## 3. Boundary decision: daemon-crossing, via the EXISTING spawn path

Fork crosses the daemon — but the **minimal** spec-first change is **not a new
endpoint**. Mechanically, **a managed fork *is* a managed spawn** whose launch
command happens to be `codex fork <uuid>` instead of `codex`. Everything else a fork
needs — a new agent id, a tmux session, a worktree, the store record, the
state/approval poller, discover-then-pin, teardown — is **exactly what
`POST /api/v1/spawn` already does** (`internal/lifecycle` `spawnTyped`/`spawnFreeForm`,
`internal/daemon` `SpawnAgent`). So the delta is:

> **Add one additive optional field, `fork_from`, to the existing `SpawnRequest`.**
> When set, the spawn path builds the launch command from the backend's
> `SessionForker.ForkCmd` instead of `LaunchCmd`, and bases the worktree on the
> parent agent. Unset ⇒ today's spawn, byte-identical.

This is strictly preferred over a new `/fork` endpoint because:
- It reuses the whole managed-spawn machinery (gates, rollback, audit, pressure,
  parent-link, notify) instead of re-implementing it.
- The spec-first surface is a **single new optional property** — the smallest
  possible daemon contract change.
- It composes with everything spawn already accepts (`model`, `permission_mode`,
  `tags`, `parent_id`, `name`).

### 3.1 The spec-first delta (concept — NOT applied here)

Per the spec-first rail (edit `openapi.yaml` → `make generate`, never hand-write
DTOs/handlers; `internal/daemon/apidocs/openapi.yaml` → `internal/daemon/oapi`),
the conceptual diff is a one-line schema addition:

```yaml
# internal/daemon/apidocs/openapi.yaml — components.schemas.SpawnRequest.properties
        fork_from:
          type: string
          description: >
            id of an existing agent whose recorded session this spawn should FORK
            (codex fork): the new agent branches the source agent's conversation
            into a divergent session. Requires a backend implementing SessionForker
            and a source agent whose backend session id is already pinned. Empty =
            a normal (non-fork) spawn.
```

After `make generate`, the generated `oapi.SpawnRequest` gains `ForkFrom string`,
which threads through the three existing mapping layers with **one added line each**
(no new types):

- `internal/daemon/strict_lifecycle.go` `spawnRequestFromOAPI`: `ForkFrom: b.ForkFrom,`
- `internal/daemon/lifecycle_adapter.go` `Spawn`: `ForkFrom: req.ForkFrom,`
- `internal/lifecycle/lifecycle.go` `SpawnRequest`: a new `ForkFrom string` field.

**No streaming/SSE/WS route is added**, so the
`oapi/config.yaml exclude-operation-ids` list is **untouched** (the memory'd gotcha
about new streaming routes needing the exclude list does not apply — `spawn` already
exists and stays a normal JSON route).

---

## 4. The `SessionForker` interface (refines parent §4.2)

The parent sketched `ForkCmd(sessionID, prompt string) (cmd string, ok bool)`. Two
refinements from grounding it against the real adapter conventions:

- It returns a **tmux-pane command string** (mirroring `ResumeCmd`), **not** an argv
  slice. `ReviewCmd`/`HeadlessCmd` return argv because they are exec'd headlessly;
  a fork is an interactive TUI typed into a pane, exactly like `LaunchCmd`/`ResumeCmd`,
  so it is a pane string the adapter shell-quotes.
- It takes an **opts struct** (symmetry with `LaunchOpts`/`ResumeOpts`) carrying
  `Model`/`Mode` — verified to be honored by `codex fork` (§1.4) — and **does not**
  take the prompt: the prompt rides the **existing** `LaunchPromptArg` seam (the
  file-backed `"$(cat …)"` positional the spawn path already appends), so the prompt
  file's 0600 security machinery is reused unchanged and `codex fork <uuid> "$(cat
  file)"` falls out for free.

```go
// SessionForker is an optional Backend extension implemented by agents that can
// BRANCH a recorded session into a new DIVERGENT one (Codex: `codex fork <id>`).
// It complements warden's snapshot (linear worktree+transcript rollback) and its
// rotate/handoff (which hand off the TASK but drop the conversation) with
// CONVERSATIONAL forking — explore an alternative reasoning path from a recorded
// session WITHOUT discarding the original, as a new warden-managed agent.
//
// A fork is structurally a managed spawn whose launch command is the fork verb, so
// it returns a tmux-pane command string exactly like LaunchCmd/ResumeCmd (NOT an
// argv): the spawn path types it into the new agent's pane and appends the same
// hint/prompt/exit suffixes. The initial prompt is delivered via the existing
// LaunchPromptArg seam (file-backed positional), NOT through this method.
//
// Additive and on-top: a backend that does not implement it is simply not forkable
// (a spawn with fork_from set against such a backend is rejected with a clear
// message). Claude implements none of this — by construction the fork path never
// runs for it, keeping Claude's launch byte-identical and regression-locked.
type SessionForker interface {
    // ForkCmd returns the launch command that forks SourceSessionID into a new
    // session, run in the new agent's worktree. ok=false ⇒ the source id is empty
    // or this backend cannot fork the given input (the caller reports a clean
    // "cannot fork" rather than launching a bare agent).
    ForkCmd(opts ForkOpts) (cmd string, ok bool)
}

// ForkOpts is the neutral input for a SessionForker.ForkCmd call. SourceSessionID
// is the BACKEND'S recorded session id (the source agent's pinned id, e.g. codex's
// rollout UUID) — never warden's agent id. Model/Mode mirror LaunchOpts and are
// resolved by the caller before the call.
type ForkOpts struct {
    SourceSessionID string // backend session id to fork from (REQUIRED; ok=false if empty)
    Name            string // display label for the new session (warden agent id)
    Model           string // already-resolved model id
    Mode            string // permission/approval mode
}
```

### 4.1 Codex implementation (concept)

```go
// ForkCmd implements agentbackend.SessionForker. It forks the EXPLICIT source
// rollout UUID (never `--last`: the fork runs in a different worktree/cwd than the
// source, and `--last` is cwd-scoped — it would miss or mis-pick). Model/sandbox map
// exactly as LaunchCmd (codex fork accepts -m/-s/-a, verified codex 0.142.3). The
// pane is already cd'd into the fork's worktree, so no -C/--cd is appended.
func (Codex) ForkCmd(o agentbackend.ForkOpts) (string, bool) {
    if o.SourceSessionID == "" {
        return "", false
    }
    cmd := "codex fork " + shellQuoteArg(o.SourceSessionID)
    if o.Model != "" {
        cmd += " -m " + shellQuoteArg(o.Model)
    }
    if sb, never := codexSandbox(o.Mode); sb != "" {
        cmd += " -s " + sb
        if never {
            cmd += " -a never"
        }
    }
    return cmd, true
}
```

### 4.2 How the spawn path consumes it (concept)

In `spawnTyped` (and, if a free-form fork is ever wanted, `spawnFreeForm`), at the
launch-command construction point, branch on `req.ForkFrom`:

```go
b := l.backendFor(sess.Backend)
launch, err := l.buildLaunch(ctx, b, req, sess, mode, promptFile) // new helper
// inside buildLaunch:
//   if req.ForkFrom == "" → today's b.LaunchCmd(...) path, unchanged.
//   else:
//     fk, ok := b.(agentbackend.SessionForker); if !ok → error
//        "backend %s cannot fork a session" (clean degrade, Claude lands here).
//     parent := l.store.Get(req.ForkFrom)                    // resolve source agent
//     srcID := parent.ClaudeSessionID                        // its pinned backend id
//     if srcID == "" → error ErrForkSourceNotPinned          // see §5
//     cmd, ok := fk.ForkCmd(ForkOpts{SourceSessionID: srcID, Name: sess.ID,
//                                    Model: l.launchModel(b, req.Model), Mode: mode})
//     if !ok → error
//     launch = cmd  // then the SAME + hints + promptArg + exitSuffix as LaunchCmd
```

The rest of `spawnTyped` (worktree, tmux, injectContext, send-keys,
seedInteractivePrompt, rollback) is **unchanged** — the fork rides all of it. The
only other fork-aware step is the worktree base (§7).

### 4.3 Why explicit-id, not `--last`

`--last` is cwd-scoped (§1.5). The fork deliberately runs in its **own** worktree (a
different cwd than the source — §7), so `--last` would find nothing (or the wrong
session). The fork therefore **requires the source agent's pinned backend session
id** and passes it explicitly. This creates a hard dependency on **discover-then-pin
having completed for the source agent** (§5).

---

## 5. Session-id handling (reuses discover-then-pin, both ends)

Two id flows, both already built (step-2 `SessionIDDiscoverer`):

1. **Reading the source id.** codex mints its own id (`SessionIDControl=false`), so a
   codex agent's id is pinned post-launch by the poller into `Session.ClaudeSessionID`
   via `DiscoverSessionID` (newest rollout whose `session_meta.cwd == workdir`). The
   fork reads `parent.ClaudeSessionID` as `ForkOpts.SourceSessionID`. **If the source
   agent's id is not yet pinned**, the fork cannot proceed deterministically →
   return a clear `ErrForkSourceNotPinned` ("source agent's session id is not yet
   known; let it run one turn, then retry"). No guessing.

2. **Pinning the fork's new id.** The forked session is a **new** rollout with a new
   `session_id` and `session_meta.cwd == <fork worktree>` (§1.6) — i.e. it looks like
   a fresh codex session to warden. So the **existing** poller path handles it with
   **zero new code**: the new agent spawns with `ClaudeSessionID == ""`
   (`SessionIDControl=false`), and `DiscoverSessionID` pins the fork's own id on a
   later tick. Transcript reads / digests / resume then key off the exact fork id.

**This is the decisive constraint behind §7:** discover-then-pin is **dir-scoped**
(newest rollout *for this workdir*). If the fork shared the source's worktree, the
locator could not tell the fork's rollout from the source's and would mis-pin **both
ends**. The fork therefore **must** have its own distinct worktree (cwd). This is a
correctness requirement, not a preference.

---

## 6. Relation to rotate / handoff / adopt / snapshot — NOT redundant

| Verb | What it carries forward | What it drops | Original |
|---|---|---|---|
| **rotate / handoff** | the **task** (worktree + warden record / a handoff message) | the **conversation** (fresh context or summary) | replaced / retired |
| **adopt** | an externally-created session warden didn't spawn | — | n/a (registration) |
| **snapshot restore** | one worktree+transcript timeline, rolled **backward** | forward divergence | rewound in place |
| **codex fork** | the **conversation / reasoning state** (rollout), branched **sideways** | nothing — original kept alive | **preserved**, runs in parallel |

Fork is **orthogonal** to all of them: rotate/handoff keep the task and drop the
conversation; fork keeps the conversation and branches it. snapshot goes backward on
one timeline; fork opens a **second** timeline. So fork is a genuinely new verb, not
a re-skin of an existing one — it surfaces a **codex-native** capability (the rollout
fork) warden has no analogue for.

**Verb/UX decision:** a **`fork_from` flag on `spawn`**, not a new top-level verb in
the daemon contract — because a fork *is* a managed spawn (§3) and rotate/handoff are
their own verbs precisely because they **mutate an existing record**, whereas fork
**creates a new one**, exactly like spawn. For ergonomics a thin `wd fork <agent>
[prompt]` CLI alias can wrap `wd spawn --fork-from <agent>` (the same way `wd handoff`
wraps spawn), and an MCP `fork_agent` twin can wrap `spawn_agent` for CLI/MCP parity
— but both are **thin wrappers over the one spawn field**, adding no new daemon
endpoint.

---

## 7. Worktree semantics — fresh sibling, off the source's branch

Re-examining the parent §4.2 "snapshot sibling" sketch against warden's worktree
model and the §5 constraint, the three options:

- **Share the source's worktree — REJECTED.** Two codex agents writing one tree
  concurrently corrupts both, **and** it breaks dir-scoped discover-then-pin for both
  ends (§5). Disqualified on correctness, twice.
- **Fresh empty worktree off `main` — REJECTED.** The forked conversation references
  files/state from the source's tree; an empty/main tree is divorced from the
  reasoning the fork inherits. Semantically incoherent for "explore an alternative
  from this point."
- **Sibling worktree off the SOURCE's branch — CHOSEN.** A new
  `.worktrees/<fork-id>` created with `git worktree add -b <fork-branch>
  <source-branch>`, so the fork starts from the source's committed state. The
  conversation continues against the tree it actually diverged from, the fork gets its
  own cwd (discover-then-pin works), and the source is untouched. This is the same
  per-agent isolation every warden agent already has — it just **bases the branch on
  the source agent's branch** instead of the repo default.

**Two-tier fidelity (phasing lever):**
- **HEAD-only (PR-1):** base off the source agent's **branch HEAD** (committed
  state). Simplest; fully coherent when the agent commits regularly via `wd commit`.
- **+ dirty-tree carry (PR-2):** also seed the source's **uncommitted** changes into
  the fork worktree, so it diverges from the EXACT live state. This is precisely what
  the **snapshot** package already does non-destructively: `git stash create` on the
  source builds a stash *commit* without perturbing it, and `git stash apply <sha>`
  re-applies it into the fork worktree. So PR-2 = "reuse `snapshot.Capture`'s stash
  primitive + apply it into the fork worktree" — no new git mechanics, and it
  finally makes the parent's "snapshot sibling" phrase literal.

---

## 8. Phasing, DoD & open questions

### 8.1 PR breakdown

- **PR-0 (CLI-local slice): NONE.** Explicitly: fork has no useful CLI-local slice
  (§2) — an untracked fork adds nothing. Recorded so it isn't re-litigated.
- **PR-1 — Managed codex fork (HEAD-only worktree).** The pilot.
  - `agentbackend`: add `SessionForker` + `ForkOpts` (§4).
  - `codex.go`: implement `Codex.ForkCmd` (§4.1).
  - **Spec-first:** add `fork_from` to `openapi.yaml` `SpawnRequest`, `make generate`,
    thread `ForkFrom` through the three mapping layers (§3.1).
  - `lifecycle`: branch the launch-command construction on `req.ForkFrom`
    (type-assert `SessionForker`, resolve source pinned id, base worktree off the
    source's branch HEAD) (§4.2, §7). `ErrForkSourceNotPinned` when the source id
    isn't pinned (§5).
  - discover-then-pin pins the fork's new id via the **existing** poller (§5) — no new
    code.
  - **Claude regression-lock:** Claude doesn't implement `SessionForker`, so
    `fork_from` + a Claude backend returns the clean "cannot fork" error and **every
    non-fork spawn is byte-identical** (`fork_from` unset = today's path).
- **PR-2 — Fidelity + ergonomics.** Dirty-tree carry via the snapshot stash
  primitive (§7); `wd fork <agent> [prompt]` CLI alias + `fork_agent` MCP twin (both
  thin wrappers over `spawn --fork-from`, for CLI/MCP parity).

### 8.2 $0-local test plan (Ollama rig)

`codex exec --oss -m qwen2.5-coder` / `-c model_provider=oss`, no auth, no spend:
1. Spawn a managed codex agent; give it a couple of turns; assert the poller **pins
   its session id** (`Session.ClaudeSessionID != ""`).
2. `wd spawn --fork-from <agent>` (or the alias). Assert: a **new** agent record; a
   **new** `.worktrees/<fork-id>` based on the source's branch; the pane launch line
   is `codex fork <source-uuid> …`; the **source agent stays alive**.
3. After a tick, assert the fork's **own** id is pinned and **distinct** from the
   source's (proves dir-scoped discovery on the separate worktree, §5).
4. Negative: `--fork-from` against a Claude agent → clean "cannot fork" error;
   `--fork-from` a source whose id isn't pinned yet → `ErrForkSourceNotPinned`.
5. Regression: a plain `wd spawn` (no `--fork-from`) is unchanged.

### 8.3 DoD surfaces a future impl will touch (per CLAUDE.md)

- **Gap doc:** `docs/agent-backends/codex.md` — extend the "Superpowers surfaced"
  section with fork.
- **Features catalogs (×2):** root `FEATURES.md` matrix + `docs/FEATURES.md` prose +
  website mirror; CLI/MCP parity (`fork_agent` twin in `internal/mcp/tools_extra.go`).
- **Website:** `site/src/content/docs/` — a guide for fork + a `reference/cli.md`
  entry mirroring the new `--fork-from` / `wd fork` help.
- **CLI help:** the cobra `spawn` `--fork-from` flag description (and the `wd fork`
  alias `Use`/`Short`/`Long`) in `internal/cli/`, kept in sync with `reference/cli.md`.
- **Skill:** `skills/warden/` — note fork as a managed-spawn variant if it changes how
  agents drive warden.
- **Tag & release:** per CLAUDE.md, one tag per feature (patch for a small
  superpower); a `v*` push triggers GoReleaser — **confirm with the maintainer before
  pushing any tag.** This spec changes no code, so its own DoD is doc-written + PR'd,
  **no tag, no release.**

### 8.4 Open questions (could not resolve on the rig / for impl)

1. **Forked-rollout flush latency.** How many poller ticks before the fork's first
   rollout line lands (gates how quickly its id pins). Confirm with a live capture;
   the discover loop already retries, so worst case is a slightly delayed pin.
2. **Explicit-id fork across cwd.** Confirm `codex fork <uuid>` launched in a
   *different* cwd than the source's recorded cwd accepts the id cleanly (help implies
   yes; verify in a tmux pane). If codex ever refuses a cross-cwd explicit id, PR-1
   would need the fork worktree to register the id differently — unlikely, but verify
   before building.
3. **PR-2 dirty-carry vs. `.gitignore`d artifacts.** `git stash create` captures
   tracked changes; untracked build artifacts won't carry. Acceptable (matches
   snapshot's own contract), but document it so the fork's tree isn't assumed
   byte-identical to the source's.
4. **Multiple forks of one source.** Each fork gets its own worktree+id, so N forks
   are independent — but confirm the dir-scoped locator stays unambiguous when several
   fork worktrees exist (it is, since each cwd is distinct; noting for the test
   matrix).

---

## 9. Out of scope — other backends' fork-equivalents (re-confirmed N/A)

Per the parent §2 live findings, re-confirmed here:
- **Antigravity `/fork` `/rewind`** are **interactive TUI slash-commands, not CLI
  flags** (absent from `agy --help`). Surfacing them is a typed-into-pane
  PromptSeeder-style problem, not a `SessionForker` launch-command — **out of scope**
  for this codex-only pilot.
- **Cursor** has **no fork** concept — **non-applicable**.

So `SessionForker` ships **codex-only**; both others are correctly excluded.

---

## 10. Summary

- **What codex fork is (verified, 0.142.3):** a native verb that branches a recorded
  session's **conversation/reasoning** (the rollout) into a **new divergent session**,
  preserving the original — *not* a working-tree op, *not* "resume into a new id". An
  interactive TUI launcher with the **same flag surface** as `resume`/launch
  (`-m/-s/-a/-C`), taking an explicit source UUID + optional prompt.
- **Boundary decision:** intrinsically **daemon-crossing** (a fork is only valuable as
  a warden-managed agent; there is **no** useful CLI-local slice, unlike
  review/models). The **minimal spec-first delta** is **one additive optional field,
  `fork_from`, on the existing `SpawnRequest`** — reuse the whole managed-spawn path;
  **no new endpoint, no streaming-route exclude-list churn.**
- **`SessionForker`:** `ForkCmd(opts ForkOpts) (cmd string, ok bool)` returning a
  tmux-pane string (mirrors `ResumeCmd`); `ForkOpts{SourceSessionID, Name, Model,
  Mode}`; prompt rides the existing `LaunchPromptArg` seam. Codex maps it to
  `codex fork <uuid> -m … -s …`.
- **Session id:** read the source's pinned id (discover-then-pin, step 2); pin the
  fork's **new** id via the **same** existing poller — dir-scoped discovery is exactly
  why the fork needs its **own** worktree.
- **Worktree:** a **fresh sibling worktree off the source's branch** (HEAD-only in
  PR-1; + non-destructive dirty-tree carry via the snapshot stash primitive in PR-2).
- **Phasing:** PR-0 none → PR-1 managed fork (interface + `codex.ForkCmd` + spec-first
  `fork_from` + lifecycle branch + HEAD-only worktree; Claude regression-locked) →
  PR-2 dirty-carry fidelity + `wd fork`/`fork_agent` parity wrappers. $0-local on the
  Ollama rig throughout.
