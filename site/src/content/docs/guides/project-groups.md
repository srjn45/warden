---
title: Project groups
description: Organize repos into named groups, get a per-project orchestrator automatically, and let grouped orchestrators coordinate without manual wiring.
---

Project groups are a lightweight organizational layer above projects: a named collection of repos that the Cockpit tree collapses and expands together. When you run an orchestrator in a grouped project, it receives peer context at spawn — its group name and the names of its sibling orchestrators — so they can coordinate across repos without you doing the wiring.

## What a project is

A **project** in warden is one repo root — the main checkout path, or a remote URL. Every agent running in any worktree of that repo shares the same project id. Pipelines carry a `project_id` back-ref too, so the Cockpit and web grid can group all the moving pieces for a repo in one place.

Projects are registered the first time you open them. They are never hard-deleted — closing a project hibernates it (agents archived but restorable) and hides it from the active surfaces; reopening flips it back.

## Creating and managing projects

### Via CLI

Manage registered projects with the `warden projects` command:

```sh
# Register an existing local directory
warden projects open-local /home/you/repos/myapp --name myapp

# Clone and register a remote repository
warden projects open-remote https://github.com/org/repo --name repo

# Register or reopen by canonical ID
warden projects open /home/you/repos/myapp

# List all registered projects
warden projects list

# Close (hibernate) a project
warden projects close /home/you/repos/myapp
```

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

Groups can be managed directly via the `warden project-groups` CLI, the REST API (`/api/v1/project-groups`), or the web interface. The TUI reads them to display the group label next to each project name in the tree.

### Via CLI

```sh
# Create a group with initial members
warden project-groups create Platform --project /home/you/repos/api --project /home/you/repos/worker

# List all groups and their members
warden project-groups list

# Show detailed group information
warden project-groups show <group-id>

# Add a project to an existing group incrementally
warden project-groups members add <group-id> /home/you/repos/another

# Remove a project from a group incrementally
warden project-groups members remove <group-id> /home/you/repos/worker

# Update group name or bulk-replace members
warden project-groups update <group-id> --name "New Platform Name"

# Delete a group (member projects remain intact)
warden project-groups delete <group-id>
```

### Via REST

```sh
# Create a group
curl -s -XPOST http://localhost:8765/api/v1/project-groups \
  -d '{"name":"Platform","project_ids":["/home/you/repos/api","/home/you/repos/worker"]}'

# List groups
curl -s http://localhost:8765/api/v1/project-groups | jq '.groups[].name'

# Incrementally add a member to an existing group
curl -s -XPOST http://localhost:8765/api/v1/project-groups/<group-id>/members \
  -H "Content-Type: application/json" \
  -d '{"project_id":"/home/you/repos/another"}'

# Incrementally remove a member from an existing group
curl -s -XDELETE http://localhost:8765/api/v1/project-groups/<group-id>/members \
  -H "Content-Type: application/json" \
  -d '{"project_id":"/home/you/repos/worker"}'

# Bulk replace group name and members
curl -s -XPUT http://localhost:8765/api/v1/project-groups/<group-id> \
  -H "Content-Type: application/json" \
  -d '{"name":"Platform","project_ids":["/home/you/repos/api"]}'
```

## Per-project orchestrators

An orchestrator is an agent that runs in a project's root under the built-in `orchestrator` role and coordinates that project's fleet rather than writing code itself.

Orchestrators are **spawned explicitly** — by you (the TUI `o` flow, the web, the CLI, or an MCP client) or by a parent agent — into an open project. Opening a project does **not** auto-spawn one: a freshly opened project starts **empty** and stays that way until something spawns into it.

A common convention is to name the orchestrator `orch-<project>` (for a project named `myapp`, `orch-myapp`), which keeps sibling orchestrators easy to address across a group, but the name is up to you.

Opening a project **restores** whatever members were hibernated when it was last closed — including an orchestrator you had spawned — but a project with no restorable members opens empty.

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
    orch-api  ●  orchestrator        ← a per-project orchestrator you spawned
    agent-abc ●  development
  ▾ worker        /home/you/repos/worker
    orch-worker ●  orchestrator
▾ Ungrouped                         ← projects not in any group
  ▾ myapp          /home/you/repos/myapp
    orch-myapp ●  orchestrator
```

Press **`←`/`→`** at any level — group, project, pipeline, or agent sub-tree — to collapse or expand it.

## FAQ

**Do I have to run an orchestrator per project?**
No. Opening a project no longer auto-spawns one — a project runs an orchestrator only if you (or a parent agent) spawn it. Spawn one into the project root when you want a resident coordinator; skip it for a project you drive directly with workers.

**What happens if the orchestrator crashes?**
The daemon does not automatically restart it — spawn a new one. If it was hibernated together with a closed project (rather than crashing), reopening that project restores it where it left off.

**Can a project belong to more than one group?**
No. Each project may belong to at most one group. The TUI shows the first group by sort order if a project is somehow in multiple groups; the store prevents this at create/update time.
