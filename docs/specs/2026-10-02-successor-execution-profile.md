# Successor Execution Profile

Date: 2026-10-02
Status: Frozen (documentation only — no production code in this commit)
Parent orchestrator: `agent-96ef4ea2`
Pipeline: `successor-execution-profile` / job `spec`

## Goal

Every Warden-managed successor path must preserve the agent's **effective
execution contract**, including loopback access to the local Warden daemon, not
merely cwd + transcript + `PermissionMode`.

A mid-session hot-swap today preserves worktree, branch, agent id, and
`PermissionMode`, then launches the successor with only
`LaunchOpts{SessionID, Name, Model, Mode}`. Network / sandbox is not a persisted
field. The successor therefore falls back to the **new backend's default
posture**. That is how a Cursor agent that can reach loopback Warden
(REST/MCP on `127.0.0.1`) becomes a Codex successor that cannot: Codex's default
is `workspace-write` with **network off**, and `PermissionMode` is the wrong
field to infer this from.

This phase freezes a first-class, backend-neutral `ExecutionProfile` on the
Agent record. Implementers must not infer network from a backend's default mode
and must not overload `PermissionMode` (approval policy is orthogonal to
network/sandbox).

## Current Baseline

Inspected before freeze:

| Seam | File | Today |
| --- | --- | --- |
| Hot-swap launch | `internal/lifecycle/switch.go` `launchSuccessor` | Copies `agent.PermissionMode` (or config default) into `LaunchOpts.Mode`. No network field. |
| Spawn / restore / role / job / adopt | `internal/lifecycle/lifecycle.go` | Same: `Mode` only. Prompt-mode spawn is `spawnFreeForm` (the job prompt's `spawnPrompt`). Fork goes through `buildLaunch` → `ForkOpts`. Restore / SwitchRole / Adopt resume go through `ResumeCmd`. |
| Neutral opts | `internal/agentbackend/backend.go` | `LaunchOpts` / `ResumeOpts` / `ForkOpts` have `Mode` (approval) only. |
| Codex | `internal/agentbackend/backends/codex.go` `codexSandbox` | Mode → `-s read-only\|workspace-write\|danger-full-access` (+ `-a never` for auto-approve aliases). Empty/`default` Mode emits **no** `-s`, so Codex applies workspace-write **with network off**. `ResumeCmd` returns `codex resume --last` and drops Mode. |
| Cursor | `internal/agentbackend/backends/cursor.go` `cursorModeFlag` | Mode → `--mode plan\|ask` / `--auto-review` / `-f`. Documented `--sandbox enabled\|disabled` and network-access config are **not** emitted. `ResumeCmd` is `cursor-agent --continue` (+ model). |
| Persist | `internal/agentstore/types.go`, `internal/store/types.go` | `PermissionMode string \`json:"permission_mode,omitempty"\``. `QuotaBinding` is the persist-only pointer pattern (`omitempty`, not in OpenAPI). |
| Handoff | `internal/lifecycle/switch.go` `swapSystemContext` | Swap direction + worktree/branch/repo. No execution contract. |
| Adapter persist | `internal/daemon/lifecycle_adapter.go` `HotSwap` | Persists `AiCli`, `Model`, `AICLISessionID`, `QuotaBinding`. Does **not** yet persist a profile field. |

Incident to lock: **empty `PermissionMode`, Cursor → Codex HotSwap**. Successor
launch is a bare `codex` (or `codex -m …`) with no network override.

## Canonical Type

Define in `internal/store/types.go` (persist DTO layer). `agentstore.Agent`
uses the same type. This is a **value**, not a `QuotaBinding`-style pointer:
omitted / zero means legacy, not "unbound domain".

```go
// ExecutionProfile is the backend-neutral sandbox/network contract for a
// Warden-managed agent. It is orthogonal to PermissionMode (approval policy).
type ExecutionProfile struct {
    // Network is loopback | full | none | "". Empty is legacy.
    Network string `json:"network,omitempty"`
}

const (
    NetworkLoopback = "loopback"
    NetworkFull     = "full"
    NetworkNone     = "none"
)

// EffectiveNetwork is the only reader launch paths may use.
// Empty/legacy fails OPEN toward daemon reachability: loopback, never the
// successor backend's default posture.
func (p ExecutionProfile) EffectiveNetwork() string {
    if p.Network == "" {
        return NetworkLoopback
    }
    return p.Network
}
```

Persist on both records with omitempty, round-trip like `QuotaBinding` through
`ToSession` / `FromSession`:

```go
// agentstore.Agent and store.Session:
ExecutionProfile ExecutionProfile `json:"execution_profile,omitempty"`
```

JSON example (pinned):

```json
{ "execution_profile": { "network": "none" } }
```

A legacy record with no field unmarshals to `{Network: ""}`. Launch must still
emit loopback flags. Do not invent a profile from `Caps.PermissionModes`.

### Stamp vs preserve

| Event | Rule |
| --- | --- |
| Warden-managed spawn (`Spawn`, `SpawnJob`) | Stamp `Network=loopback` unless the request already pinned `none` or `full`. |
| Adopt resume | Stamp `loopback` on the new Agent and pass it into `ResumeCmd`. Live-mode Adopt does not relaunch; still stamp the record so a later Restore/HotSwap is contracted. |
| HotSwap / Restore of a legacy empty profile | Treat as loopback at launch **and** stamp `Network=loopback` onto the in-memory agent so the caller persist writes it. |
| HotSwap / Restore of a pinned `none` or `full` | Preserve exactly. Must **not** upgrade `none` to loopback. |
| Bulk migrate live agents | Out of scope. Stamp only on next spawn / swap / restore. |

`SpawnRequest` (and `JobSpawnRequest` if it already mirrors permission fields)
may grow an optional `ExecutionProfile` so tests can pin `none` without a CLI
flag. No operator CLI / MCP flag in this phase.

### Orthogonality

`PermissionMode` remains approval policy (`acceptEdits`, Codex `workspace-write`,
Cursor `force`, …). `ExecutionProfile.Network` is sandbox/network. Mapping
`auto` / empty Mode → `danger-full-access` to "get network" is **forbidden**.

## Launch Seam

Add `Network string` to `agentbackend.LaunchOpts`, `ResumeOpts`, and `ForkOpts`.
Lifecycle fills it from `agent.ExecutionProfile.EffectiveNetwork()` on **every**
launch. Adapters translate; they must not default Network from Caps.

Recommend one helper so paths cannot drift, e.g. `launchNetwork(agent)` used by
`launchSuccessor`, `buildLaunch`, `spawnFreeForm`, `SpawnJob`, and
`resumeInTmux` / `resumeInTmuxWithHints`. `resumeInTmux` currently takes a
`mode` string — thread `network` the same way (or pass a filled `ResumeOpts`).

`internal/daemon/lifecycle_adapter.go` `HotSwap` persist **must** copy
`ExecutionProfile` alongside `QuotaBinding`. Restore persist must do the same
when Restore stamps a legacy empty profile.

```text
Agent.ExecutionProfile
  -> lifecycle EffectiveNetwork()
  -> LaunchOpts / ResumeOpts / ForkOpts.Network
  -> adapter translation (Codex / Cursor / no-op others)
```

## Adapter Translations

Adapters must not invent a profile from `Caps.PermissionModes` defaults.

### Codex

Do **not** rely on `auto` → `danger-full-access` for network. `danger-full-access`
remains the mapping for unrestricted **PermissionMode**, not the only way to get
loopback. Workspace-write + loopback must coexist.

Documented surface (Codex CLI `-c` / `sandbox_workspace_write.network_access`,
to be written into `docs/agent-backends/codex.md`):

| Network | Translation (in addition to existing Mode → `-s` / `-a`) |
| --- | --- |
| `loopback` (default) | Keep Mode's sandbox. If sandbox is `workspace-write` **or omitted** (Codex default), append `-c sandbox_workspace_write.network_access=true`. Never upgrade `-s` to `danger-full-access` for network. |
| `full` | Same `-c` enablement. Codex's boolean cannot OS-restrict to loopback-only at the verified CLI (v0.142.3). If Mode is already `danger-full-access`, host network is already on; do not require that Mode for loopback. |
| `none` | Omit the `-c` network_access override. |

v1 does **not** wire `features.network_proxy` domain allowlists (unverified at
the adapter's Codex version). The Agent record still distinguishes `loopback`
vs `full` for other adapters and future Codex proxy work.

`LaunchCmd` and `ForkCmd` already apply Mode. `ResumeCmd` today returns
`codex resume --last` and **drops Mode**. Restore / SwitchRole / Adopt resume
are load-bearing: `ResumeCmd` must start consuming `Mode` **and** `Network`.

Worked example (the incident fix):

```
Mode=workspace-write (or empty) + Network=loopback
  => codex -s workspace-write -c sandbox_workspace_write.network_access=true
     (empty Mode may omit -s; -c network_access=true is still required)
  => MUST NOT contain -s danger-full-access
```

### Cursor

Wire the documented `--sandbox` / network-access superpower from
`docs/agent-backends/cursor.md` rather than leaving Cursor's default.

| Network | Translation |
| --- | --- |
| `loopback` | `--sandbox enabled` **plus** network-access enabled so localhost / the Warden daemon stay reachable. Prefer a per-invocation flag from `cursor-agent --help` at the verified version. Must **not** mutate `~/.cursor/cli-config.json`. Must **not** use PermissionMode `force` / `-f` as a network substitute. |
| `full` | `--sandbox disabled` (host network). |
| `none` | `--sandbox enabled` without enabling network. |

If the verified `cursor-agent` binary exposes no launch flag for
`sandbox.networkAccess`, keep sandbox enabled and pass the documented companion
override that does not require `-f`. Confirm against `--help`; do not guess a
flag that strips current Cursor loopback reachability.

`ResumeCmd` must apply the same flags as `LaunchCmd` (today it emits only
`--continue` + model).

### Claude and other adapters

No-op on `Network` when the backend already has host / loopback access; **never
strip it**. Claude `LaunchCmd` / `ResumeCmd` with `Network=loopback` must stay
byte-identical to today's command. Antigravity's `--sandbox` is a
**PermissionMode**, not this field — do not conflate them.

## Exhaustive Successor Paths

Every path below must pass `EffectiveNetwork` into the adapter. Backend recovery
and guardian rotation already call `HotSwap` — they inherit if `launchSuccessor`
+ adapter persist are correct; still land the quota→Codex regression.

| # | Path | Code | Notes |
| --- | --- | --- | --- |
| 1 | HotSwap → launchSuccessor | `internal/lifecycle/switch.go` | Same agent id / worktree. Stamp empty→loopback; preserve pinned none/full. Persist profile in `lifecycle_adapter.HotSwap`. |
| 2 | Restore | `Lifecycle.Restore` → `resumeInTmux` | Uses `ResumeCmd`. Codex/Cursor resume must emit profile flags. |
| 3 | SwitchRole | `Lifecycle.SwitchRole` → `resumeInTmuxWithHints` | Same resume seam. |
| 4 | spawnPrompt | `Lifecycle.Spawn` → `spawnFreeForm` | Prompt / free-form spawn (`Type == ""`). Stamp loopback unless pinned. |
| 5 | spawnTyped (including fork) | `spawnTyped` → `buildLaunch` → `LaunchCmd` / `ForkCmd` | Fork is Codex `SessionForker`; `ForkOpts.Network` required. |
| 6 | spawnJob | `Lifecycle.SpawnJob` | Stamp like Spawn. |
| 7 | Adopt resume | `Lifecycle.Adopt` resume branch | Stamp loopback; pass into `resumeInTmux`. Live Adopt does not relaunch. |

User-prompt name `spawnPrompt` **is** `spawnFreeForm` in this tree.

## Handoff Markdown

`swapSystemContext` must record the preserved profile so operators and
successors can see it. Print `EffectiveNetwork` (legacy empty still shows
`loopback`):

```
Execution profile: network=loopback
```

Keep existing swap-direction / worktree / branch / repo lines. Do not put
secrets in the handoff.

## Persistence Pattern

Follow `QuotaBinding`:

- `json:"execution_profile,omitempty"`; nested `network,omitempty`
- Legacy records stay operable (empty → EffectiveNetwork loopback)
- No secrets
- **OpenAPI:** persist-only. `QuotaBinding` is not in
  `internal/daemon/apidocs/openapi.yaml`. Do **not** add `execution_profile` to
  the HTTP schema. If a later change truly requires a wire field, update
  `openapi.yaml` **first** and generate; never hand-edit generated code.

HTTP JSON that encodes `store.Session` may emit `execution_profile` via
omitempty as a side effect of the Go struct. That matches QuotaBinding and
must not trigger codegen.

## Tests The Implementer Must Land

Name these exactly (or as additional focused subtests under these names):

| Test | Package / file | Asserts |
| --- | --- | --- |
| `TestExecutionProfileRoundTripsAcrossLegacySessionBoundary` | `internal/agentstore` (sibling of `capacity_test.go`) | Agent ↔ Session `ToSession`/`FromSession` + JSON marshal/unmarshal of `ExecutionProfile`. Legacy JSON with no field → empty profile, not an error. |
| `TestLegacyRecordEmptyProfileHotSwapCursorToCodexEmitsLoopback` | `internal/lifecycle/switch_test.go` | Empty `PermissionMode`, Cursor→Codex HotSwap. Launch line contains `sandbox_workspace_write.network_access` and does **not** require `-s danger-full-access`. **This is the incident.** |
| `TestHotSwapPreservesPinnedNetworkNone` | `internal/lifecycle/switch_test.go` | Pinned `Network=none` must **not** emit loopback / `network_access=true`. Must not stamp-upgrade the record to loopback. |
| `TestHotSwapQuotaToCodexKeepsLoopback` | `internal/lifecycle/switch_test.go` | Quota HotSwap to Codex (empty or loopback profile) still emits loopback flags. Covers backend-recovery / guardian inheritance of `launchSuccessor`. |
| `TestRestoreLaunchIncludesExecutionProfile` | `internal/lifecycle/lifecycle_test.go` | Restore → `ResumeCmd` launch line includes the profile translation. |
| `TestSpawnTypedLaunchIncludesExecutionProfile` | `internal/lifecycle/lifecycle_test.go` | spawnTyped (and fork `ForkCmd` when exercised) launch line includes the profile translation. |
| `TestCodexWorkspaceWriteLoopbackDoesNotRequireDangerFullAccess` | `internal/agentbackend/backends/codex_test.go` | `Mode=workspace-write` + `Network=loopback` → `-s workspace-write` and `-c sandbox_workspace_write.network_access=true`; command must not contain `danger-full-access`. Cover Launch + Fork; Resume once `ResumeCmd` consumes Network. |
| `TestCursorLaunchEmitsSandboxNetworkFromProfile` | `internal/agentbackend/backends/cursor_test.go` | Launch (and Resume) emit `--sandbox` / network-access flags from the profile, not Cursor's no-flag default. |

Claude no-op: existing launch-string tests must remain green with
`Network=loopback` filled in (byte-identical).

## Non-Goals

- New operator CLI / MCP to set network (unless a later one-line spawn flag is
  added; this freeze does not add it)
- Changing daemon bind / auth
- Replacing or encoding network inside `PermissionMode`
- Bulk-migrating live agents beyond stamp-on-next spawn / swap / restore
- OpenAPI / `oapi-codegen` / `make gendocs`
- Site pages (no user-visible spawn flag)
- Tag / release (orchestrator-owned)
- Codex `features.network_proxy` allowlists
- Mutating `~/.cursor/cli-config.json`

## Docs (implementer)

This spec commit does **not** update product docs. The implementation job must:

- `docs/FEATURES.md` — new row: Warden-managed successors inherit
  `ExecutionProfile` (network/sandbox), orthogonal to permission mode; empty
  legacy = loopback
- `docs/agent-backends/codex.md` — `-c sandbox_workspace_write.network_access`
  mapping; `danger-full-access` stays PermissionMode-only; ResumeCmd consumes
  Mode + Network
- `docs/agent-backends/cursor.md` — `--sandbox` / network-access wired from
  profile (move out of "not mapped" / superpowers-unwired)
- `skills/warden/references/agents.md` and
  `.agents/skills/warden/references/agents.md` — successor inherits the
  execution profile (today: worktree + permission mode only)
- Site / root `FEATURES.md` coverage matrix: **skip** unless a spawn flag is added
- No tag/release

## Implementation Order (for the implementer)

1. Add `store.ExecutionProfile` + constants + `EffectiveNetwork`
2. Persist on `Agent` and `Session`; extend `ToSession` / `FromSession`; land
   round-trip test
3. Add `Network` to LaunchOpts / ResumeOpts / ForkOpts
4. Codex + Cursor translations + adapter tests
5. Lifecycle helper; wire the seven successor paths; stamp rules; adapter persist
6. HotSwap / Restore / spawnTyped tests including the Cursor→Codex incident and
   pinned-none + quota regressions
7. Handoff System Context line
8. Product docs listed above

## Frozen Shape (copy for pipeline handoff)

```go
type ExecutionProfile struct {
    Network string `json:"network,omitempty"` // loopback | full | none | ""
}
```

Empty/legacy `Network` ⇒ `EffectiveNetwork() == "loopback"`.

Successor paths: `launchSuccessor`, `Restore`, `SwitchRole`, `spawnFreeForm`
(spawnPrompt), `spawnTyped` (including fork), `SpawnJob`, Adopt resume.
