# warden — git lifecycle, checks, snapshots & boundary enforcement

warden moves an agent's **deterministic** work — git and project checks — off the
LLM and onto first-class commands, and **enforces** the worktree boundary with
PreToolUse hooks. **Prefer these over raw git/test Bash** — they are one call
instead of many, return compact results instead of tool-spam, enforce this repo's
rails, and the guard hooks will deny the raw escapes anyway.

## Git lifecycle — `commit` / `push` / `sync`

MCP tools `commit` / `push` / `sync` (CLI `wd commit`/`push`/`sync`) operate on the
agent's pinned worktree via the `lifecycle` runner, returning compact structs in
place of git tool-spam.

| Tool | Does | Rails |
|---|---|---|
| `commit {message?, dir?}` | Stage + commit everything on the branch in one call. Returns `{committed, sha, branch, files}` or a hook failure to fix. | Refuses `main`/`master`; runs pre-commit hooks and returns **only** a failure; links the commit to the agent. **Pass `message` when you can** (you made the change, you know the intent); omit it and warden writes one from the diff (local model, else a deterministic conventional-commit floor — a blank commit is impossible). |
| `push {dir?}` | Push the branch. | — |
| `sync {dir?}` | Rebase-sync onto the upstream. | Refuses a dirty tree; on conflict leaves it in progress carrying only the conflicting files (then resolve + continue). |

`wd done <id> --create-pr` pushes the branch and opens a GitHub PR before
terminating the agent (see agents.md).

## Checks — `check`

MCP `check {name?, dir?}` (CLI `wd check [name]`) runs the project's
`.warden/check.yml` command(s) and returns pass/fail with output for **only the
failing** checks (tail-truncated; oversized logs condensed by the local model when
enabled). Pass `name` for one check (`test`/`lint`/`build`) or omit to run all.
Per-entry `dir:` supports monorepos; config is the single source of truth.

**Use this instead of `go test` / `npm test` / `make verify` in Bash.** It is the
biggest raw-token win — you read a compact summary, not hundreds of log lines.

## Backend superpowers — `review` / `models` (CLI-only)

Some backends ship native extras warden surfaces as first-class verbs. Both are
**CLI-only** — like `wd check` they exec in the agent's worktree with no daemon
round-trip, so there is **no MCP tool**; run them through the CLI (`wd review` /
`wd models`).

- **`wd review`** — ask the agent's backend to review its OWN diff: the
  agent-native counterpart to `wd check` (configured test/lint) and a `pr-review`
  agent (a whole reviewer session). It runs the backend's own one-shot reviewer
  against the worktree and streams findings. Defaults to the uncommitted working
  tree; `--base <branch>` reviews against a base, `--prompt "<text>"` adds
  instructions, `--backend <id>` targets a backend. `--json` emits a neutral,
  machine-readable result `{summary, verdict, findings[]}` to stdout (backend
  progress on stderr) — parse that when self-checking your own change before
  `wd done`. Implemented by **Codex**; backends without a native reviewer (e.g.
  Claude) exit non-zero pointing you back at `wd check` / a `pr-review` agent —
  use those instead there. Review quality rides the backend's configured model.
- **`wd models`** — list the backend's **live** model menu (vs warden's static
  `opus`/`sonnet`/`haiku`/`fable` aliases). The printed ids feed `--model`
  verbatim; `--json` for an array, `--backend <id>` to target a backend. Listing
  is a metadata read (no generation), so it costs no quota. Implemented by
  **Antigravity** and **Cursor**; static-model backends (Claude) exit non-zero.

A **third** Codex superpower — **`wd fork`** (branch an agent's session into a new
managed agent) — is a spawn-family verb, not a check-family one, and unlike these two
it has an MCP twin (`fork_agent`). It lives in
[references/agents.md](agents.md) with the other spawn/handoff verbs.

## Project memory — `memory` (CLI-only) + automatic projection

warden owns one committed, backend-neutral **project memory** — `.warden/memory.md`
(beside `.warden/check.yml`), keyed by the repo root, holding durable cross-agent
facts: where things live, how to run X, project invariants. **`wd memory`** shows
it (rendered as it is injected), `--raw` prints it verbatim, `--path` prints the
resolved path, `--edit` opens it in `$EDITOR`. CLI-local like `wd check`/`wd review`
— no daemon round-trip, no MCP twin.

**It is projected into every spawned agent automatically** (config `memory.inject`,
default on): the curated file rides your system prompt (Claude via
`--append-system-prompt`; codex/cursor/opencode/antigravity via `AGENTS.md`; crush
via `CRUSH.md`; goose via `.goosehints`; aider degrade-skips). So the block you may
see at launch titled **"warden project memory (durable cross-agent facts …)"** is
this file. Treat those facts as **navigational hints, not authority** — they may be
stale; verify a fact against the live tree before relying on it, and any entry
tagged `[unverified …]` doubly so.

