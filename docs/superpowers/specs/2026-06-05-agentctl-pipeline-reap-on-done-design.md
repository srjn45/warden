# agentctl Pipelines — Reap Done-Job Agents (+ Digest Snapshot) — Design

**Date:** 2026-06-05
**Status:** Approved design, pre-implementation
**Scope:** Pipeline executor + digest reuse + TUI/web detail-pane rendering.
**Out of scope (delegated/dropped):** TUI/web `delete` verb (#1 — delegated to a
separate agent); add-jobs-to-running-pipeline (#2 — dropped, YAGNI).

## 1. Problem

When a pipeline job finishes, the agent calls `agentctl pipeline emit "<handoff>"`.
The executor (`internal/daemon/executor.go:267` `Emit`) captures the job's `Output`
and git `Branch`, marks the job `done`, and reconciles to unblock dependents — but
it **never terminates the job's session**. The tmux session and its ~1M-context
Claude process linger indefinitely.

Everything of value is already persisted in the pipeline's job record
(`Prompt`, `Handoff`, `Output`, `Branch`, `SessionID`, `Status` —
`internal/pipeline/pipeline.go:37`). So a done job's live agent holds an agent slot
and a large block of RAM purely to keep **tmux scrollback** attachable — the exact
memory-pressure problem the spawn-gate / rotate / freeze work targets.

## 2. Goal

On job completion, **reap the agent process** (free the slot + RAM) while preserving
everything a human would want afterward — the handoff output, the branch, and a
**completion digest** — so the TUI and web render full job details from the record
instead of attaching to a dead tmux session, and `agentctl digest` still answers.

## 3. Design overview

On `emit` → job `done`:

1. **Capture branch + output** into the job record (unchanged — already done).
2. **`Terminate` the job's session** — kills tmux + the Claude process, reclaiming
   the slot and RAM. **`Terminate` only — never `Teardown`.** The worktree dir and
   its git branch must survive because downstream `from:<job>` jobs base their
   worktree off this branch (this is the same invariant the `rotate` feature
   established).
3. **Snapshot a completion digest** (best-effort, async) into a new `Job.Digest`
   field, generated from the on-disk transcript + git numstat — both of which
   survive the agent's death.
4. **Reconcile** to spawn dependents — immediately, never blocked on the digest.

The job record is now fully self-describing. TUI/web render details from it; the
digest command returns the frozen snapshot.

## 4. Why the artifacts survive reaping

`digest` (`internal/daemon/digest_routes.go`) is built from three sources, **none of
which need the live agent**:

- **Transcript** — `lifecycle.TranscriptPath(sess)`, a JSONL file on disk that
  persists after the process exits.
- **Git numstat + branch** — `lifecycle.GitNumstat/GitBranch` on `sess.Workdir`; the
  worktree dir survives (Terminate-only).
- **Narrator** — `RunClaudeP` spawns its *own* short-lived `claude -p` (a new small
  process), not the reaped 1M-context one.

So the digest can be generated **after** the job agent is killed, and it degrades
gracefully (to `facts.LastMessage`) if the narrator fails — exactly as the on-demand
path does today.

## 5. Data model change

Add one field to `pipeline.Job` (`internal/pipeline/pipeline.go`):

```go
Digest *digest.Digest `json:"digest,omitempty" yaml:"-"`   // snapshot at completion
```

`digest.Digest` already carries `{Status, Summary, Files, Branch, Turns, Task}`.
Storing the whole struct (not just the summary string) means `agentctl digest` and
the web/TUI panels render the identical shape they show for a live agent. `nil`
until the async snapshot lands (or if generation is skipped).

> Note: `internal/pipeline` would gain an import of `internal/digest`. `digest` is a
> leaf package (pure parse/merge + a `Narrator` interface), so this introduces no
> cycle. If we'd rather keep `pipeline` dependency-free, the alternative is a tiny
> mirror struct in `pipeline`; decide at plan time — preference is to reuse
> `digest.Digest` directly.

## 6. Executor change (`Emit`)

Extend `Emit` (after the existing branch capture + `done` update):

```
... capture branch, write ctx, mark job done ...        # unchanged
if sess != nil:
    _ = e.life.Terminate(ctx, sess.TmuxSession)          # best-effort; never Teardown
    go e.snapshotDigest(pid, jobID, sess)                # async, off the reconcile path
return e.Reconcile(ctx, pid)                              # unchanged — dependents spawn now
```

- **`snapshotDigest`** builds the digest via an injected `digestFn` and persists it:
  `e.pstore.Update(pid, set j.Digest)`. Fire-and-forget goroutine so neither the
  agent's `emit` request nor dependent spawning waits on a `claude -p` narrator call
  (seconds). The digest simply "appears" in the record a moment later; `notify()`
  fires when it lands so the UIs refresh.
- **Injection (testability):** the Executor gains
  `digestFn func(context.Context, *store.Session) digest.Digest` (nil ⇒ skip
  snapshot), mirroring the existing injected `notify func()`. The daemon wires it to
  a `Server.buildDigest` helper (see §7). Executor unit tests pass a fake `digestFn`
  + the existing `FakeRunner` lifecycle — no real transcript or `claude -p` needed.

### 6.1 Safety with the existing transition guards

After Terminate, the poller observes the session exit and fires
`OnTransition(... → errored/orphaned)`. The existing `markJob` guards only mutate a
job whose status is `JobRunning` (→ failed) or `JobNeedsAttention` (→ running). A
reaped job is already `JobDone`, so **OnTransition is a no-op for it** — no spurious
`failed`/`needs_attention`. No new guard needed; this is verified against
`executor.go:180` `OnTransition`.

### 6.2 Races (bounded, benign)

- A `done` job cannot be `retry`'d (`retry` accepts only `failed`/`needs_attention`),
  so a done job is immutable except via pipeline `cancel`/`delete`. If the snapshot
  goroutine writes `j.Digest` after a `cancel`, it lands on a canceled pipeline —
  harmless.
- `needs_attention` jobs may also emit (recoverable path) — same reap applies.

## 7. Digest reuse (refactor + serve the snapshot)

1. **Extract** the digest-building body of `handleDigest`
   (`digest_routes.go:29-60`) into `Server.buildDigest(ctx, sess) digest.Digest`.
   `handleDigest` then calls it; the executor's injected `digestFn` is bound to it.
   Pure refactor — identical behavior for live agents.
2. **Serve the snapshot:** in `handleDigest`, if the session belongs to a pipeline
   job (`sess.PipelineID/JobID` set) **and** that job has a stored `Digest`, return
   the stored snapshot instead of rebuilding. This keeps `agentctl digest <id>`
   working for a reaped job and returns the stable completion-time snapshot rather
   than re-running the narrator (different text each call). Live (non-pipeline)
   agents are unaffected.

## 8. Session record & transcript (what we keep)

- **Keep the session record** (Terminate moves it to a terminal status; we do **not**
  `sstore.Delete` it). Pipeline sessions nest under their pipeline via
  `PipelineID/JobID`, so a terminated record does not clutter the flat agent list.
- **Keep the transcript file** on disk — it is cheap (disk, not the RAM problem the
  reap solves) and preserves the option to regenerate a digest with different focus
  later. The expensive thing — the live process — is already reclaimed by Terminate.
- Reclaiming the worktree *dir* (disk) is deferred to `pipeline delete` / cancel
  (the natural "done with this pipeline" moment) and is **not** part of this change.

## 9. TUI rendering (detail pane)

Today, selecting a pipeline job with a `SessionID` attaches the right pane to its
tmux session. After reaping, a `done` job's tmux is gone, so:

- **Selecting a `done` (or `skipped`/`failed`) job renders an inline job-details
  view** in the detail pane — `Prompt`, `Handoff`, `Output`, `Branch`, and the
  `Digest` summary — instead of attaching. Reuses the existing inline-render path
  (`renderPipeline` / the approvals-queue inline pattern), not a new screen.
- A **`running`/`needs_attention`** job still attaches to its live session as today.
- The branch point is purely "is this job's agent still alive" (terminal job status
  ⇒ render details; live status ⇒ attach).

## 10. Web rendering (drawer)

In the Pipelines tab drawer (`web/src/components/PipelinesTab.tsx`):

- A **`done` job's drawer shows job details + the digest** (prompt / handoff /
  output / branch / summary) and **omits the "jump to terminal tab" link** (the
  terminal is gone). A live job keeps the link.
