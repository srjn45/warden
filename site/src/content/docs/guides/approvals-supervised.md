---
title: Approvals & supervised mode
description: Run agents in a lighter permission mode and answer their tool-permission prompts from one inbox.
---

## Supervised mode

By default every agent runs fully autonomously with permission prompts suppressed (on the Claude backend, that's `claude --dangerously-skip-permissions`; each backend maps to its own "just do it" flag).

Pass `--supervised` to opt into a lighter permission mode (`--permission-mode acceptEdits`): file edits and common filesystem commands auto-approve, but other tools (bash writes, network calls, etc.) surface the numbered permission prompt — which the approvals inbox captures and lets you answer without attaching. A restored agent keeps its supervised setting.

```sh
warden start "refactor the auth module" --supervised
```

## The approvals inbox

Answer routine agent tool-permission prompts (from supervised agents) without attaching. Controlled by `WARDEN_APPROVALS` (on by default).

| Surface | How |
|---|---|
| **CLI** | `warden approval list` lists recognized pending prompts with their numbered options; `warden approval answer <id> <n>` answers one. |
| **Web** | One-click option buttons in the AttentionQueue. |
| **TUI** | A pinned **⏳ Approvals** row (`i` / `enter`, then `1`-`9`; `tab` cycles agents). |
| **Safety** | A TOCTOU re-capture + fingerprint re-verify guards answers; unrecognized prompts always fall back to attach. |

```sh
warden approval list                 # list pending permission prompts (with their options)
warden approval answer PROJ-350 1        # answer prompt for that agent with option 1 (e.g. "Yes")
```

Unrecognized prompts always fall back to attach. Also surfaced in the web AttentionQueue (one-click buttons) and the TUI **⏳ Approvals** row.

## Auto-approve

Let the daemon answer recognized prompts for you. Off by default; two cooperating layers.

**Per-agent toggle** — opt one agent into evaluation even when the global policy is off:

```sh
warden approval auto set PROJ-350 on
warden approval auto set PROJ-350 off
```

**Rule policy** — a real allow/deny engine. A prompt is auto-answered only when it matches an **allow** rule, matches **no deny** rule, and isn't on warden's built-in **destructive deny-list** (delete / `rm -rf` / force / push / deploy / reset --hard / …), which is checked first and always wins. Rules match by tool name, a case-insensitive glob/substring (`--pattern`), a **Go regular expression** (`--regex`) over the prompt, and/or path globs (`--paths`). A **per-agent override** (`--agent`, keyed by name or id) gets its own rule set that replaces the default for that agent. Changes take effect immediately (no restart) and are persisted to config.

```sh
warden approval auto rules                          # show the live policy
warden approval auto enable                          # turn the policy on
warden approval auto allow --tool Read               # auto-approve all Read prompts
warden approval auto allow --regex '^Bash\(git (status|diff|log)\)'
warden approval auto deny  --tool Bash --pattern rm  # belt-and-suspenders deny
warden approval auto allow --agent reviewer --tool Grep
warden approval auto clear --agent reviewer          # drop reviewer's overrides
```

Or in `~/.warden/config.yaml`:

```yaml
auto_approve:
  enabled: true
  rules:
    allow:
      - tool: Read
      - regex: '^Bash\(git (status|diff|log)\)'
    deny:
      - tool: Bash
        pattern: rm
  agents:
    reviewer:
      enabled: true
      rules:
        allow:
          - tool: Grep
```

With **no rules** configured, an enabled policy keeps the simple legacy behavior: it auto-answers every recognized, non-destructive prompt by pressing the least-privilege affirmative. Multi-select / text-entry / unrecognized prompts always fall back to manual. Both layers are also MCP tools: `set_auto_approve` (toggle) and `set_auto_approve_policy` (rules).

### Fast-Brain arbiter (optional third layer)

Set `use_fast_brain: true` (default `false`) to let a small, fast model answer the prompts your static rules can't — unmatched tool permissions and strategic questions the rules have no opinion on:

```yaml
auto_approve:
  enabled: true
  use_fast_brain: true
```

Tool permissions get a ≤1.5s fast-tier call; strategic questions get a ≤10s thinking-tier call. A decision is only applied at confidence ≥ 0.8; on timeout, error, reject, or low confidence it fails open and the prompt goes to you (or the brain agent). The destructive deny-list and the circuit breaker always run before the model, so it can never approve something they would block. The daemon reuses its existing Claude `-p` runner for both tiers.

## Workspace-trust prompts

Claude, Codex, Antigravity and Cursor each ask "do you trust this folder?" the first time they start in a directory — which, for warden, is every fresh worktree. warden answers that prompt "yes" automatically for every agent, independently of auto-approve: you chose the directory when you launched the agent there, and nothing the agent did is being approved (the prompt appears before its first turn). Each answer is recorded as a `workspace_trusted` event on the agent.

```yaml
trust_workspace: true   # default; false leaves the prompt in the approvals inbox
```

With `trust_workspace: false` the prompt is treated like any other standing grant: it shows in the approvals inbox, and auto-approve only answers it when `allow_sticky` is on.

## Prompts warden does not recognize

Every backend has a parser for its own prompt UI, and those parsers key on the CLI's wording. When a vendor rewords a prompt in an update, the parser stops matching: the agent looks busy, nothing reaches the inbox, and auto-approve never runs.

warden covers that gap with the Fast-Brain model. When a menu sits unchanged at the bottom of an agent's pane for a few seconds and no parser matches it, the model reads the pane and transcribes the prompt: the question, the command being asked about, the options, and which option is the one-time "yes".

```yaml
recognize_prompts: true   # default; false relies on the built-in parsers only
```

The model **recognizes**; it does not decide and it never presses a key. What happens to its reading:

- **Verified against the pane.** Every option label must really be on screen, in the same order. A paraphrased or invented option discards the whole reading.
- **Destructive guard reads the pane, not the summary.** If any text above the menu trips the destructive deny-list, the prompt is blocked even if the model left the command out.
- **Never less strict than the label.** An option whose label reads as a refusal is never treated as "yes"; one that reads as a standing grant ("always", "don't ask again", "trust") is always treated as sticky.
- **Same decision order.** The recognized prompt then goes through the policy rules, the sticky gate and the circuit breaker like any other, and shows in the approvals inbox.
- **Verified answer.** It is answered by moving the cursor and pressing Enter, and Enter is sent only after a re-capture confirms the cursor is on the chosen option.

The agent's status becomes `waiting_for_input` and a `prompt_recognized` event is recorded. This costs one model call per stalled, unrecognized menu (a second only if the first failed or timed out). It covers menus with a visible cursor mark (`>`, `❯`, `›`, `→`); a free-text `[y/N]` question is still left to its backend parser or to you.

### Learning prompts it has read

### Three-tier lookup

For a menu that has sat unchanged for a few seconds, warden resolves the prompt in this order and stops at the first tier that reads it:

1. **The backend's own parser**: free and exact, keyed on the CLI's wording.
2. **The known-prompts store**: a shape learned earlier, matched against the pane with no model call.
3. **The Fast-Brain model**: one model call; a verified reading can then be learned.

Whichever tier reads the prompt, the result goes through the same verification, destructive guard, policy rules and circuit breaker.

### What is stored, and what is not

An entry holds only the prompt's *shape*: the backend, the question and the ordered option labels (with quoted commands and the prompt's action replaced by a placeholder), which option is the one-time "yes", which options are standing grants, plus a hit count, timestamps and the source. It never stores the concrete command, path or any other text the prompt was asking about; a stored shape is matched back to the live pane, so everything you see in the inbox is the real on-screen text. A stored shape only says how to *read* a prompt, never whether to answer it.