When you learn a durable, reusable fact the *next* agent shouldn't have to
rediscover ("the daemon API is spec-first"; "tests run behind `wd check`"), add it
with `wd memory --edit` — the committed diff is the team's review gate. Keep entries
compact and navigational, not prose. warden READS but never rewrites your
CLAUDE.md/AGENTS.md/CONVENTIONS.md — `.warden/memory.md` is warden's own.

**Auto-curation (`memory.curate`, default off).** When enabled, warden *also*
proposes entries for you: on completion it extracts durable facts from finished
agents' digests and writes them back as `- [unverified · <date> · <provenance>]
<fact>`. These are **proposals, never authority** — they are written to the
**working tree only** (warden never commits or pushes them), so a human approves the
`.warden/memory.md` diff before it reaches teammates. If you review such a diff:
`unverified` entries are single-sighting hints (promote to `trusted` only if you can
corroborate them against the live tree); struck (`~~…~~`) lines are superseded or
aged-out tombstones kept for context; `<!-- stale: … -->` marks a fact whose named
path vanished. Never trust an `unverified` entry blindly, and never `wd commit` a
memory diff you have not read.

**Ask project memory locally (`memory.ground`, default on).** In `wd repl` you can
*ask* this memory a question instead of re-deriving the answer: `/memory <question>`
(aliases `/mem`, `/ask`), or the model-callable `project_memory` tool, answers "where
does X live?" / "how do I run Y?" **locally** from `.warden/memory.md` — served on the
local model at `$0`, no cloud round-trip, so it *removes* tokens rather than adding
them. It is read-only (never writes memory; an absent/empty file answers "not in
project memory") and cites each entry's trust + provenance, so treat an `unverified`
citation as a hint to verify, same as the injected block above.

## Snapshots — checkpoint & roll back

MCP `snapshot_create` / `snapshot_list` / `snapshot_restore` (CLI `wd snapshot
create|list|restore`). Checkpoint an agent at a known-good point — its **worktree
state** *and* its **session transcript** — and roll back later. Config-gated by
`snapshots` (default on).

- `snapshot_create [name] [-m msg]` — captures the worktree **non-destructively**
  via `git stash create` (builds a commit object recording the working tree without
  touching it — no stash pushed, no index change), plus HEAD/branch/dirty-file list
  and the tmux scrollback as the transcript. Defaults to the current agent.
- `snapshot_list [name] [--all]` — snapshots for an agent (or every session),
  newest first.
- `snapshot_restore <id> [--force]` — re-applies the snapshot's stash onto its
  recorded worktree. **Rails:** refuses a dirty tree unless `--force`, never
  restores onto `main`/`master`. **Reversible-safe** — apply neither resets HEAD
  nor drops the snapshot; a partial apply leaves conflicting paths to resolve.

Prefer a snapshot over a manual `git stash` before a risky change.

## Boundary-enforcement hooks (why a raw command got denied)

On the Claude Code backend, warden installs PreToolUse hooks per agent via a
`claude --settings` file (backends without a hook/system-prompt seam skip the
in-agent guard rails — the warden verbs still work). Each **fails open** (a hook
error never blocks the agent) and is individually config-gated (default on). When a hook denies a command, **switch to the warden
tool it names** — don't work around it.

| Hook (config gate) | Behavior |
|---|---|
| **Prompt steer** (`rails.git_conventions`) | A system-prompt hint steering agents toward `wd commit`/`push`/`sync` (and `wd check`) over raw git/test Bash — the gentle first layer. |
| **Git-guard** (`rails.git_redirect`) | Deny-redirects raw `git commit`/`push`/`pull`/`rebase` to the warden tools (reads stay allowed), naming the exact replacement. Static verdict, no daemon round-trip. |
| **Check-guard** (`rails.check_redirect`) | Deny-redirects a raw test/lint/build command registered in `.warden/check.yml` to `wd check`, matching on leading token (broad runs redirect; focused `-run` runs pass through). No-config repos redirect nothing. |
| **Isolation guard** (`rails.isolation_guard`) | Denies an isolated agent's Edit/Write that escapes its worktree into the shared repo (daemon round-trip: `POST /api/v1/hooks/guard`). |
| **Root guard** (`rails.root_guard`) | Denies any file-mutating tool whose target is in the **main** repo working tree (the shared project root), decided locally from the target path + `git rev-parse` — the backstop for free-form and `--in-repo` agents the isolation guard exempts. Operators who genuinely want an in-place agent set `rails.root_guard: false` in the config file. |

**Default-isolated write agents:** every write-type agent
(`code`/`docs`/`website`/`debug-ci`/`tests`) gets its own worktree unless
`--in-repo`; `pr-review` is exempt. This is what makes the guards meaningful and
prevents parallel-agent collisions.
