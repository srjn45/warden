# PTY runtime contract — events, state, prompts, postures

- **Status:** contract (t1 of autopilot run `01-pty-runtime-and-permission-decision-engine`). Code-backed by `internal/ptyevent` — types, enums, validation and pure decision functions only. **No runtime is implemented here.**
- **Date:** 2026-10-19
- **Consumers:** t2 PTY runtime · t3 state/prompt classifier · t4 decision router · t5 permission postures · t6 backend adapters (Antigravity first) · t7 UI/observability.
- **Rule of change:** this document, `internal/ptyevent`, and the golden fixtures under `internal/ptyevent/testdata/` change **together**. A golden diff is a contract diff (`go test ./internal/ptyevent -run . -update`, then review it).

---

## 1. Why

Today warden supervises AI CLIs by polling: a 1-tick loop runs `tmux capture-pane` for every agent, regex-classifies the visible text, and types answers back with `tmux send-keys` (§2). That has four structural problems:

1. **Silence is mistaken for state.** "No pane change for `stuckAfter`" downgrades `working → idle` (`internal/poller/poller.go:76-81`). A long tool call looks idle, and a frozen CLI looks like it's working until the timer fires.
2. **Snapshots lose the event order.** A prompt that draws and clears between two ticks is never seen; a redraw looks like activity; Warden cannot tell whether a human typed between its read and its write (TOCTOU).
3. **Permission semantics are implicit.** "Auto-approve on/off + allow/deny rules + `trust_workspace` + Fast-Brain arbiter + destructive markers" combine in code order (`poller.go:662-805`). Trust is special-cased outside the policy (`poller.go:807-838`), and `fastbrain.ClassifyPrompt` calls trust "strategic" (`internal/fastbrain/classify.go:44-46`). There's no single table a user can read.
4. **No replayable record.** Attach clients get a raw tmux PTY (`internal/daemon/attach.go:137`). Reconnecting gives them a redraw, not "what happened". Nothing is redacted at a defined boundary.

The target is an **event-driven, warden-owned PTY**. The CLI's native TUI stays untouched for humans; warden gets an ordered, redacted, replayable event journal. On top of that journal sit a **corroboration-based state machine**, a **normalized prompt contract**, and **explicit permission postures** resolved by one decision table.

Non-goals for this contract: the PTY runtime itself (t2), a terminal emulator, classifier heuristics per backend (t3/t6), API/OpenAPI shape (t7, and OpenAPI is deliberately untouched in t1).

---

## 2. Inventory of current seams

Every place the new runtime replaces, wraps or must stay compatible with. Line numbers are as of `9063945` (base of this run).

### 2.1 Process hosting and screen capture (tmux)

| Seam | Where | Notes / fate |
|---|---|---|
| `tmuxproc.Host` interface (NewSession / KillSession / HasSession / CapturePane / SendKeys) | `internal/tmuxproc/host.go:25-41` | The single seam t2 will implement a `ptyHost` behind. `Adapter.CapturePane` = `tmux capture-pane -p` (`host.go:131-133`), `SendKeys` (`host.go:136`). |
| Scrollback / extended-keys setup | `host.go:145-158` | Compat mode only. |
| Lifecycle captures pane directly (crash excerpt, digest, switch) | `internal/lifecycle/lifecycle.go:1731`, `:2843`, `:2865`; `internal/lifecycle/switch.go:264` | Replace with a journal read (§5) or a snapshot marker. |
| Snapshot captures full scrollback | `internal/snapshot/snapshot.go:147` | Replace with the journal plus the newest `MarkerSnapshot`. |
| Launch line typed via `tmux send-keys … Enter` | `lifecycle.go:2071`, `:2177`, `:2257` | Must stay < 1024 B (macOS `MAX_CANON`; see memory "launch line 1024-byte limit"). A direct PTY `exec`s argv and drops this limit for new sessions. |
| Pane-capture deps for terminal kind | `internal/daemon/terminal_poller_deps.go:44` | Plain terminals become `PresentationDirect` sessions with no classifier. |

### 2.2 Pollers

| Poller | Where | What it derives from the pane |
|---|---|---|
| Main agent poller `Poller.Run` / `tick` | `internal/poller/poller.go:1358`, `:1072`; started at `internal/daemon/server.go:170`; built at `internal/cli/daemon.go:216` | Status per agent via `classify` (`poller.go:36-83`). Pane excerpt diffing (`:1136-1160`). Loop detection (`trackLoop`, `:1408`). |
| Status classification | `poller.go:36-83` → `Backend.DetectState` (`internal/agentbackend/backend.go:186`) | working / needs_input / idle markers. Stuck downgrade from silence (`:76-81`). **This is replaced by `ptyevent.Step` (§8).** |
| Approval worker + auto-approve | `poller.go:1060` (worker), `:662-805` (`tryAutoApprove`), `:771` (`answer`), `:853` (breaker), `:879-972` (arbiter) | Becomes decision routing (§10) + TOCTOU (§11). |
| Workspace trust auto-answer | `poller.go:807-838` (`tryTrustPrompt`), toggle `SetTrustWorkspace` `:761` | Folded into the posture table as `CapTrust` (§10). |
| Rate-limit detection + limit menu | `internal/poller/detect.go:58-355`; `poller.go:974-1058` (`tryLimitMenu`); observation `internal/poller/observation.go:32-81`; scheduler `internal/daemon/ratelimit.go:101-488` | Becomes `EvLimitBanner{reason}` evidence, giving `waiting{rate_limit|quota}`. The scheduler keeps consuming observations. |
| Context-fill / compaction | `internal/poller/context.go:337` | Reads the journal head instead of the pane. |
| Crash triage excerpt | `internal/poller/crash.go:88` | Reads the journal tail (already redacted). |
| Terminal watcher (plain shells) | `internal/poller/terminal_watcher.go:41-125`; started `server.go:177` | Replaced by lifecycle events. |
| Hot-swap thresholds | `internal/lifecycle/poller.go:57-123` | Unchanged (it consumes usage, not the pane). |

