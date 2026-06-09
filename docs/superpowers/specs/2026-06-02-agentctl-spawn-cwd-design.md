# agentctl — spawn agents from the caller's directory

**Date:** 2026-06-02
**Status:** Approved (design)

## Problem

When `agentctl` spawns a prompt-mode agent (`agentctl start "<prompt>"`), the
tmux session — and therefore the `claude` process — is launched in a freshly
created, empty per-agent directory: `~/agentctl-agents/<id>`. That directory has
no repo checkout, so the agent has no code context unless the prompt points it
somewhere.

In practice the user is usually sitting in a project they want the agent to work
on. They expect the agent to be launched **from their current directory**, the
same way they would run `claude` themselves. This should hold whether `agentctl`
is invoked:

- **directly** from a shell (`agentctl start "…"` from `/some/project`), or
- **indirectly** by another Claude session driving the agentctl MCP server.

In both cases the agent should be launched from the directory open in the
current shell / session.

## Goal

Launch the agent's `claude` process from the caller's working directory, while
keeping agentctl's own per-agent data where it already lives.

Non-goal: changing typed/managed-worktree mode. That path already runs in the
repo (the CLI already defaults `--repo` to the current directory), so it is left
untouched.

## Core idea — separate "data dir" from "launch dir"

Today prompt mode conflates two distinct concepts into one directory. We split
them:

| Concept | Today | New |
|---|---|---|
| **Agent data** (the `.agentctl-prompt` file) | `~/agentctl-agents/<id>` | `~/agentctl-agents/<id>` (unchanged) |
| **Claude launch dir** (`tmux -c`, `sess.Workdir`) | `~/agentctl-agents/<id>` | the caller's cwd |

So agentctl keeps owning `~/agentctl-agents/<id>` for its bookkeeping, but
`claude` is launched from — and operates on — the caller's directory.

### `lifecycle.Spawn` prompt-mode flow (new)

1. `dataDir = filepath.Join(req.Workdir, id)` — the `~/agentctl-agents/<id>`
   base (`req.Workdir` is still set server-side to `s.workdir`). `mkdir -p`.
2. Write the prompt to `dataDir/.agentctl-prompt` (unchanged location).
3. `launchDir = req.Cwd`, falling back to `dataDir` when `req.Cwd` is empty.
4. `sess.Workdir = launchDir`.
5. `tmux new-session -d -s <id> -c <launchDir>`.
6. Launch line reads the prompt by **absolute path**:
   `claude … "$(cat '<dataDir>/.agentctl-prompt')"`.

Setting `sess.Workdir = launchDir` is deliberate and load-bearing:

- `claudeProjectDir(root, workdir)` maps the workdir to Claude Code's transcript
  project directory (used for the deterministic `--session-id` transcript lookup
  and for `restore`). Pointing it at the real project keeps that lookup correct.
- `Restore` recreates the tmux session in `sess.Workdir` (and already validates
  the dir exists via `os.Stat`), so restored agents reopen in the project too.

### Fallback

When `req.Cwd` is empty, `launchDir` falls back to `dataDir` — exactly today's
behavior. This is the safety net for the rare case where the caller cannot
determine its cwd (e.g. `os.Getwd()` fails).

## Capturing cwd at the edges

The actual `tmux new-session -c …` happens **inside the daemon**, whose cwd is
irrelevant (launchd-managed). So the caller's cwd must be captured at the edge
and sent to the daemon.

- **CLI** (`agentctl start "<prompt>"`): capture `os.Getwd()`, send as `cwd`.
- **MCP** (`spawn_agent`): capture `os.Getwd()` in the handler. The agentctl MCP
  server is a **stdio subprocess** Claude Code launches per session, so its cwd
  is the orchestrator session's working directory — exactly the "indirect" case.
- Both also accept an explicit override that defaults to cwd when omitted:
  - CLI: `--dir <path>` flag.
  - MCP: optional `dir` argument on `spawn_agent`.

## Wire plumbing

One new field, threaded end to end:

- `client.SpawnParams`: add `Cwd string`; `client.Spawn` sends `"cwd": p.Cwd`.
- `daemon.SpawnRequest`: add `Cwd string \`json:"cwd"\``.
- `daemon.handleSpawn`: keep `req.Workdir = s.workdir` (the data-dir base) and
  forward `req.Cwd`. When `req.Cwd` is non-empty, validate it is an existing
  directory; reject with `400` otherwise (guards the web path against typos /
  stale picks). Empty `cwd` is allowed (lifecycle falls back).
- `lifecycle.SpawnRequest`: add `Cwd string`.
- `lifecycleAdapter.Spawn`: pass `Cwd: req.Cwd`.

## Web — server-side directory browser

The web daemon cannot see the user's shell cwd, so the launch directory must be
chosen in the "New agent" form. A native browser folder picker cannot return an
absolute server-side path (the File System Access API deliberately hides it, and
the download dialog is browser-controlled). Since the daemon and browser are on
the same machine, the daemon browses its own filesystem instead.

### New endpoint

`GET /fs/dirs?path=<abs>` →

```json
{ "path": "/abs/current", "parent": "/abs", "entries": [ { "name": "foo", "path": "/abs/current/foo" } ] }
```

- Lists immediate **subdirectories only** of `path`.
- Defaults to the user's home directory when `path` is omitted.
- `parent` is empty at the filesystem root.
- Surfaces not-a-directory / permission errors cleanly as `400`.

### NewAgentModal

Add a mandatory Finder-style picker beneath the prompt field:

- Shows the current path, a list of subfolders (click to descend), an "up"
  control (uses `parent`), and a "Use this folder" action.
- The chosen path is sent as `cwd` on spawn.
- **Create stays disabled until a folder is selected.**

## Testing

- `lifecycle`: prompt-mode spawn with `Cwd` set → tmux launched with
  `-c <cwd>`, prompt file still written under the data dir, `sess.Workdir == cwd`.
  With `Cwd` empty → falls back to the data dir (existing behavior preserved).
- `daemon`: `handleSpawn` forwards `cwd`; rejects a non-existent `cwd` with `400`.
- `fs` endpoint: lists subdirectories, computes `parent`, rejects a file path /
  permission error.
- `web`: `spawn` includes `cwd`; light test of the modal picker / create-disabled
  gating.

## Out of scope

- Typed/managed-worktree mode (already repo-based).
- Persisting a "last used directory" in the web form (could be a later nicety).
