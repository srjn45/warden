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
| Spawn fails with `daemon error (503): request timed out` | In a very large monorepo `git worktree add` (a full working-tree checkout) can take minutes. Spawn (and commit/push/sync/check/prune/…) now get a 10-minute daemon budget instead of 30s. If a spawn is still cut, a partial worktree is now cleaned up automatically. Both budgets are configurable — `http.timeout_slow` (default `10m`) for lifecycle routes, `http.timeout_fast` (default `30s`) for everything else; raise the slow one if your repo's checkouts or hooks run longer. Ensure your daemon is up to date (rebuild + restart). |
| An agent auto-approves the same prompt over and over | The auto-approve **circuit breaker** halts approvals after `auto_approve.max_repeats` consecutive identical approvals (default 10), raises an `approval_loop` anomaly, and leaves the prompt to you — the agent shows `waiting_for_input`. The underlying command is failing (e.g. expired credentials); fix that rather than re-approving. |
| `stop`/`terminate`/`delete` says `session not found` for an agent `ls` shows | Fixed: these now resolve by the same **name or id** `ls` displays (previously only the id/ticket worked). Rebuild + restart the daemon if it predates this fix. |
| `warden prune` wants to remove a worktree with real work | Fixed: an orphan worktree carrying unmerged commits (ahead of the default branch) is now held back unless `--force`, alongside the existing dirty/unpushed guard. |
| `wd doctor` never flags a bad `local_llm.model` | Fixed: doctor now FAILS when the configured local model isn't installed in ollama (run `ollama pull <model>` or fix `local_llm.model`); the daemon also logs a loud error at startup. |
| Status never updates live | Hooks not wired into `~/.claude/settings.json`. The poller still updates it, just less promptly. |
| Agent spawned in the wrong place | Prompt-mode agents launch in your current directory — `cd` to the right place first, or pass `--dir <path>`. |
| Every spawn asks for `--force` ("memory pressure") | The spawn gate blocks **only** at **critical** OS pressure or when live agents hit `worktree.spawn_gate_max_agents`. **Warn**-level pressure is advisory and no longer blocks. Still gated? Either you're at genuine critical pressure (terminate/rotate an agent to relieve it, or `--force`) or you've hit the agent cap (raise `worktree.spawn_gate_max_agents`, or set it to `0` to disable the count trigger). Restart the daemon after changing config. |

## Cockpit-specific

The cockpit **requires tmux ≥ 3.1** — it composites real tmux panes. If tmux isn't installed, or you run `warden tui` from **inside an existing tmux session** (which would nest sessions), the cockpit can't build its panes and exits with an error. Run it from a plain terminal.