### 2.3 Prompt recognition, known prompts, Fast-Brain

| Seam | Where | Notes |
|---|---|---|
| Neutral approval type | `agentbackend.Approval` `internal/agentbackend/backend.go:81-104`; `ApprovalKindTrust` `:107` | Becomes `ptyevent.Prompt` (§9). Mapping in §9.7. |
| Per-backend parsers | `Backend.ParseApproval` `backend.go:187`; e.g. Claude `internal/agentbackend/backends/claude.go:338-410` (incl. trust `:383`); Antigravity `antigravity.go:425-570` (trust `:545`) | Adapters keep these and **emit `Prompt`** with capabilities/semantics (t6). |
| Generic menu locator | `internal/agentbackend/menu.go:85-178` (`FindMenu`, `LocateOptions`) | Basis for `Prompt.Verified`. |
| Keystroke delivery + re-parse guard | `internal/agentbackend/answer.go:24-60` | A weak TOCTOU check (same option labels). Superseded by `Revalidate` (§11). |
| Claude-shaped parser + destructive markers | `internal/approval/approval.go:81-106` (`IsDestructive`), `:108` (`Parse`) | Markers become a `CapDestructive` hint for adapters. Policy no longer string-matches. |
| Allow/deny rules, breaker | `internal/approval/policy.go:15-160`; `breaker.go:27-95` | Rules become the `AllowlistMatch` input to `Decide`. The breaker stays as a router guard. |
| Deterministic prompt category | `internal/fastbrain/classify.go:40-60` (`ClassifyPrompt`) | **Behavior change:** trust leaves "strategic" and becomes a permission with `CapTrust`. |
| Fast-Brain recognition | `internal/fastbrain/recognize.go:25-95`; poller glue `internal/poller/recognize.go:75-402` | Becomes `ProvFastBrain` readings, confident only when `Verified` and ≥ 0.95. |
| Learned known prompts | `internal/knownprompts/store.go:53-470`, template `template.go:25`; poller glue `internal/poller/learn.go:64-232` | Becomes the `LearnedShape` lifecycle (§9.6). Existing entries import as `candidate`. |
| Brain consult for strategic prompts | `internal/poller/promptchain.go:74-252` | The `route_decision` target (§10). |

### 2.4 Input paths

| Seam | Where |
|---|---|
| Prompt seeding after launch (`PromptSeeder`, `ReadyMarker`, `InputReadiness`) | `backend.go:208-226`; `lifecycle.go:2758-2800` (`seedInteractivePrompt`), `:2804` (`deliverSeed`); readiness markers e.g. `backends/aider.go:94`, `crush.go:126`, `goose.go:132`, `codex.go:731` |
| Generic key send | `lifecycle.go:2714` (`SendKeys`) |
| Approve via API | `internal/daemon/strict_core.go:319` (`ApproveSession`), `:285` (`ListApprovals`), parse `:64` |
| Auto-approve policy API | `internal/daemon/strict_approval.go:15-40`; per-agent toggle `strict_lifecycle.go:670` |
| Attach WS PTY bridge (keystrokes + resize) | `internal/daemon/attach.go:137-218`; resize message `:51-72`; routes `internal/daemon/api.go:668-669` |

### 2.5 Streams to clients

| Seam | Where |
|---|---|
| SSE session/tree snapshots (`GET /api/v1/events/stream`) | `internal/daemon/sse.go:29`; hub `internal/daemon/hub.go:7-40`; route `api.go:667` |
| TUI / CLI watch client | `internal/client/client.go:310-324` (`Watch`) |
| Per-agent and cockpit attach (raw PTY over WS) | `attach.go:74` (`handleAttach`), `:105` (`handleCockpitAttach`), `:137` (`bridgeTmux`) |

### 2.6 Transcripts and lifecycle

| Seam | Where |
|---|---|
| Transcript location/parse | `Backend.TranscriptPath` / `ParseTranscript` `backend.go:182-183`; consumers `internal/daemon/digest_routes.go:78-81`, `internal/lifecycle/switch.go:321`, `internal/digest/parse.go:38`, `internal/ctxtokens/*` |
| Session lifecycle | `lifecycle.go:1833` (`Spawn`), `:2333` (`Restore`), `:2246` (`RestoreTerminal`), `:2582` (`Terminate`) |
| Session statuses | `internal/store/types.go:79-86` (spawning, working, waiting_for_input, idle, done, errored, orphaned, rate_limited) |
| Backend capability seams | `backend.go:145-530` (Caps, PromptSeeder, InputReadiness, RateLimitDetector, QuotaScopeObserver, SessionIDDiscoverer, …); permission intents `internal/agentbackend/permission.go:10-40` |
| Adapters | `internal/agentbackend/backends/{claude,codex,antigravity,cursor,aider,opencode,crush,goose}.go`; pane fixtures `backends/testdata/<backend>/` |

---

## 3. Architecture in one picture

