# Warden Orchestrator — Local-LLM Conductor (Thin-Translator) Design

**Date:** 2026-06-25
**Feature:** A warden-aware, local-LLM REPL that turns natural-language intent into
**confirmed** warden tool calls — spawning/monitoring/tearing down agents, driving
pipelines, and running the git/check lifecycle — without ever writing code itself. Lives
as a toggle mode of the cockpit's bottom-left master pane.
**Status:** 📋 **Proposed** — phased; every phase ships value independently.
**Estimated Effort:** Phase A (tool-calling seam) ~2 days · Phase B (orchestrator loop +
registry) ~3 days · Phase C (TUI pane toggle) ~1 day · Phase D (monitoring verbs) ~2 days.
**Builds on:** [2026-06-24-warden-orchestration-brain-design.md](2026-06-24-warden-orchestration-brain-design.md)
(the responsibility-transfer + local-provider work this rides on).

---

## North star: cheaper composition, not a chat wrapper

The orchestration-brain spec moved **mechanics** onto warden (git/check/isolation, the
`internal/llm` provider seam, the `Classify`/`Summarize` swaps). This feature spends that
groundwork on the *operator-facing* side: a local model that **composes and supervises**
multi-agent work from natural language.

The single goal is **reducing operator friction on multi-step orchestration** — "stand up
two agents on the auth refactor and a review pipeline behind them," "what's running and
what's stuck," "tear down everything that's done" — without spending Claude tokens to do
it. The local model is the keyboard shortcut for arrangements that are tedious to type, not
a smarter CLI for single commands.

### Where it earns its place — and where it does not

- **Earns it:** *composition* (fan out N agents + a pipeline from one sentence),
  *monitoring* (summarize the fleet, surface what's blocked), *teardown* (reap the finished
  ones). These are multi-call arrangements where the CLI is a chore.
- **Does NOT earn it:** single commands. `wd spawn …` is already one keystroke; routing it
  through local inference only adds latency and a chance of a malformed call. The
  orchestrator must never feel slower than the CLI it sits next to — so the raw shell stays
  one keypress away (Phase C is a *toggle*, not a replacement).

## Governing principle

> **The orchestrator translates intent into warden's existing tool calls and *shows them
> before it runs them*. It conducts; it never implements.** warden still owns mechanics
> (deterministic Go), Claude still owns judgment on code, and the local model owns exactly
> one new thing: turning a sentence into a confirmable sequence of warden tool calls.

