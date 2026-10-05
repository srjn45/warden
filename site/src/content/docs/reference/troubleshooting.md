---
title: Troubleshooting
description: Preflight checks with warden doctor, and fixes for the most common symptoms.
---

## Preflight: `warden doctor`

Run preflight checks — required binaries (`tmux`, `git`, `claude`), optional ones (`gh`, warn-only), daemon reachability, and the data directory.

```sh
warden doctor
warden doctor --reconcile-membership   # offline: stamp project_id + backfill missing membership; preserve stored lists (daemon must be stopped)
```

The daemon also runs the membership reconcile automatically at boot. Use
`--reconcile-membership` only when you need to repair the on-disk store while
the daemon is down (idempotent; a fully-consistent store reports no changes).

You can also check the basics by hand:

```sh
claude --version     # the agent runtime
tmux -V              # every agent lives in a tmux window (≥ 3.1 for the cockpit)
git --version        # worktree creation/cleanup
gh --version         # only needed for pr-review agents
curl -s localhost:8765/healthz   # → {"status":"ok"} means the daemon is up
```

## Install missing dependencies: `warden setup`

If `doctor` reports a missing binary, `warden setup` installs it for you. It runs the **same checks as `doctor`**, then — for each missing dependency — prints the exact install command and prompts before running it (use `--yes` to install everything without prompting). It auto-detects Homebrew on macOS (never auto-bootstrapped) and `apt`/`dnf`/`pacman` on Linux; Claude Code uses its official installer. `setup` is idempotent and **CLI-only** (it installs host packages, so it is not exposed over MCP).

```sh
warden setup            # confirm-each install of anything missing
warden setup --yes      # non-interactive: install all missing deps
```

## Common symptoms

| Symptom | Likely cause / fix |
|---|---|
| Any command hangs or errors connecting | Daemon not running. `curl localhost:8765/healthz`; start it (`curl -fsSL https://raw.githubusercontent.com/srjn45/warden/main/scripts/install.sh \| bash`, or `warden daemon`). |
| `healthz` fails / daemon won't start | Data dir not writable. Check `WARDEN_DATA_DIR` (default `~/.warden`) and the daemon logs — macOS: `/tmp/warden.daemon.err`; Linux: `journalctl --user -u warden -e`. |
| New agent stuck at `classifying…` / type is `other` | `claude` not on the daemon's PATH. Type falls back to `other`; functionality is otherwise fine. |
| `SUBJECT` stays empty | Poller hasn't refreshed yet (it's throttled and only runs when pane content changes), or `CLAUDE_PROJECTS_DIR` is wrong. |
| `pr-review needs --pr or --branch` | pr-review requires one of those flags. |
| `remove-worktree` refuses | The agent is still running (terminate it first) or the worktree has uncommitted/unpushed work — the guard is protecting it. Commit/push, or use `--force`. (`done` no longer touches the worktree.) |
| Spawn fails with `daemon error (503): request timed out` | In a very large monorepo `git worktree add` (a full working-tree checkout) can take minutes. Spawn (and commit/push/sync/check/prune/…) now get a 10-minute daemon budget instead of 30s. If a spawn is still cut, a partial worktree is now cleaned up automatically. Both budgets are configurable — `http.timeout_slow` (default `10m`) for lifecycle routes, `http.timeout_fast` (default `30s`) for everything else; raise the slow one if your repo's checkouts or hooks run longer. Ensure your daemon is up to date (rebuild + restart). |
| An agent auto-approves the same prompt over and over | The auto-approve **circuit breaker** halts approvals after `auto_approve.max_repeats` consecutive identical approvals (default 10), raises an `approval_loop` anomaly, and leaves the prompt to you — the agent shows `waiting_for_input`. The underlying command is failing (e.g. expired credentials); fix that rather than re-approving. |
| `stop`/`terminate`/`delete` says `session not found` for an agent `ls` shows | Fixed: these now resolve by the same **name or id** `ls` displays (previously only the id/ticket worked). Rebuild + restart the daemon if it predates this fix. |
| `warden workspace prune` wants to remove a worktree with real work | Fixed: an orphan worktree carrying unmerged commits (ahead of the default branch) is now held back unless `--force`, alongside the existing dirty/unpushed guard. |
| `warden doctor` / `setup` don't mention Ollama | Expected: `local_llm` / Ollama is retired. warden's internal thinking and `warden repl` run on Fast-Brain and need no local model; legacy `local_llm.*` YAML keys still parse but are ignored. |
| Status never updates live | Hooks not wired into `~/.claude/settings.json`. The poller still updates it, just less promptly. |
| Agent spawned in the wrong place | Prompt-mode agents launch in your current directory — `cd` to the right place first, or pass `--dir <path>`. |
| Every spawn asks for `--force` ("memory pressure") | The spawn gate blocks **only** at **critical** OS pressure or when live agents hit `worktree.spawn_gate_max_agents`. **Warn**-level pressure is advisory and no longer blocks. Still gated? Either you're at genuine critical pressure (terminate/rotate an agent to relieve it, or `--force`) or you've hit the agent cap (raise `worktree.spawn_gate_max_agents`, or set it to `0` to disable the count trigger). Restart the daemon after changing config. |

## Rate-limit recovery vs `wd usage`

| Symptom | Likely cause / fix |
|---|---|
| Agent hot-swaps backends while `wd usage` still shows headroom | Pane-confirmed (or usage-API) hard limit is authoritative for that agent; `wd usage` is provider-level only. Inspect `warden status <id> --json` → `backend_recovery` and `backend_recovery_*` events. |
| Same limited backend/model gets reselected in a loop | Should be blocked by durable per-pool cooldown through the parsed reset / fallback. If it still loops, grab status JSON + a capture under `~/.warden/ratelimit-captures/` and open an issue — do not treat `wd usage` alone as proof. |
| Several agents share one exhausted weekly Claude bucket but only some swap | Bulk reconciliation only selects agents with a daemon-owned `QuotaBinding`. Unbound legacy peers (`unbound_legacy`) are skipped until their next HotSwap. Preview with `warden usage recover --dry-run`. |
| Want a preview without swapping | `warden usage recover --dry-run` (optionally `--ai-cli` / `--project`). |
| `waiting_for_capacity` and nothing moves | Every eligible candidate is exhausted/unknown; retry is armed. Override with `warden switch`, or wait for the earliest reset. |
| Want to stop automatic switching | `warden switch <id> --backend …`, stop, or delete supersedes recovery (`backend_recovery_superseded` / `recovery_superseded`). |
| Cockpit goes blank / narrow column after a hot-swap | Transient layout/geometry glitch. Quit and reopen the TUI; the local reattach path should preserve the selected agent. Terminal panes do not drive recovery. |

See [Backend hard-limit recovery → Operator playbooks](/warden/guides/backend-recovery/#operator-playbooks) and [Operator diagnostics](/warden/guides/backend-recovery/#operator-diagnostics-usage-vs-pane-vs-cooldown-vs-manual).

## Cockpit-specific

The cockpit **requires tmux ≥ 3.1** — it composites real tmux panes; if tmux isn't installed it exits with an error. Running `warden tui` from **inside an existing tmux session** is fine: warden detects `$TMUX` and lays the cockpit out as a **native tmux window** in your current session instead of nesting (force with `--tmux-native`; force the classic own-session cockpit with `env -u TMUX warden tui`).