```
            ┌──────────── warden-owned PTY (t2) ─────────────┐
 CLI proc ──┤ master fd ──read──► SplitUTF8 ─► Redactor ──┐  │
            │      ▲                                     ▼  │
            │      └──write◄── Arbitrate(lease) ◄── Input  ControlScanner
            └──────┬───────────────────────────────────────┬─┘
                   │ raw bytes (unredacted, live only)     │ Event{seq,epoch,mono,…, redacted}
                   ▼                                       ▼
        attached clients (TUI/web/app/tmux)        per-session JOURNAL (append-only)
                                                          │
                     ┌──────────────┬───────────────────────┼──────────────┐
                     ▼              ▼                       ▼              ▼
              terminal model   classifiers (t3/t6)   SSE / Cockpit (t7)  replay/reconnect
              (screen state)  Evidence→Step()→State    state+prompt        PlanResume()
                                Prompt→Decide()→Decision→Revalidate()→Input(warden)
```

- **One writer per session** assigns `Seq` and owns the journal. Everything else is a reader.
- The live attach stream is the only consumer of **unredacted** bytes, because it *is* the user's terminal. Everything that persists or fans out beyond it reads the redacted journal.
- Classification runs **on events**: output, input, lifecycle, hook, transcript append, plus one coarse silence timer. It never runs on a capture-pane poll.

---

## 4. Event schema (`internal/ptyevent/event.go`)

`Event{v, session_id, epoch, seq, mono_ns, wall, kind, <one payload>, redactions[], redacted}`

| Field | Rule |
|---|---|
| `v` | `SchemaVersion` (=1). An incompatible change bumps it, and the reader keeps a decoder for the previous version. |
| `session_id` | Warden session id (not the tmux name). |
| `epoch` | Runtime incarnation: +1 on each spawn **and** each daemon re-adoption of a live process. |
| `seq` | Starts at 1 and goes **+1 per event for the session's whole life**. It does *not* reset per epoch. It is the only ordering key. |
| `mono_ns` | Monotonic clock relative to the epoch start; non-decreasing within an epoch. `wall` is informational. |
| `redacted` | Must be `true` before an event is persisted or fanned out (`ErrNotRedacted`). |

**Kinds and payloads** (exactly one payload, matching `kind`):

