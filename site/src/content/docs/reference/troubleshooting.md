---
title: Troubleshooting
description: Preflight checks with warden doctor, and fixes for the most common symptoms.
---

## Preflight: `warden doctor`

Run preflight checks — required binaries (`tmux`, `git`, `claude`), optional ones (`gh`, `ollama`, warn-only), daemon reachability, and the data directory.

```sh
warden doctor
```

You can also check the basics by hand:

```sh
claude --version     # the agent runtime
tmux -V              # every agent lives in a tmux window (≥ 3.1 for the cockpit)
git --version        # worktree creation/cleanup
gh --version         # only needed for pr-review agents
ollama --version     # optional — only for local_llm / `wd repl`
curl -s localhost:8765/healthz   # → {"status":"ok"} means the daemon is up
```

## Install missing dependencies: `warden setup`

If `doctor` reports a missing binary, `warden setup` installs it for you. It runs the **same checks as `doctor`**, then — for each missing dependency — prints the exact install command and prompts before running it (use `--yes` to install everything without prompting). It auto-detects Homebrew on macOS (never auto-bootstrapped) and `apt`/`dnf`/`pacman` on Linux; Claude Code and Ollama use their official installers. `setup` is idempotent and **CLI-only** (it installs host packages, so it is not exposed over MCP).

```sh
warden setup            # confirm-each install of anything missing
warden setup --yes      # non-interactive: install all missing deps
```

## Common symptoms

| Symptom | Likely cause / fix |
|---|---|
| Any command hangs or errors connecting | Daemon not running. `curl localhost:8765/healthz`; start it (`./scripts/install.sh` or `warden daemon`). |
| `healthz` fails / daemon won't start | Data dir not writable. Check `WARDEN_DATA_DIR` (default `~/.warden`) and `/tmp/warden.daemon.err`. |
| New agent stuck at `classifying…` / type is `other` | `claude` not on the daemon's PATH. Type falls back to `other`; functionality is otherwise fine. |
| `SUBJECT` stays empty | Poller hasn't refreshed yet (it's throttled and only runs when pane content changes), or `CLAUDE_PROJECTS_DIR` is wrong. |
| `pr-review needs --pr or --branch` | pr-review requires one of those flags. |
| `remove-worktree` refuses | The agent is still running (terminate it first) or the worktree has uncommitted/unpushed work — the guard is protecting it. Commit/push, or use `--force`. (`done` no longer touches the worktree.) |
| Status never updates live | Hooks not wired into `~/.claude/settings.json`. The poller still updates it, just less promptly. |
| Agent spawned in the wrong place | Prompt-mode agents launch in your current directory — `cd` to the right place first, or pass `--dir <path>`. |

## Cockpit-specific

The cockpit **requires tmux ≥ 3.1** — it composites real tmux panes. If tmux isn't installed, or you run `warden tui` from **inside an existing tmux session** (which would nest sessions), the cockpit can't build its panes and exits with an error. Run it from a plain terminal.
