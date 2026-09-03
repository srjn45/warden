# warden Features

A consolidated catalog of everything `warden` can do, grouped by area. This is
the "what exists" reference — for *how to use it* day to day see
[USAGE.md](USAGE.md), and for build/install details see the
[README](../README.md).

> `warden` is aliased as `wd`; every CLI feature below works under either name.

---

## 1. Core architecture

One self-contained Go binary that wears several faces, all sharing the same
on-disk state:

| Capability | Description |
|---|---|
| **Single-binary distribution** | `warden` bundles the daemon, CLI clients, MCP server, TUI, and (in release builds) the embedded web GUI. `wd` is an installed symlink. |
| **Local daemon** | The single writer to the session store. Serves a loopback REST API (`127.0.0.1:8765`) and runs a background poller that keeps each agent's status and subject fresh. |
| **Embedded ScrivaDB session store** | Sessions persist in an embedded ScrivaDB (`github.com/srjn45/scriva`, `SyncModeNone`) rooted at `~/.warden/sessions-db/` — an `active` collection for live sessions and a `closed` collection for archived ones, each record keyed by session id. A mutation appends one record instead of rewriting a whole per-session JSON file, and there's still no database server to run. On the first launch after upgrading, warden imports the legacy `sessions/`+`closed/` JSON once and keeps those files as a read-only cold backup (see [§31 Session storage & upgrade migration](#31-session-storage--upgrade-migration)). |
| **Claude Code lifecycle hooks** | A hook script posts `SessionStart`/`Notification`/`Stop`/`SubagentStop`/`SessionEnd` to the daemon so status updates in real time without polling. Fails soft (never blocks the agent). |
| **launchd auto-start (macOS)** | Installs as an auto-starting, crash-restarting background service. |
| **Stable code identity** | One-time self-signed code-signing cert keeps the macOS TCC (Full Disk Access) grant stable across rebuilds. |
| **Security hardening** | `0700` data dir, slowloris/body/write timeouts (bypassed for SSE/WS/long-poll), refuses a non-loopback bind without a bearer token (`WARDEN_TOKEN`). |
| **`warden doctor`** | Preflight checks: required binaries (`tmux`, `git`, `claude`), optional ones (`gh`, `ollama`, warn-only), daemon reachability, data directory. |
| **`warden setup`** | Verifies the install with doctor's checks, then installs whatever is missing (idempotent — only touches absent deps). Confirm-each prompts (or `--yes` for automation); auto-detects Homebrew (macOS, never auto-bootstrapped) / `apt`/`dnf`/`pacman` (Linux); Claude Code + Ollama via their official installers. Re-runs the checks and prints a doctor-style report. **CLI-only** (installs host packages) — not exposed over MCP/daemon. |
| **`warden version`** | Prints version + build metadata (commit, build date, Go version, platform); `--version` shows the same, `version --json` for scripting. Stamped via ldflags (goreleaser + `make build`) with a VCS-stamp fallback. |

---

## 2. Spawning agents