A prompt the model read is **learned** once its answer provably worked: it was answered (by auto-approve, the arbiter, the brain or you via the approve endpoint) and a later capture shows the menu gone. The first verified success is enough, because a learned shape is re-verified against the live pane on every use and only says how to *read* a prompt, never whether to answer it. A reading that failed verification, an answer that bounced with "prompt changed", or a menu that is still showing teaches nothing. The next occurrence is then read from the store with no model call (`prompt_known` event; `prompt_learned` when it is first learned).

The store keeps itself honest (self-healing): a learned shape that trips the circuit breaker, or whose answers fail to clear the menu three times in a row, is dropped (`prompt_known_invalidated` event) and the next occurrence goes back to the model. It is bounded by `known_prompts_max` (default 500, least recently seen evicted) and entries unseen for `known_prompts_prune_days` (default 90) are pruned.

Inspect or prune the store yourself: `warden approval known list` (`--json` for scripts) shows each shape's id, backend, templated question, options (the answered one marked `*`), hit count and last-seen time; `warden approval known forget <id>` drops one that was learned wrong (it is simply re-learned next time), and `forget --all` empties the store after a confirmation (`--yes` skips it). The same is available over MCP (`list_known_prompts`, `forget_known_prompt`) and REST (`GET/DELETE /api/v1/known-prompts`); forgets are audit-logged (`known_prompt_forget`, `known_prompt_forget_all`).

## The circuit breaker

Auto-approving a prompt should unblock the agent. When the **identical** prompt keeps re-appearing after being approved — the agent is re-running a failing command (expired credentials, a broken login) and re-asking forever — approving again just burns CPU and tokens. The breaker halts auto-approval after `max_repeats` consecutive identical approvals (default **10**), records an `approval_loop` anomaly on the agent, fires your notifier, and leaves the prompt unanswered so the agent surfaces as `waiting_for_input`.

```yaml
auto_approve:
  enabled: true
  max_repeats: 10   # 0 = default (10); negative = breaker off
```

A different prompt, or roughly ten quiet minutes, resets the run. Per-agent overrides inherit the default's `max_repeats` unless they set their own. When the breaker fires, read the agent's output and fix the failing command — don't just re-approve.
