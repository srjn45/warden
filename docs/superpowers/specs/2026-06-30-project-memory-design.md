# Backend-Neutral Project Memory, Curated from Fleet Digests (#53) — Design

**Status:** 📐 Planning & design. The deep-dive `FUTURE_ENHANCEMENTS.md` §53 was
explicitly deferred to *until #52 (pluggable agent backends) completed*. #52 is now
done (steps 1–6; all T1 superpowers merged to `main`), so the projection seam and
the ≥2-real-backends demand signal §53 named as prerequisites both exist. This is
that pass.
**Date:** 2026-06-30
**Scope:** the canonical-source decision, the curation/freshness model, and the
per-backend projection + token-cost budget — the three open questions §53 left. It
resolves them and sequences shippable PRs; it does **not** reopen the framing (§53
already settled: warden owns ONE backend-neutral memory, projected per backend; no
`wd init` gate; repo-cleanliness dropped; the kernel is the cross-agent rediscovery
tax).

> This is a **planning & design** document. **No production Go code is changed by
> this spec.** It maps #53 onto the EXISTING #52 injection seam, verifies what that
> seam and the digest mechanism actually provide, names any additive surface
> precisely (spoiler: **no new `Backend` interface** — it rides the existing seam),
> and proposes the PR breakdown. Building it is a later impl pass.

> **Guiding principle, unchanged and load-bearing here:** *warden adds capability ON
> TOP of an agent; it never strips one, and it never adds ceremony.* Memory is
> **additive**: a config-gated projection over the seam #52 already built; **Claude
> untouched and regression-locked**; **no `wd init` registration gate** (implicit
> project keying, auto-created on first use); existing human files
> (CLAUDE.md/AGENTS.md/CONVENTIONS.md) **read and respected, never clobbered**
> (verify-before-trust).

---

## 1. Goal & framing — the cross-agent rediscovery tax

The warden-shaped kernel (§53, not re-derived): **Agent A** greps around, learns
where things live ("the daemon API is spec-first — edit `openapi.yaml` then
`make generate`"; "savings/spend state lives in `~/.warden`, not the tree"), finishes
its task, and is **torn down**. **Agent B** — possibly a *different backend* — starts
cold and **re-learns the same map** from scratch, burning tokens and turns on
rediscovery warden already watched Agent A pay for once.

warden is uniquely positioned to tax this once and amortize it across the fleet
because it is the orchestration layer *above* all backends: it watches the whole
fleet, already captures a **digest** on completion, and already **injects** a
launch-time addendum into every backend that can take one. The loop §53 names: *roll
durable cross-agent learnings into one canonical, backend-neutral memory, then
project it into the next agent regardless of which backend it runs.*

This is **not** "just use CLAUDE.md." CLAUDE.md is read by Claude Code **only**. The
moment warden is multi-backend (now), the eight backends each have a *different*
project-memory convention and **none is shared across all of them** (§2). That
fragmentation is exactly why warden — not any one agent — is the natural owner of one
canonical memory rendered into whatever each backend consumes.

What this spec is honest about up front (the hazards, treated in full below):
**memory poisoning** (a stale/false entry actively misleads every future agent),
**token cost** (injection adds input tokens per turn and only nets positive when the
memory is curated, consumed, and — for Claude — cache-stable), and **digest
thinness** (digests capture *what changed*, not *where things live* — §3.2 — so
curation is an extraction problem, not a copy).

---

## 2. What the projection seam ACTUALLY provides (verified against the code)

§53 asserts "the mechanism already exists in #52." Confirmed, and the shape is
better than §53 spelled out — there are **two** injection seams, not one, and warden
**already writes** the file-drop one for six backends today.

### 2.1 The two seams (`internal/agentbackend/backend.go`)

- **Launch-flag seam — `SystemPromptFlag(text) (fragment, ok)`** (backend.go:141),
  gated by `Caps.SystemPromptInject` (backend.go:102). Claude returns
  ` --append-system-prompt <shell-quoted text>` and `ok=true`; **every other backend
  returns `("", false)`.** Verified: `claude.go:356` is the only non-trivial impl;
  aider/antigravity/codex/crush/cursor/goose/opencode all return `("", false)`.
- **File-drop seam — `ContextInjector.InjectContext(workdir, text)`**
  (backend.go:343), the counterpart for backends with **no** launch flag but which
  read a rules file from the working directory on startup. Implemented by
  **codex, cursor, opencode, antigravity** (→ `AGENTS.md`), **crush** (→ `CRUSH.md`),
  **goose** (→ `.goosehints`). Verified filenames at `inject.go:18-21` and each
  adapter's `InjectContext`. **Aider implements neither** (no flag, no
  `InjectContext`).

