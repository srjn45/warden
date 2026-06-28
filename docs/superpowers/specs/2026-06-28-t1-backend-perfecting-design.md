# T1 Agent-Backend Perfecting Phase — Design

**Status:** ✅ Phase complete (steps 1–5 merged to `main`, #52) — step 6 (superpowers)
deferred as later/additive. Roadmap item #52, perfecting phase.
**Date:** 2026-06-28
**Scope:** the three "Tier-1" non-Claude backends — **Codex, Antigravity, Cursor**
**Predecessor:** `2026-06-27-pluggable-agent-backends-design.md` (the breadth-first
adapter rollout, now complete: 7 non-Claude backends merged + gap-documented).

> **Phase-complete status (2026-06-28).** The capability work has landed:
> step 1 (poller + approval routed through `Backend.DetectState`/`ParseApproval`,
> Claude regression-locked), step 2 (`DiscoverSessionID` discover-then-pin — wired in
> the poller; **codex** carries the per-backend discoverer, antigravity/cursor still
> dir-scope), step 3 (live state + approval markers for **codex, antigravity, cursor**),
> step 4 (cursor trust-prompt surfaced as an approval; transcript topology resolved —
> see §8 Q1), and step 5 (`InjectContext` rules-file injection across codex, opencode,
> antigravity, crush, cursor, goose — **aider intentionally skipped**, see its gap doc).
> Step 6 (surface each agent's superpowers) remains future/additive. Gap #4 (dollar
> pricing) was an intentional scope decision, not a closed gap: hosted backends
> (antigravity, cursor) stay tokens-only by design and codex carries no warden-side
> dollar rates yet. This DoD pass updates the gap docs, catalogs, and website to match.

> This is a **planning & design** document. No production code is changed by this
> spec. It scopes the gaps, the (modest) architecture changes, and the phase order
> for taking the T1 backends from "launches + receives a prompt + responds" to
> "near-Claude fidelity."

---

## 1. Goal & framing

The breadth-first rollout integrated every major terminal agent *minimally* — each
opens a tmux session, launches, receives its prompt, and (since PR #166) responds.
The **perfecting phase** closes the fidelity gap between those backends and Claude
Code, starting with the three the maintainer designated **T1**: Codex, Antigravity,
Cursor (Claude remains the reference and is already full-fidelity).

Guiding principle unchanged: **warden adds capability on top of an agent; it never
strips one down to a lowest common denominator.** Closing a gap means *surfacing* a
warden feature for that agent, never restricting the agent.

### Tiers (maintainer's split)
- **T1 (this doc):** antigravity, cursor, codex — perfect first.
- **T2 (next):** opencode, aider, crush, goose.

---

## 2. Architectural finding (the spine of this phase)

The `agentbackend.Backend` interface is **sound and does not need reshaping**. But an
audit shows **core does not yet fully use it** — two of the hardest seams still
scrape Claude-only markers and bypass the adapter entirely:

| Seam | Where | Status |
|---|---|---|
| State detection (working/idle/needs-input, rate-limit, crash) | `internal/poller/detect.go`, `poller/loop.go` | **Claude-hardcoded** (`claudeLimitBannerRe`, "esc to interrupt", restore-time regex). Never calls `Backend.DetectState`. |
| Approval detection | `internal/approval/` | **Claude-hardcoded** (box-drawing + numbered options). Never calls `Backend.ParseApproval`. |
| Transcript / digest | `internal/daemon/digest_routes.go` | ✅ Already routes through the backend; gates on `Caps.StructuredTranscript`. |

**Consequence:** every adapter *implements* `DetectState`/`ParseApproval`, but the
runtime never invokes them for non-Claude agents. This single wiring gap is the root
cause of every T1 gap doc's "state/approval degraded — warden infers idle from
staleness." The methods exist; the consumers don't call them.

**Implication for the phase:** the highest-leverage work is *finishing the
abstraction* (wire poller + approval through the interface), not adding new surface.

---

## 3. The five shared T1 gaps

All three T1 agents share the same five gaps (same root causes), plus per-agent
specifics (§4).

| # | Gap | Caps flag | Consequence today | Fix class |
|---|---|---|---|---|
| 1 | No live **state** detection | `DetectState`→Unknown | no working/idle/needs-input; no rate-limit/crash detect | core wiring (§2) + per-agent markers |
| 2 | No **approval** detection | `ParseApproval`→false | approvals inbox + auto-approve dead; operator must attach to pane | core wiring (§2) + per-agent markers |
| 3 | No **session-id pinning** | `SessionIDControl=false` | resume/transcript are dir-scoped, not exact-id (works only because 1 worktree/agent) | discover-then-pin (new optional method) |
| 4 | No **dollar pricing** | `Pricing=false` | `wd spend` tokens-only; `wd savings` omits agent | per-adapter pricing data |
| 5 | No **system-prompt injection** | `SystemPromptInject=false` | warden's collab/git/pipeline hints never reach the agent — **biggest multi-agent-coordination hit** | context-injection abstraction (AGENTS.md/rules file drop) |

---

## 4. Per-agent specifics

| | **Codex** | **Antigravity** | **Cursor** |
|---|---|---|---|
| Tier | A (cleanest) | A | **C (worst transcript)** |
| Transcript | ✅ rollout JSONL incl. tool/files | ⚠️ Tier A but **tool/file extraction unverified** (text-only fixture) | ❌ undocumented SQLite `store.db`; no export; stream-json parser built but **unwired** |
| Launch blocker | — | — | ⚠️ **workspace-trust prompt** blocks interactive launch (`--trust` headless-only) |
| Cost to iterate | ✅ **$0 local** (Ollama) | hosted free tier (~20 reqs/day) | hosted (existing plan) |
| Extra risk | — | internal subagents → cost attribution rolls up | double-worktree (already guarded) |
| Superpowers to surface (later) | `codex apply`, `codex review`, `fork`, `--output-schema`, sandbox modes | multi-vendor model menu, `/fork`, `/rewind`, Python SDK | parameterized models, `--auto-review`, cloud `worker`, `create-chat` (clean discover-then-pin hook) |

**Tension to name:** Cursor is "T1" by *agent quality* but the *least integrated*
(Tier C, a real launch blocker, hosted-only testing). Codex is the best-integrated
and cheapest to iterate → **codex is the pilot** for each phase step.

---

## 5. Architecture changes (modest, all additive)

Same pattern as the just-merged `PromptSeeder`: **optional interfaces, type-asserted,
Claude untouched.** No change to the registry, the neutral `Turn`/`State`/`Caps`
types, lifecycle's launch/resume flow, or the spec-first daemon API.

1. **Finish the interface wiring (the core change).** Route `poller` and `approval`
   through `Backend.DetectState` / `Backend.ParseApproval`. Wire Claude's adapter in
   first and **assert byte-identical behavior** (regression-lock), then non-Claude
   agents light up the instant their adapters carry real markers.
2. **`DiscoverSessionID(workdir) (id, ok)`** — new optional method. Lifecycle calls
   it post-launch, persists the minted id to the session → exact-id resume/transcript
   replaces dir-scoping. (Codex `session_meta`, Cursor `create-chat`/stream `session_id`,
   Antigravity `cache/last_conversations.json` all expose it.) This is the
   long-deferred "Option 2 / discover-then-pin."
3. **`InjectContext(workdir, text)`** — new optional method. For agents with no
   `--append-system-prompt`, deliver warden's collab/git/pipeline hints by dropping an
   `AGENTS.md` / rules file into the worktree instead of a CLI flag.
4. **(Cursor only) headless-capture transcript** — a launch mode that tees
   `cursor-agent -p --output-format stream-json` to an on-disk log the *already-built*
   parser reads → flips cursor to Tier A with **no SQLite dependency**.
   **Open sub-question (resolve during cursor impl):** stream-json is emitted only by
   the headless `-p` path, not the interactive TUI — so headless-capture is a
   *different launch topology* than an attachable interactive session. Decide:
   interactive-but-untranscribed vs headless-but-transcribed vs a dual mode.
5. **Pricing/token extraction** — mostly per-adapter data. Codex can carry OpenAI
   rates; hosted cursor/antigravity realistically stay tokens-only (billing is the
   user's plan). May need a token-from-transcript path (ctxtokens is Claude-calibrated).

---

## 6. Phase order (capability-first / horizontal)

The breadth rollout was *agent-first* (right for **adding** agents). Perfecting is
better **capability-first**: the gaps are shared and several need the *same* core
seam, so build it once and fan out. Codex ($0-local, Tier A) pilots each step.

1. **Core seam #1 — poller + approval → interface.** Claude regression-locked.
   *Unblocks state + approval + auto-approve for every backend at once.*
2. **Core seam #2 — discover-then-pin session-id** (`DiscoverSessionID`).
   *Unblocks exact-id resume/transcript.*
3. **Per-agent state/approval markers** — capture + implement `DetectState` /
   `ParseApproval`: codex (free) → antigravity → cursor.
4. **Cursor transcript + trust-prompt** — headless-capture mode (§5.4) + clear the
   workspace-trust launch blocker.
5. **Pricing/tokens + context-injection** (`InjectContext`, AGENTS.md) across the three.
6. **Superpowers** — surface each agent's extras as warden special features
   (`codex review` as a review step, `/fork` as a stronger snapshot, …). Purely
   additive; later.

Each step is independently shippable and PR'd against `main`, no tag until the
maintainer decides a release.

---

## 7. Testing approach (free-tier, document-and-resume)

Building/perfecting an adapter needs only enough live runs to *observe* state +
approval markers — **~3–6 agent requests each**, saved as pane-capture fixtures; the
parser is then written offline against the fixtures. **No paid subscription is
required:**

- **Codex** — $0-local on Ollama, unlimited iteration. Pilots every step.
- **Antigravity** — hosted **free tier** (already logged in, ~20 reqs/day). Capture
  markers within a day's free quota.
- **Cursor** — the maintainer's **existing plan** (already logged in); a few requests
  spend a small slice of the existing allowance, not a new purchase.

**Quota-exhaustion protocol (maintainer's instruction):** capture on the free
quota/allowance; if quota runs out mid-capture, **document the state reached** (which
markers are captured, which remain) and **resume when the quota resets**.

---

## 8. Open questions

1. **Cursor launch topology** (§5.4) — ✅ **RESOLVED:** keep cursor **interactive
   (attachable), Tier C**, and **defer** the headless-transcribed mode. Operators do
   need an attachable interactive cursor session, and forcing a headless-only
   transcribed launch would *strip* that — violating warden's "add capability on top,
   never strip the agent" principle. So cursor stays interactive (live state + approval
   detection already give warden eyes on it without a transcript), and the
   already-built `stream-json` parser waits for either a `store.db` reader or an opt-in
   headless-capture mode to flip it to Tier A — additive, no regression to the
   interactive session. Rationale recorded in `docs/agent-backends/cursor.md`.
2. **Antigravity subagent attribution** — internal subagents roll usage into one
   session; document the limitation rather than fake per-subagent numbers (carried
   from the predecessor spec §6.3).
3. **Approval-marker DSL** — once ≥3 adapters' `ParseApproval` share a shape, consider
   a small declarative marker DSL (predecessor spec §7 Q5). Lean: per-adapter code
   now, DSL only if the duplication is real.

---

## 9. Definition-of-Done (per CLAUDE.md, for the eventual feature)

Per-step PRs are code-only; the feature-level DoD applies when the phase lands:
- **Docs** — update each T1 gap doc (`docs/agent-backends/{codex,antigravity,cursor}.md`)
  flipping the now-resolved gaps; root `FEATURES.md` + `docs/FEATURES.md`; website
  `site/`; CLI help/`reference/cli.md` if `--backend` UX changes; skill if it changes
  how agents drive warden.
- **Tag & release** — confirm with the maintainer before pushing any `v*` tag.
