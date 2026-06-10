---
title: CLI command reference
description: Every warden command and its flags, generated from warden --help.
---

> Generated from `warden --help` on 2026-06-10; regenerate when commands change.

All commands accept `--addr` to point at a non-default daemon (overrides `WARDEN_ADDR`). `<TICKET>` is the agent ID — a Jira key for managed agents, or an `agent-xxxx` ID for prompt-spawned ones.

## warden

```text
spawn, monitor, and tear down Claude Code agent sessions.
Run `warden` with no arguments to open the cockpit TUI. Alias: wd.

Usage:
  warden [flags]
  warden [command]

Available Commands:
  adopt           Register the Claude session in this directory (resume it under tmux, or register the current tmux session live)
  approvals       List pending tool-permission prompts waiting for an answer
  approve         Answer a pending tool-permission prompt by option number
  attach          Attach to the agent's tmux session
  completion      Generate the autocompletion script for the specified shell
  ctx             Read and write the shared context (a namespaced key/value store agents share)
  daemon          Run the warden hub (HTTP API + poller; the single writer to the file store)
  delete          Clear an agent's stored record (archives by default; --hard to purge)
  digest          Summarize what an agent accomplished (files, branch, turns, narrative)
  doctor          Run preflight checks (required binaries, daemon, data dir)
  done            Terminate an agent and clear its record (does NOT remove the worktree)
  help            Help about any command
  ls              List all active agent sessions
  mcp             Run the MCP stdio server so an orchestrator Claude can manage agents
  msg             Send and receive directed messages between agents
  pipeline        Define and run DAG pipelines of agent jobs
  remove-worktree Remove an agent's git worktree + branch (always asks; --force overrides guards)
  restore         Recreate and resume a lost/orphaned agent (claude --resume)
  rotate          Hand this agent's work to a fresh successor in the same workspace, then retire it
  send            Type a message into an agent's claude session and press Enter
  start           Spawn an agent — `start "<prompt>"` (auto-typed), `start --dir <path>` (interactive: open Claude & wait), or `start TICKET --type <TYPE>` (managed worktree)
  stats           Show warden's resource footprint (per-agent memory/CPU, system pressure, daemon stats)
  status          Show full status for one session
  tail            Print the recent output of an agent's claude session
  terminate       Stop an agent: kill its tmux+claude session (keeps the record and worktree)
  tui             Live terminal cockpit for agents

Flags:
      --addr string   daemon address (overrides WARDEN_ADDR)
  -h, --help          help for warden
  -v, --version       version for warden
```

## warden start

```text
Spawn an agent — `start "<prompt>"` (auto-typed), `start --dir <path>` (interactive: open Claude & wait), or `start TICKET --type <TYPE>` (managed worktree)

Usage:
  warden start [TICKET|"<prompt>"] [--type <TYPE>] [--dir <PATH>] [flags]

Flags:
      --auto-restart    auto-resume this agent if it crashes (errored), capped at a few attempts
      --branch string   new branch (development) or checkout target (pr-review)
      --dir string      directory to launch the agent from (default: current directory)
      --force           spawn even when the memory-pressure gate warns
  -h, --help            help for start
      --pr string       PR number/url (pr-review)
      --repo string     repo path (default: current directory)
      --supervised      launch in acceptEdits mode (prompts for risky tools → answerable in the approvals inbox)
      --type string     task type: development|analysis|spike|pr-review|buildkite-debug|test-run|env-test|other
      --worktree        create a scratch worktree for analysis/spike
```

## warden ls

```text
List all active agent sessions

Usage:
  warden ls [flags]

Flags:
  -h, --help   help for ls
      --json   output as JSON
```

## warden status

```text
Show full status for one session

Usage:
  warden status <TICKET> [flags]

Flags:
  -h, --help   help for status
      --json   output as JSON
```

## warden attach

```text
Attach to the agent's tmux session

Usage:
  warden attach <TICKET> [flags]
```

## warden send

```text
Type a message into an agent's claude session and press Enter

Usage:
  warden send <TICKET> <message...> [flags]
```

## warden tail

```text
Print the recent output of an agent's claude session

Usage:
  warden tail <TICKET> [flags]

Flags:
  -h, --help        help for tail
      --lines int   number of pane lines to capture (default 200)
```

## warden digest

```text
Summarize what an agent accomplished (files, branch, turns, narrative)

Usage:
  warden digest <TICKET> [flags]

Flags:
  -h, --help   help for digest
      --json   output as JSON
```

## warden rotate

```text
Run inside an agent session. Phase 1 is driven by the /warden skill (the agent writes a handoff file + resume prompt and shows you). On your go-ahead, run with --confirm to spawn the successor and reap this agent.

Usage:
  warden rotate [flags]

Flags:
      --confirm                actually spawn the successor and retire this agent (required)
  -h, --help                   help for rotate
      --resume-file string     path to the handoff notes file the successor should read
      --resume-prompt string   the successor's initial task prompt
```

## warden approvals

```text
List pending tool-permission prompts waiting for an answer

Usage:
  warden approvals [flags]
```

## warden approve

```text
Answer a pending tool-permission prompt by option number

Usage:
  warden approve <TICKET> <option> [flags]
```

## warden doctor

```text
Run preflight checks (required binaries, daemon, data dir)

Usage:
  warden doctor [flags]
```

## warden stats

```text
Show warden's resource footprint (per-agent memory/CPU, system pressure, daemon stats)

Usage:
  warden stats [flags]

Flags:
  -h, --help    help for stats
      --json    output as JSON
      --watch   redraw every 3s until interrupted
```

## warden pipeline

```text
Define and run DAG pipelines of agent jobs

Usage:
  warden pipeline [command]

Available Commands:
  cancel      Cancel a pipeline (terminates running jobs)
  create      Create a pipeline from a YAML spec
  delete      Delete a pipeline's record (must not have live jobs — cancel first)
  edit-job    Edit a pending job's prompt and/or handoff
  emit        Publish this job's handoff (run from inside a pipeline job)
  list        List pipelines
  retry       Re-run a failed or needs-attention job (reopens skipped descendants)
  show        Show a pipeline's jobs and their status
  start       Start a pipeline (spawns jobs with no dependencies)
```

## warden ctx

```text
Read and write the shared context (a namespaced key/value store agents share)

Usage:
  warden ctx [command]

Available Commands:
  del         Delete a context key
  get         Print the value at a context key
  list        List context keys (optionally filtered by prefix)
  set         Set a context key (value inline, or --file / --stdin)
```

## warden msg

```text
Send and receive directed messages between agents

Usage:
  warden msg [command]

Available Commands:
  inbox       Show this agent's messages (marks them read)
  send        Send a message to an agent (wakes it if it's idle/waiting)
  wait        Block until a message arrives (or timeout), then print it

Flags:
      --as string   act as this agent id (defaults to $WARDEN_SESSION_ID)
```

## Other commands

A handful of lifecycle commands share the simple `<TICKET>` shape:

```text
warden adopt [--session-id <uuid>] [--dir <path>]   Register an existing Claude session
warden terminate <TICKET>                           Stop an agent (keeps record + worktree)
warden restore <TICKET>                             Recreate/resume a lost/orphaned agent
warden done <TICKET> [--hard]                        Terminate + clear record (worktree kept)
warden delete <TICKET> [--hard]                      Clear the stored record only
warden remove-worktree <TICKET> [--force]            Remove the git worktree + branch (destructive)
warden daemon [--addr ADDR]                          Run the hub (HTTP API + poller)
warden mcp [--addr ADDR]                             Run the MCP stdio server
```
