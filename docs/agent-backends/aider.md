# Aider backend (experimental)

Adapter: `internal/agentbackend/backends/aider.go` · Backend ID: `aider` · #52

[Aider](https://aider.chat) is a bring-your-own-model pair-programming agent. It was
warden's first **Tier-C proof** backend — it validates the neutral interface shape
against a non-Claude agent: a markdown transcript (not JSONL), no
assignable/resumable session id, a simple y/n approval UI, and a BYO-model pricing
story. This document records what works today and the honest gaps.

## CLI → warden interface mapping

| warden method        | Aider invocation                                                | Notes |
|----------------------|-----------------------------------------------------------------|-------|
| `LaunchCmd` (REPL)   | `aider --no-show-model-warnings [--model <m>] [--yes-always]`    | Interactive REPL; the prompt is seeded **after** launch (PromptSeeder), not on the launch line. |
| `LaunchPromptArg`    | *(none — returns `""`)*                                          | trailing positionals are files to add; `--message` is a one-shot that exits. warden types the task into the REPL once ready. |
| `PromptText` / `ReadyMarker` | type prompt after the `Repo-map:` startup line          | makes Aider a persistent interactive agent rather than run-once-and-exit. |
| `ResumeCmd`          | *(none — `Resume=false`)*                                        | Aider continues from the repo's chat history, not a session id warden can pin; rotate/handoff re-spawn fresh. |
| `HeadlessCmd`        | `aider --no-show-model-warnings --yes-always --no-auto-commits --message <prompt>` | One-shot for warden's classify/summarize offload. |
| `TranscriptPath`     | reads `<workdir>/.aider.chat.history.md`                         | markdown chat log in the repo root. |
| `ParseTranscript`    | parses the markdown chat log                                     | session headers / `#### ` prompts / `> Applied edit to <file>` → neutral Turns. |
| `SystemPromptFlag`   | — (unsupported)                                                  | Aider has no `--append-system-prompt` flag. |
| `InjectContext`      | **not implemented (skipped)**                                    | Aider has no rules file it auto-reads on a bare launch — see "Context injection" below. |
| `Pricing`            | — (unsupported)                                                  | BYO multi-provider. |
| `DetectState` / `ParseApproval` | live (y/n prompt)                                    | Aider's `(Y)es/(N)o` confirmation ⇒ needs-input + parsed approval. |

Permission-mode folding: warden's `yes-always` / `auto` / `acceptEdits` /
`bypassPermissions` → Aider's `--yes-always` (auto-approve). Cautious modes stay
interactive.

## Capability table

| Capability             | Caps flag              | Status | Detail |
|------------------------|------------------------|--------|--------|
| Headless one-shot      | `Headless`             | ✅    | `aider --message`. |
| Resume                 | `Resume`               | ❌    | no assignable/resumable session id; continues from repo chat history. |
| Structured transcript  | `StructuredTranscript` | ✅    | **Tier A** — markdown chat log → neutral Turns. |
| Model selection        | `ModelSelection`       | ✅    | `--model <m>` (omitted when empty so the BYO default applies). |
| Session-id control     | `SessionIDControl`     | ❌    | Aider has no assignable session id. |
| Permission modes       | `PermissionModes`      | default / `yes-always` | y/n prompt vs. `--yes-always` auto-approve. |
| System-prompt inject   | `SystemPromptInject`   | ❌ (skipped) | no launch-time flag **and** no auto-read rules file — see below. |
| Pricing / spend $      | `Pricing`              | ❌    | multi-provider BYO; no warden-side rate table. |
| State / approval detect| —                      | ✅    | y/n prompt parsed (needs-input); no static "working" marker (idle inferred from staleness). |

## Context injection (skipped — no auto-read rules file)

Unlike the other flagless backends (Codex/OpenCode/Cursor/Antigravity write
`AGENTS.md`, Crush writes `CRUSH.md`, Goose writes `.goosehints`), **Aider is
deliberately NOT wired for context injection** and does **not** implement
`agentbackend.ContextInjector`.

Aider's convention mechanism is `CONVENTIONS.md`, but it is read **only** when
explicitly added — via `aider --read CONVENTIONS.md` or a `read:` entry in
`.aider.conf.yml` (verified:
[aider.chat/docs/usage/conventions](https://aider.chat/docs/usage/conventions.html)).
A bare warden launch (`aider --no-show-model-warnings …`) does **not** auto-read any
rules file on startup.

Dropping a rules file Aider would ignore is **worse than nothing** — it leaves a
stale, unread file in the agent's worktree without delivering any guidance. So warden
injects nothing for Aider rather than write a dead file. The shared rules-file
injector (`inject.go`) is therefore intentionally **not** invoked for this backend.

Re-evaluate if a future Aider version auto-reads a rules file on a bare launch, or if
warden's launch command starts passing `--read`/a generated `.aider.conf.yml` (at
which point `CONVENTIONS.md` becomes the natural injection target and this backend can
adopt the shared helper).

## What works vs. what warden can't do yet

**Works today:**
- Launch the interactive REPL in a tmux pane (core), with the task typed in after the
  `Repo-map:` startup line (PromptSeeder).
- Headless classify/summarize offload via `aider --message`.
- Tier-A digests / "what changed" from the markdown chat log.
- Live needs-input detection + approval parsing from Aider's y/n prompt.
- Runs $0-local against a BYO Ollama model.

**Gaps (honest, breadth-first):**
1. **No resume.** Aider has no assignable/resumable session id; rotate/handoff
   re-spawn fresh (`Resume=false`).
2. **No warden-side dollar spend.** Multi-provider BYO; pricing degrades to
   tokens-only.
3. **No system-prompt / context injection.** No launch-time flag and no auto-read
   rules file — **skipped by design** (see "Context injection" above).