- The digest is fetched/displayed via the existing digest endpoint (which now returns
  the stored snapshot for reaped jobs, §7).

## 11. Rollout / safety valve

Reaping is **default-on** — no data is lost (output, branch, and digest all persist;
the transcript stays on disk). Provide a debugging escape hatch `AGENTCTL_PIPELINE_KEEP_DONE=1` that **skips the
Terminate** (and the snapshot), leaving the done agent's process alive. Note: the
TUI/web pipeline views still render terminal-status job details rather than attaching
(the attach-vs-details choice is keyed on job status, not agent liveness), so reach
the live pane via `agentctl attach <session>` or `tmux` directly. Matches the
env-gated rollout style of `AGENTCTL_APPROVALS` and `--supervised`.

## 12. Testing strategy

- **Executor (`executor_test.go`):** on `emit` → job done, `FakeRunner.Terminate`
  was called with the job's tmux session and **`Teardown`/`RemoveWorktree` was
  NOT**; the injected fake `digestFn` result is persisted to `Job.Digest`;
  `Reconcile`'s dependent spawn is not delayed by the (async) snapshot; downstream
  `from:<job>` still resolves the upstream branch after reap.
- **OnTransition no-op:** a terminal (`done`) job's session transition does not flip
  it to `failed`/`needs_attention`.
- **Digest serve (`digest_routes_test.go`):** a pipeline-job session with a stored
  `Job.Digest` returns the snapshot; a live agent rebuilds as before; refactored
  `buildDigest` is behavior-preserving.
- **Escape hatch:** with `AGENTCTL_PIPELINE_KEEP_DONE=1`, Terminate is not called.
- **TUI (`pipeline_view_test.go`):** a `done` job renders the details view (not an
  attach); a `running` job still attaches.
- **Web (`pipelines.test.ts` / component):** done-job drawer shows details + digest,
  no terminal link; live job keeps the link.
- Built via the established TDD + subagent-driven worktree workflow
  (`.claude/worktrees/pipeline-reap-on-done`, baseRef=head).

## 13. Resolved decisions

- **Terminate-only, never Teardown** — downstream branches need the worktree/branch.
- **Snapshot the full `digest.Digest` into the job record at completion**, served by
  the session digest endpoint for reaped jobs — stable, no repeated narrator cost.
- **Keep the session record + transcript**; reclaim only the live process now.
- **Default-on**, with `AGENTCTL_PIPELINE_KEEP_DONE=1` escape hatch.
- Detail-pane branch keyed on job status (terminal ⇒ details, live ⇒ attach).