### 2.2 How lifecycle already assembles and routes the addendum

`internal/lifecycle/lifecycle.go` already has the whole assembly, today carrying two
guidance strings — `collabHintGuidance` (coordinate with concurrent agents) and
`pipelineHintGuidance` (recommend splitting big tasks):

- `systemPromptHint(b, enabled, guidance)` (lifecycle.go:101) routes guidance through
  `b.SystemPromptFlag` → a launch-line fragment, or `""` when the backend can't inject.
- `injectContext(b, workdir, guidances...)` (lifecycle.go:134) type-asserts
  `ContextInjector`, joins the non-empty guidances with `\n\n`, and writes them as
  **one** warden-delimited block (`<!-- warden:begin -->` … `<!-- warden:end -->`,
  `inject.go:28`) into the backend's rules file — **idempotent** (replaced in place on
  relaunch), **never clobbering** a user's existing file content, and
  **git-excluded** (`inject.go:101`, the dropped file never lands in the agent's
  diff/PR).
- A backend that implements **neither** (aider) silently contributes nothing — the
  addendum is dropped exactly as today.

**This is the decisive finding for #53:** memory is *another guidance string*. The
projection step adds a `memoryGuidance` to the **same** `systemPromptHint` /
`injectContext` assembly, alongside `collabHint`/`pipelineHint`. **No new `Backend`
interface, no new injection block, no spawn-path change.** It rides the seam #52 built
— the same conclusion shape as the codex-fork doc's "a fork *is* a managed spawn"
(reuse, don't re-implement).

### 2.3 The per-backend projection table (real, verified)

| Backend | `SystemPromptInject` | Native rules file | Projection path | Memory delivery |
|---|---|---|---|---|
| **claude** | `true` | CLAUDE.md (native) | **launch flag** | `--append-system-prompt <memory>` |
| **codex** | `false` | AGENTS.md | **file-drop** | warden block in `AGENTS.md` |
| **cursor** | `false` | AGENTS.md | file-drop | warden block in `AGENTS.md` |
| **opencode** | `false` | AGENTS.md | file-drop | warden block in `AGENTS.md` |
| **antigravity** | `false` | AGENTS.md | file-drop | warden block in `AGENTS.md` |
| **crush** | `false` | CRUSH.md | file-drop | warden block in `CRUSH.md` |
| **goose** | `false` | .goosehints | file-drop | warden block in `.goosehints` |
| **aider** | `false` | CONVENTIONS.md / `--read` | **neither seam** | **degrade-skip** (today) |

**7 of 8 backends project today, via the existing seam, with zero new adapter code.**
Aider is the one degrade-skip (it injects via neither seam — matches the #52 reality
that the addendum is already skipped for aider). A future aider `ContextInjector`
(write `CONVENTIONS.md`, or pass `--read .warden/memory.md`) is an impl-time
enhancement, **not** a blocker (§7, open Q1). This degrade-cleanly behavior is exactly
the §5 capability-gating pattern every #52 seam used.

---

## 3. What the digests ACTUALLY provide (verified) — and why curation is hard

### 3.1 When digests fire and what they contain

`internal/digest` + `internal/daemon/digest_routes.go`:

- A digest is built **on demand** (`GET /sessions/{id}/digest`, the `wd digest` verb)
  and **snapshotted at pipeline-job completion** (the executor's `digestFn`,
  `executor.go:58/71`). It is **not** a per-turn stream — it is a completion-time
  artifact, which is the right cadence for rolling durable learnings (§4.2).
- The `Digest` shape (`digest.go:27`): `Summary` (a 1–2 sentence LLM narrative from
  `ClaudeNarrator`, degrading to the last assistant message), `Files` (`FileChange`
  with git `--numstat` +/- line counts and an `Edited` flag), `Branch`, `Turns`,
  `Task` (the first real user prompt), `Status`.
- It is built **through the backend** (`buildDigest`, `digest_routes.go:28`): a
  structured-transcript backend is parsed via its own `ParseTranscript` and bridged
  into neutral `Facts`; a non-structured backend degrades to a pane-scrape summary.
  So the digest source is **backend-neutral already** — the same property #53 wants
  for memory.

### 3.2 The honest gap: digests capture *what changed*, not *where things live*

This is the load-bearing caveat for the whole curation half of #53. A digest answers
**"what did this agent DO?"** (files touched, lines +/-, a one-line narrative of the
task). It does **not** answer **"what durable fact about this project should the next
agent know?"** ("the daemon API is spec-first"; "tests live behind `wd check`";
"never raw-git, the guard denies it"). Those are the cross-agent-rediscovery facts —
and they are **not** a field in `Digest`.

