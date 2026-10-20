---
title: Terminal sessions
description: Create, attach to, and restore a plain-shell terminal session beside a Warden fleet.
---

A terminal session is a managed interactive `$SHELL` beside your agents. It is a
first-class session with `kind=terminal`, not an AI backend: it does not select an
AI CLI or model, receive a role or prompt, spend tokens, produce a digest, or take
part in approvals. Use one for direct repository work, a local command, or an
interactive login while agents work elsewhere.

Each terminal belongs to a project. It is created only when you ask for one; Warden
does not create a default shell for every project.

## Create a terminal

From the command line, start a plain shell in a directory that resolves to an open
project:

```sh
warden agent start --kind terminal --dir .
```

`--kind terminal` is the important part. A terminal uses `$SHELL` (falling back to
`bash`) in `--dir`; agent-specific inputs such as `--aicli`, `--model`, `--role`,
and a prompt are ignored. It is not a `terminal` backend. The older
`--backend terminal` spelling remains only as a compatibility alias.

The TUI offers the same action: put the cursor in a project and press `t`. In the
web interface, open **Terminals** and choose **New terminal**. Both create a
project-owned terminal, and refuse rather than leaving an unowned shell when there
is no project target.

MCP callers use the normal spawn operation with `kind: "terminal"`; it has the same
plain-shell semantics.

## Find and attach

Terminals live under their project in the cockpit tree and in the web **Terminals**
tab. They are deliberately excluded from the agent grid and agent counts, and
`warden ls` lists AI agents rather than plain terminals.

Attach from a local terminal with the session ID:

```sh
warden attach <terminal-id>
```

For the normal tmux-hosted mode, this attaches with tmux just as it does for an
agent. In the cockpit, press `Enter` on a terminal to show it in the bottom-left
terminal pane; `a` opens the selected live session full-screen. The browser's
Terminals tab provides its own live terminal viewport.

When `runtime.pty: on`, a plain terminal is hosted directly on Warden's PTY instead
of in tmux. `warden attach` then uses Warden's native attach: terminal signals,
job control, full-screen programs, resizing, and UTF-8/ANSI behave like a local
shell. **Ctrl-]**, then **.**, detaches while leaving the shell running. There can
be several viewers but only one controller; taking control gives human input
precedence. Set `WARDEN_ATTACH=observe` to watch read-only. See the
[PTY runtime guide](./pty-runtime/) for the journal, controller lease, and other
attach controls.

## Restoration and lifecycle

What is restored depends on why the terminal stopped and on the PTY runtime mode.

- With the default tmux path (and `runtime.pty: shadow`), a daemon restart does not
  stop the tmux shell. Warden resumes observing it.
- With `runtime.pty: on`, the independent PTY holder owns the shell, so a daemon
  restart, update, or crash does not stop it. The next daemon authenticates to and
  re-adopts the existing holder; it never silently launches a replacement shell.
- Closing a project hibernates its running terminals. Reopening that project
  automatically creates a **fresh** shell with the same terminal identity and
  working directory; shell process state is not preserved.
- If a terminal is orphaned or exited, restore it with `warden agent restore <id>`.
  Restoration starts a fresh shell in its recorded working directory rather than
  resuming command state.

If a direct-runtime holder is gone, Warden reports the terminal as orphaned instead
of guessing. Restore it when you want a new shell.

## How terminals differ from agents

An agent session starts an AI CLI and has an AI backend, role, model routing,
transcript/digest behavior, and approval handling. A terminal session starts only a
shell. It is useful *beside* the fleet—not as a substitute for an agent—and still
gets project ownership, a live attach surface, and the terminal-specific recovery
rules above.

For the cockpit layout and shortcuts, see [The TUI cockpit](./tui-cockpit/). For
the browser view, see [Web mission control](./web-mission-control/).
