# Warden Orchestration Brain — Responsibility Transfer & Enforcement Design

**Date:** 2026-06-24
**Feature:** Move deterministic responsibilities (worktree isolation, git lifecycle, test/build) off Claude agents and onto warden; enforce the boundary with hooks; add an optional local-LLM provider for the fuzzy-cheap middle.
**Status:** 📋 **Proposed** — phased; Phase 0 ships value with no LLM.
**Estimated Effort:** Phase 0a ~1 day · 0b ~2 days · 0c ~1 day · Phase 1 (local provider) ~3-4 days, incremental.

---

## North star: token reduction

The single goal is **reducing tokens spent on Claude agents**, by two mechanisms:

1. **Don't make Claude do mechanical work.** When Claude runs `git status` → `git diff`
   → `git add` → `git commit` → reads each output, that's 4-6 tool round-trips and a
   lot of output tokens for a deterministic operation. warden does it in Go and returns
   one compact result.
2. **Don't send Claude raw output it doesn't need.** Test logs, diffs, and transcripts
   get compressed (deterministically, or via a small local model) before they reach
   Claude's context.

A local LLM is **not** the headline. Most of the win is deterministic orchestration with
no model at all. The local model only earns its place on the fuzzy-but-cheap tasks
(classification, summarization, headless commit messages).

## Governing principle

> **warden owns mechanics (deterministic Go). Claude owns judgment (semantic). The local
> LLM owns the fuzzy-cheap middle (summarize / classify / messages-when-Claude-is-absent).**

## What warden already owns (and why the boundary leaks)

This design is mostly *exposing and enforcing* machinery warden already has:

- **Worktree creation is already deterministic** — `lifecycle.go:363 ensureWorktree`.
  But it is gated by agent *type*: `development`/`pr-review` get a worktree;
  `code`/`tests`/`docs`/`debug-ci` **run directly in the shared repo**
  (`internal/mcp/server.go:193`). Parallel write-agents on those types collide. This is
  the operator's isolation pain — not "Claude forgets," but *policy lets non-isolated
  agents run in the shared repo, and Claude picks the type.*
- **Commit machinery already exists** — `lifecycle.go:508` (`git add -A`, `git commit -m`),
  plus dirty/unpushed detection (`status --porcelain`, `log @{u}..`). But there is **no
  `commit`/`push`/`sync` CLI command or MCP tool**; the git lifecycle is done by hand.
- **System-prompt injection is already wired** — `lifecycle.go:105 pipelineHint`,
  `:133 collabHint` use `--append-system-prompt` on every spawn.
- **Claude Code hooks are already consumed** — the daemon receives hook events at
  `POST /events` (`internal/daemon/api.go:25`, `statusForHook`). **Correction (verified
  during 0a):** warden does **not** inject hooks into spawned sessions. The status hooks
  live in `hooks/settings.snippet.json` + `hooks/warden-hook.sh` and are **merged by the
  user, by hand, into their global `~/.claude/settings.json`** (USAGE §9). They are all
  *status* events (SessionStart/Notification/Stop/SubagentStop/SessionEnd) that **fail
  soft and never block**. There is no per-session settings file warden writes today.

So the system-prompt lever and the git mechanics already exist, but the **enforcement
hook lever does not** — a blocking PreToolUse hook is net-new and needs a delivery
mechanism (see Phase 0a-2). This is a scope correction to the original assumption that the
hook was "one more matcher in a file we already write."

---

## Responsibility transfer map