Therefore **curation is an extraction/summarization problem, not a copy.** Dumping
digests into memory would fill it with "Agent X edited 4 files on branch Y" entries —
noise that *adds* tokens and rediscovery cost rather than removing them, and that
ages into poison the instant the files move. This is the single biggest reason §53 is
a "hard part," and it drives the conservative phasing in §6 (ship projection of
*curated* memory first; gate *auto-curation* behind a reviewable proposal step).

---

## 4. The three open questions, resolved

### 4.1 Q1 — Where the canonical source lives → **a warden-owned, committed `.warden/memory.md`, projected via the existing seam; existing human files read-respected, never generated or clobbered.**

The candidates (§53): (a) a single cross-tool committed file (adopt `AGENTS.md` as
canonical and *generate* per-backend variants incl. CLAUDE.md from it); (b) a
warden-owned committed `.warden/` file; (c) a machine-local `~/.warden/<project>`
store.

**Reject (c) outright.** Machine-local doesn't travel with the clone and isn't
team-shared/reviewable — it loses the biggest lever (cross-developer, cross-machine
amortization) and §53 already calls committed "still preferable." A machine-local
cache *of* the committed source is a fine impl detail, but the source of truth must
travel with the repo.

**The real fork is (a) vs (b).** Decision: **(b), with a sharp constraint that also
neutralizes (a)'s appeal.**

**Pick: `.warden/memory.md`** — a committed, warden-owned file living beside the
existing committed `.warden/check.yml` (verified present in this very repo). Projected
into every injecting backend via §2's seam.

Why warden-owned-and-committed beats AGENTS.md-as-canonical:

1. **Curation ownership / poisoning containment.** #53's memory is *auto-curated from
   digests* — possibly stale, possibly wrong (§3.2, §4.2). Writing auto-generated
   content into `AGENTS.md` means poisoning lands directly in a file **four backends
   read natively, ungated by warden**, *and* in a file humans hand-author. A
   warden-owned file keeps unverified, machine-curated memory under warden's
   delimited control, lets warden gate freshness/verify-before-trust **before**
   projecting, and keeps the human's `AGENTS.md` theirs.
2. **No native-file generation = no clobber.** Candidate (a) proposes *generating
   CLAUDE.md* (and CONVENTIONS.md, …) from the canonical source. This repo has a large
   hand-authored `CLAUDE.md`; generating/overwriting it is precisely the clobber the
   brief forbids. **warden must never write a human's native memory file.** Choosing a
   warden-owned source + *runtime injection* (never file generation) sidesteps this
   entirely: human files stay human, memory rides the transient injected block / launch
   flag.
