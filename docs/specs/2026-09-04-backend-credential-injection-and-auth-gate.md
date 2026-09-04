# Backend Credential Injection & Pre-flight Auth Gate

**Date:** 2026-09-04
**Status:** Design
**Related:** `docs/specs/2026-08-06-backend-registry.md` (registry + tiers),
`docs/security/audit-2026-06-28.md` (launch-line quoting invariants),
memory `backend-reauth-paste-code-flow` (Claude paste-code OAuth),
`backend-usage-command` (per-backend usage probing that already detects
`unauthenticated`).

---

## 0. Motivation

warden launches each backend by typing its **CLI command into a tmux pane** and
delegating authentication entirely to that CLI's own login state (the OAuth token
or config the CLI keeps under `~/.claude`, `~/.codex`, `~/.cursor`, the
Antigravity token file, …). warden itself holds no credentials.

The failure this creates: **if the backend CLI is not logged in, a freshly
spawned agent hits an interactive login prompt and silently parks in
`waiting_for_input`.** From warden's side it looks like a normal "needs a human"
state, with no indication that the blocker is *auth* rather than an approval or a
question. On a headless host, or when driving warden remotely from the app, this
is a hard stall that requires a human to notice, attach, and log the CLI in.

We want two things:

1. Where a backend accepts a key, let **warden hold the credential and authorize
   the backend itself**, so the daemon "takes care of login" and the agent never
   sees a login prompt.
2. Where a backend can only authenticate interactively (OAuth), **turn the silent
   hang into an explicit, remotely-resolvable `needs_auth` state** with a clear
   reason and a re-auth path — the best outcome technically possible for
   subscription auth.

This is explicitly **opt-in and additive**: a host that is already logged in
behaves exactly as today.

---

## 1. Current architecture (the seams we build on)

- **Launch is a typed tmux command.** `Backend.LaunchCmd`
  (`internal/agentbackend/backend.go:144`) returns a shell string that lifecycle
  types into the pane. Adapters MUST shell-quote every embedded value
  (`shellQuoteArg`, per the security audit — every backend adapter already imports
  it).
- **An environment-injection seam already exists.** `newAgentSession`
  (`internal/lifecycle/lifecycle.go:1772`) creates the session with
  `tmux new-session -e KEY=VAL …` and accepts a variadic `env ...string` (used
  today for pipeline env). Session environment set here is inherited by the shell
  that runs the backend CLI. **This is the injection point** — no new launch
  mechanism is required.
- **warden already probes auth state.** The usage prober reads the Antigravity
  token file and returns `"unauthenticated"`
  (`internal/agentbackend/backends/antigravity.go:719`); the backend registry
  (`docs/specs/2026-08-06-backend-registry.md`) already detects installed CLIs and
  their availability. We have precedent for asking a backend "are you usable right
  now?" before relying on it.
- **State detection is per-adapter.** `Backend.DetectState(pane) State`
  (`backend.go:164`) maps a captured pane to `idle|working|needs_input|unknown`.
  There is currently **no `needs_auth`** state — a login prompt collapses into
  `needs_input`.
- **Config already has a `backends` block.** `BackendsConfig`
  (`internal/config/config.go:143`) is the natural home for a credentials
  sub-block; it is documented and hot-reloaded like the rest of the config.
- **A local secret file precedent exists.** The daemon token lives at
  `~/.warden/token.env` (0600) with `token`-family CLI verbs
  (`internal/cli/token.go`). The credential store follows the same custody model.

---

## 2. The two auth models (why the design splits)

Backends do **not** share one auth mechanism. The design must treat them
separately, because what "warden authorizes it" means is fundamentally different.

### 2a. API-key backends — warden CAN fully self-authorize

These accept a credential from the **environment** and need no interactive login:

| Backend | Env credential(s) | Notes |
|---|---|---|
| Aider | `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` / provider keys | already env-driven today |
| OpenCode | provider keys | env-driven |
| Crush | provider keys | env-driven |
| Goose | provider keys | env-driven |
| **Claude Code** | `ANTHROPIC_API_KEY` | ⚠️ bills **pay-per-use**, NOT the Pro/Max subscription |
| **Codex** | `OPENAI_API_KEY` | ⚠️ bills pay-per-use, not a ChatGPT subscription |

For these, warden holding the key and injecting it via the existing `-e` seam
**completely eliminates the login hang.** The only caveat is the billing-mode
change for Claude/Codex (see §7 Decisions).

### 2b. Subscription / OAuth backends — NO static key possible

Claude (Pro/Max), Cursor, and Antigravity authenticate via **interactive OAuth
(device / paste-code)**, not an API key. There is nothing warden can "hold" to log
these in headlessly, and warden deliberately does **not** refresh the Claude OAuth
token today. For these, the realistic and complete win is **detection + surfaced
re-auth**, not headless login:

