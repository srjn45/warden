# Pluggable Agent Backends (beyond Claude Code) — Design

**Status:** Draft / design pass (roadmap item #52)
**Date:** 2026-06-27
**Branch:** `design/pluggable-agent-backends`
**Author:** design pass (Claude Code agent, isolated worktree)

> This is a **planning & design** document. No production code is changed by this
> spec. It exists to scope the abstraction, pick the first backends, and surface
> the hard problems before any implementation PR is opened.

> **Scale note — this is a large, ambitious upgrade.** Generalizing warden's
> agent layer touches ~10 subsystems (lifecycle, digest, savings, ctxtokens,
> approval, spend, repl). It is deliberately phased (§8) so each step ships value
> independently: the interface extraction is zero-behavior-change, the first
> backend proves the shape, and every subsequent agent is an isolated adapter PR.
> The goal is **one agent first (Antigravity CLI), then an adapter per agent over
> time** — the full catalog in §11 is the long-horizon target, not a single PR.

---

## 1. Goal

Today warden is tightly coupled to the `claude` binary. The spawn / attach /
lifecycle / digest / savings / approval machinery all assume Claude Code's
command-line shape, transcript format, and prompt UI.

**Goal:** factor the agent layer behind an `AgentBackend` interface so warden can
drive *other console-based coding agents* the same way it drives Claude Code —
selected per agent at spawn time — while Claude Code remains the reference
implementation and the default.

**Why it matters:** this is the single biggest lever on adoption. Every developer
running a terminal coding agent becomes a potential warden user, not just Claude
Code users.

### Non-goals (this pass)

- Driving GUI/IDE agents (Cursor IDE, Antigravity *desktop*, Cline-in-VS-Code).
  Scope is **console** agents only.
- Multi-provider billing/pricing accuracy as a hard requirement (see §6 — cost
  features degrade gracefully when a backend's pricing/tokenizer is unknown).
- Re-homing warden's own state (that's roadmap item #53, `wd init`).

---

## 2. Decisions locked for this pass

These were settled during the design brainstorm:

| Decision | Choice |
|---|---|
| Abstraction shape | **Full `AgentBackend` interface + capability flags** (not config-only) |
| Reference impl | **Claude Code** — refactor existing code into the first backend |
| Mechanical proof backend | **Aider** — Tier-C, easiest, validates the interface shape |
| Headline non-Claude target | **Antigravity CLI** (`agy`) — Tier-A, exercises transcript + state seams |
| Priority order | Claude (done) → Aider (proof) → Antigravity → Codex CLI → OpenCode |
| Degradation policy | Features gated on capabilities; missing capability ⇒ degrade or disable, never crash |

---

## 3. The coupling surface (what "tightly coupled to Claude" means in code)

Audited the existing tree. Claude leaks into warden at ~10 seams, split by
difficulty:

### Easy seams — pure command-string construction
- **Launch** — `internal/lifecycle/lifecycle.go` builds
  `claude --model X --permission-mode Y --session-id Z --name N`
  (`claudeBase` / `claudeLaunch`).
- **Resume** — `claudeResume`: `claude --resume <sessionID>`.
- **Headless one-shots** — `claude -p <arg>` for classify/summarize. *Already
  half-abstracted:* warden has a local-LLM fallback (`internal/repl/tier_wiring.go`,
  `lifecycle.go` classify/summarize) so this seam tolerates a missing backend
  headless mode.
- **Install hints / binary detection** — `internal/lifecycle/runner.go`
  `commandInstallHint` hardcodes the `claude` install curl.

### Hard seams — semantic, per-backend implementation needed
- **Transcript location + format** — `claudeProjectDir` resolves
  `~/.claude/projects/<slug>/<session>.jsonl`; `internal/digest/parse.go` parses
  Claude's exact JSONL schema (`type`, `message.role`, content blocks
  `text`/`tool_use`/`tool_result`). Feeds **digests, narration, savings, token
  counting** (`internal/ctxtokens`, `internal/savings`). **This is the single
  biggest coupling.**
- **Needs-input / approval detection** — `internal/approval/approval.go`
  *scrapes the tmux pane* for Claude's box-drawing prompt UI (`❯`, numbered
  options via `optionRe`, "Do you want…", box chars `│┃|─╭╮╰╯`). Every agent
  draws approvals differently.
- **Idle vs. working detection** — same pane-scraping approach, keyed on Claude's
  "esc to interrupt" and status strings.
