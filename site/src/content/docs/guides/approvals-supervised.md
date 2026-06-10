---
title: Approvals & supervised mode
description: Run agents in a lighter permission mode and answer their tool-permission prompts from one inbox.
---

## Supervised mode

By default every agent runs `claude --dangerously-skip-permissions` — permission prompts are suppressed and the agent runs fully autonomously.

Pass `--supervised` to opt into a lighter permission mode (`--permission-mode acceptEdits`): file edits and common filesystem commands auto-approve, but other tools (bash writes, network calls, etc.) surface the numbered permission prompt — which the approvals inbox captures and lets you answer without attaching. A restored agent keeps its supervised setting.

```sh
warden start "refactor the auth module" --supervised
```

## The approvals inbox

Answer routine Claude tool-permission prompts (from supervised agents) without attaching. Controlled by `WARDEN_APPROVALS` (on by default).

| Surface | How |
|---|---|
| **CLI** | `warden approvals` lists recognized pending prompts with their numbered options; `warden approve <id> <n>` answers one. |
| **Web** | One-click option buttons in the AttentionQueue. |
| **TUI** | A pinned **⏳ Approvals** row (`i` / `enter`, then `1`-`9`; `tab` cycles agents). |
| **Safety** | A TOCTOU re-capture + fingerprint re-verify guards answers; unrecognized prompts always fall back to attach. |

```sh
warden approvals                 # list pending permission prompts (with their options)
warden approve PROJ-350 1        # answer prompt for that agent with option 1 (e.g. "Yes")
```

Unrecognized prompts always fall back to attach. Also surfaced in the web AttentionQueue (one-click buttons) and the TUI **⏳ Approvals** row.