3. **Uniform projection.** With (b), *all* injecting backends get memory through the
   one seam warden owns — consistent behavior, one code path, one size budget. With
   (a), the four AGENTS.md-readers get it "natively" while the other four need
   injection anyway — two behaviors to reason about and a double-source for the
   AGENTS.md-readers (native file + warden block).
4. **Reviewable curation is a feature, not a side effect.** Because `.warden/memory.md`
   is committed, **every curation change is a git diff** a human (or a reviewing agent)
   approves in a PR. This is the strongest single mitigation for poisoning (§4.2) — and
   it is *only* available for a committed source. (a) shares this; (c) does not.

**Coexistence / migration (verify-before-trust, never clobber):** on first projection
warden **reads** any existing `CLAUDE.md` / `AGENTS.md` / `CONVENTIONS.md` to (i) avoid
duplicating facts already stated there and (ii) optionally seed the initial
`.warden/memory.md`, but it **never writes or rewrites them**. The file-drop seam
already merge-preserves a user's existing `AGENTS.md`/`CRUSH.md`/`.goosehints` content
(`inject.go:84` `mergeRulesFile`) and only manages its own `<!-- warden:begin -->`
block — so a backend that *also* natively reads AGENTS.md sees the human's content
**plus** warden's injected memory block, never a clobbered file.

**Honest note for the maintainer (the one place to override at impl time):** if the
team would rather make `AGENTS.md` the single canonical cross-tool file and accept
warden writing curated memory into it (betting on AGENTS.md ubiquity over
warden-ownership), that is candidate (a) and it is *coherent* — it trades poisoning
containment and no-clobber for "one file the ecosystem already knows." I pick (b)
because warden-ownership + never-generate-human-files is the safer default and aligns
with warden's existing `.warden/` config home; the alternative is documented here so
it can be chosen deliberately rather than by accident.

### 4.2 Q2 — Curation & freshness → **a debounced, completion-triggered extraction pass that PROPOSES timestamped, provenance-tagged, UNVERIFIED entries; verify-before-trust promotion; supersession/age-out; the committed diff is the human gate.**

This is the hardest part; it gets real treatment.

**Source → memory is extraction, not dump (§3.2).** The curation pass does **not**
copy digests. It reads (recent digests + the agent's transcript turns + the current
`.warden/memory.md`) and proposes **durable, reusable facts** — "where X lives", "how
to run Y", project invariants ("never raw-git", "spec-first daemon API") — discarding
the per-task noise ("edited 4 files on branch Z").

**Who summarizes, and when.** Reuse warden's *existing* offload plumbing, do not build
a new one:

- The digest already has a `Narrator` seam (`narrator.go`, `ClaudeNarrator` shelling
  `claude -p` through an injected `Run` func) and the backend has `HeadlessCmd`
  (one-shot classify/summarize, `ok=false ⇒ local-LLM path`). The curation pass is the
  **same shape**: a bounded, headless summarization call.
- **Trigger:** the **completion** hook that already builds the digest (not per-turn —
  §3.1), **debounced/batched** so a burst of completions yields one curation pass, not
  N. This bounds cost and avoids thrash.
- **Cost tier:** prefer the **local-LLM path** (#50 REPL / `HeadlessCmd` `ok=false`
  fallback) so curation is **$0** and adds no cloud spend; degrade to `claude -p` only
  where configured. Curation is a background nicety — it must never be on a paid
  critical path.

**Entry shape (the freshness machinery).** Each memory entry is a small structured
record, not a free-text blob:

```
- [unverified · 2026-06-30 · agent a1b2 · sha 04e2aed] The daemon API is spec-first:
  edit internal/daemon/apidocs/openapi.yaml, then `make generate`; never hand-write
  handlers/DTOs.
```

- **timestamp** (absolute, per [[release-tagging-style]]-style discipline — convert
  relative to absolute), **provenance** (which agent / digest / commit sha taught it),
  and a **trust state**: `unverified` → `trusted`.