- **Models + pricing + token calibration** — `internal/spend/pricing.go`,
  `internal/savings/*`, `internal/ctxtokens` carry Claude model IDs, a Claude
  pricing table, and token estimates calibrated against Claude's `count_tokens`.
- **System-prompt injection** — warden injects collab/git/pipeline hints via
  Claude flags (`collabHint`, `gitConventionsHint`, `pipelineHint` in
  `lifecycle.go`). Other agents inject (or don't) differently.

### Honest read
> Spawn / attach / tmux lifecycle / git lifecycle **generalize cleanly**.
> Digests, savings, approval-detection, and pricing are **Claude-shaped** and need
> per-backend implementations or graceful degradation. That capability gap is the
> spine of the whole design.

---

## 4. The adapter layer: `AgentBackend` interface

The abstraction is an **adapter layer**: one `Backend` adapter per CLI agent,
each normalizing that agent's command shape, transcript format, and prompt UI
into warden's neutral contract. Warden core talks only to the interface; it never
again references `claude` (or any binary) directly.

```
            ┌─────────────────────────────────────────────┐
 warden core│ lifecycle · digest · savings · approval ·    │
 (neutral)  │ ctxtokens · spend · repl                     │
            └───────────────────────┬─────────────────────┘
                                     │ Backend interface (neutral Turn/State/Caps)
        ┌────────────┬───────────────┼───────────────┬────────────┐
        ▼            ▼               ▼               ▼            ▼
   claude.go    aider.go      antigravity.go    codex.go    opencode.go   …adapters
   (Tier A)     (Tier C)      (Tier A)          (Tier A)    (Tier A)
   reference     proof         headline #1
```

Proposed home: `internal/agentbackend/` (registry + interface + per-backend
adapter impls under `backends/`). Lifecycle takes a `Backend` rather than
hardcoding `claude`.

```go
package agentbackend

// Backend describes how to drive one console coding agent.
type Backend interface {
    // Identity
    ID() string            // "claude", "aider", "antigravity", ...
    DisplayName() string
    Binary() string        // "claude", "aider", "agy"
    InstallHint() string

    // Launch / resume — strings typed into a tmux pane and run by a shell.
    // Must shell-quote untrusted values (see lifecycle.go shellQuoteArg today).
    LaunchCmd(opts LaunchOpts) string
    ResumeCmd(opts ResumeOpts) (cmd string, ok bool) // ok=false ⇒ no resume

    // Headless one-shot for classify/summarize. ok=false ⇒ use local-LLM path.
    HeadlessCmd(prompt string) (argv []string, ok bool)

    // Transcript: where it lives + how to parse it into warden's neutral turns.
    TranscriptPath(projectsDir, workdir, sessionID string) (path string, ok bool)
    ParseTranscript(r io.Reader) ([]Turn, error)

    // State detection from a captured tmux pane.
    DetectState(pane string) State            // Idle | Working | NeedsInput | Unknown
    ParseApproval(pane string) (*Approval, bool)

    // Optional system-prompt / context injection appended to LaunchCmd.
    SystemPromptFlag(text string) (fragment string, ok bool)

    // Pricing / tokenizer metadata for spend & savings (ok=false ⇒ degrade).
    Pricing() (PricingTable, bool)

    Capabilities() Caps
}

type Caps struct {
    Resume             bool
    Headless           bool
    ModelSelection     bool
    PermissionModes    []string // backend-native approval modes
    StructuredTranscript bool   // gates digests / savings / token counting
    SystemPromptInject bool
    SessionIDControl   bool     // can we *assign* the session id (Claude --session-id)?
}

// Turn is warden's neutral transcript record — backends normalize INTO this.
type Turn struct {
    Role      string   // "user" | "assistant" | "tool"
    Text      string
    ToolName  string
    Files     []string // files touched (for digest "what changed")
    Timestamp time.Time
}
```

### Registry

```go
var registry = map[string]Backend{}
func Register(b Backend) { registry[b.ID()] = b }
func Get(id string) (Backend, error)
func Default() Backend // "claude"
```

`internal/lifecycle` resolves a `Backend` from the agent's record (a new
`backend` field on the session, default `"claude"`) and calls interface methods
instead of literal `claude …` strings.

---

## 5. Capability tiers (graceful degradation)

Not every agent can do everything. Features degrade by capability, never crash.

