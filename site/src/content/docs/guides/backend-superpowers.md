---
title: Backend superpowers (review & model menu)
description: Surface a backend's native extras as first-class warden verbs — wd review (the agent's own diff reviewer, with machine-readable findings) and wd models (its live runtime model menu).
---

Warden drives many [agent backends](/warden/concepts/agent-backends/), and some of
them ship capabilities Claude Code doesn't have. Warden surfaces those as
first-class verbs — added **on top** of the agent, never as a restriction. Two are
available today:

- **`wd review`** — ask the backend to review its OWN diff (with a `--json`
  machine-readable path).
- **`wd models`** — list the backend's **live** runtime model menu.

:::note[CLI-only by design]
Both verbs **exec in the agent's worktree with no daemon round-trip** — exactly
like `wd check`. That makes them CLI-local by design: they are **not** MCP tools
and have no web/TUI surface, the same way `wd check` is the local-exec sibling of
the daemon-backed verbs. An orchestrating agent runs them through the CLI.
:::

## `wd review` — the backend's own diff reviewer

`wd review` is the agent-native counterpart to `wd check`:

| | Runs | Surface |
|---|---|---|
| `wd check` | the project's configured **test/lint/build** commands (`.warden/check.yml`) | local exec |
| `pr-review` agent | a **whole reviewer session** (an isolated checkout you spawn) | a full agent |
| `wd review` | the backend's **own one-shot reviewer** against the working diff | local exec |

It resolves the agent's backend, type-asserts the additive `agentbackend.Reviewer`
interface, and — if the backend implements it — execs the native reviewer in the
worktree and streams the findings to you. **Codex** implements it
(`codex review`); the model/provider comes from the backend's own config, so the
$0-local Ollama rig and a paid setup both work unchanged.

```sh
wd review                       # review my uncommitted changes (staged + unstaged + untracked)
wd review --base main           # review this branch's changes against a base instead
wd review --prompt "focus on error handling and nil checks"
wd review --backend codex       # target a specific backend (default: the current agent's)
```

Backends **without** a native reviewer (e.g. Claude) are not offered the verb — it
exits non-zero pointing you at `wd check` or a `pr-review` agent. That's the
"add on top" rule in action: warden never lowers a capable backend to a
lowest-common-denominator.

### `wd review --json` — machine-readable findings

Pass `--json` for a structured result. Warden runs the backend's **structured**
review form (Codex: `codex exec review`), then normalizes the backend's NATIVE
review output into a single **neutral** findings shape and prints it to stdout
(the backend's own progress goes to stderr, so stdout stays clean for piping):

```json
{
  "summary": "overall human-readable explanation",
  "verdict": "the backend's overall verdict (codex: overall_correctness)",
  "findings": [
    {
      "file": "internal/auth/token.go",
      "line": 42,
      "severity": "error",
      "title": "short headline",
      "message": "the finding text"
    }
  ]
}
```

Warden **owns this neutral shape** (`agentbackend.ReviewFindings`); it does not
impose a schema on the agent. For Codex, the structured result is Codex's own
native `review_output` (which `codex exec review` persists to the session rollout);
warden reads it back and maps it onto the neutral shape, relativizing paths to the
worktree and folding the backend's priority onto a neutral `info`/`warning`/`error`
severity. `findings` is always present (emitted as `[]`, never `null`).

:::caution[Quality rides the backend's model]
The pilot proves the *plumbing* (verb → adapter → review run → parse/stream), not
review accuracy. On a tiny local model (e.g. `qwen2.5-coder:3b`) review quality is
weak and may report no findings at all — in which case `wd review --json` reports a
clean "no structured review output" rather than erroring. The operator's real
model is where this earns its keep.
:::

## `wd models` — the backend's live model menu

Warden ships a small static alias table (`opus`/`sonnet`/`haiku`/`fable`) for the
Claude backend. Other backends expose a **live, multi-vendor menu** that the
operator's account/plan can change at any time — so guessing from a hard-coded
table is wrong. `wd models` surfaces the real menu:

```sh
wd models                       # the current agent's backend menu, one id per line
wd models --backend antigravity # e.g. Gemini 3.5 Flash (Low), Claude Opus 4.6 (Thinking), …
wd models --backend cursor      # e.g. auto, gpt-5.3-codex-low, glm-5.2-max, …
wd models --json                # emit the menu as a JSON array instead
```

It runs the backend's own list subcommand via the additive
`agentbackend.ModelLister` interface — **Antigravity** (`agy models`) and
**Cursor** (`cursor-agent --list-models`). The printed ids feed `--model`
verbatim. Listing is a **metadata read**, not a generation request, so it spends
no hosted-tier quota.

Backends with a static model set (e.g. Claude) have no live menu and are not
offered the verb — it exits non-zero pointing you at `--model` with a known id.

## Related: Cursor's `--auto-review` permission mode

Cursor's server-side "Smart Auto" classifier (`--auto-review`) — which auto-runs
safe tool calls and prompts for the rest — is surfaced not as a separate verb but
as a warden **permission mode**: `cursor` exposes
`default | plan | ask | auto-review | force`, and warden's `auto-review` mode emits
`--auto-review`. See [Agent backends → Cursor](/warden/concepts/agent-backends/)
and [Approvals & supervised mode](/warden/guides/approvals-supervised/).

## How these are built

Each superpower is an **optional, type-asserted interface** on
`agentbackend.Backend` (`Reviewer`, `StructuredReviewer`, `ModelLister`), exactly
like the merged `PromptSeeder` / `SessionIDDiscoverer` / `ContextInjector` seams. A
backend that doesn't implement an interface simply isn't offered that verb —
**Claude stays untouched and regression-locked**, no registry or neutral-type
churn, and (because these are CLI-local execs) no daemon API change. The full
design is `docs/superpowers/specs/2026-06-29-t1-superpowers-design.md`; per-backend
notes live in the gap docs ([codex](https://github.com/srjn45/warden/blob/main/docs/agent-backends/codex.md),
[antigravity](https://github.com/srjn45/warden/blob/main/docs/agent-backends/antigravity.md),
[cursor](https://github.com/srjn45/warden/blob/main/docs/agent-backends/cursor.md)).