| Responsibility | Today | Move to | LLM? | Token win |
|---|---|---|---|---|
| **Worktree isolation** | type-driven; `code`/`tests` run in repo → collisions | warden **enforces**: every write-agent gets a worktree unless explicit `--in-repo` | none | indirect (kills rework) |
| **Stage / commit** | operator by hand, or Claude via Bash | `wd commit` CLI + `mcp__warden__commit` | none (msg from Claude) | high |
| **Push** | operator by hand | `wd push` + MCP tool | none | high |
| **Pull / rebase / sync** | operator by hand | `wd sync` = fetch + rebase + deterministic conflict detect | none for detect | high |
| **Run tests / build / lint** | Claude runs, reads 100s of lines | `wd check` runs configured cmds, returns pass/fail + only failing cases | optional summarize | high |
| **Task classification** | warden calls headless Claude (`lifecycle.go Classify`) | local LLM | swap | direct (warden's own Claude spend) |
| **Output / log compression** | raw into Claude context | local summarizer, truncate fallback | local | medium |
| **Conflict *resolution*** | Claude | stays Claude — warden hands it *only the conflicting hunks* | Claude | medium |

The top four rows need **no model**. They are the bulk of the token bleed and the whole of
the isolation pain.

### What `wd commit` adds over `git commit`

It is **not** primarily about generating the message. Value:

1. **Collapses Claude's git tool-spam into one MCP call** returning a compact struct
   (`{sha, branch, files, hook}`) instead of 4-6 round-trips Claude reads.
2. **Policy rails:** refuse on `main`, refuse in the wrong worktree, refuse obvious
   secrets, enforce agent-branch-only. (Retires the operator's manual carefulness.)
3. **Deterministic hook handling:** run pre-commit, parse pass/fail, surface only the
   failure.
4. **Bookkeeping:** link the SHA to the warden agent/ticket record.

**The commit message** comes from, in order: (a) Claude passes `--message` in an
interactive session — *best, because the agent that made the change knows the intent, and
it costs a handful of tokens*; (b) a local LLM generates it in the **headless/autonomous**
path (pipeline step, scheduled agent) where no Claude is present; (c) a deterministic
conventional-commit-from-diffstat floor when no model is configured. The local LLM only
earns its place in path (b).

---

## Enforcement: how to make a vanilla tmux Claude favour warden

A tmux Claude is a normal Claude Code session with `Bash`; left alone it runs `git commit`
directly. **A prompt steers; it never forces.** Three layers, weak → strong; the middle
one is the real mechanism.

### Layer 1 — Steer (soft): system-prompt convention

Add a `warden_conventions` hint to the existing `--append-system-prompt` injection
(alongside `pipelineHint`/`collabHint`): *"Prefer `wd commit` / `wd sync` / `wd check`
over raw git/test Bash; warden handles branch rails, hooks, and bookkeeping."* Already
plumbed, near-free. Necessary but **not sufficient** — Claude will ignore it under load.
Gate behind a config flag like the existing hints.

### Layer 2 — Enforce (hard): PreToolUse hook that denies + redirects

The real answer. A PreToolUse hook inspects each `Bash` call and returns
`permissionDecision: "deny"` with a reason **fed back to the model**, redirecting it to
the warden tool:

```
Bash("git commit …")  → DENY: "Use the warden commit tool — it runs hooks, enforces the
                                branch rail, and links the SHA to this agent.
                                Call mcp__warden__commit with message=<...>."
Bash("git push …")    → DENY → mcp__warden__push
Bash("git rebase …")  → DENY → mcp__warden__sync
Bash("go test …" / configured test cmd) → DENY → mcp__warden__check
```

Claude re-routes **and learns the convention for the rest of the session.** Deterministic;
does not depend on goodwill. warden already injects the hooks config, so this is extra
matchers in a file it already writes.

Two rules that make or break it:

1. **Only block mutations.** `git status` / `git log` / `git diff` stay allowed — Claude
   must be able to inspect. Block reads and you cripple it.
2. **The deny *message* is the product.** It must name the exact replacement tool + args.
   "Not allowed" makes Claude flail and try workarounds; a precise redirect makes it
   self-correct cleanly.

### Layer 3 — Restrict (backstop): `--disallowedTools`

For the genuinely irreversible (raw `git push` to a remote), make it *unavailable* at the
permission layer, not deny-with-feedback. Use sparingly — Layer 2 redirects and teaches;
Layer 3 only fails. Reserve for what must never be attempted.

### The hook is the unifying enforcement primitive

The same PreToolUse mechanism enforces **isolation** (Phase 0a):

```
Edit/Write under the shared repo root (no worktree) → DENY:
   "This agent isn't isolated. Work happens in its worktree at <path>."
```

So every snatched responsibility is enforced by adding one matcher, not by trusting a
prompt.

### Risk: matcher robustness

Claude writes `git  commit`, `cd x && git commit`, `git -C dir commit`,
`GIT_EDITOR=… git commit`. Naive substring matching over-blocks (matches `git commit`
inside a string) or under-blocks (misses `&&`, and Claude routes around it by accident).
**The hook must parse the command into argv and match on `git` + subcommand**, not grep
the raw string. A leaky gate trains Claude that the rail is optional. Budget real care and
tests here (table-driven over the obfuscation variants above).

---

## Local LLM track

Optional, opt-in, pluggable via an Ollama / llama.cpp HTTP endpoint. warden works fully
headless without it; every LLM step has a deterministic fallback.

- **Where it fits:** task classification, output/log compression, headless commit messages,
  triage. Two tiers — a **3-7B** model for the high-frequency cheap tasks (classify,
  summarize, commit msg), a larger **14-32B coder** only if/when attempt-then-escalate is
  ever built.
- **Where it does *not* fit:** deciding code changes interactively (quality gap with Opus is
  large), and silently rewriting the operator's prompt (additive context assembly only,
  never paraphrase intent).
- **Model picks (dev audience, Jan 2026):** Qwen2.5-Coder (7B default, 14B/32B if VRAM
  allows) as the workhorse; a 3-7B for the triage/summarize tier. Alternatives:
  DeepSeek-Coder-V2-Lite, Llama 3.x.
- **Cheapest first win:** swap `lifecycle.go Classify` (already an isolated headless-Claude
  call with a graceful `TypeOther` fallback, run on every spawn) to the local model. It
  validates the provider plumbing on a low-risk, pure-classification task before touching
  summarization or anything ambitious.

**Latency floor:** on CPU, local inference can be slower than just calling Claude. Every
LLM step needs a hard timeout → deterministic fallback, or the "savings" loses on
wall-clock UX.

---

## Check configuration & cross-project scaling

Git is a closed vocabulary — a handful of mutating subcommands warden knows natively. But
**test/build/lint is open-ended** (`go test`, `pytest`, `npm test`, `cargo test`,
`make verify`, custom targets, monorepo task runners) and varies per language *and* per
project. warden must never be language-aware. Instead `wd check` is **config-driven**, and
that config is the single source of truth for both the runner and the hook.

### Config drives both the runner and the redirect matchers

The hard part is not running an arbitrary command — it is the PreToolUse hook *recognizing*
that a Bash call is a test command worth redirecting. For git that is a clean
`git <mutating-subcommand>` match; for tests it is unbounded, so the hook cannot guess. The
resolution: **the hook only redirects commands the project's config registers.** One
declaration drives both sides, so the gate and the runner can never drift:

```yaml
# .warden/check.yml (per-project, in-repo)
check:
  test:   go test ./...           # `wd check test`
  fast:   go test -short ./...     # named "test group"
  lint:   golangci-lint run
  build:  go build ./...
  all:    make verify              # delegate to the project's own runner
  api:                             # monorepo: scoped task
    cmd: go test ./...
    dir: services/api
```

`wd check [name]` runs the named entry; warden generates the hook's test-redirect matchers
from these same values (`go test …`, `golangci-lint run`, `make verify`, …). A Node project
whose config says `test: npm test` redirects `npm test` instead.

### Scaling to an unseen project — three layers (later wins)

```
built-in defaults  <  auto-detection  <  in-repo .warden/check.yml  <  warden-side per-project override
```

1. **Zero-config detection.** On first spawn in a repo, sniff for a runner and seed
   defaults: `Makefile` targets, `package.json` scripts, `Cargo.toml`, `pyproject.toml` /
   pytest, `go.mod`, `justfile`, `Taskfile.yml`. Stock Go/Node projects work with no config.
2. **Explicit `check:` config** overrides/extends — custom targets, test groups, monorepo
   package scoping.
3. **Generated hook matchers** derive from layers 1+2.

**Safety principle: unknown commands pass through.** If a repo has no config and detection
finds nothing, the hook redirects nothing on the test side and Claude runs tests raw — the
project loses the token savings but nothing breaks. The feature is effectively opt-in per
repo by virtue of config existing, and degrades to today's behavior otherwise. warden never
pretends to know every stack; it only gates what it has been told about.

### Two rules that keep it from sprawling

- **Delegate, don't reinvent.** `wd check` is a thin wrapper that calls the project's
  existing runner (`make verify`, `npm test`) — the per-language knowledge stays in the
  project's tooling, not in warden. It runs the target and parses pass/fail.
- **Named, optionally-scoped tasks** cover monorepos via a per-entry `dir:`. Still just
  config.

### Where the config lives — per-project, not global

The check config is **per-project**; a single global command list cannot be right for a Go
service and a Node app at once. Primary home is **in-repo `.warden/check.yml`**, for one
concrete reason tied to this design: warden spawns agents in **worktrees**, and worktrees
share the repo's tracked files — so a committed `.warden/check.yml` is automatically present
in every worktree with zero path resolution. (A warden-store config keyed by the *main* repo
path would need worktree→project mapping.) The warden-side per-project store remains an
*optional* override layer for users who don't want warden config committed into their repos.

Responsibility split:

- **Per-project** (repo / warden-store): the command lists, working dirs, git overrides.
  Because the hook matchers are generated from this, **the enforcement gate is itself
  per-project** — two agents in two repos get different redirect rules automatically.
- **Global** (`~/.warden` config): *policy only* — redirect enforcement on/off, hook
  timeout, fallback-on-LLM-unavailable, default permission mode. **Never a command.**

Git stays built-in (universal vocabulary) but takes the same escape hatch: an optional
per-project `git:` override for repos that wrap git (pre-push scripts) or use a different
VCS. Built-in defaults, config when reality differs — symmetric with the test side, just
with a sensible non-empty default.

## Phasing & build order

Each phase ships value independently. **Tools must exist before the hook can redirect to
them**, so within Phase 0 the order is tools → hook → prompt line.

- **Phase 0a — Isolation enforcement.** Split in two once the code was inspected:
  - **0a-1 — Default-isolate (✅ shipped, no new infra).** Every write-agent
    (development/pr-review/code/docs/website/debug-ci/tests) now gets a worktree by
    default; `--in-repo` (CLI `--in-repo`, MCP `in_repo`, wire `in_repo`) is the opt-out;
    pr-review ignores it (structural checkout). Implemented by widening
    `store.Type.DefaultWorktree()` + the `--in-repo` short-circuit in
    `lifecycle.wantWorktree`, plumbed through client/daemon/CLI/MCP. Docs + tests updated.
    *This is the bulk of the isolation relief.*
  - **0a-2 — PreToolUse backstop (pending, needs new infra).** The Edit/Write-in-repo-root
    deny. Requires the net-new hook-delivery mechanism (the original spec wrongly assumed
    warden already injected hooks — it does not; see "already consumed" above). Plan:
    a per-agent generated `--settings` file (scopes the blocking hook to warden agents
    only, leaving the user's global `~/.claude/settings.json` untouched) + a Go policy
    endpoint (`POST /hooks/guard`, table-tested) the thin shell hook calls. This mechanism
    is the reusable foundation Phase 0b/0c redirects also ride on.
  - The spawn-gate extension (warn on ≥2 `--in-repo` write-agents sharing a repo) is a
    secondary safety net now that collisions are opt-in; folds into 0a-2 or a small
    follow-up.
- **Phase 0b — Git lifecycle.** `wd commit` / `wd push` / `wd sync` as CLI commands **and**
  MCP tools, built on the existing `lifecycle.go` machinery (rails, hook parsing,
  bookkeeping, structured results). Add the git-mutation PreToolUse redirect hooks and the
  `warden_conventions` system-prompt line. *No LLM.*
- **Phase 0c — `wd check`.** Run configured test/lint/build commands, return pass/fail +
  only failing cases; add the test-command redirect hook. Optional local summarize for
  oversized failure logs (deterministic truncate fallback). *Biggest raw token win.*
- **Phase 1 — Local provider.** Ollama HTTP provider behind a config flag. Prove on
  `Classify` first → then headless commit messages → then log/transcript summarization.

## Non-goals

- Interactive code generation by the local model.
- Replacing/paraphrasing the operator's prompt (context assembly is additive only).
- Attempt-then-escalate autonomous problem-solving — explicitly out of scope for this
  spec; if ever revisited it must be gated by objective verification (tests green) +
  retry budget + transparent diff + one-key rollback.

## Open questions

- **Resolved:** check commands live in a per-project `.warden/check.yml` (in-repo primary,
  warden-store override), config generates the hook matchers, global config holds policy
  only — see [Check configuration & cross-project scaling](#check-configuration--cross-project-scaling).
- Detection seed precedence when a repo has *both* a `Makefile` and a `package.json` (e.g.
  a Go service with a web UI subdir) — prefer top-level runner, or detect per-dir?
- Should Layer 3 (`--disallowedTools`) be on by default for raw `git push`, or opt-in?
- **Resolved (during 0a):** Hook config must be a **per-agent generated `--settings`
  file**, not a shared fragment — the status hooks are merged by hand into the user's
  *global* `~/.claude/settings.json` and fail soft, so a *blocking* PreToolUse deny put
  there would fire in the user's own non-warden sessions. A per-agent settings file scopes
  the deny to warden-spawned agents and carries per-agent context. (Open sub-question:
  whether `claude --settings <path>` fully overrides or merges with the user's global
  settings — confirm before 0a-2.)