- **Verify-before-trust promotion.** A new entry starts `unverified` — a *hint*, not an
  authority. It is promoted to `trusted` only when **corroborated**: re-observed by a
  second agent, confirmed against the live tree (a cheap deterministic check — e.g. the
  path it names still exists), or **human-approved in the committed diff**. The
  projected block (§4.3) labels unverified entries as "learned context — may be stale,
  verify before relying."
- **Age-out / supersession.** Entries carry a TTL; an entry untouched/un-recorroborated
  past the TTL is demoted/pruned. A newer entry that **contradicts** an older one
  *supersedes* it (the older is struck, with a tombstone for the diff reviewer). A
  fact whose named path no longer exists is auto-flagged stale on the next deterministic
  check.

**The poisoning hazard, met head-on.** The known failure (§53): one agent's wrong
belief becomes memory and misleads the whole fleet. Mitigations, layered:

1. Entries are **claims with provenance + timestamp**, never anonymous authority.
2. **Verify-before-trust** gates promotion; the projection visibly marks `unverified`.
3. **The committed diff is a human review gate** — auto-curation writes *proposals*; a
   person (or a reviewing agent) approves the `.warden/memory.md` diff in a PR before it
   ships to teammates. This is *only* possible because the source is committed (§4.1).
4. **Supersession on contradiction** + **age-out** stop stale facts from accreting.
5. **Conservative phasing (§6):** **projection of human-curated memory ships first**
   (zero poisoning risk — a human wrote it); **auto-curation ships behind the proposal
   gate**, never auto-trusted. The high-value/low-risk half lands without waiting on the
   hard/risky half.

### 4.3 Q3 — Per-backend projection & token-cost budget → **a hard size budget, spawn-boundary stability for cache, and a backend-specific cost rule.**

The table is §2.3. The cost discipline:

**Injection only nets positive when memory is (a) curated/compact, (b) actually
consumed to skip rediscovery, and (c) — for Claude — prompt-cache-stable.** Each is a
rule:

- **(a) Hard size budget.** The projected memory has a ceiling (proposal: ≤ ~1.5–2 KB /
  a few hundred tokens — the size of a tight "where things live" list, not a wiki). The
  curation pass *summarizes to fit*; over-budget memory is trimmed (lowest-trust /
  oldest entries drop first), and for non-caching backends projection may **degrade-skip**
  rather than bloat every turn.