- pre-flight: refuse-to-hang — if logged out, don't spawn into a login prompt;
- mid-run: recognize the "please run `X login`" pane as `needs_auth`, not a
  generic approval;
- resolution: expose the existing paste-code re-auth flow, which already works over
  any warden terminal (including from the app / phone — see
  `backend-reauth-paste-code-flow`).

---

## 3. Design

Three components. Piece A solves §2a outright; Pieces B and C make §2b fail loudly
and fixably.

### A. Credential store + injection (API-key backends)

- **Store.** A per-backend credential map persisted under `~/.warden/`
  (`credentials.env`, mode **0600**, same custody as `token.env`). Each entry maps
  a backend id → an ordered set of `ENV_NAME=value` pairs. Values are opaque to
  warden; warden never parses or logs them.
- **CLI (local, host-only — like `token`):**
  - `warden backends creds set <backend> --env KEY=VALUE [--env …]` — writes/updates.
  - `warden backends creds set <backend> --env KEY --stdin` — read the value from
    stdin so it never lands in shell history / `ps`.
  - `warden backends creds list` — shows backend ids and **env-var NAMES only**
    (never values; render as `SET`/`unset`).
  - `warden backends creds clear <backend>` — remove.
  These are **CLI-only** by design (secret authoring), mirroring the existing
  `token`/`config` local-only verbs.
- **Injection.** At spawn, lifecycle resolves the launching backend's credential
  set and passes the pairs through the existing `newAgentSession(..., env...)`
  variadic → `tmux new-session -e`. No change to `LaunchCmd`; the key rides in the
  session environment, not the typed launch line (so it is never shell-quoted into
  a visible command and never appears in pane scrollback).
- **Precedence.** An existing ambient env var on the daemon host wins unless the
  operator opts into override (`--force`/config flag), so warden never silently
  shadows a deliberately-set host key.

### B. Pre-flight auth gate

- **New optional backend extension** (additive, capability-style, like
  `PromptSeeder`), NOT a required method on every adapter:
  ```go
  // AuthChecker is implemented by adapters that can cheaply answer "is this
  // backend authenticated right now?" without spawning an agent. Adapters that
  // cannot are simply not consulted (spawn proceeds as today).
  type AuthChecker interface {
      // AuthState reports the backend's current auth posture on this host.
      // reason is a short human string for surfacing; it MUST NOT contain
      // credentials, tokens, account ids, emails, or raw provider responses.
      AuthState(ctx context.Context) (ok bool, reason string)
  }
  ```
  Antigravity already has the machinery (`agyReadTokenFile`); Claude/Codex/Cursor
  check their token/config presence or a cheap `--version`-class probe that does
  not consume quota.
- **At spawn:** if the resolved backend implements `AuthChecker` and reports
  `ok=false`, and no injected credential (Piece A) covers it, the daemon does **not
  type a doomed launch line**. It creates the session record in a new terminal
  status `needs_auth` with `reason` (e.g. `"claude not logged in — run: claude`),
  and does not start the agent turn. The operator resolves auth, then a
  `retry`/`resume` starts the agent for real.
- The gate is **advisory and fail-open**: a backend with no `AuthChecker`, or a
  probe error, spawns exactly as today (we never block a launch on a flaky probe —
  consistent with the frictionless-safeguards philosophy).

### C. Mid-run `needs_auth` detection

- Add `StateNeedsAuth State = "needs_auth"` to the neutral state enum
  (`backend.go`), between `needs_input` and the richer warden status machine.
- Extend each adapter's `DetectState` to recognize its own "not authenticated /
  please log in" pane signature (e.g. Cursor's login prompt, Claude's OAuth
  paste-code screen) and return `needs_auth`. When unrecognized it stays
  `needs_input` — no regression.
- warden's status layer maps `needs_auth` to a distinct, actionable agent status
  (surfaced in TUI, API, MCP `get_agent`, SSE) so "blocked on login" is
  visibly different from "blocked on an approval".

### Surfacing (all clients)

- `needs_auth` is a first-class status on the session DTO
  (spec-first: edit `internal/daemon/apidocs/openapi.yaml` then `make generate`;
  **never** hand-edit `internal/daemon/oapi/*.gen.go`), on MCP `get_agent`/`list_agents`,
  and on the SSE stream — so the app and remote clients can show "needs login" and
  offer the re-auth terminal.
- The re-auth action reuses the existing paste-code flow over a warden terminal
  (no new auth transport).

---

## 4. Files to change (indicative)

- `internal/agentbackend/backend.go` — add `StateNeedsAuth`; add optional
  `AuthChecker` interface (additive, no change to `Backend`).
- `internal/agentbackend/backends/*.go` — implement `AuthChecker` where cheap;
  extend `DetectState` for the login-prompt signature. Keep every embedded value
  shell-quoted.
