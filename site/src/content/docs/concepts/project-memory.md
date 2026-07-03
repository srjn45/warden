---
title: Project memory
description: warden owns one committed, backend-neutral .warden/memory.md of durable cross-agent facts and projects it into every spawned agent — so the next agent (any backend) doesn't re-pay the rediscovery tax.
---

Every agent starts cold. Agent A greps around, learns where things live ("the
daemon API is spec-first — edit `openapi.yaml` then `make generate`"; "run the
tests with `wd check`, never raw `go test`"), finishes its task, and is torn
down. Agent B — often a **different backend** — starts from scratch and re-learns
the same map, burning tokens and turns on rediscovery warden already watched
Agent A pay for once.

warden is the orchestration layer *above* every backend, so it is the natural
owner of **one** canonical memory that it projects into whatever each backend
reads. That is **project memory**.

## The canonical file: `.warden/memory.md`

Project memory lives in a single committed, warden-owned file — `.warden/memory.md`,
beside the existing `.warden/check.yml`:

- **Committed & team-shared.** It travels with the clone, so every developer and
  every machine benefits, and every change to it is a reviewable git diff.
- **Keyed implicitly by the repo root** (`git rev-parse --show-toplevel`). No
  `wd init`, no registration — the file is auto-created the first time you use
  `wd memory`.
- **Backend-neutral.** It is not CLAUDE.md, AGENTS.md, or CONVENTIONS.md. warden
  **reads and respects** those human-authored files but **never rewrites them** —
  `.warden/memory.md` is warden's own, so machine-managed content never clobbers a
  file you hand-author.

Keep entries **compact and navigational** — "X lives in Y", "run Z via `wd check`",
project invariants — not prose. A tight "where things live" list is worth its
tokens; a wiki is not.

```markdown
- The daemon API is spec-first: edit internal/daemon/apidocs/openapi.yaml, then
  `make generate`; never hand-write handlers/DTOs.
- Tests/lint/build run behind `wd check` (config in .warden/check.yml).
- Never raw-git — the git-guard denies it; use `wd commit` / `wd push` / `wd sync`.
```

## Projection: one memory, every backend

At **every spawn**, warden renders a budgeted, navigational view of
`.warden/memory.md` and injects it into the new agent's system prompt through the
**same seam** the collab / pipeline / git-conventions hints already ride. No new
adapter code — memory is just one more guidance string:

| Backend | How memory reaches it |
|---|---|
| **Claude** | `--append-system-prompt` (file-backed via the hints dir, so it never bloats the launch line) |
| **codex, cursor, opencode, antigravity** | a `warden` block in the worktree's `AGENTS.md` |
| **crush** | a `warden` block in `CRUSH.md` |
| **goose** | a `warden` block in `.goosehints` |
| **aider** | *degrade-skips* — aider reads neither seam today |

**7 of 8 backends project with zero new adapter code.** For the file-drop
backends, warden manages only its own delimited block and **merge-preserves** any
existing `AGENTS.md` / `CRUSH.md` / `.goosehints` content, and the dropped file is
git-excluded so it never lands in the agent's diff.

`wd memory` (with no flags) prints exactly the render that gets injected — so it
always shows what the next agent will read.

## Cost discipline

Injection only nets positive when memory is compact, actually consumed, and — for
Claude — cache-stable:

- **Hard size budget.** The render is capped (~2 KB / a few hundred tokens); when
  it doesn't fit, the lowest-value entries drop first.
- **Spawn-boundary stability.** Memory is (re)injected only at launch, never
  rewritten mid-session — so it stays inside Claude's cached system prompt and is
  ~free after the first turn.
- **Navigational, not decorative.** A few hundred cached tokens that delete
  multiple rediscovery turns is a clear win; an uncapped dump is a clear loss.

## Toggling it

Projection is config-gated by **`memory_inject`** (default **on**). Turn it off,
or leave the file empty/absent, and a spawn is **byte-identical** to no injection —
projection is purely additive.

```yaml
# ~/.warden/config.yaml
memory_inject: true   # default; set false to disable launch-time projection
```

## Curating it — by hand, or auto-proposed (gated)

You can always curate `.warden/memory.md` **by hand** — `wd memory --edit` opens it
in `$EDITOR`, and the committed diff is the review gate.

warden can also **auto-propose** entries for you, off by default behind
**`memory_curate`**. When enabled, warden runs a debounced curation pass on the
existing completion-digest hook: as agents finish, it reads their digests +
transcripts + the current memory and extracts **durable, reusable facts** (where
things live, how to run X, project invariants), discarding per-task noise. It is
deliberately conservative and **never authoritative**:

- **Proposals, not authority.** New entries land as
  `- [unverified · <date> · <provenance>] <fact>` — a timestamped, attributed
  *claim*, projected with a "may be stale, verify before relying" caveat, never a
  silent truth.
- **Verify-before-trust.** An entry promotes to `trusted` only on **corroboration**
  — a second agent re-observing it, or a human approving the diff. A single
  sighting never auto-trusts.
- **Supersession & age-out.** A newer fact that contradicts an older one
  **supersedes** it (the old line is struck with a tombstone for the reviewer);
  un-recorroborated entries **age out** past a TTL; a fact whose named path no
  longer exists is **flagged stale** by a deterministic check.
- **The committed diff is the human gate.** The pass writes proposals to the
  **working tree only** — it **never commits and never pushes**. A human (or a
  reviewing agent) approves the `.warden/memory.md` diff in a PR before it reaches
  teammates. This is the core mitigation against memory *poisoning*: one agent's
  wrong belief can never silently mislead the whole fleet.
- **`$0` by default.** The pass prefers the local model (`local_llm`), degrading to
  headless `claude -p` only where configured — and it always runs off the critical
  path.

```yaml
# ~/.warden/config.yaml
memory_curate: false   # default OFF (opt-in — the risky half); proposals are
                       # gated by the committed diff, never auto-trusted
```

## Ask project memory locally — grounding (`memory_ground`)

Projection *adds* input tokens to every spawn. **Grounding** is the opposite lever:
in [`wd repl`](/warden/multi-agent/repl/) you can **ask** the memory a question and
warden answers it **locally**, *removing* a cloud round-trip instead of adding one.

Ask it two ways — the deterministic **`/memory <question>`** command (aliases
`/mem`, `/ask`), or, in the natural-language half, the model-callable
**`project_memory`** tool warden picks for "where does X live?" / "how do I run Y?"
questions:

```text
warden › /memory where does the daemon API live?
The daemon API is spec-first — edit openapi.yaml, then `make generate`.

grounded in .warden/memory.md:
- [trusted · 2026-06-30 · agent a1b2 · sha 04e2aed] The daemon API is spec-first:
  edit openapi.yaml then `make generate`; never hand-write handlers.
```

It is deliberately narrow and safe:

- **Local-only, `$0`.** Grounding runs on the local model (`local_llm`) and holds no
  cloud/escalation path — it is structurally free and can **never** escalate to a paid
  model. Grounding-style questions also classify to the local tier, so a bare
  natural-language project question plans locally, never in the cloud.
- **Read-only.** It reads `.warden/memory.md` the same way projection does and
  **never creates or writes it** (that stays curation's job). An absent or empty file
  answers `not in project memory` — no crash, no auto-create.
- **Grounded, with trust surfaced.** Answers are composed **only** from the matching
  entries and cite each one's **trust** (`unverified` / `trusted` / `human`) and
  **provenance**, so a stale hint visibly reads as a hint. If the memory can't answer,
  it says so plainly rather than inventing.
- **Degrades cleanly.** With no local model configured it returns the matching
  entries **verbatim** (still `$0`) instead of escalating.

```yaml
# ~/.warden/config.yaml
memory_ground: true    # default ON — it only REMOVES cost (a local answer instead
                       # of a cloud round-trip); read-only, never writes memory
```

## See also

- [`wd memory` in the CLI reference](/warden/reference/cli/#warden-memory)
- [Lifecycle commands & rails](/warden/guides/lifecycle-and-rails/)
- [Agent backends](/warden/concepts/agent-backends/)
