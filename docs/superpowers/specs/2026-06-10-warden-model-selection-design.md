# Warden Model Selection — Design Spec

**Date:** 2026-06-10  
**Status:** Future enhancement — high-level design. Detail pass required before implementation.

---

## Summary

Allow warden to choose the right Claude model for each agent automatically, with a manual override at every entry point. The core principle: model selection should be transparent, configurable, and never surprising.

---

## Store Change

Add `Model string` to `store.Session`. Empty string = daemon resolved to default. Persisted so `warden ls` and digests can report what model an agent ran on. Restored sessions reuse the stored model (no re-classification on restore).

---

## Auto-Selection: Two-Step Resolution

### Step 1 — Configurable default per agent type (fast, no LLM call)

A map of agent type → default model ID, configured via env vars:

```
WARDEN_MODEL_SUPERVISED=claude-opus-4-8      # supervised worktree agents (risky, complex)
WARDEN_MODEL_PIPELINE=claude-haiku-4-5       # pipeline jobs (parallel, disposable)
WARDEN_MODEL_FREEFORM=claude-sonnet-4-6      # default free-form agents
WARDEN_MODEL_INTERACTIVE=claude-sonnet-4-6   # interactive (no prompt) agents
```

These are the defaults. Operators can tune them (e.g., point pipeline jobs at Sonnet if quality matters more than cost for their workload). New agent types added in the future get a default without touching lifecycle code.

### Step 2 — Prompt classification (optional, ~1-2s latency)

Applied only when Step 1 resolves to the free-form default (Sonnet) and `WARDEN_MODEL_CLASSIFY=1` is set (off by default — opt-in since it adds latency and burns tokens).

The daemon runs:
```
claude -p "Classify this task complexity: simple|moderate|complex. One word only. Task: <first 500 chars of prompt>"
```

Maps to: `simple → haiku`, `moderate → sonnet`, `complex → opus`.

Step 2 is skipped entirely when:
- Agent type already resolved to a non-Sonnet model in Step 1
- `WARDEN_MODEL_CLASSIFY=0` (or not set)
- Prompt is empty (interactive agent)

### Manual Override (always wins)

Manual selection takes precedence over both steps above.

| Surface | How |
|---|---|
| CLI | `warden spawn --model opus` (or `haiku`, `sonnet`, full model ID) |
| Web | Dropdown in `NewAgentModal` (shows friendly names + note on cost/capability) |
| MCP | `model` param in spawn tool |
| Pipeline spec | `model` field per job in pipeline YAML/JSON |

The resolved model is logged in the spawn event for auditability.

---

## Short Model Name Aliases

Daemon maps short aliases to full versioned model IDs at spawn time:

| Alias | Resolves to |
|---|---|
| `haiku` | `claude-haiku-4-5-20251001` |
| `sonnet` | `claude-sonnet-4-6` |
| `opus` | `claude-opus-4-8` |

Full model IDs are also accepted directly (for pinning to a specific version).

The alias→ID mapping is updated with Claude releases — one place in the daemon, not scattered across CLI/MCP/web.

---

## Display

- **TUI list row:** model badge after status (e.g., `[opus]`)
- **TUI agent detail overlay (`i`):** model field
- **Web agent tab header:** model badge alongside context size badge
- **`warden ls` output:** model column (short alias)
- **Digest:** "ran on claude-sonnet-4-6" in summary

---

## What Gets Passed to Claude

The `--model <id>` flag is appended to `claudeBase()` / `claudeLaunch()` in lifecycle. If model is empty/default, no flag is passed (claude uses its own default, which is Sonnet currently).

---

## Open Questions for Detail Pass

1. Should pipeline jobs inherit the pipeline's model or always use `WARDEN_MODEL_PIPELINE`? Probably per-job override in the pipeline spec with pipeline-level default.
2. Classification prompt: is a single-word response reliable enough, or do we need structured output (`claude -p` with `--output-format json`)?
3. Cost display: show estimated cost bracket (cheap/mid/expensive) in the model selector as a UX hint?
4. Should restore always reuse the stored model, or offer to re-classify on restore?
