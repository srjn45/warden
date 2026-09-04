---
title: Project groups
description: Organize repos into named groups, get a per-project orchestrator automatically, and let grouped orchestrators coordinate without manual wiring.
---

Project groups are a lightweight organizational layer above projects: a named collection of repos that the Cockpit tree collapses and expands together. Opening any project in a group also guarantees a live orchestrator in that directory, and grouped orchestrators receive peer context at spawn so they can coordinate without you doing the wiring.

## What a project is

A **project** in warden is one repo root — the main checkout path, or a remote URL. Every agent running in any worktree of that repo shares the same project id. Pipelines carry a `project_id` back-ref too, so the Cockpit and web grid can group all the moving pieces for a repo in one place.

Projects are registered the first time you open them. They are never hard-deleted — closing a project hibernates it (agents archived but restorable) and hides it from the active surfaces; reopening flips it back.

## Creating and managing projects

### In the TUI

Press **`o`** in the Cockpit control pane to open a project:

| Option | What it does |
|---|---|
| **Local** | Opens an existing local directory as a project |
| **Remote** | Clones a remote URL into the workspace and registers the result |
| **New** | Scaffolds a fresh `git init` project in the workspace |

Opened projects appear in the TUI tree. Use **`←`/`→`** (or `h`/`l`) to collapse/expand a project group, pipeline, or agent sub-tree.

### Via REST

```sh
# Register a local path
curl -s -XPOST http://localhost:8765/api/v1/projects/local \
  -d '{"path":"/home/you/repos/myapp","name":"myapp"}'

# Clone a remote URL
curl -s -XPOST http://localhost:8765/api/v1/projects/remote \
  -d '{"url":"https://github.com/org/repo","name":"repo"}'

# List all registered projects
curl -s http://localhost:8765/api/v1/projects | jq '.projects[].name'
```

## Creating and managing groups

A **project group** is a named collection of project ids. One project may belong to at most one group; membership is stored on the group, not on the project, so deleting a group never touches the repos inside it.

Groups are managed through the REST API (`/api/v1/project-groups`) or the web interface. The TUI reads them to display the group label next to each project name in the tree.

```sh
# Create a group
curl -s -XPOST http://localhost:8765/api/v1/project-groups \
  -d '{"name":"Platform","project_ids":["/home/you/repos/api","/home/you/repos/worker"]}'

# List groups
curl -s http://localhost:8765/api/v1/project-groups | jq '.groups[].name'

# Add a project to an existing group
curl -s -XPOST http://localhost:8765/api/v1/project-groups/<group-id>/members \
  -d '{"project_id":"/home/you/repos/another"}'
```

## Per-project orchestrators (auto-spawn)

When you open a project — whether via the TUI `o` flow, the web, or the REST API — the daemon runs a **guarantee hook** that ensures exactly one orchestrator session named `orch-<project>` is alive in the project directory:

- **Already running** — no-op.
- **Recorded but not running** — the daemon revives it from its transcript so the conversation history is preserved.
- **Not recorded** — a fresh orchestrator agent (role `orchestrator`, workdir = project root) is spawned and linked to the project via `ProjectID`.

The guarantee is **best-effort**: spawn/revive failures are logged but never fail the open request. The orchestrator hibernates with the project (archived when you close the project, restored when you reopen it).

The orchestrator session name is stable: `orch-` followed by a sanitized, length-capped slug of the project's display name. For a project named `myapp` the name is `orch-myapp`.

## Peer awareness (grouped orchestrators)

An orchestrator that belongs to a project group receives a **peer-awareness addendum** in its system prompt at every fresh launch or resume. The addendum contains:

- The group name it belongs to.
- The names of its sibling orchestrators (the other `orch-*` sessions in the same group).

This lets grouped orchestrators coordinate work across repos — for example, sending a targeted message to a peer — without you having to look up session ids or wire any context manually:

```sh
# Send a message to a sibling orchestrator by its stable name
warden msg send orch-worker "The API schema changed, please update your client"
```

The peer context is recomputed from the live store at every (re)launch, so adding a project to a group is sufficient — no restart is needed.

## TUI tree anatomy

Once you have projects and groups the Cockpit tree renders like this:

```
▸ Platform                          ← group header (h/l to expand/collapse)
  ▾ api            /home/you/repos/api
    orch-api  ●  orchestrator        ← auto-spawned per-project orchestrator
    agent-abc ●  development
  ▾ worker        /home/you/repos/worker
    orch-worker ●  orchestrator
▾ Ungrouped                         ← projects not in any group
  ▾ myapp          /home/you/repos/myapp
    orch-myapp ●  orchestrator
```

Press **`←`/`→`** at any level — group, project, pipeline, or agent sub-tree — to collapse or expand it.

## FAQ

**Can I disable the auto-spawn orchestrator?**
Not per-project — the guarantee hook runs on every open path. If you need a project without a resident orchestrator, open it via the REST API directly (`POST /api/v1/projects/local`) rather than the TUI `o` flow; the hook still fires on all paths today, so the cleanest workaround is to stop the orchestrator after it spawns (`warden stop orch-<name>`).

**What happens if the orchestrator crashes?**
The daemon does not automatically restart it. The next time you open that project (e.g. reopen from the TUI or the web), the guarantee hook revives it.

**Can a project belong to more than one group?**
No. Each project may belong to at most one group. The TUI shows the first group by sort order if a project is somehow in multiple groups; the store prevents this at create/update time.
