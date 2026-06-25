# warden plugin system — design (#47)

**Status:** shipped · **Date:** 2026-06-25

## Motivation

warden's task types and lifecycle are deliberately opinionated, but operators
occasionally want to extend it without forking: a custom agent task type with its
own isolation policy (e.g. a `lint-bot` type), or a side effect at a defined
moment in an agent's life (notify Slack on commit, record a metric on spawn).
Roadmap item #47 asked for exactly this — *custom task types + lifecycle hooks* —
while flagging it speculative. So this is a **thin, well-tested MVP**: a clean
extension seam with one working example plugin, not a general-purpose runtime.

## The plugin-model decision: subprocess, not Go-plugin or WASM

Three options were on the table:

| Option | Verdict | Why |
| --- | --- | --- |
| Go `plugin` package | **rejected** | linux/mac-only, requires the plugin to be built with the exact toolchain + build flags as warden, fragile across Go versions, no Windows. A non-starter for a distributed binary. |
| WASM (e.g. `wazero`) | **rejected for MVP** | pure-Go and sandboxed, genuinely attractive — but it adds a heavy dependency and an ABI/host-call surface to design and version, for a feature with *no driving use case yet*. Not worth the weight now. |
| **External executable over JSON-over-stdio** | **chosen** | zero new dependencies, language-agnostic (a plugin can be a shell script, Python, a Go binary…), and — decisively — it **mirrors warden's existing external-process hook mechanism**. |

The deciding factor is consistency. warden already shells out to subprocesses at
defined points and fails open: the per-agent PreToolUse guard hooks
(`warden hook guard` / `git-guard` / `check-guard`, installed via a
`claude --settings` file) read a JSON request on stdin, emit a JSON verdict on
stdout, run under a hard timeout, and **allow on any error**. The plugin system
is the same pattern applied to lifecycle events. An operator who understands the
guard hooks already understands plugins, and warden's code reuses the same
mental model (bounded `CommandContext`, parse-or-skip, log-and-continue).

If a concrete use case later demands sandboxing or in-process performance, WASM
can be added behind the same `Dispatcher` seam without changing the config schema
or the wire protocol.

## Architecture

```
config (plugin_registry, plugins gate)
   │  []plugin.Spec
   ▼
plugin.Load(specs) ──► *plugin.Registry ──┬─► Registry.Lookup ──► store.SetCustomTypeLookup(seam)
   (validates)                             │      (custom task type policies)
                                           └─► plugin.NewDispatcher(reg) ──► daemon Server.plugins
                                                  (lifecycle hook events)        │
                                                                                 ▼
                                              spawn / commit / check routes call s.plugins.Dispatch(...)
```

- **`internal/plugin`** is the whole feature, in three files:
  - `protocol.go` — the versioned wire types (`Request`, `Response`,
    `SessionMeta`, `HookEvent`).
  - `plugin.go` — the config-facing `Spec`, the validated `Plugin` descriptor,
    and the `Registry` (custom-type index + subscriber lookup).
  - `dispatcher.go` — the fail-open `Dispatcher` (subprocess invocation, timeout).
- **`internal/store`** gains a tiny seam (`customtype.go`) so its closed `Type`
  enum can recognize plugin-provided types *without importing plugin* (see below).
- **`internal/config`** adds the `plugins` gate + `plugin_registry` list.
- **`internal/daemon`** holds the dispatcher on the `Server` and calls it at the
  spawn / commit / check routes.
- **`internal/cli`** adds the thin `wd plugin list` command.

## Custom task types vs. the closed `Type` enum

`store.Type` is a closed enum consumed by exhaustive `switch`es (`Valid`,
`DefaultWorktree`, `NormalizeType`) and by `lifecycle.wantWorktree`. Custom types
must slot in **without breaking those switches and without changing any built-in's
behavior**.

The mechanism is a **function-var seam**. `store` declares:

```go
var customTypeLookup func(name string) (CustomTypePolicy, bool)
func SetCustomTypeLookup(fn ...)              // daemon installs the registry's Lookup
func (t Type) Builtin() bool                  // the exhaustive built-in switch
```

Each enum method now reads: *built-in? behave exactly as before. Otherwise,
consult the lookup.*

- `Valid()` — built-in → true; else true iff the lookup knows the name.
- `DefaultWorktree()` — built-in → unchanged; else the custom type's declared
  `Worktree` policy.
- `NormalizeType()` — built-in (and legacy aliases) → unchanged; a registered
  custom name is preserved as-is; everything else still collapses to `other`.

