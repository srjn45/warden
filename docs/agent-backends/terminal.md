# Terminal backend (plain shell)

warden's adapter for a **plain interactive shell** — deliberately **not** an AI
agent. It gives warden a first-class way to open an ordinary terminal in an
agent's directory and manage it with the same lifecycle every other backend
gets: an isolated worktree (or the shared repo), a tmux pane you can attach to,
`wd commit`/`push`/`sync`, snapshots, teardown, and the cockpit/TUI listing.

Adapter: [`internal/agentbackend/backends/terminal.go`](../../internal/agentbackend/backends/terminal.go)
· Backend id: `terminal` · select with `--backend terminal`.

---

## What it is for

The **"human seat" beside the fleet**: a shell parked in a repo or worktree — to
run a build, poke at a failing test, or drive git by hand — that warden tracks
and tears down like any managed agent, without an AI CLI in the loop. Everything
warden does *around* an agent (isolation, git rail, attach, snapshots, cockpit)
applies; only the AI *inside* the pane is absent.

## Usage

```sh
# Open a plain shell in the current directory, managed as an agent
warden start --backend terminal --dir .

# In an isolated worktree off a ticket (managed spawn)
warden start SHELL-1 --type development --backend terminal
```

In the **TUI**, press `n` for the new-agent form, then `ctrl+t` to pick the
backend (`terminal` appears in the picker alongside `claude`, `codex`, …). Over
**MCP**, pass `backend: "terminal"` to `spawn_agent`.

The task **prompt is ignored** — a shell would execute whatever text it's fed, so
warden never types the prompt into it. Leave the prompt blank; type your commands
directly after attaching.

## How it launches

The launch line typed into the tmux pane is just the user's login shell:

```sh
${SHELL:-bash}
```

The pane is already created with the agent's worktree as its working directory
(as for every backend), so the shell opens directly *on that directory* — no `cd`
needed. It is a **nested** shell (not `exec`) on purpose: warden appends its
exit-code capture (`; printf '%s' "$?" > …`) after the command, and only a
non-exec'd child returns control so that capture runs when you type `exit` —
which is how warden learns the terminal session ended.

## Capability table

Terminal is the fully-degraded backend — an interactive pane and nothing else:

| Capability               | Cap field              | Terminal | Notes |
|--------------------------|------------------------|:--------:|-------|
| Headless one-shot        | `Headless`             | ❌       | A shell has no non-interactive classify/summarize mode. |
| Resume by session        | `Resume`               | ❌       | No session to pin; rotate/handoff open a fresh shell. |
| Structured transcript    | `StructuredTranscript` | ❌       | No conversation log ⇒ `wd digest` degrades to "no transcript". |
| Model selection          | `ModelSelection`       | ❌       | No model — it's a shell. |
| Session-id control       | `SessionIDControl`     | ❌       | No session id. |
| System-prompt inject     | `SystemPromptInject`   | ❌       | Nothing to inject a persona/hints into. |
| Pricing / spend          | `Pricing`              | ❌       | No model tokens ⇒ omitted from `wd spend`/`wd savings`. |
| State / approval detect  | —                      | ❌       | `DetectState` → Unknown; no approval UI to parse. |

Because `DetectState` always returns Unknown, warden keeps the agent's status
as-is and learns it finished only from the exit-code capture (you typing `exit`),
the same conservative stance as the interactive backends with no positive
working/idle marker (crush / goose / opencode).

## Tier decision

**No tier.** Terminal is not an AI agent, so the transcript-fidelity tiers
(A/C) don't apply. It exists to bring warden's *management* lifecycle to a plain
shell — capability degradation is total by design, not a gap to close.