| Tier | Definition | Examples | What works | What degrades |
|---|---|---|---|---|
| **A — full fidelity** | Structured transcript on disk + headless + resume + model select | Claude Code, Antigravity CLI(*), OpenCode | Everything: digests, savings, token counts, narration, approval detection | — |
| **B — interactive only** | tmux session, no parseable transcript | (any TUI agent without on-disk logs) | spawn, attach, lifecycle, git, pipelines, send-keys | digests/savings → pane-scrape fallback or **disabled**; token counts → heuristic only |
| **C — headless-friendly** | Strong non-interactive mode, git-native, simple session model | Aider | spawn (interactive *or* `-p`), git lifecycle, classify/summarize | resume / structured digest may be limited |

(*) Antigravity transcript store location is **unconfirmed** — see §7 open
questions. If it's parseable it's Tier A; if not, Tier B.

**Degradation rules:**
- `!Caps.StructuredTranscript` ⇒ digest narrator falls back to a pane-scrape
  summary (lower fidelity) and savings tracking is **disabled** for that agent
  (savings requires real token deltas).
- `!Caps.Headless` ⇒ classify/summarize always use the local-LLM path
  (already exists).
- `!Caps.Pricing` ⇒ `wd spend` shows tokens (heuristic) but not dollars for that
  backend; `wd savings` omits the agent.
- `!Caps.Resume` ⇒ `rotate`/handoff re-spawn fresh instead of `--resume`.

---

## 6. Per-backend mapping

### 6.1 Claude Code (reference impl, Tier A) — *already shipped, refactor only*

| Seam | Value |
|---|---|
| Binary | `claude` |
| Launch | `claude --model <m> --permission-mode <mode> --session-id <id> --name <n>` |
| Resume | `claude --resume <id>` |
| Headless | `claude -p <arg>` |
| Transcript | `~/.claude/projects/<slug>/<id>.jsonl` (JSONL, parsed today) |
| Approval UI | box-drawing + numbered options (`approval.go` today) |
| Permission modes | `auto`, `acceptEdits`, `bypassPermissions` |
| Caps | all true |

Work = mechanical extraction of existing code into `backends/claude.go`. No
behavior change; existing tests become the backend's tests.

### 6.2 Aider (proof backend, Tier C)

| Seam | Value / note |
|---|---|
| Binary | `aider` |
| Launch | `aider --model <m> [--yes-always for auto]` (interactive TUI in tmux) |
| Headless | `aider --message "<prompt>"` / `--message-file` (non-interactive) |
| Resume | continues from chat history in repo by default; **no session-id assignment** ⇒ `SessionIDControl=false`, `Resume` weak |
| Transcript | `.aider.chat.history.md` (markdown) + `.aider.input.history` in repo root — **parseable but markdown, not JSONL** ⇒ needs a markdown parser into `[]Turn`; or treat as Tier B if not worth it |
| Approval UI | simple y/n prompts ("Apply edit? (y)es/(n)o") — much simpler regex than Claude's |
| Permission modes | `--yes-always` (auto) vs default (prompt) |
| Notable | git-native: auto-commits each change → plays well with warden git lifecycle; repomap is its edge |

Aider is the **proof** because its launch/headless/approval seams are simple, so
it validates the interface shape with minimal risk. Its transcript is markdown —
a good forcing function for `ParseTranscript` returning neutral `Turn`s from a
non-JSONL source.

### 6.3 Antigravity CLI (`agy`) (headline target, Tier A pending transcript)

Google's Gemini CLI successor (Gemini CLI retired 2026-06-18). Built in Go.

| Seam | Value / note |
|---|---|
| Binary | `agy` |
| Launch | `agy` → full TUI (conversation pane, `>` prompt, status bar) — **tmux-drivable** |
| Headless | `agy -p "<prompt>"` + `--output-format json` |
| Resume | `-c`/`--continue` (latest) or `--conversation <id>` |
| Model select | `--model`; `/model` slash command mid-session |
| Permission | `--headless` + `--approve <policy>` (`--approve all` = auto) |
| Auth | SSH/headless prints auth URL + one-time code (handshake) |
| Transcript | **❓ UNCONFIRMED** — Gemini used `~/.gemini/tmp/<hash>/chats/`; Antigravity path TBD |
| Caps | Resume ✓, Headless ✓, ModelSelection ✓, StructuredTranscript ❓ |

