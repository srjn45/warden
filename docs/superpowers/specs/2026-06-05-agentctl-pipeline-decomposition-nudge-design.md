# agentctl — pipeline-decomposition recommendation (design)

Date: 2026-06-05

## Problem

Mac freezes ("forces restart") while using agentctl traced to **long-lived,
large-context agents**, not a daemon leak. Investigation (see memory
`agentctl-freeze-investigation`) established:

- The daemon is clean (~24 MB RSS, 22 FDs); the Mac never actually rebooted
  (38 h uptime, no panics); no disk-swap exhaustion (Swapouts=0).
- The leading cause is a transient memory spike: a long-running **1M-context**
  agent hits auto-**compaction** (a large in-process V8 allocation). With several
  large agents alive at once, this can tip macOS into memory-compressor thrash →
  whole-UI freeze that recovers.

The key insight (from the user): the lever is **per-agent context lifetime**, not
agent count. A concurrency cap was rejected because it limits throughput without
addressing per-agent growth. Decomposing work into a **pipeline of short-lived,
bounded-context stages** keeps each agent small and — crucially — **returns each
stage's memory to the OS when the agent is torn down on completion**, which
in-process compaction never does (the process stays large).

The current `skills/agentctl/SKILL.md` rubric only reasons about **dependencies**
("does a step wait for another's result?"). It has no notion of task size, so a
big self-contained task falls into "plain agent (default)" — exactly the
long-runner that causes the spike.

## Goal

Make "decompose large/long-running work into a pipeline" a standing recommendation
reaching two audiences:

1. **The orchestrating session** — via the skill rubric + broadened triggers, so it
   proactively suggests decomposition when a task looks large.
2. **Every spawned worker agent** — via injected system-prompt guidance, so an agent
   that finds its task is large advises splitting it (non-blocking), then proceeds.

## Non-goals

- No concurrency cap (explicitly rejected — costs throughput, doesn't address
  per-agent context growth).
- No auto-teardown of idle/finished ad-hoc agents (separate future idea).
- No web / TUI / MCP changes.
- No change to `Restore`/resume (continues an existing context — a fresh nudge is
  pointless) or to `SpawnJob`/pipeline jobs (already decomposed).

## Design

### Component A — `skills/agentctl/SKILL.md` (docs only)

1. **Add a second axis to "Choosing the tool":**

   > **Second axis — size & longevity:** would one agent accumulate a large or
   > long-lived context (a multi-phase task, a long unattended run, anything likely
   > to approach the context limit and auto-compact)? If yes, **decompose it into a
   > pipeline of bounded stages** — even when the steps are sequential and one agent
   > could do them. Each stage gets a fresh, small context and is **torn down on
   > completion**, returning memory to the OS and avoiding the compaction spikes a
   > long-lived large-context agent causes.

2. **Add a "Don't" bullet:** *"…run a big multi-phase task as one long-lived plain
   agent — decompose it into pipeline stages so each agent stays small and closes
   when its phase finishes."*

3. **Broaden the frontmatter `description:` triggers** to fire on size/longevity
   intent, not only explicit "pipeline"/"spawn" phrasing. Add phrases such as:
   *"this is a big / multi-phase / long-running task", "break this down", "this
   will take a while"*. This is what makes the orchestrator suggest decomposition
   **proactively** rather than only when "pipeline" is named.

### Component B — `internal/lifecycle/lifecycle.go` (code)

1. **Guidance constant** — the advisory injected as a system-prompt addendum:

   > You were launched as a standalone agentctl agent. If this task is large or
   > spans multiple distinct phases (e.g. analyze → implement → test → review) such
   > that you would accumulate a very large context, briefly recommend up front that
   > it be split into an agentctl pipeline of smaller stages (each a short-lived
   > agent with a fresh, bounded context), then proceed with the task as a single
   > agent unless told otherwise.

2. **Env-gated helper** (e.g. `pipelineHintFlag() string`) returning the fragment
   `--append-system-prompt <shell-quoted guidance>`, or `""` when the env var
   `AGENTCTL_NO_PIPELINE_HINT` is set (any non-empty value). On-by-default with an
   opt-out, matching the repo's env-gate convention (`AGENTCTL_APPROVALS`, etc.).

3. **Thread into `claudeLaunch`** via a new parameter (e.g.
   `appendSystemPrompt string`); when non-empty, `claudeLaunch` inserts the
   `--append-system-prompt` flag (properly shell-quoted) into the launch string.
   - `Spawn` (lines ~467 and ~497) passes `pipelineHintFlag()`'s guidance.
   - `SpawnJob` (line ~888) passes `""`.
   - The resume/restore builder (`--resume` variant) is unchanged.

   Result: **fresh plain agents get the nudge; pipeline jobs and restores do not**,
   purely by which call site supplies the text — no runtime job-type check needed.

4. **Behavior:** non-blocking — advise then proceed.

### Component C — Testing (TDD)

- `pipelineHintFlag`: returns the `--append-system-prompt …` fragment by default;
  returns `""` when `AGENTCTL_NO_PIPELINE_HINT=1`; the guidance is shell-quoted so
  it survives `sh -c` / `tmux send-keys` intact.
- `claudeLaunch`: includes `--append-system-prompt` exactly when given a non-empty
  fragment; omits it when empty; existing `--session-id`/`--name`/permission-flag
  output is otherwise unchanged (byte-identical when fragment empty).
- Spawn vs SpawnJob: a `Spawn` launch string contains the flag (default env); a
  `SpawnJob` launch string does not.

## Rollout / operational notes

- Requires **rebuild + restart of the daemon** to take effect (behavior is in the
  binary). New spawns only; existing/running agents are unaffected.
- Skill changes take effect immediately for sessions that load the skill.

## Risks & mitigations

- **Noise on small tasks:** wording is conditional ("if this task is large…"), so a
  small task triggers no advisory. Opt-out env (`AGENTCTL_NO_PIPELINE_HINT`)
  available if it ever becomes noisy.
- **Worker can't create a pipeline itself:** intentional — the advisory is for the
  human/orchestrator reading the agent's output to act on; the worker still
  completes its task.
- **`--append-system-prompt` support:** standard Claude Code CLI flag; verify the
  installed `claude` accepts it during implementation (smoke a spawned agent).