This keeps the feature inside the orchestration-brain spec's safety envelope. That spec's
non-goals — *"interactive code generation by the local model"* and *"attempt-then-escalate
autonomous problem-solving"* — are **preserved verbatim**: the orchestrator has no
file-editing tools in its registry at all (see [Tool registry](#tool-registry-the-warden-surface-as-functions)),
and it does not decide irreversible things on its own (see [The confirm gate](#the-confirm-gate-the-load-bearing-invariant)).

### It hosts the operator's shell — and stays passive over it

The orchestrator REPL **runs on top of a real terminal shell**, the same way Claude Code's
prompt sits over a Bash session:

- A bare line is intent for the model. A **`!`-prefixed line is a raw shell command** the
  operator submits directly to the underlying shell — the orchestrator does not interpret,
  rewrite, or gate it; it just runs in the shell and the output streams to the operator.
- The orchestrator **observes** that command and its output (they enter its conversation
  context) and **reports it back exactly as-is** — no paraphrase, no summarization, no
  "interpretation" of what the operator ran. This mirrors the brain spec's additive-only
  rule: the model never rewrites the operator's actions or words.
- **On an error, the orchestrator does nothing.** A non-zero exit, a stack trace, a failed
  build — the orchestrator surfaces the output verbatim and then *waits*. It does not
  diagnose, propose a fix, spawn an agent, or take any other action until the operator
  explicitly asks ("what went wrong?", "fix that", "spawn someone on this"). Passive
  observation, operator-initiated action — always.

Note this does **not** put a shell tool in the model's hands: the `!` capability belongs to
the *operator*, not the registry. The orchestrator can read what the operator ran; it can
never run a shell command itself (see [Tool registry](#tool-registry-the-warden-surface-as-functions)).

---

## Thin translator vs. autonomous brain (scope decision)

Two readings of "orchestrator" differ by orders of magnitude in cost and risk:

| | **Thin translator (this spec)** | Autonomous conductor (out of scope) |
|---|---|---|
| What it does | NL → a sequence of warden tool calls, shown for confirmation | Watches agents, decides who's stuck, reassigns work unprompted |
| Judgment | none — operator confirms every mutation | the model makes delegation calls |
| Fallback story | clean: a bad/garbled plan is *rejected at the gate*, costs nothing | none — "it made a bad call" has no deterministic undo |
| 7B-model viability | yes, because every action is confirm-before-execute | no — this is exactly the Opus quality gap the brain spec warns about |

This spec commits to the **thin translator**. The autonomous conductor is explicitly
deferred and, if ever revisited, inherits the brain spec's bar for escalation: *objective
verification (tests green) + retry budget + transparent diff + one-key rollback.*

---

## Architecture: a second front-end onto the daemon

warden already has one programmatic front-end onto every capability the orchestrator
needs — the MCP server (`internal/mcp/server.go`), which wraps the daemon client. The
orchestrator is a **second front-end onto the same daemon client**, driven by a local-LLM
tool-calling loop instead of by Claude over MCP stdio. **No new business logic** — the
canonical implementations (`lifecycle.Commit/Push/Sync/Check`, `spawn`, pipelines, ctx
store, messaging) stay exactly where they are.

```
  operator types NL  ─▶  orchestrator REPL (master pane)
                              │
                              ├─ llm.Chat (local model, tool-calling loop)   ← Phase A
                              │     emits proposed tool call(s)
                              ▼
                         confirm gate  ──▶  operator approves / edits / rejects   ← Phase B
                              │ (approved)
                              ▼
                         tool registry  ──▶  same daemon client the MCP server uses
                              │
                              ▼
                         daemon  ──▶  lifecycle / store / pipeline / ctx (unchanged)
```

### Why reuse the daemon client and not MCP

The MCP path exists to expose tools to a *Claude* process over stdio. The orchestrator is
a Go process that already links the daemon client directly (as the CLI does), so it calls
the client in-process — no second MCP server, no stdio hop. The registry's job is purely
**schema translation**: present each client method as a function the local model can call,
and route the model's chosen call back to that method.

---

## Phase A — Tool-calling seam in `internal/llm`

Today `internal/llm/llm.go` is a one-method single-shot seam:

```go
type Completer interface {
    Complete(ctx context.Context, prompt string) (string, error)
}
```

…and `Ollama` posts to `/api/generate` (`internal/llm/ollama.go`). An orchestrator needs
**multi-turn chat with function-calling**. Add an *additive* seam — `Complete`,
`Classify`, and `Summarize` callers are untouched:

```go
// Chatter drives a multi-turn, tool-calling conversation. Returns either an
// assistant message (text) or one-or-more tool calls the caller must execute and
// feed back as tool results on the next turn.
type Chatter interface {
    Chat(ctx context.Context, msgs []Message, tools []ToolSchema) (Reply, error)
}

type Reply struct {
    Text      string     // assistant prose, when no tool call
    ToolCalls []ToolCall // structured calls the model wants to make
}
```

Implementation notes:

- Back it with Ollama's `/api/chat` (the `tools` field), which Qwen2.5-Coder and the
  other workhorse models from the brain spec support. Same tiny-client discipline as the
  existing `Ollama`: non-streaming, hard timeout, 1 MiB read cap, errors so the caller can
  bail.
- **Reliability floor.** 7B tool-calling is imperfect. The loop must defensively handle
  (a) malformed JSON args, (b) a call to a tool that isn't registered, (c) the model
  narrating a tool call in prose instead of emitting structured JSON. Each is a *recoverable
  turn*: feed the error back as a tool result ("that tool doesn't exist / args didn't
  parse") and let the model retry within a bounded turn budget, then give up gracefully to
  the operator. A garbled plan never executes — it's rejected, not run.
- Reuse the existing config: `local_llm` / `local_llm_url` / `local_llm_model` /
  `local_llm_timeout`. The orchestrator simply *requires* `local_llm` on (it has no
  deterministic fallback — it's an interactive surface, not a pipeline step).

## Phase B — Orchestrator loop + tool registry

A new `internal/orchestrator` package and a `warden orchestrator` (alias `wd orch`) CLI
subcommand running the REPL.

### The loop

1. Read an NL line from the operator.
2. Assemble context (system prompt = "you are warden's orchestrator; you conduct agents,
   you never edit code"; + a compact current-fleet snapshot from `list_agents`).
3. Run the `Chat` tool-calling loop until the model proposes a tool call or answers in
   prose.
4. **Read-only calls execute immediately** (see registry split below); **mutating calls
   hit the confirm gate**.
5. Feed tool results back; repeat until the model yields prose; print it.

### Tool registry: the warden surface as functions

The registry exposes the **operator-relevant** subset of the MCP surface
(`internal/mcp/server.go`), split by side-effect. This split is the safety backbone:

**Read-only (auto-execute, no gate):**
`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`,
`list_approvals`, `ctx_get`, `ctx_list`, plus pipeline status reads.

**Mutating (confirm gate required):**
`spawn_agent`, `adopt_agent`, `send_to_agent`, `send_message`, `terminate_agent`,
`delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set` /
`ctx_cas` / `ctx_append`, pipeline create/cancel/delete.

**Deliberately absent — there is no file-editing tool in the registry.** No `Edit`,
`Write`, `Bash`, no shell escape. This is the structural enforcement of "conducts, never
implements": the orchestrator *cannot* write code because the capability is not in its
hands. Code work is delegated by `spawn_agent`-ing a Claude agent, full stop.

Git/check go through the warden tools (`commit`/`push`/`sync`/`check`) for the same reason
the brain spec routes Claude through them — branch rails, hooks, bookkeeping, compact
results — and so the orchestrator's git actions land in the same agent-pinned audit trail.

### The confirm gate (the load-bearing invariant)

Every mutating call is rendered to the operator **before execution** as the concrete action
it will take, e.g.:

```
orchestrator wants to:
  spawn_agent(type=development, prompt="refactor auth token rotation", dir=…/warden)
  spawn_agent(type=development, prompt="add tests for token rotation",  dir=…/warden)
  pipeline create  review ← [the two above]
[a]pprove   [e]dit   [r]eject
```

- **approve** runs the call(s); **edit** lets the operator fix args before running;
  **reject** drops them and returns control to the prompt.
- Batched plans (the composition case) confirm as a unit so a multi-agent arrangement is
  one decision, not five.
- This reuses warden's existing supervised-spawn / approval machinery conceptually; where a
  spawned agent itself needs approvals, those still flow through the normal
  `list_approvals` / `approve` path.

Confirm-before-execute is **non-negotiable** and not config-gated for mutations — it is the
reason a 7B model is safe to put in this seat. (A future "trusted read-only autopilot" could
let *read* verbs stream without confirmation, which they already do.)

## Phase C — Cockpit master pane hosts the orchestrator-over-shell

The cockpit's bottom-left pane is the master **shell** (`internal/tui/compositor.go:83`,
the `masterID` split running `$SHELL`). It is load-bearing beyond decoration: it's where
the operator types `wd` commands and, critically, its **cwd seeds where `n`-spawned agents
launch** (`compositor.go:92` comment). The orchestrator does not *replace* that shell — it
**wraps** it:

- The pane runs `warden orchestrator`, which itself owns a persistent child shell session
  (started in `masterCwd`). Bare lines go to the model; `!`-lines go to that shell (see
  [It hosts the operator's shell](#it-hosts-the-operators-shell--and-stays-passive-over-it)).
  Because the shell is a real persistent session, **`cd` and environment persist across
  `!` commands**, and its cwd still seeds `n`-spawned agents — spawn-dir semantics are
  preserved, not lost.
- Mechanically the only `buildCockpit` change is the pane's command: `self + " orchestrator"`
  instead of bare `$SHELL`. Layout, ids, and navigation bindings are untouched. The
  orchestrator process is responsible for the embedded shell (PTY), not tmux.
- **Escape hatch to a raw shell** stays available via the already-planned
  [2026-06-09-tui-master-pane-shell-toggle](../plans/2026-06-09-tui-master-pane-shell-toggle.md)
  mechanism — one keypress drops the pane to a bare `$SHELL` for anyone who wants the
  orchestrator out of the way entirely. The `orchestrator` config flag (below) sets which
  face the pane starts on.
- The orchestrator can run standalone (`wd orch` in any terminal) too — the cockpit pane is
  just its most natural home.

### Embedded-shell mechanics

The whole point is **warden orchestration on top of the operator's *own* shell** — not a
reimplemented mini-shell. So `!` runs in the operator's real `$SHELL`, started as a login/
interactive shell that sources their normal config (rc/profile), so **aliases, functions,
`PATH`, env, and command behavior are identical to what they'd get typing in their own
terminal** — including `--help` / usage output, which is just the underlying tool's own
output passed through untouched. warden adds capabilities above the shell; it changes
nothing about the shell.

Hosting it from a Go REPL means a PTY: the orchestrator allocates a pty for the child
`$SHELL`, writes `!`-line input to it, and tees the output **both** to the operator's screen
(verbatim, live) **and** into a capture buffer the model reads as context. Two things to get
right:

- **MVP scope: `!` is non-interactive only.** The MVP restricts `!` to non-interactive
  commands — it runs the command, streams output to completion, and returns to the prompt.
  Full-screen / interactive programs (a pager, `vim`, a REPL, anything that takes over the
  terminal) are **out of scope for the MVP**; wiring the pty through to the operator's
  terminal for the command's lifetime and handing control back on exit is a deliberate
  follow-up. (For now those are run in the raw-`$SHELL` escape-hatch pane.)
- **Capture bounds.** The buffer the model sees is tail-truncated under a hard cap (reuse the
  brain spec's `maxCheckOutputLines`/`truncateTail` discipline) so a chatty `!` command can't
  blow the local model's context — the operator always sees the full stream regardless.

## Phase D — Monitoring verbs (the second half of the value)

Composition is half the win; **supervision** is the other half. Add first-class NL handling
for fleet questions, implemented as read-only registry calls + a local-model summarization
pass (reusing `lifecycle.Summarize`, already routed through the local model in the brain
spec's Phase 1b):

- "what's running / what's stuck" → `list_agents` + per-agent status, condensed.
- "what's agent X doing" → `get_agent_output` tail, summarized.
- "anything waiting on me" → `list_approvals` + `read_inbox`.
- "clean up" → propose `terminate`/`delete` of terminal agents through the confirm gate.

These are exactly the high-frequency, low-stakes reads where a local model's summarization
is good enough and Claude tokens would be wasted.

---

## Hardware-aware model selection & capability tiers

The brain spec defaults `local_llm_model` to `qwen2.5-coder:7b`, which needs ~5-6 GB of
VRAM (Q4) and **won't fit a 4 GB GPU**. A small machine should run a 1.5B/3B model — but a
1.5B model cannot reliably plan a multi-agent arrangement. The resolution is two coupled
mechanisms: **suggest a model from the hardware**, and **declare a capability tier per
feature so an under-capable model degrades gracefully instead of emitting a confident-wrong
plan.**

### Suggest the model from system config

At setup / `wd doctor` time, detect available VRAM (and fall back to system RAM for
CPU/unified-memory machines) and **recommend** — never silently force — a `local_llm_model`:

| Detected VRAM (Q4) | Recommended workhorse | Notes |
|---|---|---|
| ≥ 20 GB | `qwen2.5-coder:32b` | full headroom |
| ~10 GB | `qwen2.5-coder:14b` | the brain spec's upper tier |
| ~6 GB | `qwen2.5-coder:7b` | brain spec default |
| **~4 GB** | **`qwen2.5-coder:3b`** | this machine; 1.5B as the safe floor |
| ≤ 2 GB | `qwen2.5-coder:1.5b` | summarize/classify only |

Detection is best-effort and platform-specific (`nvidia-smi` query, `rocm-smi`, Apple
unified memory via `sysctl`); on any failure it suggests the conservative 1.5B and lets the
operator override. warden **recommends and the operator sets** `local_llm_model` — never an
automatic silent swap, consistent with the brain spec's "never paraphrase the operator's
intent" posture applied to their config.

### Tier the features, degrade when the model can't reach them

Each orchestrator capability declares the model strength it needs. A static model→tier
table tells warden what the *configured* model can do; when a request needs a tier above the
model's, warden **escalates or degrades — it never lets a small model attempt a big task and
ship the result through the gate as if it were sound.**

| Tier | Min model | Capability | Below-tier behavior |
|---|---|---|---|
| **T0 — read/summarize** | any (≥1.5B) | fleet summarize, classify, single-field extract (Phase D, brain-spec tasks) | already covered: local → headless Claude → deterministic floor (brain spec) |
| **T1 — single action** | ~3B | NL → **one** warden tool call (`spawn one agent`, `commit`) with clean args | escalate the single planning call to Claude; else surface the proposed CLI command for the operator to run |
| **T2 — composition / planning** | ~7B | multi-agent fan-out, pipeline construction, dependency reasoning | escalate planning to Claude; else refuse and suggest manual decomposition into T1 asks |

Model→tier floor (Qwen2.5-Coder family, Q4): `0.5–1.5B → T0` · `3B → T0–T1` ·
`7B → T0–T2` · `14B+ → T0–T2 with reliability headroom`.

### How a request is routed to a tier

1. A **cheap T0 complexity pre-classification** (a one-shot classify any model can do, the
   same seam as the brain spec's `Classify`) buckets the incoming request's needed tier
   before the expensive planning turn.
2. If needed-tier ≤ model-tier → plan locally.
3. If needed-tier > model-tier → **escalate** the single planning step to headless Claude
   (the brain spec already shells `claude -p`), which returns the *same* confirm-gate plan —
   so only the rare hard *plan* spends a few Claude tokens; **execution is still token-free
   warden tool calls.** This keeps the north star intact: warden never makes Claude do the
   mechanical work, only the occasional plan a small model can't form.
4. If escalation is disabled or Claude is unavailable → **degrade, never fake it:** tell the
   operator the task is beyond the local model and hand them the CLI / suggest breaking it
   into smaller asks. The Phase-A reliability floor (malformed-call → bounded retries) is the
   runtime backstop; tiering avoids wasting the operator's time on plans the model will botch
   in the first place.

This is **planning-quality escalation, gated by the confirm gate** — it does *not* cross
into the brain spec's deferred attempt-then-escalate autonomy: the operator still confirms
every mutation, whoever drafted the plan.

---

## Config

Reuses the brain spec's `local_llm*` block (`local_llm`, `local_llm_url`,
`local_llm_model`, `local_llm_timeout`); orchestrator requires `local_llm` on. New keys,
all global *policy* only (never a command), consistent with the brain spec's config rules:

- `orchestrator` (default off) — whether the cockpit master pane *starts* in orchestrator
  mode vs. shell mode. The toggle keypress works regardless; this only sets the initial face.
- `local_llm_escalate` (default on) — when a request needs a capability tier above the
  configured model, escalate that one planning step to headless Claude. Off ⇒ degrade to the
  operator instead (no Claude tokens). The *gate still confirms every mutation* either way.
- `local_llm_tier` (default auto) — the configured model's capability tier (T0/T1/T2),
  derived from the model→tier table; an explicit override for models warden doesn't know.

`local_llm_model` itself stays operator-set; warden only *recommends* a value from detected
hardware at setup / `wd doctor` time (see
[Hardware-aware model selection](#hardware-aware-model-selection--capability-tiers)).

## Non-goals

- **Any code editing by the local model.** Enforced structurally — no edit/write/bash tool
  in the registry.
- **Autonomous (unconfirmed) mutation.** The confirm gate is mandatory for every
  side-effecting call.
- **Autonomous conductor** (deciding delegation/reassignment unprompted) — deferred; would
  inherit the brain spec's escalation bar.
- **Acting on `!` command output unprompted.** The orchestrator observes and reports
  verbatim; on error or otherwise it takes no action until the operator asks. No
  auto-diagnose, auto-fix, or auto-spawn.
- **Interpreting/rewriting the operator's `!` commands or their output.** Passthrough is
  literal; reporting is verbatim (additive-only, per the brain spec).
- **Replacing the master shell.** The orchestrator hosts a real shell underneath (`!`
  passthrough), and a raw-`$SHELL` escape hatch stays one keypress away.
- **A second MCP server.** The orchestrator links the daemon client in-process.
- **Running an under-capable model on an over-tier task and shipping the result.** A
  request above the model's tier escalates or degrades; it is never attempted-and-faked.
- **Auto-swapping `local_llm_model` from detected hardware.** warden recommends; the
  operator sets.

## Open questions

- **Plan granularity at the gate.** When the model proposes a 5-call arrangement, confirm
  as one batch (fewer interruptions) vs. step-through (finer control)? Lean batch with an
  expand-to-step affordance.
- **Context budget for the fleet snapshot.** How much of `list_agents` to feed each turn
  before it crowds a 7B context window — likely a compact one-line-per-agent digest, not
  full records.
- **Model floor / tier boundaries.** Where exactly do T1 and T2 fall on the Qwen2.5-Coder
  ladder? The table puts T2 at ~7B, but that needs measuring tool-call validity rate on a
  fixed prompt set per model size before the model→tier map is trusted — especially the
  3B→T1 and 7B→T2 thresholds that decide when a 4 GB machine escalates.
- **Complexity pre-classification accuracy.** Routing to a tier relies on a T0 classifier
  judging a request's complexity *before* planning. How reliable is that on a small model,
  and what's the failure mode if it under-rates a request (a small model attempts a T2 task)?
  The Phase-A reliability floor catches malformed output, but a *plausible-but-wrong* plan is
  the residual risk the confirm gate must catch.
- **Escalation cost vs. north star.** `local_llm_escalate` spends Claude tokens on hard
  plans — rare by design, but worth metering: if a 4 GB machine escalates constantly, the
  honest answer may be "this machine should run T0/T1 only and type T2 arrangements via the
  CLI," not "escalate every time."
- **Standalone auth.** `wd orch` outside the cockpit needs the daemon token like any client
  — confirm it reads the same `~/.warden/daemon.env` path the rest of the CLI uses.

---

## Phase ordering & dependency summary

Build strictly **A → B → C → D**. Each phase is independently shippable and leaves warden in
a working state; the order is a hard dependency chain, not a preference.

```
A ── tool-calling seam (internal/llm.Chatter)
│        prerequisite: brain spec Phase 1a (llm provider + Ollama client)
▼
B ── orchestrator loop + registry + confirm gate  (wd orch REPL)
│        prerequisite: A (needs Chat) + brain spec Phase 0b/0c (commit/push/sync/check tools)
│        ⇒ first usable, standalone-runnable milestone
├──────────────▶ C ── cockpit pane hosts orchestrator-over-shell (+ ! passthrough)
│                        prerequisite: B; touches only buildCockpit's pane command
│                        independent of D
└──────────────▶ D ── monitoring verbs (fleet summarize / triage / cleanup)
                         prerequisite: B (registry) + brain spec Phase 1b (Summarize)
                         independent of C
```

| Phase | Depends on | Unblocks | Ships on its own as |
|---|---|---|---|
| **A** — `Chatter` seam | brain 1a (provider) | B | a tool-calling local-LLM client (no user surface yet) |
| **B** — loop + registry + gate | A; brain 0b/0c (git/check tools) | C, D | `wd orch` — composition via NL, confirm-before-execute |
| **C** — cockpit pane + `!` shell | B | — | orchestrator-over-shell as the master pane's face |
| **D** — monitoring verbs | B; brain 1b (`Summarize`) | — | NL fleet supervision / triage / cleanup |

Key points:

- **A is the only net-new infrastructure.** It extends `internal/llm` with a chat /
  tool-calling interface alongside the existing single-shot `Completer`; everything after it
  is composition of capabilities warden already has.
- **B is the first shippable product** and is fully usable standalone (`wd orch` in any
  terminal) before the TUI work — proving the loop, registry, and gate without touching the
  cockpit.
- **C and D both fan out from B and are mutually independent** — they can be built in either
  order or in parallel once B lands. C is UI-only (one pane-command change in `buildCockpit`
  + the embedded PTY shell); D is registry-only (read verbs + summarization).
- **External dependencies all point back at the orchestration-brain spec:** A needs its
  provider seam (1a), B needs its git/check tools (0b/0c), D needs its `Summarize` routing
  (1b). If those phases aren't all in by the time this starts, A/B can proceed first and
  C/D wait on 1b — but none of this work should begin before brain Phase 1a exists.

**Where capability-tiering lands (cross-cutting):** the tier-routing (pre-classify →
escalate/degrade) is part of **Phase B**'s loop — it's how B decides whether to plan locally
or escalate, so B is where it's built and tested. The **hardware detection + model
recommendation** is a standalone `wd doctor` / setup enhancement with no dependency on A–D;
it can ship independently at any time (and ideally lands early so a small machine is steered
to a fitting model before the orchestrator is ever run).