**Two real design risks (unique to Antigravity):**
1. **Internal subagents.** `agy`'s main agent auto-spawns parallel subagents
   (shown in the status bar). Warden drives the *top-level* session and treats it
   as one warden agent — fine for lifecycle — but it muddies:
   - **idle/working detection** (status bar shows running subagents; different
     signal than Claude's "esc to interrupt"), and
   - **per-agent token/cost attribution** (subagent usage rolls into one
     session — `wd spend` granularity drops).
2. **Orchestration overlap.** Antigravity is "agent-first" and overlaps warden's
   own orchestration. We must be clear that warden drives `agy` as a *single*
   coding session, not as a competing orchestrator. Document this boundary.

### 6.4 Codex CLI (next, Tier A) — *scoped, not first*

OpenAI's agent; #1 on Terminal-Bench 2.1. `codex -p`/exec mode, JSON output,
session resume. Map fully in its own follow-up once Antigravity lands.

### 6.5 OpenCode (next, Tier A) — *scoped, not first*

SST's agent; most-starred OSS (~176k). Technically the cleanest fit:
**SQLite session store + JSON export**, `run`/`--session`/`--continue`/
`--format json`, ACP server (ND-JSON over stdin/stdout), `opencode serve`
headless server. Strong future Tier-A backend; the SQLite store means
`ParseTranscript` reads a DB, not a file — a good generalization test.

---

## 7. Open questions (need confirmation before implementation)

1. **Antigravity transcript store** — where does `agy` persist conversations, and
   is the format parseable into `Turn`s? Determines Tier A vs B. *Action: install
   `agy`, run a session, inspect `~/.antigravity` / `~/.config/antigravity`.*
2. **Aider transcript fidelity** — is `.aider.chat.history.md` rich enough for
   useful digests, or do we declare Aider Tier B (no structured digest)?
3. **Backend selection UX** — global default + per-spawn override. Flag name?
   (`--backend antigravity` / `wd spawn --agent aider`). Config key in
   `~/.warden`? (ties into #53.)
4. **Per-backend pricing** — do we ship pricing tables for non-Claude models, or
   start with "tokens only, no dollars" for non-Claude and add pricing later?
5. **Approval detection generalization** — keep per-backend `DetectState`/
   `ParseApproval`, or extract a small declarative DSL (prompt markers + option
   regex) so simple agents need only data, not code? (Lean: per-backend code now,
   DSL later if ≥3 backends share a shape.)
6. **Skill update** — `skills/warden/` guidance assumes Claude. Does the skill
   need per-backend notes, or stay Claude-centric with a "backends differ" note?

---

## 8. Implementation phases (proposed)

1. **Phase 0 — extract interface, zero behavior change.** Introduce
   `internal/agentbackend`, move Claude into `backends/claude.go`, route lifecycle
   through the registry. All existing tests pass unchanged. *No new backend yet.*
2. **Phase 1 — Aider (proof).** Implement the Aider backend end-to-end: launch,
   headless, simple approval regex, markdown transcript → `Turn`. Add
   `--backend` selection plumbing + capability-gated degradation. This is where
   the abstraction is validated.
3. **Phase 2 — Antigravity (`agy`).** Resolve transcript open question; implement
   Tier-A backend; handle subagent status-bar state detection; document the
   orchestration boundary. Headline launch.
4. **Phase 3 — Codex CLI, then OpenCode.** Each its own PR, reusing the now-proven
   interface. OpenCode's SQLite store exercises a non-file transcript source.

Each phase is independently shippable. Phase 0 must merge before any backend.

---

## 9. Definition-of-Done checklist (per CLAUDE.md, for the eventual feature)

When a backend actually ships (not this design doc):
- **Tag & release** — one tag per backend (minor for the abstraction + first
  backend; patch per subsequent backend). Confirm before pushing the `v*` tag.
- **Docs** — `README.md` (supported-agents list), `docs/FEATURES.md` +
  root `FEATURES.md` matrix, `docs/USAGE.md` (backend selection), website
  (`site/src/content/docs/` guide + `reference/cli.md`).
- **CLI help** — `--backend` flag help in `internal/cli/`, kept in sync with
  `reference/cli.md`.
- **Skill** — `skills/warden/` if backend selection changes how agents drive
  warden.

---

## 11. Feature differences & gaps matrix

The crux of the adapter work is that agents disagree on exactly the seams warden
depends on. This matrix is the gap analysis — **each ✗ / ❓ is an adapter task or
a degradation decision.** (Compiled from vendor docs + landscape research,
mid-2026; ❓ = unconfirmed, verify during that adapter's implementation.)

| Capability (warden seam) | Claude Code | Antigravity (`agy`) | Aider | Codex CLI | OpenCode | Gemini CLI† |
|---|---|---|---|---|---|---|
| Console/TUI (tmux-drivable) | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Headless one-shot | `claude -p` | `agy -p` | `--message` | `exec`/`-p` | `run` | `-p` |
| Structured output (JSON) | ✓ (JSONL) | ✓ `--output-format json` | ✗ (md) | ✓ | ✓ `--format json` | ✓ |
| Assignable session id | ✓ `--session-id` | ✗ (auto) | ✗ | ❓ | ✓ `--session` | ✗ |
| Resume by id | ✓ `--resume` | ✓ `--conversation` | ✗ (repo history) | ✓ | ✓ `--continue` | ✓ |
| Model selection flag | ✓ | ✓ | ✓ | ✓ | ✓ `provider/model` | ✓ |
| Permission/approval modes | 3 modes | `--approve <policy>` | `--yes-always` | sandbox policy | per-tool | `--yolo` |
| Transcript store | `~/.claude/projects/**.jsonl` | ❓ (`~/.antigravity`?) | `.aider.chat.history.md` | ❓ | **SQLite DB** | `~/.gemini/tmp/<hash>/chats/` |
| Transcript format | JSONL | ❓ | Markdown | ❓ | SQL rows | JSON |
| System-prompt injection | flag | ❓ | `--read`/conventions file | ❓ | agent config | flag |
| Pricing/tokenizer known | ✓ | ❓ | BYO model | ✓ | BYO (75+ providers) | ✓ |
| Internal subagents (attribution risk) | no | **yes** | no | no | optional | no |
| **Warden tier** | **A** | **A**(❓) | **C** | **A** | **A** | **A** (retired) |

† Gemini CLI included for reference only — retired 2026-06-18, superseded by
Antigravity. Not a build target.

**Gap themes that drive adapter design:**
- **Transcript heterogeneity is the deepest gap.** JSONL (Claude), Markdown
  (Aider), SQLite (OpenCode), unknown (Antigravity/Codex). `ParseTranscript` must
  accept an `io.Reader`/source abstraction, not assume a file of JSONL. OpenCode's
  SQLite store may need a `TranscriptSource` variant (DB query vs file read).
- **Session-id control is rare.** Only Claude (and OpenCode via `--session`) let
  warden *assign* the id. For others, warden must *capture* the agent-generated id
  (parse it from first output / store file) → new `Caps.SessionIDControl=false`
  path: discover-then-pin instead of assign.
- **Approval UIs are all different.** Box-drawing+numbered (Claude), policy flags
  (Antigravity/Codex), y/n (Aider), per-tool (OpenCode). Per-adapter
  `DetectState`/`ParseApproval`; revisit a declarative marker DSL once ≥3 adapters
  share a shape (§7 Q5).
- **Cost attribution breaks with subagents.** Antigravity's internal subagents
  roll usage into one session → `wd spend` granularity drops for that backend;
  document the limitation rather than fake per-subagent numbers.

---

## 12. Future scope — full agent catalog (eventual targets)

We start with **one** (Antigravity CLI). Everything below is the long-horizon
target set, classified by how it fits warden's **console-driven, tmux-attached**
model. Classification *is* part of the gap analysis: not everything here is a
viable warden backend.

### 12.1 Console agents — viable warden adapters (the real backlog)
Tracked, prioritized roughly by reach/fit. Each becomes its own adapter PR.

| Agent | Vendor | OSS? | Notes for adapter |
|---|---|---|---|
| Claude Code | Anthropic | ✗ | reference impl (done) |
| Antigravity CLI (`agy`) | Google | ✗ | **headline #1**; subagent attribution risk |
| Aider | Paul Gauthier | ✓ | proof backend; git-native, md transcript |
| Codex CLI | OpenAI | ✗ | Terminal-Bench #1; sandbox exec |
| OpenCode | OpenCode AI | ✓ | most-starred OSS; SQLite store, 75+ providers, ACP |
| Qwen Code | Alibaba | ✓ | Apache-2.0; gemini-cli fork → similar shape |
| GitHub Copilot CLI | GitHub/MS | ✗ | GitHub-integrated; check headless maturity |
| Amazon Q Developer CLI | Amazon | ✗ | AWS-focused; enterprise reach |
| Goose | Block | ✓ | Apache-2.0; native MCP |
| Crush | Charm | ✓ | terminal-native (Charm TUI) |
| Mistral Vibe | Mistral | ✗* | Apache-2.0 source available |
| Amp | Sourcegraph | ✗ | "deep mode" autonomous research |
| iFlow | iFlow AI | ✓ | subagents + file-permission control |
| Kimi Code CLI | Moonshot | ✓ | 100-agent swarm (orchestration-overlap risk) |
| OpenClaw | OpenClaw | ✓ | Chinese model ecosystem gateway |
| BLACKBOX | BLACKBOX | mixed | proprietary + BYOK |

### 12.2 IDE/editor-bound — **out of scope** (not console-drivable)
These are VS Code/JetBrains/editor extensions or GUI apps; they don't present a
single attachable terminal session. Excluded per §1 non-goals unless they ship a
true headless CLI.
- Cursor (IDE; `cursor-agent` CLI is a possible *future* exception), Windsurf,
  Cline (VS Code, 5M installs), Continue.dev, Roo Code, Zed, Kiro (spec-driven IDE).

### 12.3 Infra, not agents — **not backends**
Runtimes and routers warden could *point a backend at*, but which aren't agents
themselves:
- **Inference runtimes:** Ollama, llama.cpp, LM Studio, vLLM, Tabby.
- **Model routers:** OpenRouter, 9router, CLIProxyAPI.
- These matter only as the *model* an OSS BYOK agent (Aider/OpenCode/Goose) is
  configured with — they're orthogonal to the adapter layer.

> **Maintenance:** as agents launch/retire (cf. Gemini→Antigravity), revisit this
> catalog. Don't build speculatively — promote an agent from 12.1 to an active
> adapter PR on a real demand signal, after the interface is proven on Antigravity.

---

## 13. Local testing & free-tier matrix (for adapter development)

Building an adapter only requires running the target CLI enough to observe its
**flags, transcript file/format, and approval UI** — a handful of requests, not
real coding. So even stingy free tiers suffice. **No paid subscription is
required to add support for any agent below.**

| Agent | Free to test? | How (no subscription) | Catch |
|---|---|---|---|
| Claude Code | ✅ (have it) | installed | — |
| Aider | ✅ fully free, no account | install + **Ollama** local models | slower locally; fine for probing |
| Antigravity (`agy`) | ✅ free tier, no card | sign in w/ personal Gmail | ~20 agent reqs/day (was 250); enough to probe, not a daily driver |
| Codex CLI | ✅ via ChatGPT Free | ChatGPT account ($0) | lowest limits |
| OpenCode / Goose / Qwen | ✅ OSS BYOK | **Ollama** local or free API tier | none for local |
| Copilot CLI | ✅ 50 premium req/mo | GitHub account | monthly cap |
| Amazon Q Dev | ✅ free tier | AWS account | AWS-flavored |

**Universal free backend:** install **Ollama** once (local, offline, $0) and it
serves every OSS BYOK agent (Aider, OpenCode, Goose, Qwen) — no API cost, no
accounts. Use `ollama_chat/<model>` form with Aider.

**Per-phase install (only when that phase starts — nothing needed during design):**
- Phase 1 (Aider proof): `uv tool install aider` (or `pip install aider-install`)
  + Ollama. Both free.
- Phase 2 (Antigravity): install `agy`, sign in w/ Gmail (free tier). **This is
  when to resolve §7 Q1 — inspect where `agy` writes transcripts and in what
  format** (determines Tier A vs B).

> The maintainer has only used Claude Code to date. Each non-Claude agent's exact
> behavior (transcript path/format, approval UI, idle signal) is therefore
> **discovered empirically when its adapter is built**, not assumed up front —
> which is exactly what the capability-flag + graceful-degradation design (§5)
> accommodates.

---

## 14. Sources (landscape, mid-2026)

- Gemini CLI → Antigravity CLI transition (retired 2026-06-18):
  Google Developers Blog, The Register, OSTechNix.
- Antigravity CLI (`agy`) usage/flags: DEV "Hands-On Guide", Antigravity Lab
  headless articles, Google Cloud "Choosing your surface".
- Landscape/market: Pinggy OSS roundup, morphllm Terminal-Bench 2.1 leaderboard
  (Codex #1 83.4%, Claude Code #2 78.9%), GitHub star counts (OpenCode ~176k,
  Gemini ~105k, Codex ~92k).
- OpenCode CLI capabilities (SQLite store, JSON export, ACP): OpenCode docs.
- Full agent catalog (§12): DEV "Every AI Coding CLI in 2026: The Complete Map
  (30+ Tools Compared)".
- Gemini CLI session path (`~/.gemini/tmp/<hash>/chats/`) — reference for
  Antigravity store investigation.