| Feature | Description |
|---|---|
| **Prompt-spawn** | `warden start "<prompt>"` — no repo or type needed. Runs `claude` in the caller's directory (or `--dir`). |
| **Auto-classification** | The daemon classifies a prompt-spawned agent's type with `claude -p` shortly after creation (falls back to `other`). |
| **Auto-generated subject** | Each agent gets a one-line ≤8-word summary of what it's doing, seeded from the prompt and refreshed by the poller from the transcript or tmux pane (throttled, change-gated). |
| **Managed worktree spawn** | `--type` creates/adopts a git worktree where the type needs one. |
| **Worktree adoption** | If a worktree for the ticket already exists, the spawn reattaches to it instead of erroring. |
| **Configurable permission mode** | Per-agent and global control over Claude permission level. CLI flag: `--permission-mode <mode>` (values: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`). Legacy alias: `--supervised` (equivalent to `--permission-mode acceptEdits`). Global default: the `default_permission_mode` config setting (defaults to `auto`). Runtime change: `warden agent permission-mode set <id> <mode>`. Display: PERMISSION_MODE column in `warden ls`, permission_mode field in `warden status`. Stored in session: mode preserved on restore/resume. Empty mode means "use global default" and displays as `default`. |
| **Model selection** | Per-agent model selection via `--model` flag (CLI and MCP). Short aliases for common models: `opus`, `sonnet`, `haiku`, `fable`. Config default: the `model_default` setting. Fallback: `claude-sonnet-4-6` if not specified. Display: MODEL column in `warden ls`, model field in `warden status`. Stored in session: model preserved on restore/resume. |
| **Agent roles** | A **role** is a named, persistent system-prompt **persona** attached to an agent (*who the agent is*), plus a set of default spawn flags and a default model tier. CLI: `--role <name>` on `warden start`; MCP: `role` on `spawn_agent`; UIs: TUI new-agent `ctrl+r` picker, web **+ New agent** Role dropdown. Fixed built-in catalog (no user-defined roles): `general` (default, no persona), `orchestrator`, `planner`, `worker`, `autopilot`, `brain` — browse with `warden agent role list` / MCP `list_roles`. The legacy names `implementer`/`auto-merger`/`reviewer` are no longer first-class roles (that work is now a **task** — see **Tiered model routing** below) but still resolve to `worker` for back-compat. Each role carries a persona, default flags (`type`/`model`/`permission_mode`/`auto_approve`/`tags`) that fill only the fields the caller left unset (explicit request > role default > global default; tags are unioned; `auto_approve` OR-ed), and a **default model tier** (`general`/`worker`/`brain`→tier-2, `orchestrator`/`planner`/`autopilot`→tier-1) that feeds the tiered router. Switch a running agent's role with `warden agent role set <id> <name>` / MCP `set_role` — it persists the name and **relaunches** the agent so the new persona re-injects (its in-flight turn is discarded); `general`/empty clears the persona. Only the role **name** is stored in the session (`Session.Role`, empty ⇒ `general`, back-compat — no store migration); the persona re-resolves from the `internal/role` registry at every (re)launch, so nothing persona-shaped is persisted. Injected through the same seam as warden's collab/git/pipeline hints — Claude via `--append-system-prompt` (file-backed, never inlined on the launch line), the injecting backends (Codex/OpenCode/Cursor/Antigravity/Crush/Goose) prepended into their rules file; Aider (no injection seam) degrades silently. An empty persona injects nothing, so a plain/`general` spawn is byte-identical to before roles existed. |
| **Tiered model routing** (`--task` / `--tier`) | warden resolves each spawn's **backend + model** by **quota headroom within a model tier**, so a fleet spreads across providers instead of exhausting one (`internal/router.Resolver`). Two optional inputs (*what the agent is doing* / an explicit tier) feed it: `--task <name>` names a unit of work from the built-in **task registry** (`internal/task`, the canonical task→tier source) — tier-1 `analysis`/`architecture`/`design`/`research`/`spike`, tier-2 `code-review`/`development`/`docs`/`pr-review`, tier-3 `debug-ci`/`merge-pr`/`monitor-ci`/`release`; `--tier <tier>` pins `tier-1`/`tier-2`/`tier-3` directly. Target tier precedence: **explicit `--tier` > task tier > role default tier > tier-2**. Within the tier the resolver scores models by headroom (`1 − used/limit`), filters rate-limited/ineligible backends (≥ threshold, default 90%), and picks the highest-headroom candidate (round-robin among ties). A pinned `--backend`/`--model` **bypasses** the router; a first spawn **degrades** to request defaults when no resolver is wired — routing never hard-fails a spawn (`lifecycle.resolveSpawnTarget`, mirroring the hot-swap successor path). **Surfaces:** `--task` and `--tier` are on `warden start` (CLI) and the REST spawn body (`task`/`tier`); `tier:` (alongside `role:`) is also a **pipeline job** field — the pipeline Job spec has no `task:`. Over **MCP**, `spawn_agent` routes by `role` only (its default tier feeds the resolver — no `task`/`tier` params). Distinct from `--type` (worktree policy). See `docs/specs/tiered-model-routing.plan.md` and `docs/specs/agent-roles.md`; end-to-end coverage in `internal/lifecycle/tier_trio_integration_test.go`. |
| **Backend selection** | Per-agent agent backend via `--backend <id>` (CLI) / `backend` param (`spawn_agent` MCP), kept at CLI/MCP parity. **Only `claude` is fully tested and stable**; `codex` and `antigravity` are **β beta** (live-verified state, approval, and transcript fidelity, still maturing); `aider`, `opencode`, `crush`, `goose`, and `cursor` are 🧪 experimental / work-in-progress — functionality may be reduced or unverified. `claude` (default, Tier A — full fidelity), `aider` (Tier A — 🧪 experimental; bring-your-own-model: pass `--model`, e.g. `ollama_chat/qwen2.5-coder:3b`), `opencode` (Tier A — 🧪 experimental; bring-your-own-model: pass `--model`, e.g. `ollama/qwen2.5-coder:3b`), `codex` (Tier A — β beta; BYO provider via Codex config / `-m`), `crush` (Tier A — 🧪 experimental; BYO model via config, prompt seeded post-launch via `PromptSeeder`), `goose` (Tier A — 🧪 experimental; BYO provider via `GOOSE_PROVIDER`/`GOOSE_MODEL` env, no model flag on session launch), `cursor` (Tier C — 🧪 experimental; hosted Cursor plan via `cursor-agent`, rich native permission modes, **no structured transcript yet** so no digests), `antigravity` (Tier A — β beta; Google-hosted free tier via `agy`, multi-vendor model menu). Stored in `Session.Backend` (empty ⇒ claude, back-compat — no store migration); lifecycle resolves the adapter from `internal/agentbackend`'s registry. Backends declare **capability flags** and warden **degrades gracefully** per design §5 when one is missing: a non-pricing backend shows tokens-not-dollars in `wd usage spend` and is omitted from `wd usage savings`; a non-resumable backend re-spawns fresh on rotate/handoff instead of `--resume`; a non-structured-transcript backend falls back to a pane-scrape digest; a backend with no launch-time `--append-system-prompt` flag still receives warden's pipeline/collab/git hints via a rules file it auto-reads on startup (`InjectContext` — `AGENTS.md` for Codex/OpenCode/Cursor/Antigravity, `CRUSH.md` for Crush, `.goosehints` for Goose), and only a backend that auto-reads no such file (Aider) skips the hints entirely. Aider's markdown transcript parses into structured digests (Tier A); it runs an autonomous `--message` task that exits when done (no persistent loop) and has no assignable/resumable session id. OpenCode stores its transcript in SQLite — the adapter sources it via `opencode export <session>` (clean `{info,messages[]}` JSON ⇒ Tier A digests, the design's "DB query, not file read" case) — runs a persistent TUI loop (prompt seeded via `--prompt`), and **does resume**: it mints its own session id, so resume is keyed off the agent worktree (`opencode -c` continues that directory's last session, verified dir-scoped), and rotate/handoff/restore work. Codex persists sessions as JSONL rollout files (`$CODEX_HOME/sessions/`) — the adapter resolves the most recent rollout for the worktree (dir-scoped) and parses `response_item` records into Turns; resumes with `codex resume --last` and additionally **discovers + pins** the minted `session_id` from the rollout's `session_meta` header (`DiscoverSessionID`) so resume/transcript can resolve by exact id; warden also detects Codex's live state + numbered approval prompt and injects its hints via `AGENTS.md`. Crush stores sessions in a per-project SQLite DB (`.crush/crush.db`) — the adapter sources the transcript via `crush session show <id> --json` and resumes via `--continue` (dir-scoped); warden launches the bare TUI, waits for it to be ready, then auto-types the initial task prompt via `PromptSeeder`. Goose stores sessions in a global SQLite DB — the adapter sources via `goose session export --format json`; warden pins its own agent id as the Goose session **name** so resume is name-deterministic (`goose session -r --name <id>`); model is config/env-driven (`GOOSE_PROVIDER`/`GOOSE_MODEL`), not a launch flag. Cursor (`cursor-agent`) is a hosted plan: warden surfaces its rich native permission modes (`plan`/`ask`/`auto-review`/`force`), detects live state + command-allowlist/workspace-trust prompts, and resumes dir-scoped (`--continue`); its interactive transcript is an unreadable SQLite `store.db` with no export verb, so it stays **Tier C** (no digests) until a `store.db` reader lands — warden never passes Cursor's own `-w/--worktree` (warden owns the worktree). Antigravity (`agy`) is a Google-hosted free tier with a multi-vendor model menu (Gemini/Claude/GPT-OSS): warden parses its plaintext trajectory JSONL into Turns (**Tier A** digests, incl. tool calls / files changed), detects live state + the `Do you want to proceed?` permission menu and the launch-time workspace-trust prompt, and resumes dir-scoped (`agy -c`). **Terminals are no longer a backend** — a plain interactive `$SHELL` beside the fleet is now a first-class **session kind** (`kind=terminal`, §8), not a `--backend` value, so `terminal` is gone from the `--backend` picker and `GET /api/v1/backends` (`backend=terminal` is still accepted as a back-compat alias for `kind=terminal`). Per-backend gap docs: [`docs/agent-backends/codex.md`](agent-backends/codex.md), [`crush.md`](agent-backends/crush.md), [`goose.md`](agent-backends/goose.md), [`cursor.md`](agent-backends/cursor.md), [`antigravity.md`](agent-backends/antigravity.md). |
| **Spawn presets** | Save reusable spawn defaults under a name and replay them. `warden project preset save <name> [spawn flags]` persists `--type`/`--model`/`--permission-mode` (`--supervised`)/`--auto-restart`/`--worktree`/`--in-repo` to `~/.warden/presets.yaml`; `warden project preset list` shows them. `warden start --preset <name>` seeds those defaults, and any explicit CLI flag still overrides the preset. Per-invocation inputs (ticket, branch, PR, dir) are not stored. |
| **Prompt templates** | Save reusable, variabled prompt *bodies* (where presets store flags). `warden project prompt-template save <name> --prompt "…{{VAR}}…"` (alias `pt`) persists a prompt body with `{{VAR}}` placeholders to `~/.warden/prompt-templates.yaml`; the declared variables are auto-derived from the body. `warden project prompt-template list` shows each template and its variables. `warden start --prompt-template <name> --set FILE=foo.go --set X=y` resolves the template into the spawn prompt (every declared variable must be supplied; an unknown `--set` is rejected). An explicit positional prompt still wins, and `--prompt-template` is free-form only (no `--type`). |
| **Library umbrella** (`warden project library`, alias `lib`) | One discoverable entry point over all three kinds of reusable launch config: spawn **presets**, **prompt templates**, and the built-in pipeline **templates**. `warden project library list` shows all three in labeled sections (presets + their stored defaults, prompt templates + their variables, and pipeline templates + a short description); `warden project preset save <name> [spawn flags]` delegates to `warden project preset save` and `warden project prompt-template save <name> --prompt "…"` delegates to `warden project prompt-template save`. Purely additive — no new storage or format: it reuses the existing preset store, the prompt-template store, and the embedded template catalog, and the standalone `preset`, `prompt-template`, and `pipeline list-templates` commands keep working unchanged. Pipeline templates are embedded/read-only, so there is no `save-template` (author a pipeline from a YAML spec with `pipeline create -f`). Also exposed over MCP as `library_list` (returns `{presets, prompt_templates, templates}`). |

### Task types (`--type`)

| Type | Worktree | Notes |
|---|---|---|
| `development` | yes (new branch) | `.worktrees/<ticket>` on a branch named after the ticket |
| `pr-review` | yes (PR branch) | Detached worktree; runs `gh pr checkout <PR>`. Needs `--pr`/`--branch` |
| `analysis` | opt-in (`--worktree`) | Runs in the repo by default |
| `spike` | opt-in (`--worktree`) | Same as analysis |
| `code` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `docs` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `website` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `debug-ci` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `tests` | yes (new branch) | Isolated in `.worktrees/<id>`; pass `--in-repo` to share the repo |
| `other` | no | Catch-all / unrecognized type strings |

---

## 3. Lifecycle management

| Command / feature | Description |
|---|---|
| `stop` | **The single umbrella teardown verb.** Default `wd agent stop <TICKET>` = full teardown: terminate the session, clear (archive) the record, **and** remove the git worktree + branch (asks for confirmation first unless `--yes`). Subtractive flags: `--keep-record`, `--keep-worktree` (`--keep-worktree` alone == the old `done`), `--hard` (purge record), `--pr`/`--base` (open a GitHub PR first while the agent is intact), `--force`/`--delete-adopted-branch` (worktree guards). Safe order: PR → terminate → clear record → remove worktree, so a failed push leaves the agent running. |
| `terminate` | Stop an agent (kill tmux + claude); **keeps** the record and worktree. The safe, reversible "stop" default. Alias for `stop --keep-record --keep-worktree`. |
| `restore` | Recreate and resume a lost/orphaned agent's session (`claude --resume`). |
| `recover` | Safety net for the tombstone reaper: scans **archived** records for ones whose tmux session is confirmed still alive (a stale `orphaned` status racing a daemon restart could previously let one get archived out from under a live session). Bare `wd agent recover` only reports candidates; `--apply` re-inserts each one into the active store under its original id — any children (linked via `parent_id`, untouched by archiving) reconnect automatically. `--json` for scripting. Mirrors the `recover_agents` MCP tool. |
| `done` | Terminate **and** clear the record in one step (worktree kept). Alias for `stop --keep-worktree`. `--hard` purges instead of archiving. `--create-pr` first pushes the agent's branch and opens a GitHub PR (`gh`) titled from the agent and bodied from its digest (`--base` sets the target, default main) — the PR is opened *before* termination, so a failure leaves the agent running to retry; an existing PR for the branch is reported, not re-created. |
| `delete` | Clear the stored record (archive by default, `--hard` purge). Leaves tmux + worktree alone. Alias for `stop --keep-worktree` (record only). |
| `remove-worktree` | Remove the git worktree + branch. **Destructive** — refuses while the agent runs or has uncommitted/unpushed work unless `--force`. Alias for `stop --keep-record` (worktree only); always asks unless `--yes`. |
| `worktree` | Umbrella for warden's worktree operations — **list** and **prune**. Bare `worktree` prints the list (same as `worktree list`). |
| `worktree list` | List warden-owned worktrees under `.worktrees`, joined to active/archived records (provenance-tracked). Alias: `worktree ls`. |
| `worktree prune` | Reclaim orphaned warden worktrees (always prompts; `--force` overrides guards, `--include-archived` widens scope). Retention is policy-driven via the `worktree.keep_done` / `worktree.auto_prune` config settings. Available unchanged as the top-level alias `prune`. |
| `adopt` | Register an existing Claude session — resume newest-for-dir under tmux, or live-register a running tmux session. |
| **Cascade cleanup** | Deleting a pipeline/agent cascades cleanup of its shared-context keys and (on hard-delete) its mailbox inbox. |

---

## 4. Observation & interaction

| Command / feature | Description |
|---|---|
| `ls` | List active agents (type, status, age, dir, subject). `--json` for machine output. `--watch`/`-w` live-updates the table on every agent state change over the daemon's SSE stream (Ctrl+C to exit); combine with `--json` to stream one JSON snapshot per change. |
| `status <id>` | Full detail for one agent — workdir, subject, worktree, branch, PR, events. `--json` available. |
| `attach <id>` | Attach your terminal to the agent's tmux session interactively. |
| `send <id> <msg>` | Type a message into the agent's claude session and press Enter. |
| `tail <id>` | Print recent terminal output (`--lines N`). |
| `digest <id>` | Completion digest — files touched, branch, turn count, and a best-effort `claude -p` narrative. `--json` available. |
| **Stuck / attention detection** | Agents flagged `waiting_for_input`, `idle` (stuck), `orphaned`, or `errored`, surfaced across all interfaces. |

---

## 5. Approvals inbox

Answer routine Claude tool-permission prompts (from supervised agents) without
attaching. Controlled by the `approvals` config setting (on by default).

| Surface | How |
|---|---|
| **CLI** | `warden approval list` lists recognized pending prompts with their numbered options; `warden approval answer <id> <n>` answers one. |
| **Web** | One-click option buttons in the AttentionQueue. |
| **TUI** | A pinned **⏳ Approvals** row (`i` / `enter`, then `1`-`9`; `tab` cycles agents). |
| **Safety** | A TOCTOU re-capture + fingerprint re-verify guards answers; unrecognized prompts always fall back to attach. |

### Auto-Approve

Automatically answer recognized tool-permission prompts. Off by default (opt-in
safety). Two cooperating layers:

1. **Per-agent toggle** (`warden approval auto set <id> on|off`) — opt one agent into
   evaluation even when the global policy is disabled.
2. **Rule policy** (the `auto_approve` config block / `warden approval auto set`
   subcommands) — an allow/deny engine evaluated for every participating agent.

**Decision order** (a prompt is auto-answered only if all pass):
- The built-in **destructive deny-list** (delete, `rm -rf`, force, push, deploy,
  reset --hard, …) **always wins** — it is checked first and is not configurable.
- It must match an **allow** rule and match **no deny** rule. Deny wins over allow.
- It must be a recognized yes/no prompt with an affirmative option; the
  least-privilege ("yes" over "yes & don't ask again") affirmative is pressed.
  `allow_sticky: true` permits pressing standing "don't ask again" options.

**Rule fields** (a rule matches when every present field matches; an absent field
is a wildcard — an empty rule matches everything, so it is refused on the CLI):
- `tool` — exact tool name, case-insensitive (e.g. `Read`, `Bash`).
- `pattern` — case-insensitive glob/substring over `Tool(arg)` and the question.
- `regex` — a **Go regular expression** over `Tool(arg)` and the question.
- `paths` — globs against path tokens in the action argument.

**Per-agent overrides** live under `agents:` keyed by agent name or id; each is its
own `{enabled, allow_sticky, rules}` block that replaces the default for that agent
(and can enable auto-approve for just that agent).

**Backward compatible:** with **no rules** configured, an enabled policy is the
simple legacy toggle — it approves every recognized, non-destructive prompt. So
`auto_approve: true` keeps working unchanged.

**Circuit breaker** (always on): when the *identical* prompt keeps re-appearing
after being approved — the agent is re-running a failing command and re-asking,
so approval isn't unblocking it — warden stops auto-approving after
`max_repeats` consecutive identical approvals (default 10), records an
`approval_loop` anomaly event, fires the operator notification, and leaves the
prompt to a human (the agent shows `waiting_for_input`). `max_repeats: 0` uses
the default; a negative value disables the breaker; per-agent overrides inherit
the default's value when unset. A different prompt, or ~10 quiet minutes,
resets the run.

**Configuration:**
```yaml
# ~/.warden/config.yaml
auto_approve:
  enabled: true
  allow_sticky: false
  rules:
    allow:
      - tool: Read
      - regex: '^Bash\(git (status|diff|log)\)'
    deny:
      - tool: Bash
        pattern: rm
  agents:
    reviewer:                 # override for the agent named/ided "reviewer"
      enabled: true
      rules:
        allow:
          - tool: Grep
```
```bash
warden approval auto rules                       # show the live policy
warden approval auto enable                       # master switch on
warden approval auto allow --tool Read            # append an allow rule
warden approval auto allow --regex '^Bash\(git (status|diff)\)'
warden approval auto deny  --tool Bash --pattern rm
warden approval auto allow --agent reviewer --tool Grep
warden approval auto clear --agent reviewer       # drop a per-agent override
warden approval auto set abc123 on                    # per-agent participate toggle
```

Rule changes via the CLI/MCP take effect **immediately** (no restart) and are
persisted back to the config file. Editing `config.yaml` by hand applies on the
next daemon start.

**Safety:**
- Off by default (must explicitly enable).
- The destructive deny-list always wins; no allow/regex rule can un-block it.
- Only works with recognized prompt grammar (strict parser); never retries on
  failure (fail-safe to manual approval).
- Does not bypass the approvals inbox (works alongside it); all attempts are logged.

---

## 6. Multi-agent collaboration

| Feature | Description |
|---|---|
| **Pipelines** (`warden pipeline`) | YAML-defined **DAG of dependent agent jobs**. The daemon runs them: dependency-free jobs start first, each job's `emit` publishes output and unblocks dependents — keeping the lead orchestrator agent off the critical path. Sub-commands: `validate` (client-side spec check, CI-friendly exit codes), `create` (from a spec file **or** a built-in `--template`), `list-templates`, `start`, `pause`/`resume` (halt new spawns while in-flight jobs finish), `show`, `list`, `edit-job`, `retry`, `cancel`, `delete`. Drivable from the CLI, the MCP tools (create/start/show/cancel/list), and with full TUI + web visibility (DAG view). |
| **Pipeline templates** | Four `go:embed`-bundled starters — `analyze-implement-review`, `parallel-tasks`, `test-fix-verify`, `research-synthesis` — render via `warden pipeline create --template <name>` with `{{NAME}}`/`{{REPO}}` (auto-filled) and `--set KEY=VALUE` placeholder substitution. `warden pipeline template list` lists them and their placeholders. |
| **Conditional steps** (`run_if`) | Per-job `run_if: success\|failure\|always` (default `success`). A job runs only when its dependencies settled the right way — `failure`/`always` handlers let a pipeline route around a failed upstream and still complete, and the handler's prompt is told which upstream failed. |
| **Shared context** (`warden context`) | A namespaced key/value blackboard all agents can read/write: `ctx set`/`get`/`list`. |
| **Directed messages** (`warden message`) | Per-agent inbox: `msg send` (wakes a parked idle/waiting agent), `msg inbox`, `msg wait` (blocks in the daemon until a message arrives). |
| **File-conflict detection** (`warden workspace`) | The daemon watches each active agent's worktree and warns (via the inbox, deduplicated) when two agents are editing the same file. Detection is **fsnotify-first**: edits record dirty paths in memory and conflict checks compare those sets **without spawning git on every save**; a slower `git diff` reconcile (`collab.git_reconcile_interval`, default `2m`) refreshes state after commits/reverts and when events are missed. Read-only git uses `GIT_OPTIONAL_LOCKS` and skips when `index.lock` is held so user `git pull` / `git add` are not blocked. The `collab.interval` poll (default `10s`) reconciles the inotify watch set against the active-session view. Degrades cleanly to pure git polling if fsnotify is unavailable or the inotify watch budget (80% of the per-user limit) is exhausted. `GET /api/v1/collab/conflicts` serves a cached snapshot so dashboard polls do not multiply git subprocesses. Inspect with `collab conflicts` / `collab who-is-editing <file>`, the `get_collaboration_status` / `who_is_editing_file` MCP tools, or the **File conflicts** card on the dashboard's Others tab. Spawned agents also get a system-prompt hint to check `who_is_editing_file` and their inbox before editing shared files. Tunable via `collab.enabled` / `collab.interval` / `collab.git_reconcile_interval` / `collab.hint`. |
| **Branch tracking** (`warden workspace branches`) | Opt-in daemon monitor that reports, per active agent with a branch, its **GitHub CI status** (latest `gh run list` inside the worktree → success/failure/pending/none) and its **standing vs `origin/main`** (commits behind/ahead, and whether already merged). Alerts are **informational, never blocking**: a newly-observed CI failure delivers an inbox note to the agent **and** a desktop notification to the operator (desktop is reserved for CI failures); a merged branch or one fallen >10 commits behind delivers an inbox nudge. A 5-minute dedup window keyed on `(branch, signal-state)` suppresses repeats but re-alerts on a state change (pending→failure). Every subprocess **fails open** — a missing/unauthenticated `gh`, a timeout, or a non-repo worktree simply skips that branch for the tick. Inspect read-only via `warden workspace branches` (`--json`), `GET /api/v1/collab/branches`, or the `get_branch_status` MCP tool. Off by default; enable with `branch_track.enabled` / tune `branch_track.interval` (default `2m`). |

---

## 7. Handoff — one verb (`warden agent handoff`)

`warden agent handoff` is the single concept for passing work to another agent. It has
three modes, distinguished by who runs the work next and whether the caller
survives. Phase 1 (writing the handoff file + resume prompt) is driven by the
`/warden` skill; this verb performs the delivery.

- **New delegate (default)** — spawns a fresh delegate in its **own isolated
  worktree** for a sub-task; the source agent **keeps running**.
- **Existing agent (`--to <id>`)** — delivers the handoff into an already-running
  agent's inbox (waking it); the source agent **keeps running**.
- **Retire self (`--retire`, requires `--confirm`)** — spawns a successor in the
  calling agent's **same workdir/worktree**, then reaps the caller
  (self-succession). This is what the `warden agent rotate` **alias** runs.

`--retire` and `--to` are **mutually exclusive** (one reaps the caller, the other
never does). Invariants enforced at compile time via the minimal client
interfaces each mode uses:

- The **retire** path **reuses the worktree by cwd and never removes it** (the
  rotator interface omits worktree removal). **Spawn-before-reap** is fail-safe —
  if the successor fails to spawn, the caller keeps running.
- The **delegate / `--to`** paths **never terminate the source** (the handoff
  interface omits termination). For `--to`, the handoff file's **content is
  inlined** into the message — the recipient runs in a different worktree and
  can't read the source's file by path.

`warden agent rotate` and the `rotate_agent` MCP tool remain as **thin aliases** for the
retire mode (`warden agent handoff --retire` / `handoff_agent {retire:true}`) — same
flags, same behavior.

### Fork — branch an agent's session sideways (`warden agent fork` / `fork_agent`)

`warden agent fork <agent> ["<prompt>"]` (shorthand for `warden start --fork-from
<agent>`) forks an existing agent's recorded session into a **new** managed agent.
Where handoff/rotate **carry the task but drop the conversation**, a fork
**branches the conversation/reasoning itself**: it continues the source's session
rollout (Codex's `codex fork`) in a divergent timeline as its own managed agent —
a fresh sibling worktree off the source's branch HEAD, seeded with the source's
uncommitted **tracked** changes (dirty-tree carry, default-on; untracked /
`.gitignore`'d build artifacts are not carried), with its own tmux session warden
monitors and tears down. **The source agent keeps running, untouched** — fork
branches *sideways*, contrasting with `snapshot` (rewinds one timeline) and
`rotate`/`handoff` (reap or delegate the task). The fork inherits the source's repo
+ backend; `--type` defaults to `development`.

Fork is **backend-gated**: only backends implementing `agentbackend.SessionForker`
are forkable — **Codex** today; forking a non-forking backend (e.g. Claude)
reports a clean "cannot fork" (additive, on-top — Claude is untouched). The
source's backend session id must already be pinned (it must have run a turn). Like
handoff/rotate it is a **thin wrapper over the one `fork_from` spawn field — no new
daemon endpoint** — so unlike the worktree-local `wd git review` / `wd backend model`
superpowers it crosses the daemon and has **MCP + CLI parity** via the `fork_agent`
MCP tool (design step 6, #52).

---

## 8. Terminal UI (cockpit)

`warden tui` (or bare `warden`) opens a **tmux-composited cockpit** with three
panes: the **control** pane (a navigator tree, top-left), a **terminal** pane
(bottom-left), and a full-height **agent** pane (right) for the selected agent.

| Feature | Description |
|---|---|
| **Control pane** | Polls the daemon ~1×/sec; browse with `↑`/`↓` without disturbing the viewports. A tree of four fixed collapsible sections — **Approvals · Pipelines · Agents · Terminals**. Each agent row carries a compact **backend** token (claude/aider/…); the full **agent info** pane (`i`) lists every stored field — backend, model, role, tags, context, location, refs, rate-limit, lifecycle, plumbing, last pane excerpt. An agent with no recorded backend renders as **claude**. |
| **Approvals section** | Pending permission prompts live in a persistent section (count + one row per prompt) rather than an overlay; `Enter`/`i` opens the answer overlay focused there. |
| **Pipelines section** | Pipelines are one of the four fixed sections; expand/collapse, open running jobs, retry failed jobs. |
| **Agent sub-trees** | Agents spawned by another agent (`spawn_agent`) nest under their parent as a collapsible sub-tree (`▸ / ▾`, indented per depth, `h`/`l` to toggle); a cross-project child surfaces under its own dir with a `↳ from <parent>` backlink. Deleting a parent with live children keeps it as a muted **terminated tombstone** header (`terminated · N running`) with no attach pane, so children never orphan; the daemon reaps the tombstone once the whole sub-tree goes terminal. `Enter` on a finished agent or tombstone opens its stored detail instead of attaching to a dead session. |
| **Terminals section + terminal pane** | Terminals are first-class `kind=terminal` sessions listed under the **Terminals** section and shown in the bottom-left **terminal** pane. A **default terminal** opens in the launch directory at startup, and the cockpit always keeps at least one. Names update live as the shell `cd`s (`<index>. <repo>:<rel>/ (<branch>)`). Press **`t`** to create/focus a terminal in the opened agent's directory (`(c)reate`/`(f)ocus`); `x` closes one. |
| **Directory groups** | `o` opens a directory as a group (becomes the spawn target for `n`), with `/fs/dirs` tab-completion. |
| **Agent info + editing** | `i` opens the **agent info** pane (every stored field) with three interactive controls — **toggle auto-approve**, **cycle force-compact** (inherit → on → off), and open the **event log** (`e`, newest first). `esc` walks back: events → info → tree. |
| **In-cockpit actions** | `n` new agent, `t` new/focus terminal, `s` send, `a` attach (full-screen), `d` digest overlay, `i` agent info, `e` event log, `p` approvals, `c` context/message inspector, `x` terminate/cancel/close, `D` delete pipeline record, `?` help. |
| **Viewport rotation** | Global **Alt** bindings (work from any pane, even while typing): `Alt+t` cycles the terminal pane over terminals, `Alt+a` cycles the agent pane over all agents, `Alt+p` cycles the agent pane over pipeline agents. Add **Shift** (`Alt+Shift+t/a/p`) to rotate in reverse. Each rotation grabs focus on the pane it drives, so cycling agents drops you straight into the session (mirroring the terminal rotation). A config-free **`Ctrl-b` prefix fallback** (`Ctrl-b` then `t`/`a`/`p`, Shift for reverse) runs the same rotation for terminals that don't send Alt/Option as Meta — **macOS Terminal.app / iTerm2** by default. |
| **Opened marker (◆)** | The agent (Agents section or Pipelines job row) shown in the agent pane and the terminal shown in the terminal pane are marked with a **◆** — and their name carries a bold magenta badge — in the control tree; it tracks both `Enter`-open and the Alt rotation, so what's docked stays visible after the cursor moves. |
| **Pane focus** | Move focus with `Alt+←/→/↑/↓` (no tmux prefix). |
| **Native scrolling** | Per-agent tmux sessions enable `mouse on` + raised `history-limit` for wheel/copy-mode scrolling of long output. |

> Requires tmux ≥ 3.1. From a plain terminal it builds its own tmux session;
> from **inside an existing tmux session** it lays out as a **native tmux
> window** in that session instead of nesting (auto-detected via `$TMUX`;
> force with `warden tui --tmux-native`, or force the classic own-session
> cockpit with `env -u TMUX warden tui`). The native-window layout has **no
> terminal pane** (control + agent only), so the default terminal, `t`,
> Enter-on-terminal, and `Alt+t` degrade to a status hint there. See USAGE.md §7.

---

## 9. Web GUI

The daemon embeds a React (Astro) dashboard at `http://localhost:8765` — no
separate server.

| Feature | Description |
|---|---|
| **URL-routed shell** | Tabs are real URLs via the History API — `/cockpit` (home), `/pipelines`, `/metrics`, `/archive`, `/others` (catch-all, last), and `/agent/<id>` per pinned agent. `/tui` is a full-screen route (launched from the top bar, not a tab). Back/forward, refresh, and shareable deep links all work; `/` redirects to `/cockpit`. |
| **Full-screen TUI** | A highlighted top-bar **▢ TUI** button opens a full-screen terminal (`/tui`) that streams the **literal `warden tui`** — the daemon builds a shared three-pane cockpit (control pane + terminal pane + agent pane) and bridges it to the browser over the same WebSocket PTY as a per-agent attach (xterm.js). It takes the whole viewport, edge-to-edge and non-scrollable, with none of the dashboard chrome; **Ctrl+Q exits** back home (from any pane). Every TUI keybinding (`enter/n/t/o/s/a/i/x/D/r/?/j/k/g/G`), Alt+Arrow pane navigation, Alt+t/a/p viewport rotation, the real terminal (with its own autosuggestions/tab-completion), and the real agent (Claude Code by default) in the agent pane behave exactly as they do locally. Shared across clients (`window-size latest` lets the active client drive sizing); built lazily on first attach. **Self-healing:** because the cockpit lives in the tmux server (not the daemon) it survives daemon restarts/reinstalls, so before reusing an existing session the daemon validates its shape (three panes; the top-left control pane genuinely running `warden tui --pane=control`) and transparently kills+rebuilds a wedged one — every attach lands on a healthy cockpit. `warden tui --rebuild-web-cockpit` forces a fresh rebuild on demand (an escape hatch that no longer needs shelling into tmux). Shift+Enter is mapped to the agent's newline. It's a desktop/laptop surface — the three-pane cockpit wants width, so there's no mobile key bar; a few browser-reserved chords (`Ctrl+T/W/N`) are the only fidelity gap, mostly reclaimed by installing the PWA. |
| **Cockpit (home)** | Default view: a slim **Fleet** header (totals · busy · waiting · errored, pressure, per-dir counts) above the canonical agent grid. The old *Quick spawn* widget and duplicate *All agents* mini-grid were removed. |
| **Others (catch-all)** | The former *Overview*, renamed: holds *Needs you* (attention queue with one-click approvals), *File conflicts*, and *Recent activity*. The landing spot for any not-yet-homed widget. |
| **Live fleet over SSE** | No manual refresh; coloured busy/idle badges (Starting, Busy, Needs input, Idle, Done, Error, Orphaned) + each agent's subject. |
| **Metrics tab** | A responsive grid of uPlot chart cards — **two columns** on wide screens (each per-agent chart sits beside its fleet-wide total), a **single column** on phones: **CPU per agent** + **Total CPU**, **Memory per agent** (GiB) + **Total memory**, **Context per agent** (client-accumulated time series of live context fill, legend dot colored `ok`/`warning`/`critical`; in-session only — resets on full reload), **Number of agents** (fleet size over time), **Tokens saved** (the saved-tokens trend from the savings ledger with a **window picker** — `24h`/`48h` bucket by hour, `7d`/`30d`/`All` by day — so a fresh ledger plots a real curve not a single point; a per-bucket saved area + a running-cumulative line + a headline saved-tokens/$ figure; a "set `savings: true`" hint when the ledger is disabled), and **Savings by feature** (a per-feature stacked-area breakdown of that trend). A full-width **Live footprint** card carries the former Resources panel. |
| **Context & Messages overlay** | No longer a tab — opened from a small **🗒 header button** as a dismissible overlay (Esc closes). |
| **Agent grouping** | The Cockpit grid buckets agents into collapsible panes by **Directory / Type / Status / Tag / Agent** (the *Agent* dimension groups by backend — claude/aider/…; multi-tagged agents appear under each tag; untagged agents bucket together). The choice is saved to LocalStorage. |
| **Per-agent backend logo** | Each agent card's header carries the brand mark of the backend driving it (claude/aider/…), so you can tell at a glance which agent is which. An agent with no recorded backend (pre-#52) shows as **claude**; an unregistered backend gets a monochrome lettermark chip. |
| **Interactive terminal** | Pin an agent to get a live `tmux attach` bridged to the browser over a WebSocket (xterm.js) — type into the agent and watch it respond. |
| **Create agent** | **+ New agent** prompt box with a directory picker (live prefix autocomplete) and a **Supervised** checkbox. |
| **Terminate with git guard** | Surfaces a 409 → **Force** + optional hard-delete when there's uncommitted/unpushed work. |
| **Digest panel** | View an agent's completion digest in the browser. |
| **Pipeline DAG view** | Pipelines rendered as their dependency graph (`PipelineDag`). |
| **Browser notifications** | Opt-in desktop notification when an agent enters `waiting_for_input` (gated to hidden tabs). |
| **Theme toggle (light/dark/system)** | Header toggle cycles **System → Light → Dark**, defaulting to System (follows the OS via `prefers-color-scheme`). The choice persists in `localStorage` (`warden.theme`) and is applied before first paint to avoid a flash. The palette is themed through CSS custom properties keyed on `<html data-theme>`, which also pins `color-scheme` so system colors flip on an explicit override; the wordmark swaps to match the resolved theme. |
| **Keyboard shortcuts** | Global shortcut layer: `?` toggles a help overlay (also reachable from the header `?` button), `n` new agent, `/` focus the agent filter, `r` refresh the fleet, `1`–`9` jump to a tab, `j`/`k` next/previous tab, `Esc` close a modal/overlay or blur a field. Single-key bindings stay dormant while typing in a field (Esc still fires). |

---

## 10. Orchestration (MCP)

`warden daemon mcp` is a stdio MCP server so an orchestrator agent session (e.g. Claude) can manage
the fleet through tool calls. **81 tools** are exposed — every fleet/data feature
the CLI has, so the skill/MCP can drive warden at full parity (only the
host/process/interactive/secret commands in the [feature catalog](../FEATURES.md)
stay CLI-only). Tools exposed:

| Tool(s) | Purpose |
|---|---|
| `list_agents` / `get_agent` | List agents / full detail for one |
| `spawn_agent` | Spawn (prompt mode or `type`+`repo`; `supervised` opt-in) |
| `adopt_agent` | Register an existing Claude session |
| `send_to_agent` / `get_agent_output` | Type into / read recent output of an agent |
| `digest` | Compact catch-up summary of one agent |
| `stop_agent` | **Umbrella teardown.** Default = full teardown (terminate + clear record + remove worktree); `keep_record` / `keep_worktree` subtract steps, `hard` purges, `pr`/`base` open a PR first, `force`/`delete_adopted_branch` for the worktree guards |
| `terminate_agent` / `restore_agent` | Stop (reversible) / resume an agent |
| `delete_agent` / `remove_worktree` | Clear record / remove worktree (guarded) |
| `list_worktrees` / `prune_worktrees` | List / reconcile a repo's worktrees |
| `handoff_agent` / `rotate_agent` | Hand off work — delegate to new/`to` existing agent, or `retire`→successor in place; `rotate_agent` is an alias for `handoff_agent {retire:true}` |
| `fork_agent` | Fork an agent's recorded session into a new managed agent (branches the conversation; Codex-only; dirty-tree carry; source keeps running) — thin wrapper over `spawn_agent` with `fork_from` set, mirroring `warden agent fork` |
| `ctx_set` / `ctx_cas` / `ctx_append` / `ctx_get` / `ctx_list` | Shared-context blackboard |
| `send_message` / `read_inbox` / `wait_for_message` | Directed messaging (park/wake) |
| `get_collaboration_status` / `who_is_editing_file` | File-conflict detection |
| `get_branch_status` | Per-agent CI + branch-vs-main status |
| `list_approvals` / `approve` | List / answer pending tool-permission prompts |
| `set_auto_approve` / `set_auto_approve_policy` | Per-agent auto-approve toggle / manage the allow-deny rule policy (tool/glob/regex/paths, per-agent) |
| `set_permission_mode` | Permission-mode change for a running agent |
| `commit` / `push` / `sync` | Git lifecycle on the agent's pinned worktree — staged commit (auto-message when omitted), push (`force` → `--force-with-lease`), rebase-sync — returning compact structs instead of raw git output (see §22) |
| `check` | Run the project's `.warden/check.yml` checks, returning pass/fail with output for only the failing ones (see §22) |
| `create_pipeline` / `list_pipelines` / `show_pipeline` | Create a DAG pipeline from a YAML spec / list / inspect (jobs, branches, handoffs) |
| `start_pipeline` / `cancel_pipeline` / `pause_pipeline` / `resume_pipeline` | Run / cancel / pause / resume a pipeline |
| `retry_pipeline_job` / `edit_pipeline_job` / `emit_pipeline_output` / `delete_pipeline` | Per-job retry / edit a pending job / set handoff output / delete a pipeline |
| `validate_pipeline` / `list_pipeline_templates` | Local spec validation / built-in templates (no daemon) |
| `library_list` | Browse saved spawn presets, saved prompt templates, and built-in pipeline templates in one call (no daemon) |
| `list_schedules` / `get_schedule` / `create_schedule` / `enable_schedule` / `disable_schedule` / `delete_schedule` | List / get / create / enable / disable / delete daemon cron/at schedules (see §28) |
| `snapshot_create` / `snapshot_list` / `snapshot_restore` | Worktree+transcript checkpoints & rollback (see §23) |
| `insights` | Mine fleet history for patterns & parallelization wins (see §25) |
| `get_metrics` / `get_pressure` | Live/historical resource metrics / memory-pressure gate (see §11) |
| `savings` | Token-savings ledger (see §29) |
| `search` / `history` / `audit_log` | Full-text search / archived agents / action audit trail |
| `list_plugins` | Registered plugins, their task types & hook events (see §26) |
| `export_sessions` / `import_sessions` | Serialize / load agent session metadata (see §19) |
| `list_backends` / `rescan_backends` / `set_backend_tier` / `set_default_backend` / `set_thinking_mode` | The agent-backend registry — list / rescan detection / set tier / set default / set internal-thinking mode (see §35). Enable/disable is CLI/web/TUI + REST `PATCH /backends/{id}` only |

> All MCP tools are thin wrappers over the same daemon routes (or local helpers)
> the CLI uses, so an orchestrator agent session can drive a multi-stage workflow
> (analyze→implement→review) — including pause/resume/retry/edit-job and
> scheduling — without shelling out. The complete coverage matrix is in
> [FEATURES.md](../FEATURES.md).

### `/warden` Claude skill

A packaged Claude Code skill teaches any Claude session *how and when* to manage
the fleet (triage, create-from-prompt, relay "tell X to do Y",
terminate-with-confirmation, daemon-down handling, self-rotation). It drives the
MCP tools, falling back to the `warden` CLI when the MCP server isn't registered.

---

## 11. Observability & notifications

| Feature | Description |
|---|---|
| **Resource metrics** | `internal/metrics` collects per-agent process-tree RSS/CPU, system memory/swap/pressure, and daemon self-stats. Exposed via `GET /api/v1/metrics` + `GET /api/v1/metrics/history`. |
| **Memory-pressure spawn gate** | `internal/pressure` reads the kernel's own pressure signal — macOS `kern.memorystatus_vm_pressure_level`, Linux PSI (`/proc/pressure/memory` avg10, mapped conservatively onto the same normal/warn/critical scale; other platforms degrade to normal) — sampled every 5s, and decides each spawn. A spawn is **blocked** (`428` + confirmation payload, re-run with `--force`) only at **critical** pressure or when live agents reach `worktree.spawn_gate_max_agents`. **Warn** pressure is **advisory** — the spawn proceeds and the daemon logs it — because warn is a common, recoverable state and hard-gating there just trained reflexive `--force`. The `GET /api/v1/pressure` gauge reports the level (UI colours warn amber, critical red) and whether spawns are currently blocked. Master switch `worktree.spawn_gate`. |
| **`warden inspect resources`** | CLI view of the resource metrics. |
| **Metrics recorder** | Optional 15s JSONL recorder (the `metrics` setting). |
| **Agent performance history** | The recorder's samples roll up per agent into runtime, peak/latest/trend RSS, avg/peak CPU, context-token trend, and changed-file count, plus conservative anomaly warnings (climbing memory, climbing/critical context, pinned CPU). Surfaced via `warden inspect resources --history [--agent ID]` and `GET /api/v1/metrics/history?summary=true[&agent=ID]`. |
| **Crash & anomaly detection** | Beyond stuck-state reclassification, the poller flags an **OOM kill** (SIGKILL/exit 137), an **infinite loop** (a churning pane cycling through a few states, distinct from the stuck timer's stale pane), and a **pre-crash context** condition (a critical-but-still-working agent that can't be auto-compacted). Each records a durable `anomaly` event and fires a best-effort notification through the `OnAnomaly` seam (once per episode). |
| **Desktop notifications** | The `notify.enabled` setting posts a desktop notification (macOS `osascript` / Linux `notify-send`, log fallback) when an agent needs attention (`waiting_for_input`, stuck `idle`, `orphaned`, `errored`). |
| **Webhook / Slack notifications** | When `notify.webhook_enabled` is on, warden also POSTs a JSON payload to `notify.webhook_url` for every alert that goes to desktop notifications — attention-needed transitions (`waiting_for_input`, `errored`, `orphaned`) and context-size warning/critical alerts. A **Slack incoming-webhook URL works out of the box** (the payload's `text` field is what Slack renders); generic consumers get `{text, title, body}`. Best-effort and non-blocking: a short timeout bounds each POST and failures are logged, never propagated. Runs alongside (not instead of) desktop notifications via the same notifier seam — this is what makes "watch from your phone" push real. |
| **Context-size guard** | `internal/ctxtokens` reads each live agent's context-window fill from its transcript and classifies it `ok`/`warning`/`critical`. The poller shows a state-colored token figure in `ls`/TUI/web, alerts once per upward crossing (`tokens.warn_alert`), and auto-sends `/compact` at `critical` when the agent is idle (`tokens.auto_compact`, cooldown-guarded). Master switch `tokens.guard`; thresholds `tokens.warn`/`tokens.critical`. |
| **Force-compact (busy agents)** | Opt-in (`tokens.force_compact`, off by default; per-agent override via `warden agent compact set <id> on\|off\|inherit`). When an agent goes `critical` while **still working** — the case `tokens.auto_compact` can't touch — the poller runs a small state machine that mirrors the manual fix: interrupt the agent (Escape), `/compact` once it idles, then send `tokens.compact_resume_prompt` so it resumes the discarded work. **Destructive**: the in-flight turn is lost. An interrupt that doesn't take within ~45s is abandoned, falling back to the pre-crash nudge. Drivable via the `set_force_compact` MCP tool. |
| **Structured logging** | `internal/logging` installs a `log/slog` logger (also bridging the standard `log` package) so daemon logs carry structured fields. Level (`log.level`: `debug`/`info`/`warn`/`error`) and format (`log.format`: `text`/`json`) are configurable; `warden daemon --log-level`/`--log-format` override them. |

---

## 12. Configuration (YAML config file)

Settings live in a single YAML file (default `~/.warden/config.yaml`). Run
`warden config init` to generate a fully-commented file, edit values, and warden
**applies the change live — no daemon restart** (see §12.1); `warden config`
prints what's live. `--config <path>` selects an alternate file; `--addr
<host:port>` overrides the daemon address per-command.

### 12.1 Live hot-reload

The daemon **watches `~/.warden/config.yaml` and applies edits without a
restart** (fsnotify, 500 ms debounce; a burst of writes coalesces into one
reload). A **bad edit keeps the last-good config** and alerts the owner ("config
reload failed — keeping last-good settings") rather than degrading to defaults —
so a mid-run typo can never wipe your settings. Keys that genuinely need a
restart are **logged as changed-but-pending** rather than silently ignored.

**Hot-reloads (applied on the next tick/spawn):** the whole `autopilot` template
(plan/manager/merge settings + adding/removing plans + the per-repo reconcile),
`auto_approve` policy, `tokens.*` (context/token guard), `rails.*`,
`model_default`, `default_permission_mode`, the pipeline/collab/memory
system-prompt hint gates, `notify.*` + webhook, `api_docs`, and the
`scheduler_enabled` route gate.

**Still needs a restart (logged on change):** `addr`, `data_dir`,
`claude_projects_dir`, `metrics`, the scheduler reconcile loop cadence,
`trusted_proxies`, `http.timeout_*`, `plugins.*`, `local_llm.*`,
`collab.enabled`/interval, `branch_track.enabled`/interval, `rate_limit.*`
timers, `auto_restart.*`, `log.*`, `memory.curate`, snapshots, and the autopilot
guardian tick `interval`.

| Setting | Default | Description |
|---|---|---|
| `addr` | `127.0.0.1:8765` | Daemon listen address (a non-loopback bind requires `WARDEN_TOKEN`) |
| `data_dir` | `~/.warden` | Warden state directory (sessions, prompts, inbox, pipelines, metrics) |
| `claude_projects_dir` | `~/.claude/projects` | Root of Claude Code transcript dirs (poller reads these) |
| `model_default` | `claude-sonnet-4-6` | Default model for spawned agents (id or alias: `sonnet`/`opus`/`haiku`/`fable`) |
| `default_permission_mode` | `auto` | Default permission mode (valid: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `plan`) |
| `notify.enabled` | `false` | Desktop notifications |
| `notify.webhook_enabled` | `false` | POST notifications to `notify.webhook_url` on attention transitions (runs alongside `notify.enabled`) |
| `notify.webhook_url` | _(empty)_ | Webhook endpoint; a Slack incoming-webhook URL works out of the box |
| `approvals` | `true` | The approvals inbox |
| `auto_approve` | `false` | Auto-answer recognized prompts. Bare on/off, or an allow/deny rule policy (tool/glob/regex/paths + per-agent overrides) |
| `tokens.guard` | `true` | Context-size guard master switch (gauge + alert + auto-compact) |
| `tokens.warn_alert` | `true` | Notify once per upward crossing into warning/critical (needs `notify.enabled`) |
| `tokens.auto_compact` | `true` | Auto-`/compact` at `critical` when the agent is idle (cooldown-guarded) |
| `tokens.force_compact` | `false` | Interrupt a `critical` **busy** agent, `/compact`, then resume it. Destructive; per-agent override via `warden agent compact set` |
| `tokens.compact_resume_prompt` | _(built-in)_ | Resume message sent to a force-compacted agent once compaction lands |
| `tokens.warn` | `200000` | Warning threshold in context tokens (resets with critical if critical ≤ warn) |
| `tokens.critical` | `400000` | Critical threshold in context tokens (auto-`/compact` band) |
| `allow_nonloopback` | `false` | **Deprecated / inert** — no longer bypasses auth; a token is mandatory for any non-loopback bind |
| `worktree.spawn_gate` / `worktree.spawn_gate_max_agents` | `true` / `5` | Soft spawn gate. Blocks (428, needs `--force`) only at **critical** OS memory pressure or when live agents reach the cap; **warn**-level pressure is **advisory** (spawns proceed, logged). A `0` cap disables the count trigger (pressure-only gating). |
| `tokens.budget_gate` / `tokens.budget_daily_usd` / `tokens.budget_weekly_usd` | `false` / `0` / `0` | Soft warning before spawning when measured model spend has reached a $ cap (see §30) |
| `metrics` | `true` | Record per-agent metrics to disk |
| `pipeline.keep_done` / `pipeline.hint` | `false` / `true` | Keep a job's agent after completion / append the decomposition hint |
| `memory.inject` | `true` | Project the repo's curated `.warden/memory.md` into every spawned agent's system prompt (Claude → `--append-system-prompt`; other backends → their `AGENTS.md`/`CRUSH.md`/`.goosehints` warden block). Off, or an empty/absent file, is byte-identical to no injection (see §22) |
| `memory.curate` | `false` | Auto-propose durable memory entries from completion digests into `.warden/memory.md` (see §22). Debounced pass writes **`unverified`, timestamped, provenance-tagged** proposals to the **working tree only** — never commits/pushes, so the committed diff is the human gate. Verify-before-trust promotion, supersession-on-contradiction, TTL age-out, deterministic path-staleness. Prefers the `$0` local model. Opt-in |
| `memory.ground` | `true` | Answer project questions (\"where does X live?\", \"how do I run Y?\") **locally** in `wd backend repl` from `.warden/memory.md` (see §22), via the `/memory` command and the `project_memory` tool. Served on the **local model only** — it **removes** cloud round-trips rather than adding tokens, so it is default on. Read-only (never creates/writes memory); with no local model it degrades to the matching entries verbatim (`$0`), never escalating to a paid model; answers cite each entry's trust + provenance |
| `worktree.keep_done` / `worktree.auto_prune` | `true` / `false` | Keep a worktree after its agent is done / auto-reclaim orphaned worktrees |
| `auto_restart.max` / `auto_restart.reset` | `3` / `5m` | Auto-restart attempts for an errored opted-in agent / health window that resets the counter |
| `rate_limit.auto_resume` / `rate_limit.resume_prompt` | `true` / `continue` | Auto-pick the "Stop and wait" limit-menu choice (Enter-first, verified) and auto-resume agents after any limit (session clock, weekly weekday/date, or monthly-spend) clears — typing `resume_prompt` to nudge the agent back to work (`""` = bare keypress). `rate_limit.retry_interval` / `spend_retry_interval` / `buffer` tune timing; every hit is snapshotted to `<data_dir>/ratelimit-captures/` for parser fixtures |
| `log.level` / `log.format` | `info` / `text` | Daemon log verbosity (`debug`/`info`/`warn`/`error`) and format (`text`/`json`); `warden daemon --log-level`/`--log-format` override |
| `rails.isolation_guard` | `true` | PreToolUse hook blocking an isolated agent from editing files outside its worktree (§22) |
| `rails.git_conventions` | `true` | Append the prompt steer toward `wd commit`/`push`/`sync` over raw git Bash (§22) |
| `rails.git_redirect` | `true` | PreToolUse hook denying raw `git commit`/`push`/`pull`/`rebase`, redirecting to the warden tools (reads stay allowed) (§22) |
| `rails.check_redirect` | `true` | PreToolUse hook redirecting a raw test/lint/build command registered in `.warden/check.yml` to `wd check` (§22) |
| `local_llm.enabled` | `false` | Route fuzzy-cheap tasks (classify, summarize, commit messages) to a local Ollama model; falls back to Claude on any error (§21) |
| `local_llm.url` / `local_llm.model` / `local_llm.timeout` | `http://localhost:11434` / `qwen2.5-coder:7b` / `20s` | Local Ollama server URL, model tag, and per-call hard timeout (§21) |
| `local_llm.tier` / `local_llm.escalate` | `auto` / `true` | Orchestrator planning-tier override (`auto`/`t0`/`t1`/`t2`) / allow one over-tier planning step to escalate to headless Claude (§17) |
| `local_llm.classifier` | `heuristic` | How the REPL buckets a request's needed planning tier: `heuristic` (cheap surface signals, no model call) or `model` (a one-shot local-model classification, one extra round-trip, falls back to the heuristic on any error) (§17) |
| `local_llm.repl` | `false` | **Deprecated / no-op.** Historically ran `wd backend repl` in the cockpit's shell pane; the cockpit no longer hosts a REPL pane (its bottom-left pane is now a first-class terminal), so this setting is inert. Run the REPL standalone with `wd backend repl` (§17). |

> **Config namespacing:** Settings are organized into five YAML blocks — `rails`, `tokens`, `notify`, `worktree`, `local_llm`. Old flat keys (`token_guard`, `local_llm_url`, `notify`, `spawn_gate`, `worktree_keep_done`, `isolation_guard`, `git_redirect`, etc.) are deprecated aliases — they still work and migrate to the namespaced form when `warden config init` is re-run.
>
> The old `WARDEN_*` environment variables are no longer read — the daemon warns
> once at startup if any are still set. The per-agent IPC vars warden injects
> (`WARDEN_SESSION_ID`, `WARDEN_PIPELINE_ID`, `WARDEN_JOB_ID`) are not configuration.

---

## 13. Remote access & authentication

Reach the dashboard and API from a phone, tablet, or any other device — not just
the local machine. Setup recipes (LAN / Tailscale / Cloudflare Tunnel) live in
[USAGE.md](USAGE.md).

| Feature | Description |
|---|---|
| **Bearer-token auth** | A 256-bit `crypto/rand` token gates every non-loopback request (constant-time compare). SSE/WS clients pass it as `?token=`. Binding the daemon to a non-loopback address is **refused unless a token is set**, so remote access can't be opened accidentally. |
| **Read-only token** | An optional second token, `WARDEN_READONLY_TOKEN`, grants view-only access: it may read everything (all GETs + the live event stream) but every state-changing action and the interactive attach return `403`. Mint it like the primary (`warden daemon token generate`); `warden daemon token show --readonly` prints it back. Only honored alongside a primary `WARDEN_TOKEN` — the daemon refuses to start with a read-only token but no primary one. |
| **Token management** | `warden daemon token generate` mints a token, `warden daemon token show` prints the current one (to paste into a remote client), and `warden daemon token rotate` regenerates it in place and restarts the daemon. Persisted to `~/.warden/token.env` (`WARDEN_TOKEN=<hex>`, `0600`); the `WARDEN_TOKEN` env var overrides the file so the secret can stay off disk. |
| **Brute-force protection** | Per-IP rate-limiting on auth failures. |
| **Web UI auth** | A token-entry modal appears on `401`, with `localStorage` persistence and a sign-out control; the static SPA shell stays public so the modal can load. |
| **Mobile-responsive dashboard** | A keyboard-aware layout sized to the visible viewport: a bottom nav that stays pinned above the soft keyboard, single-column grids, full-screen modal sheets, and an interactive terminal you can swipe to scroll (driving tmux/the agent's scrollback like a wheel) with a sticky on-screen key bar (Esc/Tab/Ctrl-C/↑↓/Bottom). |
| **API reference** | Interactive OpenAPI docs at `/api/docs` (raw spec at `/api/docs/openapi.yaml`) document every daemon route and the `bearerAuth` scheme for remote/API consumers — see §27. |

---

## 14. Full-text search

Find agents across a growing fleet without scrolling the grid. The matcher is
in-memory and case-insensitive, AND-ing every whitespace-separated term against a
haystack built from each session's id, name, ticket, type, subject, prompt,
branch, tags, and last-pane excerpt.

| Feature | Description |
|---|---|
| **`warden inspect search <query…>`** | CLI search over active sessions; multiple words are ANDed. `--closed` folds in the archived (`closed/`) store too, `--json` prints raw records. Renders with the same table as `warden ls`. |
| **`GET /api/v1/search?q=&closed=`** | Daemon endpoint (`internal/daemon/search_routes.go`); a blank `q` is a `400`. Returns the standard `{sessions:[…]}` shape. |
| **Web search bar** | The dashboard carries a search box that filters the agent grid live, client-side, mirroring the backend matcher (`web/src/lib/search.ts`) for instant feedback. |

**Tags.** Sessions carry an optional `Tags []string` (`warden start --tags backend,urgent`),
normalized to lowercase and deduped. Tags are part of the search haystack — a bare
`warden inspect search backend` finds every agent labelled `backend`. `warden ls --tag backend
--tag urgent` filters the list to agents carrying *every* given tag (AND semantics, repeatable
or comma-separated). Untagged sessions stay nil and JSON-omit the field, so the change is
backward-compatible with records that predate tags.

---

## 15. Agent history & archive

Browse and search agents that have already ended — warden persists every closed
session to the `closed/` store (newest-first), and this surfaces it.

| Feature | Description |
|---|---|
| **`warden inspect history`** | Lists archived sessions. `--since` accepts a duration (`24h`, `90m`, `7d`, `2w`), a date, or an RFC3339 timestamp; `--type` filters by normalized task type; `--limit` caps the count; `--json` prints raw records. |
| **`GET /api/v1/history?since=&type=&limit=`** | Daemon endpoint (`internal/daemon/history_routes.go`); `since` is RFC3339 (`400` on a bad value), `type` is normalized, `limit` caps the result. |
| **Web Archive tab** | A 🗄 Archive tab fetches history with since (all / 24h / 7d / 30d) and type selectors, plus a client-side text filter, rendering a table of ID / Name / Type / Status / Branch / Updated / Subject. |

---

## 16. Batch operations

Act on many agents at once from the web cockpit instead of one tile at a time.

| Feature | Description |
|---|---|
| **Multi-select** | Per-tile checkboxes on the Cockpit grid, with Shift-click range selection. Selections are pruned automatically when an agent ends. |
| **Bulk action bar** | A floating bar appears while ≥1 agent is selected, offering bulk **Message…**, **Terminate**, and **Delete** (the destructive ones need a second click to confirm). |
| **Sequential fan-out** | Actions reuse the existing per-agent endpoints (`POST /api/v1/sessions/{id}/terminate`/`delete`/`messages`) one at a time (`web/src/lib/batch.ts`); the bar reports partial success and keeps failures selected for retry. (Goroutine-parallel fan-out is parked as #36.) |

---

## 17. Interactive mode / REPL (`wd backend repl`)

warden's **interactive mode**: a proper terminal REPL to drive the fleet. Run it
standalone (`wd backend repl`, aliases `wd backend repl` / `wd i`). It
drives the fleet two ways — a **deterministic `/`-command half** that needs no
model, and a **natural-language half** that turns intent into **confirmed** warden
tool calls without spending cloud-model tokens. **It conducts; it never implements** —
there is no edit/write/bash/shell tool in its registry, so all code work is
delegated by `spawn_agent`-ing an agent.

It **starts without a local model** — the `/` commands and `!`-shell always work;
only the natural-language half needs `local_llm: true` (a bare line says so when
it's off). This deterministic fallback is why interactive mode no longer hard-fails
without a model.

| Feature | Description |
|---|---|
| **Real line editor** | readline-backed: arrow-key cursor movement, ↑/↓ history persisted to `~/.warden/orch_history`, Ctrl-R reverse-search, Ctrl-A/E/W/K/U editing, a **live `/`-command menu** that filters as you type (each matching verb + its summary, painted under the prompt, Claude-Code style) plus Tab completion of `/` command names **and live agent ids**, **guided argument forms** (pick-lists + free text) when a command needs more input, Ctrl-C to abandon a line, Ctrl-D (or `exit`) to close back to the shell. Prompt/headings are colourised via lipgloss, auto-disabled on non-TTY output and `NO_COLOR`. |
| **Deterministic `/` commands** | Type a `/`-verb (`/agents`, `/spawn <prompt>`, `/tell <id> <text>`, `/stop`, `/commit`/`/push`/`/sync`/`/check`, `/pipelines`, `/ctx*`, `/approvals`, … `/help`) and warden runs the exact verb with **no model in the loop** — reads auto-execute, mutations pass the same confirm gate as the model's calls. An unknown `/verb` is caught with a hint rather than falling through to the model. This is the reliable half: it keeps working when the local model is slow or wrong. |
| **Human-readable read output** | Deterministic read results are rendered for a person, not dumped as raw JSON: `/agents`/`/pipelines` print aligned tables (id · status · type · name · what), `/agent <id>` a tight labelled block (empty fields omitted), `/ctx` a key/value table, and `/inbox`/`/collab`/`/approvals` one compact line each; an unknown shape falls back to indented JSON. The local model still receives the structured JSON for natural-language planning — only the operator-facing `/`-command output is reshaped. |
| **Guided argument forms** | When a `/` command needs more than was typed, warden collects the arguments interactively instead of erroring: a **numbered pick-list** for fields with a known set (`model`, `permission_mode`, `type`, yes/no booleans) and **free text** for open fields. It opens automatically when a required arg is missing (bare `/spawn`), or for **every** field on a `+`-suffixed verb (`/spawn+ <prompt>`). Structure is deterministic (fields + valid options from warden's own enums, so it works model-off); when a local model is present each field opens with a **suggested value** inferred from your words — Enter accepts, type overrides, `-` clears to the config default. The model can only pre-select a *valid* option, never invent one. Shared with the gate's `[e]dit` flow, so the model's NL plans get the same pick-lists. After the form, mutations still pass the confirm gate. |
| **NL → tool-call loop** | Backed by the `internal/llm` `Chatter` seam (Ollama `/api/chat`, multi-turn tool-calling). A bounded turn budget stops runaway loops. The path is hardened against small-model slips: hallucinated args (a fabricated `repo`, a bogus `model`/`type`) are scrubbed before the gate, a required-arg gap is caught and fed back rather than approved into a doomed call, and a malformed tool call is retried as a recoverable hiccup instead of failing the turn. |
| **Read-vs-mutate registry** | Read-only verbs auto-execute (`list_agents`, `get_agent`, `get_agent_output`, `get_collaboration_status`, `read_inbox`, `list_approvals`, `ctx_get`, `ctx_list`, `pipeline_list`, `pipeline_get`). The same daemon client the MCP server uses — no new business logic. |
| **Mandatory confirm gate** | Every mutating verb (`spawn_agent`, `send_to_agent`, `terminate_agent`, `delete_agent`, `restore_agent`, `approve`, `commit`, `push`, `sync`, `check`, `ctx_set`, `send_message`, `pipeline_create`, `pipeline_cancel`, `clean_up`) requires explicit operator approval before it runs — **non-config-gated**, can't be disabled. A batched plan confirms as one unit. |
| **Capability-tier routing** | A cheap pre-classify buckets each request's needed tier against the model's tier (`modelTier`, override with `local_llm_tier`). Classification defaults to a deterministic heuristic; set `local_llm_classifier: model` to swap in a one-shot local-model verdict (falls back to the heuristic on any error) for the hard single-sentence asks the heuristic can't see. Within tier ⇒ plan locally; over tier ⇒ escalate one planning step to headless Claude (`local_llm_escalate`, default on) or degrade honestly — execution always stays token-free warden calls. |
| **Monitoring verbs** | `fleet_digest` / `agent_digest` summarize fleet & per-agent state (reusing the `Summarize` routing), `pending_for_me` surfaces what needs the operator, and `clean_up` proposes terminate/delete of finished agents through the same confirm gate. |
| **Local project grounding** | Ask a project question — `/memory <q>` (`/mem`/`/ask`), or the model-callable `project_memory` tool — and warden answers it **locally** from the repo's `.warden/memory.md` (#53 PR-3, `memory.ground`, default on). Read-only and `$0`: served on the **local model only** (never a cloud round-trip), it cites each entry's trust (`unverified`/`trusted`/`human`) + provenance so a stale hint reads as a hint, degrades to the matching entries verbatim when no local model is wired, and answers "not in project memory" for an absent/empty file (never auto-creating it). Grounding-style questions classify T0, so the tier router keeps them on the local plan — no escalation, no spend. |
| **`!`-shell passthrough** | A `!`-prefixed line runs in a persistent embedded `$SHELL` (cwd/env persist) and tees output to the terminal. The REPL takes **no action** on that output — no auto-diagnose/fix/spawn; it reports verbatim and waits. The output is visible as context to the next natural-language turn. A shell that can't start (no PTY) is non-fatal. |
| **Hardware-aware model recommendation** | `wd doctor` best-effort detects accelerator/host memory (NVIDIA VRAM via `nvidia-smi`, Apple unified memory via `sysctl`, else system RAM) and **recommends** a `local_llm_model` sized to fit. It only ever recommends — the operator sets the model; warden never silently swaps it. |
| **`wd backend suggest` — memory-ranked model picker** | A dedicated recommender that auto-detects **two** figures from the *same* memory pool: **total** memory (the bound) and **average free** memory (sampled a few times, smoothing spikes) — VRAM for an NVIDIA GPU, the unified pool on Apple Silicon, else Linux `MemAvailable`. It scores a curated, **tool-calling-forward** catalog (Qwen3, gpt-oss, Mistral Small, Qwen2.5) by **conductor suitability** — not raw size or coding skill, since the REPL routes tool calls and never writes code. Scores are calibrated against the [Berkeley Function-Calling Leaderboard](https://gorilla.cs.berkeley.edu/leaderboard.html) (BFCL v4), weighted toward the multi-turn subcategory the REPL's loop exercises — so a leaner agentic model outranks a bigger coding-tuned one. Each model is marked `fits now` / `free memory first` / `too large`. The ★ pick is the best-scoring model that runs **comfortably now** while leaving headroom for your real workload (Docker, DBs, IDE, Claude sessions, the daemon). `--samples`, `--total-gb`/`--free-gb` overrides, `--json`. |

---

## 18. Audit log (`warden inspect audit`)

An append-only trail of the daemon's meaningful actions — who did what, when, to
which object — for after-the-fact review. The daemon writes one JSON object per
line to `~/.warden/audit.jsonl` (`internal/audit`), with a stable schema so old
lines stay parseable as fields are added.

| Feature | Description |
|---|---|
| **Recorded actions** | The daemon logs `spawn`, `terminate`, `delete`, `approve`, and pipeline `pipeline_start` / `pipeline_cancel` at the point each succeeds. Each record carries `time` (when), `action` (what), `actor` (who — the request origin), `target` (the agent/pipeline acted on), and an action-specific `detail` map (name, repo, type, option, hard, …). |
| **Real actor behind a proxy** | By default `actor` is the immediate peer address — which is the proxy (`127.0.0.1`) behind a tunnel. Set `trusted_proxies` (IPs/CIDRs) and, when the peer is one of them, the actor is resolved from `X-Forwarded-For` (the real client). `X-Forwarded-For` is trusted **only** from a configured proxy, so a direct client can't forge it; the auth-failure throttle still keys on the spoof-resistant peer IP. |
| **Best-effort writes** | Recording is fire-and-best-effort: a write failure is logged and swallowed so it never blocks or fails the action being audited. Auditing is on whenever the daemon runs; a nil writer (tests) is a safe no-op. The file is created `0600` — owner-only — since it can name agents and prompts. |
| **`warden inspect audit`** | Reads and renders the trail (newest last). `--tail N` keeps the most recent N (default 50, `0` = all), `--action` filters by action, `--target` by substring of the agent/pipeline ID, `--since`/`--until` by window (`24h`, `7d`, `2w`) or date, and `--json` prints raw records. It reads the file directly (not via the daemon), so it works even while the daemon is down; malformed/partial lines are skipped. |

---

## 19. Export / import sessions

Serialize session **metadata** to JSON for backup, sharing, or migration between
machines, then read it back into another store. Worktrees, branches, and tmux
sessions are **not** serialized as files and are **not** recreated on import — an
imported record simply remembers where its (now absent) worktree used to live.

| Feature | Description |
|---|---|
| **`warden inspect export`** | Dumps active agent records as a versioned JSON envelope (`{version, exported_at, sessions}`) on stdout. `--all` folds in the archived (`closed/`) store too. Reuses the existing `/sessions` + `/history` reads — no new export endpoint. |
| **`warden inspect import`** | Reads an export envelope from stdin and inserts its records. **Idempotent by id**: a record whose id already exists is skipped, so re-importing the same dump is a no-op. `--merge` overwrites colliding records with the imported data instead; `--json` prints the per-id result. A new id whose name collides with a different record is imported with the alias dropped (reported under `renamed`). |
| **`POST /api/v1/import?merge=`** | Daemon endpoint (`internal/daemon/import_routes.go`); decodes the envelope and inserts each record keyed on id (`400` on a bad body, `422` on a store error). The `store.Export` / `store.ImportResult` envelope types live in `internal/store/portability.go`. |

---

## 20. Docker / container deployment

A multi-stage [`Dockerfile`](../Dockerfile) and [`docker-compose.yml`](../docker-compose.yml)
package the daemon for containerized remote access. The remote-access auth model
(non-loopback bind requires `WARDEN_TOKEN`) carries over unchanged.

| Feature | Description |
|---|---|
| **Multi-stage, lean image** | Stage 1 (`node:22-alpine`) builds the web dashboard; stage 2 (`golang:1.26-alpine`) produces a static `CGO_ENABLED=0` binary with the dashboard `go:embed`-ed in; stage 3 is an `alpine:3.20` runtime carrying only the binary plus `tmux` + `git` + `ca-certificates`. Runs as an unprivileged `warden` user. |
| **Persistent state volume** | `~/.warden` (the session store + config) is a named volume (`/home/warden/.warden`), so records survive container restarts. The import never touches disk worktrees, matching §19 semantics — imported records remember absent worktrees rather than recreating them. |
| **Remote-access defaults** | The entrypoint binds `0.0.0.0:8765`; compose maps the port and threads `WARDEN_TOKEN` from the host environment (required — the daemon refuses a non-loopback bind without it). Front the port with Tailscale / a Cloudflare Tunnel rather than exposing it directly. |
| **tmux/claude boundary** | The image ships `tmux` (hard runtime dependency — every agent runs inside a tmux session) and `git` (worktree-isolated agents). It deliberately omits the `claude` CLI to stay lean: the container hosts the daemon/API/dashboard out of the box; driving live agents additionally needs `claude` + credentials layered in. |

---

## 21. Local-LLM provider (`internal/llm`)

An opt-in, Ollama-compatible local model that handles warden's "fuzzy but cheap"
work without spending cloud-model tokens. Off by default (`local_llm`); every call has a
hard timeout and a deterministic fallback, so warden behaves exactly as before when
the model is off, unreachable, or wrong. The REPL (§17) is the one surface
that *requires* it; everything below degrades silently to prior behavior.

| Feature | Description |
|---|---|
| **Provider seam** | `internal/llm` exposes one-method `Completer` / `Chatter` interfaces over a small non-streaming Ollama client (`/api/generate`, `/api/chat`) with a hard timeout, response byte cap, and an error-so-the-caller-falls-back contract. The daemon builds the provider only when `local_llm.enabled` is on. |
| **Task classification** | `lifecycle.Classify` routes a prompt-spawned agent's type guess through the local model first, falling back to headless Claude, then `other`, on any error. |
| **Activity summaries** | The ≤8-word agent subject (`Summarize`) routes through the same seam, falling back to headless Claude on any error *or empty reply* (an empty summary carries no signal, so unlike `Classify` it is not trusted). |
| **Check-failure condensation** | An **oversized** check-failure log (output past the line cap) is condensed by the local model into its distinct failures; deterministic tail-truncation is the fallback. Within-cap failures skip the model entirely. |
| **Headless commit messages** | `wd commit` / MCP `commit` no longer require `-m`: a missing message is distilled by the local model from the staged diff (`git diff --cached`, capped to 16 KiB) into a Conventional-Commits subject, with a path-derived conventional-commit floor as the guaranteed fallback — a blank commit is impossible. |

Gated by `local_llm.enabled` (+ `local_llm.url` / `.model` / `.timeout`); the REPL's
tier knobs (`local_llm.tier` / `.escalate`) live in §17.

---

## 22. Lifecycle commands & boundary enforcement

warden moves deterministic responsibilities off Claude agents — git and checks —
onto first-class commands, and **enforces** the worktree boundary with PreToolUse
hooks: steer via the system prompt → deny-and-redirect the raw escapes. The hooks
are delivered through a per-agent `claude --settings` file; each **fails open** (a
hook error never blocks the agent) and is individually config-gated (default on).
Most of this needs no LLM.

| Feature | Description |
|---|---|
| **`wd commit` / `push` / `sync` (CLI + MCP)** | Git lifecycle on the agent's pinned worktree via the `lifecycle` runner, returning compact structs in place of git tool-spam. Rails: no commit/push on main/master, no dirty-tree sync, pre-commit-hook failure surfaced as a result; `sync` leaves conflicts in progress carrying only the conflicting files. Commit message is auto-filled when `-m` is omitted (§21). `push --force-with-lease` (`force: true` on the tool) is the only force path — never a bare `--force` — so a rebased branch never clobbers a teammate's push. |
| **`wd check [name]` (CLI + MCP)** | Runs the per-project `.warden/check.yml` command(s) and returns pass/fail with output for only the **failing** checks (tail-truncated, oversized logs condensed per §21). Per-entry `dir:` for monorepos; config is the single source of truth; no-config / unknown-name return friendly errors. The biggest raw-token win. |
| **`wd git review` (CLI-only, agent-native)** | Asks the agent's backend to review its OWN diff — the agent-native counterpart to `wd check` (which runs configured test/lint commands) and to a `pr-review` agent (which stands up a whole reviewer session). It execs the backend's native one-shot reviewer (Codex: `codex review`) in the agent's worktree and streams the findings. Defaults to the uncommitted working tree; `--base <branch>` reviews against a base, `--prompt` adds instructions, `--backend` picks the backend. Like `wd check` it is **local worktree exec, no daemon round-trip** — so it is **CLI-only by design** (no MCP twin). Backends without a native reviewer (e.g. Claude) are not offered the verb — it exits non-zero pointing at `wd check` / `pr-review`. Surfaced via the additive `agentbackend.Reviewer` interface (design step 6, #52). |
| **`wd git review --json` (CLI-only)** | The structured/machine-readable path: warden runs the backend's structured review (Codex: `codex exec review`), normalizes the backend's NATIVE review output into a **neutral findings shape** `{summary, verdict, findings[]}` (`agentbackend.ReviewFindings`; each finding `{file, line, severity, title, message}`), and prints that JSON to stdout (the backend's own progress goes to stderr). warden owns the neutral shape, not the schema the agent emits. Review quality rides the backend's configured model — a tiny local model may report none; the operator's real model is where it earns its keep. Surfaced via the additive `agentbackend.StructuredReviewer` companion. |
| **`wd project memory` (CLI-only) + launch-time projection** | warden owns one committed, backend-neutral **project memory** — `.warden/memory.md` (beside `.warden/check.yml`), keyed implicitly by the repo root, auto-created on first use, holding durable cross-agent facts ("where X lives", "run Y via `wd check`", project invariants). `wd project memory` shows/edits it (`--raw`, `--path`, `--edit`; CLI-local, no daemon round-trip, like `wd check`/`wd git review`). At **every spawn** warden **projects** the budgeted, navigational render into the agent's system prompt through the SAME seam the collab/pipeline/git hints ride — **Claude** via `--append-system-prompt` (file-backed through the hints dir, so it never bloats the launch line), **codex/cursor/opencode/antigravity** via their `AGENTS.md` warden block, **crush** via `CRUSH.md`, **goose** via `.goosehints`; **aider** degrade-skips (neither seam). 7/8 backends project with zero new adapter code. Config-gated by `memory.inject` (default on); off — or an empty/absent file — makes the Claude launch **byte-identical** to no injection. warden READS but never rewrites your CLAUDE.md/AGENTS.md/CONVENTIONS.md. **Auto-curation (#53 PR-2, `memory.curate`, default OFF):** on the existing completion-digest hook, a debounced extraction pass proposes **`unverified`, timestamped, provenance-tagged** entries into `.warden/memory.md` — writing to the **working tree only, never committing or pushing**, so the committed diff is the human review gate. Proposals promote to `trusted` only on corroboration (a second agent, or human approval); a contradicting fact **supersedes** an older one (struck with a tombstone); un-recorroborated entries **age out** past a TTL; a fact whose named path vanished is **flagged stale** by a deterministic check. The pass prefers the `$0` local model, degrading to `claude -p` only where configured, and never sits on a paid critical path. **Local grounding (#53 PR-3, `memory.ground`, default ON):** in `wd backend repl` you can ask a project question ("where does the spawn gate live?", "how do I run the tests?") and warden answers it **locally** from `.warden/memory.md` — the `/memory` (`/mem`/`/ask`) command and the model-callable `project_memory` tool, both read-only and served on the **local model only**, so it *removes* a cloud round-trip rather than adding tokens. It reads memory the same read-only way projection does (never auto-creating it), cites each entry's **trust** (`unverified`/`trusted`/`human`) and **provenance** so a stale hint reads as a hint, and — with no local model configured — degrades to returning the matching entries verbatim (`$0`), never escalating to a paid model. This is #53 PR-1 (projection) + PR-2 (gated curation) + PR-3 (local grounding): it taxes the cross-agent rediscovery cost once and amortizes it across the fleet, any backend, without ever letting one agent's wrong belief silently poison the rest. |
| **`wd backend model` (CLI-only, agent-native)** | Surfaces a backend's **live runtime model menu** (vs warden's hard-coded `opus`/`sonnet`/`haiku`/`fable` alias table). Backends whose model set is a runtime, multi-vendor menu implement it — **Antigravity** (`agy models`, listing Gemini/Claude/GPT-OSS variants) and **Cursor** (`cursor-agent --list-models`) — via the optional `agentbackend.ModelLister` interface; the ids print one per line (or `--json` for an array) and feed `--model` verbatim. Listing is a metadata read (the backend's own list subcommand, not a generation request), so it spends no hosted-tier quota. `--backend` picks the backend. **Local worktree exec, no daemon round-trip ⇒ CLI-only by design.** Backends with a static model set (Claude) have no live menu and are not offered the verb — it exits non-zero pointing at `--model` with a known id. |
| **Default-isolated write agents** | Every write-type agent (`code`/`docs`/`website`/`debug-ci`/`tests`) gets its own worktree unless `--in-repo`; `pr-review` is exempt (see §2). This is what makes the isolation guard meaningful and fixes parallel-agent collisions. |
| **Isolation guard** (`rails.isolation_guard`) | A PreToolUse hook denies an isolated agent's Edit/Write that escapes its worktree into the shared repo (`warden check boundary` → `POST /api/v1/hooks/guard`). |
| **Git-guard** (`rails.git_redirect`) | A PreToolUse Bash hook quote-aware argv-parses each command and deny-redirects raw `git commit`/`push`/`pull`/`rebase` to the warden tools (reads stay allowed), the deny message naming the exact replacement. Static verdict, no daemon round-trip. |
| **Check-guard** (`rails.check_redirect`) | A PreToolUse Bash hook deny-redirects a raw test/lint/build command the project's `.warden/check.yml` registers to `wd check`, matching on leading-token prefix (broad runs redirect; focused `-run` runs pass through). No-config repos redirect nothing. |
| **Prompt steer** (`rails.git_conventions`) | A Layer-1 system-prompt hint steering agents toward `wd commit`/`push`/`sync` (and `wd check`) over raw git/test Bash — the gentle first layer before the deny hooks. |

---

## 23. Snapshots / checkpoints (`wd workspace snapshot`)

Checkpoint an agent at a known-good point — its **worktree state** *and* its
**session transcript** — and roll back to it later. Config-gated by `snapshots`
(default on); the daemon owns the snapshot store — metadata in an embedded ScrivaDB
under `<data_dir>/snapshots-db/`, transcripts as flat blobs under
`<data_dir>/snapshots/` (see §33 for the storage format and upgrade migration).

- **`wd workspace snapshot create [name] [-m msg]`** (CLI + MCP `snapshot_create`) — captures
  the worktree **non-destructively** via `git stash create` (it builds a commit
  object recording the working tree *without* touching it — no stash entry pushed,
  no index change), recording the HEAD/branch/dirty-file list plus the tmux pane
  scrollback as the transcript. `[name]` defaults to the current agent
  (`WARDEN_SESSION_ID`); the daemon pins capture to that agent's own worktree.
- **`wd workspace snapshot list [name] [--all]`** (CLI + MCP `snapshot_list`) — snapshots
  for an agent (or every session), newest first.
- **`wd workspace snapshot restore <id> [--force]`** (CLI + MCP `snapshot_restore`) —
  re-applies the snapshot's stash onto its recorded worktree. **Rails:** refuses a
  dirty tree unless `--force`, and never restores onto `main`/`master` (same guards
  as `wd sync`/`remove-worktree`). **Reversible-safe** — stash *apply* neither
  resets HEAD nor drops the snapshot, so the snapshot stays usable; a partial apply
  leaves conflicting paths in the tree for resolution (the `wd sync` handoff). The
  saved transcript path is surfaced for the operator regardless.

Built as a self-contained `internal/snapshot` package (pure helpers + a runner
over the shared `lifecycle.Runner` command seam), wired through the daemon →
client → CLI/MCP like the other lifecycle verbs.

## 24. First-run tutorial (`wd tutorial`)

A friendly, idempotent walkthrough of warden's core loop for new operators —
**spawn → watch → talk → tear down** — plus pointers to the cockpit TUI and the
web GUI. It changes nothing: each step shows the exact command to try when you're
ready (`wd start`, `wd ls`/`wd status`, `wd agent attach`/`wd send`, `wd agent done`, `wd tui`).

- **`wd tutorial`** — prints the walkthrough, then writes a `tutorial-complete`
  marker in `<data_dir>` so the first-run hint stops appearing.
- **`wd tutorial --skip`** — writes the marker *without* running the steps
  (silences the hint immediately).
- **`wd tutorial --reset`** — deletes the marker so the tour (and the hint) run
  fresh. (`--reset` and `--skip` are mutually exclusive.)

**First-run hint.** On a normal invocation, if the marker is absent, warden prints
a single non-blocking line to **stderr** nudging you toward `wd tutorial`. It is
shown **only** when stdout is an interactive **TTY** *and* the marker is missing
*and* the `tutorial` config setting is on (default on) — and never for piped /
non-TTY output or the machine/full-screen surfaces (daemon, MCP, hooks,
completion, the cockpit root, `wd tui`, the tutorial itself). The walkthrough is
never auto-run and never blocks; automation and the daemon/MCP paths are
untouched. Disable the hint entirely with `tutorial: false` in the config.

Implemented as a thin CLI verb (`internal/cli/tutorial.go`) over pure,
unit-tested helpers — marker read/write/reset, the step list, and the
suppression predicate — with no daemon change.

---

## 25. AI-powered insights (`wd usage insights`)

Mine warden's **own history** — completed and active agent sessions plus the
resource metrics it already records — into actionable suggestions. Like the
REPL and digest, it is a **deterministic statistics core that needs no
LLM**, with an **optional local-LLM narration layer** that degrades gracefully to
the deterministic text whenever the model is off, unreachable, errors, or returns
an empty reply. Config-gated by `insights` (default on).

- **`wd usage insights`** (CLI + MCP `insights`) — aggregates history into a report:
  - **session duration by type** — count, median / p90 / max per agent type, with
    individual runs flagged as **outliers** when they exceed 2× the type's median.
  - **parallelization opportunities** — pairs of **finished, same-repo** sessions
    whose run windows did **not** overlap and whose edited file sets are
    **disjoint**, i.e. they could have run concurrently; each carries the wall-clock
    time the shorter run could have saved.
  - **frequently co-edited files** — file pairs touched together across multiple
    sessions, a hint for module coupling.
  - **error rate by type** — errored/orphaned over total per type.
  - **busiest hours (UTC)** — when agent activity clusters.
  - **live agent anomalies** — surfaced straight from the metrics summarizer.
- **Flags** mirror the history/digest neighbors: `--since <24h|7d|2w|date>` to bound
  the window, `--limit` to cap archived sessions mined, `--session <id|name>` to
  scope the parallelization suggestions to one session, and `--json` for the raw
  structured report.

Built as a self-contained, fully unit-tested `internal/insights` package — pure
aggregation (`Analyze`), the parallelization suggester (`SuggestParallelization`),
and the narrator (`Narrate` over the `llm.Completer` seam, a nil completer meaning
deterministic-only) — behind a shared `client.Insights` aggregator that both the CLI
and MCP call. Duration / error-rate / busy-period analysis covers **all** sessions;
the file-set-dependent co-edit and parallelization analysis is strongest on active or
digestible sessions, since archived file sets are reconstructed best-effort from
digests. See
`docs/superpowers/specs/2026-06-25-warden-ai-powered-insights-design.md`.

## 26. Plugin system (`wd project plugin`)

Extend warden with **custom agent task types** and **lifecycle hooks** without
forking — a thin, default-off, fail-open extension seam. A plugin is an **external
executable** registered in config and invoked over a documented, versioned
**JSON-over-stdio protocol** (request on stdin, response on stdout, hard timeout),
deliberately mirroring warden's existing PreToolUse guard hooks rather than the
fragile Go `plugin` package or a heavy WASM runtime. Config-gated by `plugins`
(**default off**, since plugins run external code).

- **Lifecycle hooks** — a plugin subscribes to events (`pre-spawn`, `post-spawn`,
  `pre-commit`, `post-commit`, `pre-check`, `post-check`, plus a reserved
  `pre-teardown`); warden invokes it at the spawn/commit/check points with the
  agent's session metadata + event payload. Hooks are **advisory and fail-open**:
  a missing, slow, non-zero-exit, or malformed plugin is logged and skipped, and a
  failing plugin never aborts the others — it can never block, error, or crash an
  agent. A `pre-` hook cannot veto the action (observers, not gates).
- **Custom task types** — a plugin declares new `--type` names, each with its own
  worktree isolation policy. These slot into warden's closed `store.Type` enum via
  a function-var seam so validation/worktree logic recognizes them **without
  changing any built-in type's behavior**; names that collide with a built-in or
  another plugin are rejected at config load.
- **`wd project plugin list`** — show registered plugins, their executable paths, declared
  custom task types (with isolation policy), and subscribed hook events; it also
  surfaces any config errors the daemon would reject.

Configure with the `plugins.enabled` gate + a `plugins.registry` list (name, path, events,
task_types) in `~/.warden/config.yaml`. Self-contained, fully unit-tested
`internal/plugin` package (`protocol` / `registry` / `dispatcher`). A worked
example lives under `examples/plugins/` (a post-commit notifier). See
`docs/superpowers/specs/2026-06-25-warden-plugin-system-design.md`.

---

## 27. API docs / OpenAPI (`/api/docs`)

A machine-readable **OpenAPI 3.x** description of the daemon's REST API, plus an
interactive **Swagger UI**, so remote/API consumers have a real reference instead
of reading `internal/daemon`. Config-gated by `api_docs` (default on).

| Feature | Description |
|---|---|
| **`GET /api/docs`** | Interactive Swagger UI page. Served from a **pinned, vendored** copy of `swagger-ui-dist@5.17.14` embedded in the binary — **no runtime CDN**, so it works offline and inside the container image. |
| **`GET /api/docs/openapi.yaml`** | The raw OpenAPI document (`application/yaml`). |
| **Spec-first: server generated from the spec** | `openapi.yaml` is the single source of truth; the daemon's typed ("strict") chi server is generated from it with `oapi-codegen` (`internal/daemon/oapi`, via `go generate`). The `*Server` implements the generated `StrictServerInterface`, and ~18 response schemas alias the real Go types (`store.Session`, `lifecycle.*Result`, `snapshot.Snapshot`, `pipeline.Pipeline`, …) via `x-go-type`, so the wire format is byte-identical with zero adapters. |
| **Drift guard (compiler + codegen)** | Every `operationId` becomes an interface method the daemon must implement — an undocumented or mismatched endpoint is a **build failure**, not a stale doc. A CI guard (`make generate-check`) also fails if the committed generated code drifts from the spec. `TestSpecMatchesRoutes` remains as a route-presence smoke test for the hand-registered public/streaming routes outside codegen. |
| **Public surface** | Like `/healthz` and the static SPA shell, the docs are unauthenticated — the spec holds no secrets — while still documenting the `bearerAuth` scheme that gates every data/action route. |

Self-contained `internal/daemon/apidocs` package (`//go:embed` of the spec +
Swagger UI assets), reusing the daemon's existing embed+handler pattern. See
`docs/superpowers/specs/2026-06-25-warden-openapi-api-docs-design.md`.

---

## 28. Native scheduler (`wd schedule`)

Recurring (`--cron`) and single-shot (`--at`) triggers that the daemon fires on
its own timer — no external crontab needed. Each schedule fires **either** one
agent spawn **or** a pipeline, through the same internal seams the `/spawn` and
pipeline routes use (never by shelling out to the CLI). **Opt-in:** gated by
`scheduler_enabled` (default **off**) — the routes return 403 and the reconcile
loop is a no-op until you enable it, because schedules only fire while the daemon
is running. This reverses the original "use OS cron" decision
(`docs/superpowers/specs/2026-06-10-warden-scheduled-pipelines-decision.md`),
keeping the concern as the default-off gate.

| Feature | Description |
|---|---|
| **`wd schedule create <name> --cron "0 9 * * *" --type pr-review --repo <p> --prompt "…"`** | Recurring agent spawn. `--cron` is a 5-field spec (`robfig/cron/v3`, `@daily` etc. supported). |
| **`wd schedule create <name> --at 2026-06-27T09:00 --prompt "…"`** | Single-shot agent spawn — fires once at/after the time, then goes inactive. `--at` is RFC3339 or `2006-01-02T15:04` (local time). |
| **`wd schedule create <name> --cron "…" --pipeline <spec.yaml>`** | Fire a pipeline instead of an agent. Each fire creates a fresh pipeline whose name is timestamp-suffixed, so recurring runs never collide. |
| **`wd schedule list`** | All schedules with kind (cron/at), mode (agent/pipeline), spec, enabled state, next run, and last error. |
| **`wd schedule show <id>`** | One schedule, including the durable `last_run_session_id` + `last_run_status` of its most recent fire. |
| **`wd schedule enable <id>` / `disable <id>`** | Toggle a schedule without deleting it. Disable clears `next_run`; enable re-arms it (cron → next occurrence, at → its time). Idempotent; the record and last-run history are preserved. |
| **`wd schedule delete <id>`** | Remove a schedule (id == its name). |
| **Scheduled-run session linkage** | Every fired run carries a `schedule_id` (+ `schedule_name`) back-reference on its session — set on agent-mode fires and inherited by a scheduled pipeline's job sessions — surfaced in `GET /sessions`, `GET /sessions/{id}`, and the SSE stream. Lets a client separate scheduled runs from ad-hoc agents and drill into a live run's terminal by filtering one field. Advertised by the **`scheduled-agents`** capability (`GET /api/v1/capabilities`). |
| **No backfill** | On daemon startup each schedule's next-run is recomputed from the wall clock: a cron schedule resumes at its next *future* occurrence (a run missed while the daemon was down is **not** replayed), while a past-due single-shot fires once. |
| **Fail-soft loop** | A fire error is recorded in the schedule's `last_error` and logged; it never crashes the once-a-minute reconcile loop or stops other schedules firing. An agent-name collision fails just that fire (honest over silently renaming). |
| **Full MCP + audit** | `list_schedules` / `get_schedule` / `create_schedule` / `enable_schedule` / `disable_schedule` / `delete_schedule` (MCP) mirror the CLI; create/delete/enable/disable are written to the audit log (`schedule_create` / `schedule_delete` / `schedule_enable` / `schedule_disable`). |

Persisted by an **embedded ScrivaDB** (`github.com/srjn45/scriva`, opened with
`SyncModeNone`) rather than one flat JSON file: schedules live in a `schedules`
collection rooted at `~/.warden/schedules-db/`, each record keyed by schedule id,
so a write appends one record instead of rewriting the whole-store map (the same
pattern used for sessions/`ctxstore`/`mailbox`). On the **first daemon launch
after upgrading**, warden performs a **one-time import**: if a legacy
`~/.warden/schedules.json` exists it is decoded and bulk-loaded into the new
collection, then a `.schedules-filedb-imported` sentinel is written **last** (so
a crash mid-import loses nothing — the derived `schedules-db/` is wiped and
rebuilt from the intact legacy JSON on the next open). The legacy
`schedules.json` is left in place as a read-only backup, never deleted. This is
an **internal storage swap only**: the store's public API is unchanged, so the
scheduler routes and reconcile loop behave identically. Pure next-fire logic
lives in `internal/schedule` (table-tested); the daemon's reconcile loop is
`internal/daemon/scheduler.go`.

---

## 29. Token-savings ledger (`wd usage savings`)

A real, **append-only ledger** of the tokens warden's lifecycle features have kept
out of agents' context windows — a measured proof point, not an estimate. Each time
a feature avoids dumping output into the transcript, the saving is recorded; `wd
usage savings` reads it back. Config-gated by `savings` (default on); the daemon owns the
store under `<data_dir>/savings/` and serves it at `GET /api/v1/savings` (403 when off).
Also reachable under the cost umbrella as `wd usage savings` (see §30).

The report keeps two axes honest and **never blends them into one percentage**:

- **Context axis** — how much leaner agent context stayed (raw output that *would
  have* entered Claude vs. what actually did), reported as a reduction % and dollars.
- **Offload axis** — Claude work moved off entirely onto the local LLM
  (classify/summarize), reported as dollars; it keeps nothing in-context, so it is
  never folded into the context %.

| Feature | Description |
|---|---|
| **`wd usage savings`** (CLI + MCP `savings`) | Per-feature table sorted biggest-win-first: saved tokens, raw tokens, and event count, under the two headline axes. An empty ledger reads as an explicit "nothing recorded yet". |
| **What records a saving** | `wd check` (raw build/test output kept out of the transcript), `wd commit`/`push`/`sync` (git plumbing output), auto-/`/compact` context reclaim, and local-LLM offload (classify/summarize calls that never hit Claude). |
| **`--benchmark`** | The headline A/B proof: *without warden* (raw tokens that would have entered the model) vs. *with warden* (what actually did), the reduction %, the leaner factor, dollars saved, a per-day **saved-tokens sparkline**, and — when transcript spend was observed — the cut as a share of real measured model spend. Built to screenshot. |
| **`--since <window\|date>`** | Scope to a window (`24h`, `7d`, `2w`) or a date. |
| **`--json`** | Structured summary for tooling. |
| **`--audit`** | Print a few retained **raw-vs-kept provenance samples** so a skeptic can eyeball the actual bytes behind the counts. Requires `savings_samples` (off by default — samples retain substrings of real build/test/git output, which may be sensitive). |
| **`--calibrate`** | Measure this workload's true **bytes-per-token** ratio against Claude's `count_tokens` endpoint (needs `ANTHROPIC_API_KEY` + retained samples) and persist it, so figures stop relying on the generic 4-bytes/token heuristic. **Forward-only:** it prices events recorded after calibration; earlier rows keep their heuristic counts. `--calibrate-max` caps the paid calls. |

Every figure states its **basis** — `CALIBRATED` (workload-measured) or `HEURISTIC`
(4 bytes/token) — so the claim is never ambiguous, and dollars are priced at the
Opus input/output rates. Self-contained, fully unit-tested `internal/savings`
package (`store` / `savings` / `calibrate`); the CLI rendering lives in
`internal/cli/savings.go`.

> **Dollar figures are estimates** based on published list prices (as of 2026-06);
> they exclude prompt-cache tokens and any volume/batch/enterprise discounts, so
> they may differ from your actual bill. Token counts are exact (read from the
> transcript).

## 30. Cost governance (`wd usage spend` + budget gate)

The cost counterpart to the savings ledger: where `wd usage savings` reports what warden
kept **out** of context, `wd usage spend` reports what agents **actually billed** the model provider.
The daemon already reads each agent's REAL input/output tokens from its transcript
(`internal/spend`); cost governance prices that per model into dollars and gates on
it (dollar pricing currently covers the Claude backend; bring-your-own-model backends report tokens only). Config-gated by `savings` (the same switch — spend is the cost half of the same
measured data); the daemon serves it at `GET /api/v1/spend` (403 when off).

| Feature | Description |
|---|---|
| **`wd usage spend`** (CLI + MCP `spend`) | The measured spend priced per model and rolled up three ways — **per agent**, **per repo**, **per day** — with a headline `total / today / this week`. `--by agent\|repo\|day` shows one rollup; `--json` for tooling. An empty meter reads as "nothing measured yet". |
| **Per-model pricing** | A small `$/Mtok` table (`internal/spend/pricing.go`): Opus `$5/$25`, Sonnet `$3/$15`, Haiku `$0.8/$4` (in/out); an unrecognized model is priced at the Opus tier so spend is never silently under-counted. Opus rates are kept in sync with the savings ledger. |
| **Budget gate** | A **soft** spawn gate, sibling to the memory-pressure gate: when today's or the trailing-week's measured spend reaches a configured cap, a non-forced spawn returns `428` with a confirmation payload; re-run with `--force` to proceed. Off by default. Tunable via `tokens.budget_gate` / `tokens.budget_daily_usd` / `tokens.budget_weekly_usd` (a `0` cap disables that axis). |
| **`$` in `wd ls`** | A **COST** column shows each agent's measured spend beside its context fill (best-effort — `—` when the feature is off). Also surfaced in `wd inspect search` / `wd inspect history`. |
| **Web Metrics tab** | A **Cost per agent** card: the `total / today / this week` headline plus a live per-agent cost bar chart (sorted by `$`, top-N costliest with the rest folded into an `others` row) beside the RSS/CPU charts. |

Self-contained, fully unit-tested `internal/spend` package (`store` / `pricing` /
`report` / `budget`); the CLI rendering lives in `internal/cli/spend.go`.

> **Dollar figures are estimates** based on published list prices (as of 2026-06);
> they exclude prompt-cache tokens and any volume/batch/enterprise discounts, so
> they may differ from your actual bill. Token counts are exact (read directly from
> the transcript).

**Cost umbrella (`wd cost`).** A single discoverable entry point over both financial
views, mirroring the `library` umbrella over reusable launch config. `wd usage spend`
and `wd usage savings` are the very same commands as the top-level `wd usage spend` and `wd
usage savings` (every flag — `--by`, `--json`, `--benchmark`, `--since`, `--audit`,
`--calibrate` — wired straight through); `wd cost` with no subcommand prints a
combined at-a-glance summary with a **SPEND** section and a **SAVINGS** section,
reusing both render helpers verbatim (`internal/cli/cost.go`) so the figures never
disagree with the standalone commands. Purely additive — no new storage, pricing, or
MCP tool: both axes are already exposed over MCP (`spend`, `savings`), and the
top-level `wd usage spend` / `wd usage savings` remain as aliases. Resource footprint
(memory/CPU/pressure) is a **different axis** and stays under `wd inspect resources` (§ Live
resource metrics), deliberately not folded into `wd cost`.

---

## 31. Session storage & upgrade migration

Sessions are persisted by an **embedded ScrivaDB** (`github.com/srjn45/scriva`,
opened with `SyncModeNone`) rather than one JSON file per session. The store is
rooted at `~/.warden/sessions-db/` and holds two collections, each record keyed
by session id:

- **`active/`** — live sessions. `Get` and every mutator target this collection,
  so "active-only" semantics stay structural (an archived session is invisible to
  `Get` and immutable to mutators).
- **`closed/`** — archived sessions. `warden archive` writes the closed copy
  first, then deletes the active record — so a crash between the two leaves the
  session recoverable in `active`, never lost.

A mutation now **appends one record** instead of rewriting a whole per-session
JSON file (the write-amplification the old file store carried, the same pattern
already retired for `ctxstore`/`mailbox`). This is an **internal storage swap
only**: the `store.Store` interface is byte-for-byte unchanged, so the daemon
REST API, the CLI/MCP clients, and `warden inspect export`/`import` behave identically —
there is still no database server to run.

### Upgrading from a JSON-file store

On the **first daemon launch after upgrading**, warden performs a **one-time
import**:

1. It decodes every legacy `~/.warden/sessions/*.json` and `~/.warden/closed/*.json`
   individually into the new `active`/`closed` collections (a corrupt or
   unsafe-id file is skipped with a warning, never blocking the upgrade).
2. It folds in the old provenance backfill (the former `.provenance-migrated`
   pass), so no separate migration step is needed.
3. It writes a `~/.warden/.sessions-filedb-imported` sentinel **last**, only
   after both collections load successfully.

The legacy `sessions/` and `closed/` directories are **read-only from this point
and kept as a cold backup** — this release never deletes them (a later release
will, behind its own guard). The import is **atomic at the directory level**: if
it fails partway (e.g. disk full), the sentinel is not written, so the next boot
wipes the half-built `sessions-db/` and re-imports from the intact legacy JSON —
no data is lost.

> **Downgrade caveat (release-note item).** Because the legacy JSON is left
> intact, downgrading to a pre-migration binary still reads your **pre-upgrade**
> history — nothing from before the upgrade is ever lost. But sessions **created
> or mutated after** the upgrade live only in `sessions-db/`, and a downgrade
> cannot see them (post-upgrade writes are ScrivaDB-only). This is inherent to any
> forward migration.

Design detail lives in
[`docs/specs/2026-07-03-sessions-filedb-migration.md`](specs/2026-07-03-sessions-filedb-migration.md).

## 32. Pipeline storage & upgrade migration

Pipelines are persisted by the same **embedded ScrivaDB**
(`github.com/srjn45/scriva`, opened with `SyncModeNone`) rather than one JSON
file per pipeline. The store lives in a `~/.warden/pipelines-db/` directory
holding a single `pipelines` collection, each record keyed by its pipeline id, so
a `create`/`edit-job`/`emit`/`retry` **appends one record** instead of rewriting
a whole `<id>.json` file. As with sessions this is an **internal storage swap
only** — `pipeline.Store`'s public API (`Create`/`Get`/`List`/`Update`/`Delete`)
is unchanged, so the executor, the daemon pipeline routes, and the CLI/MCP/TUI
surfaces behave identically.

On the **first daemon launch after upgrading**, warden performs a **one-time
import**: it decodes every legacy `~/.warden/pipelines/*.json` into the new
collection (a corrupt or unsafe-id file is skipped with a warning, never blocking
the upgrade), then writes a `~/.warden/.pipelines-filedb-imported` sentinel
**last**. The legacy `pipelines/` directory is left **read-only as a cold
backup** — this release never deletes it. The import is **atomic**: if it fails
partway, the sentinel is not written, so the next boot wipes the half-built
`pipelines-db/` and re-imports from the intact legacy JSON — no data is lost.

## 33. Snapshot storage & upgrade migration

Snapshot **metadata** (`wd workspace snapshot`) is persisted by the same **embedded ScrivaDB**
(`github.com/srjn45/scriva`, opened with `SyncModeNone`) as sessions, in a
`snapshots` collection rooted at a sibling `~/.warden/snapshots-db/` directory,
each record keyed by snapshot id. A capture **appends one record** instead of
writing a whole per-snapshot JSON file, mirroring the sessions/`ctxstore`/`mailbox`
swap. This is an internal storage change only: the store's `Put`/`Get`/`List` API
is unchanged, so the snapshot REST routes and CLI behave identically.

The captured **transcript stays a flat file** at `~/.warden/snapshots/<id>.transcript`
(unchanged path) — deliberately kept *out* of the ScrivaDB record so a multi-megabyte
scrollback never bloats the metadata store. This is a design choice, not a
regression: the record only carries a `transcript_path` pointer to that blob.

On the first daemon launch after upgrading, warden runs a **one-time import**: it
decodes every legacy `~/.warden/snapshots/<id>.json` into the collection (a corrupt
or unsafe-id file is skipped with a warning, never blocking the upgrade), then
writes a `~/.warden/.snapshots-filedb-imported` sentinel **last**. The legacy
`<id>.json` files (and every `.transcript` blob) are **left in place as a
read-only backup** — this release never deletes them. The import is atomic at the
directory level: if it fails partway the sentinel is not written, so the next boot
wipes the half-built `snapshots-db/` and re-imports from the intact legacy JSON.

---

## 34. Autopilot (autonomous agent runs)

> ⚠️ **Unattended operation is inherently risky.** Use `warden autopilot disable`
> (the kill switch) any time you need to stop. Workers always land into their
> run's integration branch (default `autopilot/<plan-name>`), never directly into
> `main`. See the [Autopilot guide](https://srjn45.github.io/warden/guides/autopilot/).

Autopilot is warden's **goal-directed, long-running autonomous mode**. You
describe a goal in a plan file, enable autopilot, and warden runs it — spawning a
**manager** agent that breaks the goal into tasks, delegates each task to a
**worker** agent in an isolated worktree, gates the worker's PR through CI, and
lands it into an integration branch. The manager heals itself when stuck (guardian
loop) and escalates to progressively cheaper backends when rate-limited (cost-tier
ladder).

### 34.1 Plan file

Authored by the operator as a named file under `plans/` (for example,
`plans/release.yaml`). `warden autopilot init --name release` scaffolds and
registers it; `warden autopilot register <file>` registers an existing plan. Contains a
`goal`, optional `constraints` (injected into every manager and worker spawn), and
an optional coarse `tasks` list. The manager decomposes the goal into tasks if the
list is empty. The file is owner-editable mid-flight; the manager re-reads it on
each planning cycle.

### 34.2 Agent topology — manager, worker, resolver

Every autopilot agent is tagged `autopilot` + `run:<run_id>`. The daemon applies
the tags mechanically: agents (and pipelines) created by an autopilot-owned
caller inherit its ownership tags from the request's agent identity, so the
roster never depends on the manager's prompt remembering them. A run is a small
fleet with separated jobs:

- **Manager** (role `autopilot`) — a single long-lived headless agent the daemon
  Controller spawns on the cheapest available backend. It orchestrates the run via
  warden's own MCP tools (`spawn_agent`, `land`, `ctx_*`, etc.), reads the ledger
  to know what's landed, and restarts cleanly after context rotation or a guardian
  heal. Multiple named runs may be active in one repository. *(Historically "the brain"; the
  ledger key `autopilot.brain` keeps that name for back-compat.)*
- **Worker** (role `worker`) — spawned one per task by default; owns the task
  end-to-end (implement → self-review → open a PR on the integration branch →
  drive CI green → merge) and reports status back to the manager. A large task may
  instead be delegated to a pipeline of `implementer`/`reviewer`/`auto-merger`
  agents.
- **Resolver** (role `brain`) — an on-demand, short-lived agent the manager spawns
  to unblock a stuck worker or make an ad-hoc design/architecture decision without
  human interaction, then report the call back.

#### Plan-scoped hierarchy and slot ids

Each registered plan is a stable tree root (display name; `run_id` stays an
internal hash). Fixed **slot ids** survive manager rotation and daemon restarts —
the same pattern as pipeline job ids (`<pipelineID>-<jobID>`):

```text
<plan-name>                  # display root
├── <scope>-autopilot        # manager slot (role autopilot)
├── <scope>-guardian         # guardian slot (daemon inspectability session)
├── plan                     # checklist from plan YAML + ledger
└── workers                  # grouped by ledger state
    └── <task-id>            # worker sessions / manager pipelines
```

`<scope>` is derived from the plan name (sanitized; disambiguated with a
`_<run_id>` suffix when two runs would collide). Plan names must not end with
`-autopilot` or `-guardian` (reserved slot suffixes).

Session records carry explicit back-ref fields — `autopilot_run_id`,
`autopilot_slot` (`autopilot` | `guardian` | `worker`), and `autopilot_task_id`
(workers only) — as the display/reconciliation contract. The `run:<run_id>` tags
remain the authorization channel. Autopilot workers are **not** parented to the
manager via `parent_id`; ownership is tag- and back-ref-based (like pipelines).

TUI, web, REST, and MCP render the same hierarchy. `warden autopilot status`
includes `integration_branch`, `manager_slot_id`, and `slot_scope` per run.

### 34.3 Guardian & overwatch

Two daemon-internal supervisors run on the guardian's ticker while a run is active.

The **guardian** is a heal loop that keeps the manager alive. When the manager's
heartbeat goes stale (wedged with pending work), the guardian fires:

| Stage | What the guardian does |
|---|---|
| 1 — nudge | Send a steering message to the manager |
| 2 — restart | **Hot-swap** the manager in-place on the same backend (stable `<scope>-autopilot` slot id; context handoff via `.warden/handoff-<slot-id>.md`) |
| 3 — rotate | **Hot-swap** onto the next backend down the cost tier (slot id unchanged) |
| 4 — backoff | All backends exhausted or rate-limited: wait (capped-exponential backoff), notify, then retry from stage 1 |

Guardian rotation calls `Lifecycle.HotSwap` directly — it does **not** go through
the backend quota-recovery coordinator. Cold start (missing slot) spawns with
`Ticket = <scope>-autopilot` and adopts a live session on `ErrExists`. There is
no terminal failure state — the guardian always retries. Backoff state
(`backoff`, `tier`, `last_heartbeat`, `context_level`) is visible in
`warden autopilot status`. A planned rotation (context critical or cadence
interval reached) uses the same hot-swap seam — the manager slot id does not
change and workers need no `parent_id` repoint.

The **overwatch** is a backstop (not an agent) that keeps the manager *tending its
workers*. It derives the run's worker roster from the `run:<run_id>` tag and, only
while the manager itself is idle, nudges it to attend workers that fall idle or
wait on input — event-driven (debounced ~5m) or a periodic ~1h heartbeat. Its
cadences are generous, fixed constants (frictionless-safeguards philosophy — a
backstop, not a pacer). It only ever messages the manager; it never touches a
worker itself.

### 34.4 Cost-tier backend selection

The manager (and guardian on rotate) selects backends from cheapest to most
expensive: free tier (`antigravity`), then subscription backends (`claude`,
`codex`), then gated pay-per-use backends (explicit opt-in required).

**Sourced from the backend registry (§35).** The cost-tier ladder and the
paid-autopilot gate are now derived from the registry store — a backend's tier is
whatever you set with `warden backend tier <id> <tier>` (or the web/TUI), and the
gate is the store's `allow_paid_autopilot` setting. The deprecated
`autopilot.brain.backends.{free,subscription,pay_per_use}` ladder and
`autopilot.brain.allow_pay_per_use` config keys are **imported once** into the
store on the first boot after upgrade (a one-time, sentinel-guarded migration) and
then **ignored** — later edits live in the registry, and the daemon logs a
deprecation warning, then removes the imported keys when compatibility permits. Only **installed, enabled,
non-`local`** backends are eligible for the ladder.

### 34.5 Ownership guard

Autopilot-owned agents (tagged `run:<run_id>`) cannot be destructively modified by
non-owning agents. Operations such as `terminate`, `remove-worktree`, or
`hard-delete` from a different context return `403 not_owned`. The human operator
can always act on autopilot agents directly.

### 34.6 Approval routing

While autopilot is active, approval prompts from worker agents are routed to the
manager's mailbox. The manager answers routine tool-permission prompts using its
auto-approve policy, keeping workers unblocked without stalling on human input.
The operator's approval queue is not affected.

Legacy `autopilot.plans[]` entries remain readable during the compatibility
window. At daemon boot they are copied without overwrite into the repository's
`plans/` directory and durably registered; root `autopilot.plan.yaml` becomes
`plans/default.yaml`. Sources are retained as rollback backups, migration is
idempotent, and an actionable warning tells the operator when the config entry
can be removed.

### 34.7 Land (idempotent merge)

`warden autopilot land <agent-or-branch>` (CLI) / `land` (MCP) merges one autopilot worker
branch into the integration branch. The operation is idempotent (re-landing a
branch is a no-op), guarded (ownership check), and gated (CI gate or local
`.warden/check.yml` checks must be green). The manager calls `land` automatically;
the operator may call it manually (e.g. to bypass a stuck gate after inspection).

### 34.8 Integration branch (per-plan)

Each run resolves **one** integration branch at register/preflight time, persists
it on `RunRecord.IntegrationBranch`, and every consumer (preflight, land, worker
spawn digest, status API, ledger) reads that stored value — workers must not
guess the branch; a wrong PR base returns `ErrWrongBase`.

**Default for new runs:** `autopilot/<sanitized-plan-name>` (e.g. plan
`notifications` → `autopilot/notifications`). **Concurrent runs in one repo**
each get a distinct branch so they cannot land into each other's trees.

**Precedence** (`autopilot.merge.target_branch` in config):

| Template value | Behavior |
|---|---|
| empty or `autopilot/integration` (legacy default) | Derive `autopilot/<plan>` per run |
| contains `{{plan}}` | Expand to the sanitized plan name (e.g. `integration/{{plan}}`) |
| any other value | Custom global override as-is |

**Grandfathering:** runs that already resolved to `autopilot/integration` keep
that branch across upgrade and daemon restart — warden never re-derives a stored
branch. New runs after upgrade use the per-plan default unless overridden.

**CI gating (`gate: auto`):** warden picks `ci` only when a workflow's
`on.pull_request.branches` covers the **resolved** branch. Workflows listing
`autopilot/integration` exactly do **not** cover `autopilot/<plan>`. When no
workflow matches, `gate: auto` downgrades to `local` and preflight/status emit
an explicit warning. **`warden autopilot init`** prints a hint to add
`autopilot/**` to workflow triggers — that glob covers every per-plan branch.
Never auto-merged to `main` — the operator reviews the integration branch and
fast-forwards `main` when satisfied.

### 34.9 Run ledger

The daemon's durable record of run state. Canonical storage is a JSON array at
the ctx key `autopilot.<run_id>.tasks` (dot-namespaced; the ctx store rejects
`/`). Each row is `{id, state, worker_id, branch, pr, note, updated_at}`;
`state` is one of `pending` → `assigned` → `in_progress` → `pr_open` → `gated`
→ `landed` and is **validated on write**. Optional overlay keys
`autopilot.<run_id>.tasks.<id>.state` and `.branch` exist so the TUI can segment
workers by ledger state without parsing the array; the array remains the source
of truth. Landings are an append-only JSON array at
`autopilot.<run_id>.landings` (`{branch, sha, pr, landed_at}`), written
authoritatively by the daemon. The journal is `autopilot.<run_id>.journal`.

Plan-file task statuses (`pending` / `active` / `done` / `failed`) are a separate
checklist enum, not ledger states. Illegal ledger-state *transitions* (and extra
spec-machine labels such as `fixing` / `replanned`) are not enforced here.

Persists across manager restarts and daemon restarts — re-enabling autopilot
continues from the ledger.

### 34.10 Per-repo (project-level) switch

The autopilot switch is **per-repository**, not one global flag. `warden autopilot
on` run inside a repo enables **only that repo** — other repos are unaffected.
`warden autopilot enable --repo <root>` (MCP: `set_autopilot { repo }`) targets a
different repository. The plan/manager/merge **template** stays global in the
`autopilot` config block; per-repo state is just the on/off bit and its run.

The enabled set is **persisted** as marker files under
`<data_dir>/autopilot/enabled/`, so previously-enabled repos **come back up
automatically across a daemon restart**. It is the source of truth for which
repos are on — a config hot-reload re-applies the template but never resets the
enabled set. `warden autopilot status` lists the enabled repos (`enabled_repos`
in the wire status; the scalar `enabled` now means "any repo is on"). `warden
autopilot disable` is per-repo — disabling one repo leaves other enabled repos
running.

### 34.11 Run completion marker

When the manager has verified the plan's `done_when` criteria, it declares the
run complete (MCP `autopilot_complete` / `POST /api/v1/autopilot/complete`; no
arguments — the run is inferred from the calling manager's own identity, and only
a run's own manager may complete it). The daemon then:

- **writes an in-place marker** into the plan file — `status: complete` and
  `completed_at: <RFC3339>` — preserving every other key, its ordering, and your
  inline comments (it round-trips the YAML nodes, not a struct re-marshal);
- **tears the manager down** gracefully (in-flight workers keep running);
- **retains the run ledger** (state `complete`).

**Preflight skips a complete plan:** a plan carrying `status: complete` is not
registered as an active run, so a finished run is **never executed again by
mistake** on a future enable or daemon restart. `autopilot_complete` is
idempotent — a second call is a no-op. To re-run a completed plan, remove the
`status: complete` line (or point the config at a fresh plan file).

### 34.12 Config hot-reload

The entire `autopilot` config block **hot-reloads with no daemon restart** (see
§12.1). Editing `~/.warden/config.yaml` re-applies the plan/manager/merge template,
the backend cost ladder, `allow_pay_per_use`, and the guardian heal thresholds,
and re-runs the per-repo reconcile over the persisted enabled set. Adding a
`plans[]` entry starts it; **removing one tears down its run** (a config-presence
sweep, so a transient preflight failure never kills a still-configured run). The
guardian **tick cadence** (`interval`) is the one autopilot setting still read
once at loop start — changing it needs a restart.

### 34.13 Known limitations

- `rate_limit.auto_resume` and `auto_restart` are global config toggles, not
  per-run autopilot overrides. Configure them in `~/.warden/config.yaml`.
- Guardian stage 3 (rotate) requires more than one free-tier backend. With only
  `antigravity` in the free tier, the guardian falls back directly to backoff
  after a restart fails.
- `Controller.SelectWorkerBackend(runID)` is exposed but the manager picks worker
  backends itself; worker-backend selection is not daemon-filled.

## 35. Agent-backend registry & internal-thinking router

`warden usage [--json] [--refresh]` reads provider-owned limit windows for every
exact `subscription` registry row without rescanning or changing routing state.
Results are concurrent, cached briefly in sanitized daemon memory, and retain
successful rows when another provider fails (exit 2). Codex has structured usage
(primary/secondary windows). Cursor supplies **three** subscription windows
(`included` / `auto` / `api`) from the dashboard `GetCurrentPeriodUsage` RPC —
Composer and `cursor-grok-*` on included, exact `auto` on auto, and Claude/GPT/Gemini/Kimi/GLM
on api — never flattened to one percent. Antigravity supplies **two** subscription
windows (`gemini` / `non-gemini`) parsed from its `fetchAvailableModels` RPC (local
OAuth2 token discovery + refresh), mirroring the free tier's separate Gemini and
non-Gemini model pools — never flattened to one percent, and degrading gracefully to
two `null`-percentage rows if the endpoint is unreachable. Claude reports a **single**
session (`five_hour`) window — used percent + reset time — read from the OAuth
`/api/oauth/usage` endpoint the `claude` CLI's `/usage` pager uses, authenticated with
the local Claude Code access token and degrading gracefully to one `null`-percentage
row if the endpoint is unreachable. Every result
carries a deterministically sorted `usage` array of distinct limits with stable
id/scope/label, nullable percentage and reset time, and nullable safe model
selectors; provider pools and Codex primary/secondary windows are never flattened.
Synthetic local quota estimates are excluded.

warden keeps a **persistent registry** of the coding-agent CLIs it can drive, so
"which backends exist, how they're billed, and which is the default" is a durable,
inspectable fact rather than something re-derived on every spawn. The registry is
the **single source of truth** that both autopilot's cost-tier ladder (§34.4) and
the internal free/local thinking router read from.

### 35.1 The store

One record per backend plus a reserved settings singleton, in an embedded ScrivaDB
collection at `~/.warden/backends` (`internal/backendstore`). Each backend row
carries:

- **Detection facts** (refreshed by a rescan): `installed`, `binary_path`,
  `detected_at`.
- **User preferences** (preserved by a rescan): `tier`, `default` (at most one row
  is default), `enabled`.
- **Router state**: `limited_until` — stamped when a free backend returns a
  rate-limit / spend signal, so it drops out of the internal-thinking walk until it
  clears (config `backends.limit_retry`, default `15m`).
- `is_local` — the reserved **`local`** row: a `$0`, never-limited, never-default
  class for the local model.

**Detection is a fact; tiering is a preference.** A `rescan` reconciles the
detection fields (adds newly installed CLIs, marks vanished ones uninstalled) and
**never** touches your tier / default / enabled choices.

### 35.2 Tiers & the default

Tiers are `free` · `subscription` · `pay_per_use` · `unclassified` (a newly
detected CLI starts `unclassified`, treated as *not free*), plus the reserved,
system-set `local`. Exactly one backend may be the **default** (the backend an
empty `warden start --backend` / `spawn_agent {}` resolves to); the reserved
`local` row can never be a user-agent default and is rejected. (Terminals are no
longer a backend row — they are a `kind=terminal` session, §8.)

### 35.3 Internal-thinking router (free/local only, never paid)

warden does its own **internal thinking** — task classification, activity
summaries, agent naming, digest narration, and memory curation — and routes it
**strictly through free and local backends** (`internal/internalrouter`). It
**never makes a paid call**. The store's **internal-thinking mode** picks the walk:

- **`local_only`** — route internal thinking to the local model only.
- **`free_plus_local`** (the default) — try eligible **free** CLI backends first
  (default-first, then stable id order), falling back to the never-limited local
  model.

A free CLI backend is eligible only when it is installed, enabled, tier `free`, and
not currently limited. On a rate-limit / spend signal the router stamps that
backend's `limited_until` and continues down the list; when the walk is exhausted
the caller **degrades gracefully** (deterministic slug / skipped narration /
default bucket / no curate proposal) rather than escalating to a paid backend.
Paid (`subscription` / `pay_per_use`) and `unclassified` backends are never called
for internal thinking in either mode.

### 35.4 Autopilot ladder — one source of truth

Autopilot's cost-tier ladder and paid-autopilot gate derive from this registry
(§34.4): only installed, enabled, non-`local` rows are eligible, bucketed by tier.
The deprecated `autopilot.brain.backends` ladder and
`autopilot.brain.allow_pay_per_use` gate are **imported once** (a sentinel-guarded,
idempotent migration on the first boot after upgrade) and then ignored — the store
value wins thereafter, and the daemon warns if the config still carries the keys.

### 35.5 Surfaces

| Surface | How |
|---|---|
| **CLI** | `warden backend list` / `rescan` / `tier <id> <tier>` / `default <id>` / `enable <id>` / `disable <id>` / `thinking-mode <mode>` |
| **MCP** | `list_backends`, `rescan_backends`, `set_backend_tier`, `set_default_backend`, `set_thinking_mode` (enable/disable is CLI/web/TUI + REST only) |
| **Web** | the **🧩 backends** panel (AttentionBar): table with Tier dropdown, Default radio, Enabled checkbox, live Limited countdown, thinking-mode select, ⟳ Rescan |
| **TUI** | the Backends page (`b`): `t` cycle tier · `d`/`enter` default · `e`/space enable · `m` thinking-mode · `r` rescan |
| **REST** | `GET /api/v1/backends`, `POST /api/v1/backends/rescan`, `PUT /api/v1/backends/default`, `PUT /api/v1/backends/thinking-mode`, `PATCH /api/v1/backends/{id}` (tier / enabled) |

### 35.6 Config

The `backends` block currently has one sub-key: `backends.limit_retry` (Go
duration, default `15m`) — how long the internal-thinking router skips a free CLI
backend after it returns a rate-limit / spend signal, before retrying it. Tiers,
the default, enabled flags, and the thinking mode live in the **store**, not the
config file, and are edited via the surfaces above.