- **(b) Consumed, not decorative.** Memory earns its tokens only if agents read it to
  *skip* rediscovery. Keep it factual and navigational ("X lives in Y", "run Z via
  `wd check`"), not prose. This is the same bar `collabHint`/`pipelineHint` already
  meet.
- **(c) Cache stability — Claude-specific.** `--append-system-prompt` rides Claude's
  **system prompt → prompt-cached**; `cache_read ≈ 10%` of fresh input (verify current
  rates via the `claude-api` skill at impl time, don't hard-code). So a **stable** memory
  block is ~free after the first turn. The discipline: **memory changes apply at the
  next SPAWN boundary, never mid-session** — rewriting the injected block mid-session
  would invalidate the cache and re-bill the whole prefix. (warden already re-injects
  only at launch/relaunch, so this falls out naturally — but it must be stated so a
  future "live memory refresh" feature doesn't silently wreck the cache.)

**Backend-specific budget (the §53 point).** Caching behavior differs:

- **Claude (hosted, caches):** stable memory ≈ free after turn 1. Most generous budget.
- **codex / cursor / antigravity (hosted):** caching is provider-dependent; assume the
  file is re-read into context each turn. Budget moderate; rely on size cap.
- **BYO-model backends — aider / opencode / crush / goose (local or arbitrary
  providers):** **may not cache at all** → the memory adds its **full** token count
  every turn. Budget is tightest here; the size cap is mandatory and projection should
  degrade-skip before bloating. (Aider already degrade-skips entirely — §2.3.)

Net cost rule: **compact + stable + navigational.** A few hundred cached/curated tokens
that delete multiple rediscovery turns is a clear win; an uncapped digest dump is a clear
loss.

---

## 5. The additive surface — **no new `Backend` interface; rides the existing seam**

Mirroring the codex-fork finding ("one additive field, not a new endpoint"), the #53
finding is **smaller**: **no new `Backend` interface at all.** The §2 seam
(`SystemPromptFlag` + `ContextInjector`) already delivers arbitrary text per backend,
capability-gated, Claude via flag and six others via file-drop. Memory is just a new
*guidance string* fed into the **existing** `systemPromptHint` / `injectContext`
assembly.

What the impl *does* add (lifecycle/daemon internals, **not** the backend contract):

1. **A memory store/reader** — locate `.warden/memory.md` from the repo root (implicit
   keying, §4.1), parse its entries, render the budgeted projection text. A small new
   package (e.g. `internal/memory`), neutral, with no backend coupling.
2. **One more guidance string in the assembly** — `memoryGuidance(repoRoot)` feeding
   `systemPromptHint(b, l.cfg.GetMemoryInject(), text)` and the `injectContext(b,
   workdir, …)` guidance list, exactly alongside `collabHint`/`pipelineHint`
   (lifecycle.go:90-150). Config-gated like its siblings (`memory_inject`, default on).
3. **A curation pass** (§4.2) — reusing the `Narrator`/`HeadlessCmd`/local-LLM
   plumbing, triggered on the completion/digest hook, debounced, writing *proposals* to
   `.warden/memory.md`.

**Claude regression-lock.** Claude's projection is its existing
`SystemPromptFlag` → `--append-system-prompt` path, byte-identical to how
`collabHint`/`pipelineHint` already ride it; with `memory_inject` off or an empty
`.warden/memory.md`, **every launch is byte-identical to today**. No `State`/`Turn`/
`Caps` field, no registry change, no spec-first daemon API edit (projection is
lifecycle-local at spawn; the curation trigger reuses the existing digest hook). If a
*future* surface wants memory edited/queried **inside a running session** or over the
HTTP API (e.g. a `wd memory` daemon verb, a TUI panel), **that** crosses the daemon
boundary and goes **spec-first** (edit `openapi.yaml` → `make generate`, never
hand-write handlers; new streaming routes need the `oapi/config.yaml` exclude list) —
flagged here so it isn't smuggled in. The core projection deliberately stays
spawn-local to avoid it.

---

## 6. Phasing, DoD & open questions

### 6.1 PR breakdown (independently shippable, codex-$0-pilotable)

- **PR-0 (CLI-local slice): a thin `wd memory` reader.** Optional, low-cost: a
  CLI-local verb to **show** / **edit** the resolved `.warden/memory.md` for the current
  repo (read-only print + open-in-`$EDITOR`), no daemon change — the same CLI-local
  shape as `wd check`/`wd review`. Useful immediately (a human can hand-author memory)
  and de-risks PR-1's keying. *Could fold into PR-1.*

- **PR-1 — Projection of a human-curated memory (the core win, zero poisoning risk). ✅ SHIPPED.**
  Implemented: `memoryGuidance` is threaded through lifecycle's existing
  `systemPromptHint` / `injectContext` assembly (all three spawn paths), config-gated
  by the new `memory_inject` key (default on). Projection is **read-only** (it never
  auto-creates the file — that stays the `wd memory` verb's job — so a repo with no
  memory.md, and `memory_inject` off, both leave the Claude launch byte-identical; a
  regression-lock test asserts this). 7/8 backends project via the existing seam; aider
  degrade-skips. The `internal/memory` public API (`Store.Locate`, `Parse`,
  `Memory.Render`/`RenderDefault`) was reused unchanged.
  - `internal/memory`: locate/parse/render `.warden/memory.md`, budgeted (§4.3).
  - Implicit project keying: derive the repo root from `git rev-parse --show-toplevel`
    (warden already shells `rev-parse`, `git.go:125`); **no `wd init` gate**;
    auto-create the file on first use. Read-respect existing CLAUDE.md/AGENTS.md/
    CONVENTIONS.md, never clobber (§4.1).
  - Lifecycle: feed `memoryGuidance` into the **existing** `systemPromptHint` /
    `injectContext` assembly; config-gated (`memory_inject`). 7/8 backends project via
    the existing seam; aider degrade-skips.
  - **Claude regression-lock:** rides `SystemPromptFlag`; off/empty = byte-identical.
  - **$0-local pilot on the Ollama codex rig** (`codex exec --oss -m qwen2.5-coder`):
    hand-write a `.warden/memory.md`, spawn a managed codex agent, assert the warden
    block appears in its worktree `AGENTS.md` carrying the memory; assert Claude launch
    is unchanged. This alone solves the cross-agent rediscovery tax for human-curated
    facts.

- **PR-2 — Auto-curation from digests, gated as proposals (the hard, risky half).**
  - The completion-triggered, debounced extraction pass (§4.2) via the existing
    `Narrator`/`HeadlessCmd`/local-LLM plumbing; **$0** on the local-LLM path.
  - Writes **`unverified`, timestamped, provenance-tagged** entries as **proposals**;
    verify-before-trust promotion; supersession/age-out. The committed diff is the human
    gate. **Never auto-trusted.**
  - $0-local on the same rig: complete a couple of agents, assert candidate entries are
    proposed (not silently trusted), and that a contradicting later entry supersedes an
    earlier one.

- **PR-3 (separable follow-on) — REPL #50 local grounding (the adjacent win).** Answer
  project questions from `.warden/memory.md` **locally** via the #50 REPL (`internal/repl`
  exists). This is the cleanest token lever in the whole feature: it **removes** cloud
  round-trips (vs. injection, which *adds* tokens) by serving "where does X live?" from
  the canonical memory with a local model. Separable and explicitly noted as a follow-on,
  not part of the core projection.

### 6.2 Explicitly kept dropped / out of scope

- **`wd init` registration gate — dropped (stays dropped).** Project keying is implicit
  (`git rev-parse --show-toplevel`, auto-create on first use). A registration wizard is a
  regression against zero-ceremony spawn ([[warden-adds-on-top-never-strips]]).
- **"Repo cleanliness" as a motivation — dropped (stays dropped).** State lives in
  `~/.warden`; the tree is already clean. Not a reason to build anything.
- **Per-project partitioning of `savings`/`spend`/`insights`** (today a global blob) —
  a **reporting nicety, out of scope for now**. It is not the memory system; build it
  only if the commingling is actually felt. Noted, not scheduled.
- **Generating/overwriting human native files** (CLAUDE.md/CONVENTIONS.md) — **rejected
  on principle** (§4.1): warden reads and respects them, never writes them.

### 6.3 DoD note (per CLAUDE.md)

This spec is a **planning artifact** — it changes no production code, so its own DoD is:
doc written, PR'd, **no tag, no release**. The DoD below applies to the eventual feature
PRs (PR-1…PR-3), walked per CLAUDE.md:

- **README / `docs/FEATURES.md` / root `FEATURES.md` matrix** — add "project memory"
  once PR-1 lands (two catalogs + website mirror, per [[features-catalog-structure]]).
- **`docs/USAGE.md`** and a new **`docs/specs/`** entry if the store format needs one.
- **Website** (`site/src/content/docs/`) — a `concepts/` page (the canonical-memory
  model) + a `guides/` page (curating `.warden/memory.md`) + a `reference/cli.md` entry
  mirroring `wd memory` `--help`.
- **Skill** (`skills/warden/`) — note that memory is projected automatically and how an
  agent should treat `unverified` entries (verify before relying).
- **CLI help** — the cobra `wd memory` command `Use`/`Short`/`Long`/flags in
  `internal/cli/`, kept in sync with `reference/cli.md`.
- **CLI/MCP parity** — if `wd memory` gets an MCP twin it goes in
  `internal/mcp/tools_extra.go` ([[features-catalog-structure]]).
- **Tag & release** — one tag per feature (patch for the projection slice; the batch may
  warrant a minor), per [[release-tagging-style]]; a `v*` push triggers GoReleaser —
  **confirm with the maintainer before pushing any tag**, batch ≤3.

### 6.4 Open questions left for impl-time

1. **Aider projection.** Aider degrade-skips today (no flag, no `ContextInjector`). A
   future `Aider.InjectContext` (write `CONVENTIONS.md`) or a `--read .warden/memory.md`
   launch arg would bring it to parity — confirm aider's read-only-context semantics on
   the rig before building; not a PR-1 blocker.
2. **Memory format.** Markdown bullet list (human-editable, diff-friendly) vs. a small
   structured front-matter-per-entry format (machine-parseable trust/TTL fields). Lean
   markdown-with-inline-tags (the §4.2 sketch) for reviewability; revisit if the curation
   pass needs richer structure.
3. **Curation model & trigger debounce window.** Which local model, what debounce
   interval, and whether to run curation in the daemon vs. a short-lived headless agent —
   resolve on the rig (latency/quality tradeoff), default to the $0 local-LLM path.
4. **Verify-before-trust corroboration signal.** Exactly which cheap deterministic checks
   promote `unverified → trusted` (path-still-exists is obvious; others?) — tune during
   PR-2.
5. **Size budget number.** The ≤~1.5–2 KB ceiling is a starting proposal; calibrate
   against real prompt-cache `cache_read` economics (current Claude rates via the
   `claude-api` skill) and BYO-model non-caching cost.
6. **Live-session memory surface.** If/when memory should be queryable/editable inside a
   running session or over the API, that is the one daemon-crossing extension — goes
   **spec-first** (§5).

---

## 7. Summary

- **Canonical source:** a **warden-owned, committed `.warden/memory.md`** (beside the
  existing `.warden/check.yml`), projected via the existing seam — **not** AGENTS.md-as-
  canonical (avoids writing auto-curated/poisonable content into a human, natively-read
  file) and **not** machine-local (loses team-shared/reviewable/travels-with-clone).
  Existing CLAUDE.md/AGENTS.md/CONVENTIONS.md are **read and respected, never generated or
  clobbered** (verify-before-trust). The committed source makes every curation change a
  reviewable diff — the strongest poisoning mitigation. The AGENTS.md-as-canonical
  alternative is documented for a deliberate maintainer override.
- **Projection:** rides the **existing #52 injection seam** — `SystemPromptFlag`
  (Claude, launch flag) + `ContextInjector` (six backends, file-drop into their
  AGENTS.md/CRUSH.md/.goosehints warden block). **7/8 backends project with zero new
  adapter code; aider degrade-skips.** Memory is just another guidance string in the
  existing `systemPromptHint`/`injectContext` assembly.
- **Additive interface:** **none on `Backend`.** The new surface is lifecycle/daemon-
  local: a neutral `internal/memory` store, one more guidance string, and a curation
  pass reusing the existing `Narrator`/`HeadlessCmd`/local-LLM plumbing. **Claude
  launch byte-identical when off/empty; no spec-first daemon API change** for the core.
- **Curation & freshness:** completion-triggered, **debounced** extraction (not a dump —
  digests capture *what changed*, not *where things live*) proposing **timestamped,
  provenance-tagged, `unverified`** entries; **verify-before-trust** promotion;
  supersession/age-out; the committed diff is the human gate. **Auto-curation ships
  behind the proposal gate, never auto-trusted.**
- **Cost discipline:** **compact + stable + navigational** — a hard size budget,
  spawn-boundary stability to preserve Claude's prompt cache (`cache_read ≈ 10%`), and a
  **backend-specific budget** (BYO-model aider/opencode/crush/goose may not cache → full
  tokens/turn → tightest cap / degrade-skip).
- **Phasing:** PR-0 thin `wd memory` reader → **PR-1 projection of human-curated memory
  (the core, zero-poison win, $0-local on codex)** → PR-2 gated auto-curation from
  digests ($0-local) → PR-3 separable REPL #50 local grounding (the token-*removing*
  adjacent win). Dropped items (`wd init` gate, repo-cleanliness, file generation, spend
  partitioning) stay dropped/out-of-scope.