`lifecycle.wantWorktree` needs **no change**: it already routes through
`DefaultWorktree()`, so a custom type with `worktree: true` is isolated by default
(honoring `--in-repo`), and one with `worktree: false` stays in-repo.

Why this direction: `plugin` imports `store` (the wire `SessionMeta` projects a
`store.Session`), so `store` cannot import `plugin` — a cycle. The single
write-once function var is set at daemon startup before any agent is served and
only read afterward, so it needs no lock. With no plugins installed the var is
nil and the enum behaves **byte-for-byte as it always has** — proven by
`TestBuiltinsUnchanged*`.

## The wire protocol (v1)

`protocol_version: 1` is stamped on every request and expected on every response.

**Request (stdin):**
```json
{
  "protocol_version": 1,
  "event": "post-commit",
  "session": {"id":"…","type":"…","repo":"…","worktree":"…","branch":"…","workdir":"…"},
  "payload": {"key":"value"}
}
```

**Response (stdout):** advisory only — recorded, never gating.
```json
{ "protocol_version": 1, "ok": true, "message": "…" }
```

A plugin may exit 0 with empty stdout to silently acknowledge. `Payload` is a
`map[string]string` so events can carry extras (commit `sha`/`branch`/`committed`,
check `name`/`passed`) and grow without a version bump.

### Hook events

`pre-spawn`, `post-spawn`, `pre-commit`, `post-commit`, `pre-check`,
`post-check`, `pre-teardown`. The spawn, commit, and check points are wired in
this MVP (the daemon routes that call `s.life.Spawn/Commit/Check`). `pre-teardown`
is defined in the protocol for forward-compatibility but not yet dispatched.

Dispatch lives at the **daemon route handlers**, not inside `lifecycle.go`. That
is where the session record + request context exist, and it mirrors how the guard
hooks and git/check redirects are wired — keeping `lifecycle` pure. This is the
one notable "ambiguous fork" resolved toward the existing pattern.

## Security posture: default-off, fail-open

- **Default off.** The `plugins` gate defaults to `false`. Plugins execute
  arbitrary external code, so enabling them is a deliberate, documented opt-in —
  the same stance as `local_llm`. A fresh install runs zero plugin code.
- **Fail-open, always.** Every invocation is bounded by a hard `CommandContext`
  timeout (5s; `WaitDelay` guarantees the kill even when a grandchild holds the
  stdout pipe). A missing binary, non-zero exit, timeout, malformed JSON, empty
  output, `ok:false`, or version mismatch is **logged and skipped** — it never
  blocks, errors, or panics the agent, and a failing plugin never aborts the
  dispatch of the others. Hooks are observers, not gates: even a `pre-` event's
  response cannot veto the action.
- **Strict config, lenient runtime.** `Load` rejects blank/duplicate names, blank
  paths, unknown events, and custom-type names that collide with a built-in or
  another plugin — loudly, at startup. But a *bad config* degrades to
  plugins-disabled (logged) rather than refusing to start the daemon, and a
  *bad plugin at runtime* fails open. `Load` does **not** stat the executable
  (it may legitimately not exist yet; dispatch fails open anyway).

## Invariants

1. With plugins off (the default), warden behaves exactly as before — the
   `Type` enum, spawn isolation, and every route are unchanged.
2. No built-in task type's validity or worktree policy is ever altered by a
   registered custom type.
3. A plugin can never block, slow (beyond the timeout), error, or crash an agent.
4. Custom type names are unique across built-ins and all plugins.
5. The protocol is versioned; the response version is checked (and logged on
   mismatch) but never used to reject — consistent with fail-open.

## Testing

- **Registry** (`plugin_test.go`): register/lookup, custom-type isolation policy,
  rejection of every bad-config class, cross-plugin duplicate-type rejection,
  whitespace trimming, the `Lookup`-satisfies-`store`-seam compile-time check.
- **Store seam** (`customtype_test.go`): a registered custom type validates and
  gets its declared policy; **built-ins are byte-for-byte unchanged** both with
  and without a lookup installed; unknown names stay invalid/collapsed; legacy
  aliases still map.
- **Protocol** (`protocol_test.go`): request/response marshal round-trips,
  `MetaFromSession`, `ValidEvent`.
- **Dispatcher** (`dispatcher_test.go`): nil-safe no-op, happy path against a
  stub runner, **every fail-open path** (runner error, malformed JSON, empty
  output, `ok:false`, version mismatch — each proven not to abort the loop), a
  real-subprocess happy path + missing-binary + timeout against tiny temp
  scripts, and a concurrency/race check.
- **CLI** (`plugin_test.go`): `formatPluginList` enabled/disabled/empty/error
  states and canonical event ordering.

No test touches the network or a real daemon.
