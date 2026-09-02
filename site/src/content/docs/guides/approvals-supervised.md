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

## The circuit breaker

Auto-approving a prompt should unblock the agent. When the **identical** prompt keeps re-appearing after being approved — the agent is re-running a failing command (expired credentials, a broken login) and re-asking forever — approving again just burns CPU and tokens. The breaker halts auto-approval after `max_repeats` consecutive identical approvals (default **10**), records an `approval_loop` anomaly on the agent, fires your notifier, and leaves the prompt unanswered so the agent surfaces as `waiting_for_input`.

```yaml
auto_approve:
  enabled: true
  max_repeats: 10   # 0 = default (10); negative = breaker off
```

A different prompt, or roughly ten quiet minutes, resets the run. Per-agent overrides inherit the default's `max_repeats` unless they set their own. When the breaker fires, read the agent's output and fix the failing command — don't just re-approve.