| Kind | Payload | Notes |
|---|---|---|
| `output` | `Output{data, controls[], partial}` | Raw PTY bytes after redaction. Cut on UTF-8 boundaries (`SplitUTF8`, holding back at most 3 bytes; `partial` = forced flush). `controls` summarizes alt-screen, clear, title, bell, bracketed paste, cursor show/hide, and generic CSI/OSC/ESC/DCS (`ControlScanner`; an unterminated sequence over 4096 B → `aborted`). |
| `input` | `Input{data, origin, client_id, decision_id, secret, secret_len}` | `origin ∈ {human, operator, warden}` (§7). Secret input carries **no data**, only its length. |
| `resize` | `Resize{cols, rows, client_id, origin}` | Non-zero. Last-writer-wins on the PTY (today's tmux `window-size latest`). |
| `lifecycle` | `Lifecycle{phase, pid, exit_code, signal, backend, presentation, client_id, detail}` | `phase ∈ spawned, ready, exited, signaled, crashed, recovered, attached, detached`. `presentation ∈ direct, tmux`. |
| `state` | `StateChange{from, to, reason, evidence[]}` | Only committed transitions, never candidates. `evidence` is non-empty. |
| `prompt` | `PromptEvent{phase: observed/updated/cleared, prompt, fingerprint}` | `observed` / `updated` carry a valid `Prompt`. |
| `decision` | `DecisionEvent{id, fingerprint, posture, action, option_index, rule, reason, decided_at_seq, revalidation}` | One event when decided. The revalidation code is recorded before delivery. |
| `marker` | `Marker{type: gap/snapshot/trim/reset, from_seq, to_seq, snapshot, cols, rows}` | Journal-level. |

**Pipeline order inside the writer:** `read → SplitUTF8 → Redactor.Feed → ControlScanner.Feed(redacted) → Event`. Control offsets therefore refer to redacted bytes. Placeholders (`[REDACTED:<kind>]`) contain no control bytes.

**Stream rules** (`ValidateStream`, ids are stable):

| Rule id | Contract |
|---|---|
| `event-invalid` | Each event passes `Event.Validate`. |
| `single-session` | A stream is one session. |
| `seq-duplicate` / `seq-contiguous` | Strictly +1. The **only** legal hole is the range named by an immediately preceding `marker{gap, from_seq=seq+1, to_seq}` (`gap-marker-covers-hole`). |
| `epoch-monotonic` / `epoch-start` | The epoch never decreases. The first event of a new epoch is `lifecycle{spawned|recovered}`. |
| `mono-monotonic` | `mono_ns` never decreases within an epoch. |
| `no-io-after-exit` | After `exited`/`signaled`/`crashed`, no output or input until the next epoch. |

Consumer ordering (`CheckNext`): apply `next`, **silently drop `duplicate`** (reconnect overlap is expected), and on `gap` stop applying and resynchronize (§6). `foreign` and `regression` are protocol errors.

---

## 5. Persistence, retention, replay, redaction boundary

**Journal.** One append-only log per session holds the redacted `Event`s (JSONL wire format pinned by `testdata/streams/*.jsonl`). The store backend is t2's choice: a ScrivaDB collection, or segment files under the data dir referenced from it. The single writer appends synchronously before fan-out, so **a client never sees an event the journal does not have**.

**Snapshots.** The writer runs a terminal model (t2). Every `N` output bytes (default 256 KiB), every alt-screen exit, and on clean detach, it emits `marker{snapshot, to_seq, snapshot, cols, rows}`, a redacted rendered screen. Replay can start from the newest snapshot instead of from seq 1.

**Retention** (generous defaults, configurable — guards fire only at extremes):

| Knob | Default | Effect |
|---|---|---|
| `journal.max_bytes_per_session` | 64 MiB | Beyond it, the oldest events up to the oldest retained snapshot are dropped and a `marker{trim, to_seq}` is appended. |
| `journal.max_age` | 14 days after session end | The whole journal is deleted with the session's archive. |
| `journal.keep_snapshots` | 8 | Older snapshots are trimmed with their events. |

`OldestSeq` is always `≥ 1` and always directly follows a retained snapshot or is seq 1. A trim never leaves a cursor-addressable hole: the snapshot covers it.

**Gap markers.** If the writer cannot keep up (bounded queue overflow), it drops output and appends `marker{gap, from_seq, to_seq}` *at the position of the hole*, then forces a snapshot. A gap is never silent.

**Redaction boundary.**

- **Unredacted:** only the live PTY → attached-client stream (the user's own terminal, preserving native UX), and in-memory buffers of the deterministic parser on the writer's goroutine.
- **Redacted (mandatory):** the journal, snapshots, SSE/Cockpit/TUI state, prompt `Evidence.Raw`, every Fast-Brain/brain input, logs, crash triage, digests, and anything sent off-node (hub, notifications).
- `Redact` is the single routine. Typed kinds: `anthropic_key, openai_key, github_token, aws_access_key, google_api_key, slack_token, jwt, bearer_token, url_credential, secret_assignment, private_key, typed_input`. Contextual kinds keep their key visible (`Bearer [REDACTED:bearer_token]`, `https://bob:[REDACTED:url_credential]@host`).
- Streaming (`Redactor`): it withholds the trailing partial line (after the last `\n`/`\r`, ≤ 512 B; if longer, it cuts after the last whitespace), never cuts through a complete match, and holds a dangling `Bearer ` / `password: ` prefix together with its value. An open `-----BEGIN … PRIVATE KEY-----` block is held for up to 64 KiB, and on overflow or flush it is **redacted to the end (fail closed)**. The runtime flushes on a short idle timer (≈100 ms) so the journal lag stays small; the live stream never waits.
- **Typed secrets (input).** Input is secret when `SecretInput(tty, credentialPrompt)`: canonical mode with echo off (classic `getpass`), **or** a credential prompt is on screen (API-key entry, OAuth paste-code box). Echo-off alone is *not* a signal: every raw-mode AI CLI TUI runs with echo off. Secret input stores `secret=true, secret_len`, never bytes. Non-secret input still goes through `Redact` (pasted tokens).
- Reconnect therefore shows `[REDACTED:…]` where a secret scrolled past while the client was away. That is intended.

---

## 6. Reconnect / attach protocol

Transport (t2/t7): one WebSocket per attach, a superset of today's `GET /api/v1/sessions/{id}/attach`.

1. Client → `hello{session_id, cursor?: {session_id, epoch, seq}, cols, rows, mode: "interactive"|"observe"}`.
2. Server → `PlanResume(cursor, window)`:

| Mode | When | Server sends |
|---|---|---|
| `tail` | cursor == head | live events from head+1 |
| `replay` | cursor in retention | events `cursor.seq+1 … head`, then live |
| `snapshot` | fresh attach with a snapshot; cursor fell out of retention; cursor predates a `reset` marker | newest snapshot (client **resets** its terminal model), events `snapshot.to_seq+1 …`, live |
| `reject` | cursor for another session; cursor **ahead** of head (journal lost in a crash); unrecoverable with no snapshot | `reject{reason}`. The client discards state and re-attaches fresh. |

Fresh attach with no snapshot → `replay` from `OldestSeq`.

3. **Replay–live handoff without loss.** The server subscribes to the live feed *before* reading the journal and buffers live events while replaying. Overlap is resolved by `CheckNext` (duplicates dropped). Any `gap` during the session → the client re-sends `hello` with its cursor.
4. **Interactive clients** get raw unredacted bytes for **live** events and redacted bytes for **replayed** ones. Observe-mode clients (dashboards, phone previews over the relay) get redacted bytes only.
5. **Client → server:** binary frames are human input (`origin=human`, `client_id` = this socket). Text frames carry `resize{cols, rows}` (today's `{"type":"resize"}`, `attach.go:51`) and `ack{seq}`, an optional cursor checkpoint the server stores per client for "resume where I left off".
6. **Crash recovery.** On daemon restart the runtime re-adopts live processes (compat: the tmux server keeps them) and opens epoch+1 with `lifecycle{recovered}`. If the screen is unknown it emits `marker{reset}` and then a fresh snapshot. Clients behind the reset get `snapshot`. The state machine restarts at `unknown` and needs fresh evidence (§8). It never re-asserts a pre-crash state from memory.

---

## 7. Input ownership (`internal/ptyevent/input.go`)

Origins: `human` (keyboard in an attached client), `operator` (explicit API/CLI/MCP send on someone's behalf: `send_to_agent`, `wd send`, orchestrator messages), and `warden` (automatic: a decision answer, a prompt seed, a rate-limit resume). Warden input **must** cite a `decision_id` or a `reason`.

`Arbitrate(lease, request, cfg)`:

1. **Human input is always written immediately** and takes the lease. If warden or an operator held it, `PreemptsWarden=true`, and the router cancels pending decisions (their `Revalidate` fails on `HumanSeq` anyway).
2. Malformed automatic input → `reject`.
3. Automatic input is **deferred** while a human holds the lease: within `HumanQuiet` (3 s) of their last keystroke, **or** while they have a half-typed line open. Enter closes the line; Ctrl-C, Ctrl-U and a lone Esc clear it; arrow keys don't open it. A warden write deferred past `MaxDefer` (30 s) is **abandoned** and re-decided from fresh evidence. Operator writes simply wait.
4. Otherwise → `write`. `Release` frees the line when the write completes.

The lease's `HumanSeq` (journal seq of the last human input) feeds TOCTOU (§11). Resize uses no lease (last writer wins), but every resize is journaled with its origin.

---

## 8. State machine (`internal/ptyevent/state.go`)

States: `unknown` (no corroborated evidence yet) · `working` · `idle` · `waiting{reason}` with `reason ∈ startup, rate_limit, quota, auth, network, subprocess` · `waiting_for_permission` · `waiting_for_decision` · `needs_attention` · `exited`.

### 8.1 Evidence

| Evidence | Class | Source (who emits it) |
|---|---|---|
| `process_spawn`, `process_exit` | **strong** | runtime lifecycle |
| `ready` | **strong** (only from `unknown` / `waiting{startup}`) | adapter `ReadyMarker` / `InputReady` on the screen model |
| `hook{state, reason}` | **strong** | backend-native hooks (e.g. Claude Stop / Notification) where an adapter wires them |
| `working_marker` | positive | adapter busy marker ("esc to interrupt", spinner line) |
| `idle_marker` | positive | adapter input-box-at-rest marker |
| `prompt{Prompt}` | positive | classifier (§9) |
| `limit_banner{reason}` | positive | adapter `RateLimitDetector` etc. A banner without a typed reason is ignored. |
| `uncertain` | positive | classifier saw a blocking screen it cannot read |
| `output_activity`, `input`, `prompt_cleared`, `turn_start`, `turn_end`, `silence{quiet}` | **corroborating only** | runtime / transcript reader / silence timer |

**Never from silence alone.** `silence`, `output_activity` (a redraw or clock), `input`, `prompt_cleared` and transcript turn boundaries cannot move the state by themselves. They only corroborate a screen, hook or process signal. Silence, however long, holds the current state. A stalled CLI shows up as an advisory "quiet for X" in t7, never as a state flip. This deliberately drops today's `stuckAfter` downgrade (`poller.go:76-81`).

### 8.2 Transitions (`propose`, in priority order)

| # | Evidence in the observation | → Target | Commit |
|---|---|---|---|
| 1 | `process_exit` | `exited` | immediate |
| 2 | `process_spawn` | `waiting{startup}` | immediate |
| — | (current is `exited`: nothing else applies) | — | — |
| 3 | `hook{s}` | `s` | immediate |
| 4 | `ready` while `unknown`/`waiting{startup}` | `idle` | immediate |
| 5 | `uncertain` | `needs_attention` | debounced |
| 6 | `prompt` not `Confident` | `needs_attention` | debounced |
| 6 | `prompt` permission | `waiting_for_permission` | debounced |
| 6 | `prompt` decision | `waiting_for_decision` | debounced |
| 7 | `limit_banner{r}` with a typed `r` | `waiting{r}` | debounced |
| 8 | `working_marker` | `working` | debounced (1 hit) |
| 9 | `output_activity` + (`input` or `prompt_cleared` or `turn_start`) | `working` | debounced (1 hit) |
| 10 | `idle_marker` + (`silence ≥ IdleQuiet` or `turn_end`) | `idle` | immediate |
| 10 | `idle_marker` alone | `idle` | debounced |
| — | anything else (incl. silence alone) | no proposal | — |

### 8.3 Debounce / hysteresis (`DefaultHysteresis`)

A non-immediate proposal commits once it has been proposed in **`MinHits` distinct observations spanning ≥ `Dwell`**, with no contradicting proposal in between. A proposal equal to the current state voids the pending candidate. An observation with no proposal (silence) neither advances nor resets it. A candidate not re-proposed within `PendingTTL` (30 s) expires.

| Target | Dwell | MinHits |
|---|---|---|
| working | 0 | 1 |
| idle | 1.5 s | 2 |
| waiting | 1 s | 2 |
| waiting_for_permission / waiting_for_decision | 200 ms | 2 |
| needs_attention | 2 s | 2 |

`IdleQuiet` = 5 s. Every committed transition is journaled as `state{from, to, reason, evidence}` and lists exactly the evidence that justified it.

### 8.4 Mapping to today's `store.Status`

`working→working`, `idle→idle`, `waiting{rate_limit|quota}→rate_limited`, `waiting{startup}→spawning`, `waiting_for_permission|waiting_for_decision|needs_attention→waiting_for_input` (with a new sub-field), `exited→done|errored` by exit code, compat `orphaned` unchanged. t7 adds the finer states to the API additively. Existing clients keep reading the old enum.

---

## 9. Normalized prompt contract (`internal/ptyevent/prompt.go`)

### 9.1 Shape

```
Prompt{
  fingerprint, shape_version, instance_key, backend, backend_version,
  class: permission|decision|unknown,
  capabilities: [read|write|execute|network|credential|publish|destructive|trust|other],
  subject, target, target_in_workspace, question,
  options: [{index, label, key, semantic, scope, default}],
  cancel_path: {key?, option_index?}, navigate,
  provenance: parser|learned|fastbrain, confidence, verified, learned_shape_id,
  evidence: {raw (redacted), redactions, from_seq, to_seq}
}
```

- **`fingerprint`** = hash(shape_version, backend, `Template(question)`, `Template(option)…`). `Template` folds quoted strings, call arguments, paths, numbers, case and whitespace. It is **stable across instances** (another command in the same `Bash(...)` box) and **changes under shape drift**: a reworded question, an added, reordered or relabelled option, a new shape version, another backend. Changing `Template` re-keys every learned shape, so bump `shape_version`.
- **`instance_key`** = hash(fingerprint, subject, target). It separates "same box, different command", which is the TOCTOU check.
- **`subject` / `target`**: who asks (Bash, Edit, WebFetch, workspace) and what it acts on (command, path, URL, directory), redacted. `target_in_workspace` is computed by the adapter against the session worktree.
- **Evidence** is the redacted excerpt plus the journal range it was rendered from, so any decision can be audited back to bytes.

### 9.2 Capability taxonomy

`read` · `write` · `execute` · `network` · `credential` (keychain, tokens, login) · `publish` (push, release, deploy, post) · `destructive` (delete, overwrite, force, reset, drop) · `trust` · `other`.

**Workspace/directory trust IS a permission** (`CapTrust`). Trusting a folder makes the CLI load that folder's hooks, MCP servers and settings, which is code execution by proxy. A prompt may carry several capabilities (`git push` = execute+network+publish). A posture must permit **all** of them. `other` means "permission, capability unmapped". Postures below dangerous route it to a human.

### 9.3 Options

`semantic ∈ allow_once, allow_session, allow_always, deny, deny_feedback, cancel, choice, freeform`. `scope ∈ once, session, project, workspace, global`. `default` = what Enter selects now (at most one). `cancel_path` = how to back out without choosing (a key and/or an option index). Auto-deny picks `deny` ≻ `deny_feedback` and **never** `cancel`, since cancel aborts the turn or exits the CLI and that is a human's call.

### 9.4 Permission vs decision

- **Permission:** asks to grant an authority for an action the agent already chose. It is answerable by posture.
- **Decision:** strategic or multiple-choice (which approach, plan approval, "Type something else", free text). It is **never answered by a posture**, including `dangerously-execute-all`. It routes to the decision router (brain consult → human) and the state is `waiting_for_decision`.
- **Unknown:** the classifier cannot tell → `needs_attention`.

### 9.5 Provenance and confidence

| Provenance | Confident when |
|---|---|
| `parser` (adapter, deterministic) | `confidence ≥ 0.9` (adapters report a degraded match below it) |
| `learned` | the shape is `Usable` **and** `verified` on screen **and** `confidence ≥ 0.9` |
| `fastbrain` | `verified` (every option label located on the live screen) **and** `confidence ≥ 0.95` |

`Confident(p, learnedUsable)` also requires `Validate()` to pass and `class ≠ unknown`. Screen text is data, not instructions. Fast-Brain claims are only ever *verified against* the screen, never trusted on their own.

### 9.6 Learned-shape lifecycle (`internal/ptyevent/learned.go`)

```
candidate ──Validate──► validated ──Pin──► versioned ──(expires)──► Pin again (version+1)
    └─────────────────────── Retire (any time, irreversible) ───────────────► retired
```

- **candidate:** proposed from a Fast-Brain reading. It may help *recognition*, but no decision is ever taken on it (it's not `Usable`, so `Confident` fails → `needs_attention`). Today's `knownprompts` entries import here.
- **→ validated** requires ≥ 3 consistent, verified observations, **and** confirmation by a human answer or a deterministic parser (never self-confirmed), **and** ≥ 1 regression fixture (redacted screen + expected `Prompt`, replayed in CI).
- **→ versioned** pins the CLI version, the scope (`session` / `project` / `user`) and an expiry (≤ 30 days). Only `versioned`, unexpired, same-backend, same-CLI-version, in-scope shapes are `Usable`. A CLI upgrade makes a shape unusable until it is re-validated (format drift).
- **retired:** on a breaker trip, a revalidation failure from option drift, or a human contradicting it. A re-learn starts a new candidate.
- **Never blind approval:** a learned shape only *classifies*. `Decide` still applies the posture.

### 9.7 Mapping from `agentbackend.Approval`

`Action→target` (with the tool name split into `subject`), `Question→question`, `Options[i]→options[i].label`, `SelectedIdx→default`, `AffirmativeIdx/AffirmativeSticky→`the semantic of that option (`allow_once` or `allow_always`), `Kind=="trust"→capabilities=[trust]`, `Navigate→navigate`, `Inferred→provenance=fastbrain`. Adapters (t6) add the capabilities and the remaining option semantics. `approval.IsDestructive` markers become a `destructive` capability hint.

---

## 10. Postures and decision tables (`internal/ptyevent/posture.go`)

Postures: `read-only` · `read-write` · `allowlisted` · `dangerously-execute-all`. Actions: `approve(option)` · `deny(option)` · `ask_human` (understood permission; approvals inbox; state stays `waiting_for_permission`) · `route_decision` · `needs_attention`.

### 10.1 Global order (`Decide`)

1. Invalid posture / no prompt / class unknown / **not confident** → `needs_attention` (fail closed).
2. Class `decision` → `route_decision` in **every** posture.
3. `dangerously-execute-all` → `approve` every confident permission, including trust, using the extracted allow option. It is deliberately not constrained by legacy allow/deny policy.
4. Allowlist **deny** match → `deny` (with a refusal option), else `ask_human`.
5. All capabilities permitted → `approve` with the **least-privilege allow option**: `allow_once` ≻ `allow_session` ≻ `allow_always`. Standing grants are allowed only if `AllowStanding` (today's `allow_sticky`), or the posture is dangerous, or the prompt is trust-only (trust is inherently standing). Otherwise `ask_human` (`standing-grant-not-allowed` / `no-least-privilege-allow`).
6. Not permitted → `read-only`: `deny` (`posture-forbids`), or `ask_human` for trust/other or when no refusal option exists. Other postures: `ask_human`.

### 10.2 Posture × capability ("✓" = pre-approved)

| Capability | read-only | read-write | allowlisted | dangerously-execute-all |
|---|---|---|---|---|
| read | ✓ | ✓ | ✓ | ✓ |
| write | **deny** | ✓ | ✓ if allowlist allows, else human | ✓ |
| execute / network / credential / publish | **deny** | human | ✓ if allowlist allows, else human | ✓ |
| destructive | **deny** | human | **human even if allowlisted** | ✓ |
| trust (own worktree) | human | ✓ | ✓ | ✓ |
| trust (foreign dir) | human | human | human | ✓ |
| other | human | human | human | ✓ |

The full prompt-fixture × allowlist × posture matrix (~270 rows) is pinned in `internal/ptyevent/testdata/decisions.golden.txt`. Invariants are asserted independently of the golden (`TestDecisionMatrix`):

- approve ⇒ confident, permission class, no deny rule, an allow-semantic option; never in read-only beyond `read`; never destructive outside dangerous;
- deny ⇒ a refusal-semantic option;
- unconfident ⇒ `needs_attention`;
- confident decision ⇒ `route_decision`;
- **dangerously-execute-all approves every confident permission regardless of allow/deny rules (trust included), and nothing else.**

### 10.3 Mapping today's configuration (t5)

Posture says *what may be decided*. Whether Warden *writes* the decision is a separate per-session `act` switch: today's `auto_approve.enabled` OR the per-session opt-in OR autopilot ownership (`poller.go:668-673`). With `act=false` the router still runs `Decide`, but it surfaces every `approve`/`deny` as `ask_human`, with the decided action attached as a suggestion. That is exactly today's "auto-approve off" behavior. Posture mappings: `auto_approve` rules → `allowlisted`; `trust_workspace` → own-worktree trust in `read-write`/`allowlisted`; claude `bypassPermissions` / agy `dangerously-skip-permissions` (`IntentSkipAll`, `permission.go:16`) → `dangerously-execute-all`; `IntentAcceptEdits` → `read-write`; `IntentReadOnly`/`IntentPlan` → `read-only`. The circuit breaker and `UseFastBrain` arbiter stay as **router** guards after `Decide`. They may downgrade an `approve` to `ask_human`, never upgrade.

---

## 11. TOCTOU revalidation (`internal/ptyevent/toctou.go`)

Right before writing the keystrokes of an `approve`/`deny`, the router calls `Revalidate(pending, live, maxAge)` against the **live** journal head. All of these must hold, else the decision is dropped and re-decided from fresh evidence (never retried blindly):

| Code | Check |
|---|---|
| `not-actionable` | the action is approve/deny |
| `stale` | age ≤ `maxAge` (30 s) |
| `human-intervened` | `lease.HumanSeq ≤ decided_at_seq` |
| `state-changed` | state is still `waiting_for_permission` |
| `prompt-gone` | a prompt is on screen |
| `prompt-changed` | same `fingerprint` |
| `target-changed` | same `instance_key`/subject/target (**same box, different command**) |
| `option-moved` | the chosen option still exists at the same index with the same label and semantic |

Navigate menus recompute their cursor moves from the live prompt's `default`. Delivery then goes through `Arbitrate` (§7). A `defer` past `MaxDefer` abandons the decision. The result is journaled in `DecisionEvent.revalidation`.

---

## 12. Migration strategy — tmux becomes presentation/compat only

| Phase | What changes | tmux role |
|---|---|---|
| **M0** (this PR) | Contract package and spec. No behavior change. | unchanged |
| **M1** (t2) | `ptyHost` implements `tmuxproc.Host` for **new** sessions behind `runtime.pty: off|shadow|on` (default `off`). In `shadow`, warden spawns inside tmux as today but feeds the journal from a `tmux pipe-pane -O` / control-mode stream. It runs `Step`/`Decide` **in parallel** with the poller, logs disagreements, and writes nothing. | process host + presentation |
| **M2** (t3–t5) | Classifier + decision router + postures become authoritative in `on`. The poller tick no longer captures panes for those sessions (it keeps them for legacy ones). Approvals API reads `Prompt`s. | presentation for `tmux attach` users |
| **M3** (t2/t7) | `direct` presentation: warden owns the PTY master. Attach WS speaks the §6 protocol. The cockpit/TUI renders from the journal. `tmux attach` remains available via an opt-in `presentation: tmux` per session. | opt-in compat |
| **M4** | Legacy sessions (spawned before M1) stay operable for their lifetime via the poller path. They migrate on next restart/relaunch. The poller retires once no legacy session remains. | legacy only |

Compat guarantees: existing sessions and their tmux names keep working (attach, send, approve, kill) throughout. `store.Status` keeps its values (§8.4). The launch-line 1024 B rule keeps applying to tmux-hosted sessions. `orphaned`/restore semantics (`lifecycle.go:2333`) stay until M4. The direct runtime re-adopts processes on daemon restart (§6). Under tmux compat, the tmux server is what keeps processes alive across daemon restarts, exactly as today.

---

## 13. Contract tests and fixtures

`go test ./internal/ptyevent` (all table-driven):

| Area | Test | Fixtures |
|---|---|---|
| Event validation | `TestEventValidate` | — |
| Ordering, gaps, duplicates, epochs, crash recovery, no-io-after-exit, redaction gate | `TestStreamFixtures`, `TestCheckNext` | `testdata/streams/*.jsonl` (golden wire format) |
| Reconnect with cursor, retention, reset, journal loss | `TestPlanResume`, `TestReconnectReplayConverges` | — |
| Partial UTF-8 across chunks (every cut) | `TestSplitUTF8`, `TestSplitUTF8EveryCut` | — |
| Terminal control sequences (alt-screen, OSC title BEL/ST, DCS, CAN abort, ESC restart, split sequences, overlong) | `TestControlScanner*` | — |
| Redaction of typed secrets, cross-chunk at every byte cut, fail-closed key blocks, typed input | `TestRedact*`, `TestSecretInput` | built at runtime (no token-shaped literals in the repo) |
| Concurrent human/Warden input ownership | `TestInputOwnership`, `TestHumanLineOpen` | — |
| State machine incl. never-from-silence, hysteresis, trust → permission, multi-choice → decision, crash/respawn | `TestStateMachine`, `TestStepReportsEvidence` | — |
| Prompt validity, confidence, adversarial format drift (reword/reorder/relabel/injection-shaped labels) | `TestPromptValidate`, `TestConfident`, `TestFingerprintDrift` | `testdata/prompts/*.json` (incl. Antigravity trust from the real pane fixture) |
| Posture × classification matrix + invariants | `TestDecisionMatrix`, `TestDecideEdgeCases` | `testdata/decisions.golden.txt` |
| TOCTOU | `TestRevalidate` | — |
| Learned-shape lifecycle | `TestLearnedShapeLifecycle` | — |

Adapters (t6) extend the corpus with `testdata/prompts/<backend>_*.json` produced from their pane fixtures. Learned shapes add regression fixtures in the same format.

---

## 14. Hand-off notes per task

- **t2 runtime:** implement `tmuxproc.Host` + journal writer + §6 transport. The writer must emit only events that pass `Validate`, and streams must pass `ValidateStream`. Use `SplitUTF8 → Redactor → ControlScanner` in that order. Flush the redactor on a ~100 ms idle timer. Read termios for `SecretInput`.
  - *t2-1 (landed as `internal/ptyruntime`, library only):* `Session` (direct PTY host, writer pipeline, termios-gated input, `Arbitrate`, resize, exit/teardown), `Journal` (JSONL segments, each after the first starting at a snapshot marker so retention drops whole segments; torn-tail recovery), `Subscribe`/`Attach` fan-out (redacted replay→live, unredacted live-only; overflow cuts the subscriber with `ErrResync`), and `Host` (a `tmuxproc.Host` over sessions). Snapshot markers carry `to_seq = seq-1`, so `PlanResume`'s `FromSeq` is the snapshot event itself. Dropped output is journaled as a one-seq hole (`marker{gap, from=to=seq+1}`) followed by a forced snapshot.
  - *t2-2 (landed as `internal/ptyhost` + daemon wiring):* `runtime.pty: off|shadow|on` (default `off`, hot-reloaded, applies to sessions created afterwards; `off` stops mirrors). `ptyhost.Manager` owns the runtime: `Host()` is the routed `tmuxproc.Host` lifecycle now uses for every pane (create → tmux, mirrored in shadow/on; every other op → whichever host owns the name), `TerminalHost()` creates `kind=terminal` panes on a direct PTY in `on` (journals under `<data>/pty/direct/<id>`). **Shadow** = `tmux pipe-pane -O` into a FIFO read by `ptyruntime.Mirror` (same writer core as `Session`: redaction, controls, screen, snapshots, fan-out; `<data>/pty/shadow/<id>`), observe-only, reaped by a 2 s reconcile when the pane dies (`exited{detail:"tmux session ended"}`), re-adopted at daemon start as epoch+1 `recovered` + `reset` + a capture-pane-seeded snapshot. Agents stay in tmux in `on` (mirrored). **Direct-terminal restart:** the shell dies with the daemon (it owns the master); startup recovery re-runs `RestoreTerminal` for each running terminal with a direct journal → `crashed` (if the exit was never journaled) / `spawned` + `reset` + snapshot. **Transport:** `GET /api/v1/sessions/{id}/pty` (excluded from codegen like `/attach`; streaming path; read-only tokens allowed but forced observe-only). Frames per §6; `plan.head` is the replay/live boundary interactive clients switch from event data to raw binary frames at. `/attach`, session input and output bridge to a direct terminal. Known gap: a direct terminal has no tmux session, so the TUI cockpit's tmux attach of it is unavailable in `on` (t7).
  - *What t3 consumes:* `Manager.Observe(func(name string, ev ptyevent.Event))` — one consumer per runtime-backed session tailing from the head at start, in seq order, resubscribing on `ErrResync` (overlap dropped); runs on the consumer goroutine, must not block. `Manager.Lookup(name)` → `Target{Stream (Subscribe/Journal/Epoch), Direct *ptyruntime.Session (nil for shadow), Presentation}`; `Stream.Subscribe(cursor)` for replay; `Direct.Emit` (or `Mirror.Emit`) journals `state`/`prompt`/`decision`; `Direct.Lease()` / `Input(Write{Origin: warden, DecisionID})` for t4. The daemon's observer (`Server.onPTYEvent`) today only refreshes SSE on lifecycle events and CASes an exited direct terminal to `orphaned` exactly like the terminal watcher (no double transition); pollers remain authoritative for every session until t3 runs `Step` on these events (shadow: log disagreements, write nothing).
- **t3 classifier:** produce `Observation`s on events and fold them with `Step(DefaultHysteresis)`. Emit `Prompt`s that `Validate`. Never emit transitions from silence.
- **t4 router:** `Decide → Revalidate → Arbitrate → write`, journaling `DecisionEvent`. The breaker and arbiter may only downgrade.
- **t5 postures:** the config/API surface for `Posture` per session/role, plus the §10.3 mapping.
- **t6 adapters (Antigravity first):** map the parser output to `Prompt` with capabilities, semantics, cancel path and `target_in_workspace`. Add fixtures.
- **t7 UI:** additive API for the fine states, prompts and decisions. Render "quiet for X" as advisory, never as a state.
