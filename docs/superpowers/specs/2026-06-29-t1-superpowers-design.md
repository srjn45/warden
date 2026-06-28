# T1 Agent-Backend Superpowers (Phase Step 6) — Design

**Status:** 📐 Planning & design (opens **step 6 of the T1 perfecting phase**, #52).
Steps 1–5 are merged to `main` (poller/approval through the interface,
discover-then-pin session-id, per-agent state/approval markers, `InjectContext`
rules-file injection, pricing-estimate disclosure) plus the DoD docs.
**Date:** 2026-06-29
**Scope:** the three T1 non-Claude backends — **Codex, Antigravity, Cursor** —
and the **additive** surfacing of each one's NATIVE extras as warden features.
**Predecessor:** `2026-06-28-t1-backend-perfecting-design.md` (the perfecting
phase; §4 lists the candidate superpowers, §5 the additive optional-interface
pattern, §6 the codex-pilots-each-step rule, step 6 deferred to *this* doc).

> This is a **planning & design** document. **No production Go code is changed by
> this spec.** It scopes each candidate superpower against the real CLIs (verified
> live, §2), maps it onto an existing warden seam, names the additive interface it
> needs, rates value/effort/cost, picks the **$0-local codex pilot**, and sequences
> the per-superpower PRs.

> **Guiding principle, unchanged and load-bearing here:** *warden adds capability
> ON TOP of an agent; it never strips one to a lowest common denominator.* A
> superpower is the **surfacing** of a warden feature for that agent — never a
> restriction on the agent. Every item below is **purely additive**: an optional,
> type-asserted interface; **Claude untouched and regression-locked**; no registry,
> no neutral-type churn, no spec-first daemon API change (§4, §4.4).

---

## 1. Goal & framing

Steps 1–5 took the T1 backends from "launches + receives a prompt + responds" to
near-Claude fidelity on the *shared* seams (state, approval, session-id,
context-injection, pricing disclosure). Those gaps were **deficits** — warden
features that worked for Claude and not yet for the others.

Step 6 is the opposite shape. Each T1 agent ships **native capabilities Claude
does not have** — `codex review`, `codex fork`, antigravity's multi-vendor model
menu, cursor's server-side auto-review. Surfacing them is not closing a deficit;
it is **letting warden expose a per-agent strength as a first-class warden
feature**. This is exactly "add on top": the agent already does the thing, warden
just gives the operator a warden-native handle on it.

The work stays **capability-first and codex-piloted** (predecessor §6): codex is
$0-local on Ollama (`codex exec --oss -m qwen2.5-coder`), Tier A, and the
cleanest-integrated, so the **first** superpower implemented must be a codex one,
testable end-to-end at $0. Antigravity (hosted free tier, ~20 reqs/day) and cursor
(the maintainer's existing plan) follow under the **document-and-resume** quota
protocol (predecessor §7) — capture markers within free quota, and if quota runs
out mid-capture, document the state reached and resume on reset.

---

## 2. What each CLI ACTUALLY exposes (verified live)

The predecessor §4 listed *candidate* superpowers. Before designing around any
flag, each was confirmed against the installed binaries. **Findings (corrections in
bold):**

| Agent | Candidate (spec §4) | Real CLI surface (verified) | Verdict |
|---|---|---|---|
| Codex | `codex review` | ✅ `codex review [PROMPT]` **and** `codex exec review` — non-interactive, `--uncommitted`, `--base <branch>`, `--commit <sha>`, `--title`. **Stronger than assumed: diff-scoping is built in.** | **Keep — pilot** |
| Codex | `codex fork` | ✅ `codex fork [SESSION_ID] [PROMPT]`, `--last`, `--all` — branches a recorded session into a new divergent one. | Keep |
| Codex | `--output-schema` | ✅ on `codex exec`: `--output-schema <FILE>` (JSON Schema for the final response), plus `--json` (JSONL events) and `-o/--output-last-message <FILE>`. | Keep (pairs w/ review) |
| Codex | sandbox modes | ✅ `-s read-only\|workspace-write\|danger-full-access`. | **Already done** — `codexSandbox()` maps these onto warden permission modes (`codex.go`). Cut as "new". |
| Codex | `codex apply` | ⚠️ `codex apply <TASK_ID>` applies a **Codex Cloud** task's diff to the local tree — needs cloud tasks, **not $0-local**, and overlaps warden's own worktree/git model. | **Cut** |
| Antigravity | multi-vendor model menu | ✅ `agy models` lists the live menu (Gemini 3.x, Claude Sonnet/Opus 4.6, GPT-OSS 120B); `--model` selects per session. | Keep |
| Antigravity | `/fork`, `/rewind` | ⚠️ **Not CLI flags.** Absent from `agy --help`; they are **interactive TUI slash commands** only. Surfacing needs typed-into-pane (PromptSeeder-style), not a launch flag. | Keep — but document-and-resume |
| Antigravity | Python SDK | ⚠️ Out of warden's tmux/CLI driving model; a separate integration surface, no $0 necessity. | **Cut** (out of scope) |
| Cursor | parameterized models | ✅ `--model <model>` (bracket/parameterized syntax), `--list-models`. | Keep (low value) |
| Cursor | `--auto-review` | ✅ `--auto-review` = "Smart Auto": a **server-side classifier auto-runs safe tool calls and prompts for the rest**. A cursor-native *permission posture*. | Keep |
| Cursor | cloud `worker` | ⚠️ `cursor-agent worker` runs agents in a cloud worker (k8s probes, labels, auth-token mounts) — heavy infra, hosted, overlaps warden's own orchestration. | **Cut / defer** |
| Cursor | `create-chat` | ✅ `cursor-agent create-chat` returns a chat id — but this is the **discover-then-pin session-id hook already designed in step 2** (`SessionIDDiscoverer`), not a new superpower. | Folds into step 2 |

Net: the candidate list survives contact with the binaries, with three cuts
(`codex apply`, antigravity Python SDK, cursor cloud `worker`), one
already-shipped (codex sandbox modes), and one re-classified (cursor `create-chat`
is a session-id hook, not a superpower).

---

## 3. The warden seams a superpower can plug into

Surfacing means mapping onto an **existing** warden feature. The relevant seams,
located in the tree:

| Seam | Where it lives | Today | A superpower would… |
|---|---|---|---|
| **Review** | `wd check` runs `.warden/check.yml` configured commands (`internal/lifecycle/check.go`); the **`pr-review` agent type** (`store.TypePRReview`, isolated checkout) spawns a *reviewing agent*; Claude's `/code-review` is a Claude-Code skill, not warden core. | warden has **no agent-native diff-review verb** — only configured shell-command checks and a spawned reviewer. | …add an agent-native review step that asks the backend's own reviewer to read the diff. |
| **Snapshot** | `snapshot create/list/restore` (`internal/cli/snapshot.go`, `internal/snapshot`) — non-destructive git stash of the worktree + the session transcript, restorable. | rolls a worktree+transcript back to a known-good point (single linear timeline). | …add **conversational branching** (fork a session into a divergent one), not just linear rollback. |
| **Permission mode** | `set-permission-mode` (`internal/cli/permission_mode.go`), `set_auto_approve` policy. Codex sandbox modes already mapped via `codexSandbox()`. | warden maps its modes onto each backend's native approval vocabulary. | …expose a backend-native posture warden lacks (cursor `--auto-review` server classifier). |
| **Model menu** | `lifecycle/models.go`, `Caps.ModelSelection`, the resolved `-m`/`--model` flag. | warden resolves one model id per agent; codex/antigravity/cursor already pass it through. | …surface the backend's **full live vendor menu** to warden's picker (antigravity). |
| **Structured result** | `HeadlessCmd` (one-shot classify/summarize offload), digest's structured-transcript path. | warden's headless offload is text-in/text-out. | …request a **machine-readable** result via `--output-schema`/`--json` (e.g. structured review findings). |

---

## 4. The additive architecture

Same pattern as the merged `PromptSeeder` / `SessionIDDiscoverer` /
`ContextInjector`: **optional interfaces on `agentbackend.Backend`, type-asserted
at the call site, Claude untouched.** A backend that does not implement an
interface simply doesn't get that superpower — exactly as `SystemPromptFlag`
returning `ok=false` skips injection today. No change to the registry, the neutral
`Turn`/`State`/`Approval`/`Caps` types, or the lifecycle launch/resume flow.

### 4.1 `Reviewer` — agent-native diff review (the pilot seam)

```go
// Reviewer is an optional Backend extension implemented by agents that expose a
// NON-INTERACTIVE, diff-scoped code review as a first-class subcommand (Codex:
// `codex review --uncommitted|--base <branch>|--commit <sha>`). It is the
// agent-native counterpart to warden's configured `.warden/check.yml` checks and
// its spawned `pr-review` agent: instead of running project test commands or
// standing up a whole reviewer session, warden asks the backend's OWN reviewer to
// read the working diff and report findings. Additive and on-top — a backend that
// does not implement it is simply not offered `wd review` (the verb reports the
// backend has no native review and points at `pr-review`/`wd check`).
type Reviewer interface {
    // ReviewCmd returns the argv for a one-shot review run in the agent's workdir.
    // scope selects what to review (uncommitted working tree, or a base branch);
    // schemaFile, when non-empty, requests a machine-readable result the caller can
    // parse (codex: --output-schema). ok=false ⇒ this backend offers no native review.
    ReviewCmd(opts ReviewOpts) (argv []string, ok bool)
}

type ReviewOpts struct {
    Scope      string // "uncommitted" (default) | "base"
    Base       string // base branch when Scope=="base"
    SchemaFile string // optional JSON-Schema path for a structured result ("" = prose)
    Prompt     string // optional extra review instructions
}
```

**How core invokes it.** A new **CLI-only** verb `wd review` (`internal/cli/`)
resolves the agent's backend, type-asserts `Reviewer`, and — if present — execs the
returned argv in the agent's worktree, streaming the review to the operator (and,
with `SchemaFile`, parsing the structured findings). This mirrors `wd check`'s
local-exec shape exactly. **No daemon API change:** the review runs locally against
the worktree like a check; it does not need a new openapi.yaml operation or a
generated handler. **If** a later iteration wants the review surfaced *inside a
running session / the TUI / the approvals path* (e.g. "attach codex review output to
agent X's pane"), THAT would cross the daemon boundary and **must** go spec-first
(edit `openapi.yaml` → `make generate`, never hand-write handlers) — flagged here so
it isn't smuggled in. The pilot deliberately stays CLI-local to avoid it.

### 4.2 `SessionForker` — conversational branching (future)

```go
// SessionForker is an optional Backend extension for agents that can BRANCH a
// recorded session into a new divergent one (Codex: `codex fork <id> [prompt]`).
// It complements warden's snapshot (linear worktree+transcript rollback) with
// conversational forking — explore an alternative from a past point WITHOUT
// discarding the original. Keys off the same pinned session id discover-then-pin
// produces (SessionIDDiscoverer), so it only lights up once the id is known.
type SessionForker interface {
    ForkCmd(sessionID, prompt string) (cmd string, ok bool)
}
```

Plugs into the **snapshot** seam as an additional verb (`snapshot fork` or a
sibling), not a replacement — snapshot's git-stash rollback and a conversational
fork are different tools (see §5 on why this is *not* the pilot).

### 4.3 `ModelLister` — live vendor menu (future)

```go
// ModelLister is an optional Backend extension for agents whose model set is a
// live, multi-vendor menu discoverable at runtime (Antigravity: `agy models`;
// Cursor: `--list-models`). It surfaces the real menu to warden's model picker
// instead of a hard-coded alias table. Additive: backends without it keep the
// current resolved-id behavior.
type ModelLister interface {
    ListModels() (models []string, ok bool)
}
```

Plugs into the **model-menu** seam (`lifecycle/models.go`).

### 4.4 What does NOT change

- **Claude adapter:** untouched. It implements none of these (no native
  diff-review subcommand, no fork, a static model set), so by construction every
  new path is skipped for Claude — launch/resume/state stay byte-identical and
  regression-locked, the same guarantee §5 of the predecessor relied on.
- **Daemon API (spec-first):** the pilot (`wd review`) is a local exec — **no
  openapi.yaml edit, no `make generate`, no streaming-route exclude-list churn.**
  Any future "review/fork inside a live session" surface is the only thing that
  would cross the daemon boundary, and it goes spec-first if/when it's built.
- **Neutral types & registry:** no new `State`/`Turn`/`Caps` fields required for
  the pilot. (A later `Caps.NativeReview` *flag* is optional sugar for UX gating;
  the type-assert already gates behavior, so it's not required.)

---

## 5. The codex $0 pilot pick — **`codex review` as `wd review`**

**Pick: surface `codex review` as a new warden review step (`wd review`), via the
`Reviewer` interface.** Justification against the brief's two criteria
(best warden-native fit + $0-local testable), with the runner-up weighed:

**Why `codex review` over `codex fork`:**

1. **It fills a real warden gap; fork partly duplicates one.** warden today has
   *no agent-native diff review* — only `.warden/check.yml` shell commands (which
   run tests, not a semantic review) and the `pr-review` agent type (which stands
   up a whole reviewer session). `codex review` is **strictly stronger** than
   either for "have an agent read this diff and flag issues," and it's net-new
   surface. `codex fork`, by contrast, overlaps the **existing** snapshot
   create/restore feature; its genuinely-new bit (conversational branching vs
   linear rollback) is real but *incremental on an existing seam*, not net-new.
   The brief says cut anything redundant with an existing feature unless strictly
   stronger — review clears that bar cleanly; fork is a softer case → fork ships
   later (§6 PR-B).
2. **It's $0-local end-to-end.** `codex review --uncommitted` runs against the
   Ollama rig (`-c model_provider=oss -m qwen2.5-coder`) with no auth and no
   spend — the full pilot (interface + `wd review` + adapter `ReviewCmd` +
   structured-output parse) is testable for free, unlimited iteration. fork is
   *also* $0, so this isn't the deciding factor — but review meets it without
   compromise.
3. **Clean diff-scoping the verb maps onto directly.** `--uncommitted` (working
   tree) and `--base <branch>` line up 1:1 with `ReviewOpts.Scope`, and warden
   agents live one-per-worktree so "review my changes" is unambiguous.
4. **It composes with `--output-schema`.** The same pilot can request structured
   findings (`codex exec` + `--output-schema`), giving warden machine-readable
   review output to render — a second candidate superpower surfaced for nearly
   free, and a template for "structured result" on other backends.

**Honest caveat to validate during impl:** on a tiny local model
(`qwen2.5-coder:3b`) review *quality* will be weak — the pilot proves the
**plumbing** (verb → adapter → review run → parse/stream), not review accuracy.
That's the right thing to prove at $0; quality rides the operator's real model.
**Resolve during impl:** whether `codex review` honors `--output-schema` directly
or only `codex exec review` does (the structured path may need the `exec`
sub-form) — if the two diverge, `ReviewCmd` picks the form based on whether
`SchemaFile` is set.

---

## 6. Phase order (codex pilot → antigravity → cursor)

Each PR is independently shippable, PR'd against `main`, **no tag** until the
maintainer decides a release.

1. **PR-A — Codex review (the $0 pilot).** Add the `Reviewer` optional interface +
   `ReviewOpts`; implement `Codex.ReviewCmd`; add the `wd review` CLI verb
   (type-asserts `Reviewer`, execs in the worktree, streams output; degrades with a
   clear message for non-reviewer backends). Capture a `codex review` fixture on the
   Ollama rig. Claude untouched. **CLI-local, no daemon change.**
2. **PR-B — Codex fork + structured review.** Add `SessionForker` + `Codex.ForkCmd`
   wired into the snapshot seam (a `fork` sibling to `restore`); add the
   `--output-schema` structured-result path to `wd review` (parse findings into a
   neutral shape). Both $0-local on the same rig. *Gated behind PR-A's interface
   landing.*
3. **PR-C — Antigravity model menu (+ document /fork /rewind).** Add `ModelLister`
   + `Antigravity.ListModels` (`agy models`) feeding the model picker. Capture the
   `/fork` `/rewind` TUI markers **within free-tier quota**; if quota runs out,
   **document the state reached and resume on reset** (predecessor §7). Surfacing
   the interactive slash commands (typed-into-pane) is scoped as a follow-on, not
   blocking the model-menu win.
4. **PR-D — Cursor auto-review + parameterized models.** Map cursor `--auto-review`
   onto a warden permission mode (extend the cursor adapter's mode mapping — same
   shape as `codexSandbox()`, no new interface); surface parameterized `--model` /
   `--list-models` via `ModelLister`. cursor `create-chat` is **not** here — it's
   the step-2 `SessionIDDiscoverer` hook. Capture on the maintainer's existing plan;
   document-and-resume on quota. cursor cloud `worker` is **cut/deferred** (heavy
   hosted infra, overlaps warden orchestration).

Rationale for the order: codex is $0 and Tier A so it pilots both the **review**
seam (PR-A) and the **fork/structured** seams (PR-B) for free; antigravity's
clean, low-effort, CLI-flag model menu (PR-C) lands the next visible win; cursor
(PR-D) is last because its strongest superpower (`--auto-review`) needs live
hosted capture and its session-id hook already belongs to step 2.

---

## 7. Open questions

1. **`wd review` scope default & output rendering.** Default to `--uncommitted`
   (the agent's working tree) — agreed. Open: does review output go to stdout only
   (CLI-local, pilot), or should it *also* be capturable as a snapshot-adjacent
   artifact for later reference? The latter starts to pull toward the daemon
   boundary (§4.4) — keep the pilot stdout-only and revisit.
2. **`--output-schema` form divergence (§5 caveat).** Confirm whether structured
   output requires `codex exec review` vs `codex review`; `ReviewCmd` branches on
   `SchemaFile` if so.
3. **Fork vs snapshot UX overlap.** A conversational `fork` and a worktree
   `snapshot restore` are different operations that will *look* similar to an
   operator. Decide naming/placement (a `snapshot fork` verb vs a top-level
   `fork`) so the two don't confuse — resolve during PR-B, not now.
4. **Antigravity slash-command capture cost.** `/fork` `/rewind` are interactive
   only; capturing their pane markers spends free-tier quota. Lean: document the
   model-menu win (PR-C, a pure CLI flag, near-zero quota) and treat the
   slash-command surfacing as document-and-resume, not a blocker.
5. **`Caps.NativeReview` flag?** The type-assert already gates behavior; a Caps
   flag is only UX sugar (e.g. for a TUI to show/hide a "review" affordance). Add
   only if a consumer needs to *advertise* the capability without attempting it.
   Lean: skip until a consumer needs it (mirrors the predecessor's "DSL only if
   duplication is real" stance).

---

## 8. Definition-of-Done (per CLAUDE.md)

This spec is a **planning artifact** — it changes no production code, so its own
DoD is: doc written, PR'd, **no tag, no release**. The DoD below applies to the
**eventual step-6 feature PRs** (PR-A…PR-D), walked per CLAUDE.md:

- **Gap docs** — update `docs/agent-backends/{codex,antigravity,cursor}.md` to add a
  "Superpowers surfaced" section as each lands (codex review/fork first).
- **Features catalogs (×2)** — root `FEATURES.md` matrix + `docs/FEATURES.md` prose,
  plus the website mirror; keep MCP/CLI parity (a `wd review` verb that gets an MCP
  twin goes in `internal/mcp/tools_extra.go`).
- **Website** — `site/src/content/docs/` — a guide (`guides/`) for `wd review` and a
  `reference/cli.md` entry mirroring the new `--help`.
- **CLI help / manual** — the new cobra command's `Use`/`Short`/`Long`/flags in
  `internal/cli/`, kept in sync with `reference/cli.md`.
- **Skill** — `skills/warden/` if `wd review` / fork changes how agents drive warden.
- **Tag & release** — one tag per feature (patch for a small superpower, minor for
  the batch); a `v*` push triggers GoReleaser, so **confirm with the maintainer
  before pushing any tag** (and batch ≤3 tags per push).

---

## 9. Summary

- **Pilot:** `codex review` → a new `wd review` step via the additive `Reviewer`
  interface — net-new warden surface (warden has no agent-native diff review),
  $0-local on the Ollama rig, clean diff-scoping, composes with `--output-schema`.
  Beats `codex fork` (which overlaps existing snapshot) as the first pick.
- **Architecture:** optional, type-asserted interfaces (`Reviewer`, then
  `SessionForker`, `ModelLister`); Claude untouched; **no spec-first daemon API
  change** for the pilot (CLI-local exec like `wd check`), with the one
  daemon-crossing future case flagged.
- **Order:** PR-A codex review (pilot) → PR-B codex fork + structured review →
  PR-C antigravity model menu → PR-D cursor auto-review + parameterized models.
  Cuts: `codex apply`, antigravity Python SDK, cursor cloud `worker`.
- **Verified against the real binaries** (codex 0.142.3, agy, cursor-agent);
  corrected three spec-§4 assumptions (antigravity `/fork` `/rewind` are TUI-only
  not CLI flags; cursor `create-chat` is the step-2 session-id hook; codex sandbox
  modes are already shipped).