- `internal/lifecycle/lifecycle.go` — resolve + inject credential env in the spawn
  path (through `newAgentSession`'s existing `env` variadic); consult `AuthChecker`
  before typing the launch line; emit `needs_auth`.
- `internal/config/config.go` — add a `credentials`/auth-gate sub-block under
  `BackendsConfig` (e.g. `auth_gate: bool`, override policy), documented + hot-reloaded.
- `internal/cli/` — `warden backends creds …` verbs (CLI-only secret authoring).
- `internal/daemon/apidocs/openapi.yaml` (+ `make generate`) — `needs_auth` status
  + optional `auth` reason field on the session DTO; then MCP + SSE surfacing.
- New `~/.warden/credentials.env` custody (0600), a small loader alongside the
  token loader.
- Docs (§ DoD): README, `docs/FEATURES.md` + root `FEATURES.md`, `docs/USAGE.md`,
  website guide + `reference/cli.md` (via `make gendocs`), and `skills/warden/`
  (agents should learn to read `needs_auth` and drive re-auth). Flip this spec's
  Status when it ships.

---

## 5. Security considerations

- **Custody:** credentials at rest in `~/.warden/credentials.env`, mode 0600,
  never in the repo, never in a worktree, never in `config.yaml` (which is
  world-readable-ish and hot-reloaded/logged).
- **Never on the launch line:** keys ride in the **session environment** via
  `-e`, not in the typed `LaunchCmd`, so they never appear in pane scrollback, the
  audit log's command strings, `ps`, or a shell history.
- **Never logged / never surfaced:** warden logs env-var **names**, never values;
  `creds list` shows `SET/unset`, never the secret. `AuthChecker.reason` and the
  `needs_auth` API field are subject to the same privacy rules as the recovery
  surface (§8 of the recovery spec): no credentials, tokens, account ids, emails,
  or raw provider responses.
- **`-e` visibility caveat:** the `tmux new-session -e KEY=VALUE` argument is
  itself visible in the daemon process's own `ps` argv at creation time. Evaluate
  passing secret env via a file-backed mechanism (a per-session env file sourced by
  the launch shell, or `tmux set-environment` from a piped value) if argv exposure
  is deemed unacceptable — decide in review (§7 Open Questions).
- **Override guard:** warden does not shadow a deliberately-set ambient host key
  unless the operator explicitly opts into override.

---

## 6. Phases

1. **Phase 1 — `needs_auth` state + pre-flight gate (no secrets).** Add
   `StateNeedsAuth`, the optional `AuthChecker` interface, wire the pre-flight gate
   in lifecycle, and surface the status through API/MCP/SSE/TUI. This alone kills
   the *silent* hang for every OAuth backend and is the lowest-risk slice (no
   credential handling). Ship first.
2. **Phase 2 — credential store + injection (API-key backends).** The 0600 store,
   the `creds` CLI verbs, and env injection through `newAgentSession`. Solves the
   hang outright for the §2a backends.
3. **Phase 3 — mid-run `DetectState` auth signatures + re-auth surfacing.**
   Per-adapter login-prompt detection and the app-facing re-auth terminal action.
4. **Phase 4 — docs + independent review (DoD).**

Phase 1 is independently shippable and valuable; Phases 2–3 layer on top.

---

## 7. Decisions to make before code (open questions)

1. **Billing default for Claude/Codex.** Injecting `ANTHROPIC_API_KEY` /
   `OPENAI_API_KEY` switches those backends from subscription to pay-per-use
   billing. Default posture: **do not inject unless the operator explicitly sets a
   key for that backend** (never silently move a user off their subscription).
   Confirm.
2. **`-e` argv exposure** (see §5) — accept it, or go file-backed for secret env
   from the start?
3. **Scope of the credential store** — global (host-wide) only, or also
   per-project overrides? Proposal: host-global for v1.
4. **Should the pre-flight gate be default-on or opt-in?** Proposal: default-on but
   fail-open (only acts when an `AuthChecker` positively reports logged-out), since
   it never blocks a launch that would otherwise succeed.

---

## 8. Acceptance criteria

- A backend with an injected key spawns and runs with **no login prompt**, on a
  host where that CLI was never interactively logged in.
- A logged-out OAuth backend with no injected key produces a `needs_auth` session
  with a clear, credential-free reason — **not** a silent `waiting_for_input` —
  visible in TUI, `get_agent`, and the SSE stream.
- A token that expires **mid-run** surfaces as `needs_auth`, not a generic
  approval.
- Credentials never appear in logs, the audit log, pane scrollback, `creds list`
  output, or any API field.
- A fully logged-in host with no credentials configured behaves **exactly as
  today** (pure additive; fail-open).
- DoD checklist (README/docs/site/skill/CLI-help) walked and reported.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
