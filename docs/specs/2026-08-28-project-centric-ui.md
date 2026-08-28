# Implementation Plan: Project-Centric Cockpit & TUI

This plan outlines the end-to-end implementation of the Project-first architecture, elevating projects to first-class database entities and redesigning the TUI to reflect this hierarchy.

## Phase 1: Foundation (`projectstore`)
Elevate "Project" from an implicit directory to a persisted database entity.
1. **Schema:** Add a `projects` bucket to ScrivaDB tracking:
   - `ID` (canonical project key, e.g., remote URL or local path)
   - `Name` (display name)
   - `Path` (local absolute path in the workspace)
   - `Status` (Open/Closed)
2. **Relationships:** Update `store.Session` (agents) and `pipeline.Pipeline` models to include an optional `ProjectID` field.
3. **Daemon API:** Add REST/MCP endpoints for `ListProjects`, `OpenProject`, `CloseProject`.

## Phase 2: Open Project Git Operations
Implement the core mechanics for the three Open Project paths.
1. **Local:** Normalize the directory path, register it in `projectstore`, mark as Open.
2. **Remote:** 
   - Accept GitHub/GitLab URL.
   - Read global workspace path from `~/.warden/config.yaml`.
   - Execute `git clone` into the workspace.
   - Register in `projectstore`, mark as Open.
3. **New:** 
   - Accept project name.
   - `mkdir` in workspace, `git init`.
   - Write template `README.md`.
   - `git add . && git commit -m "chore: project initiated using warden"`.
   - Register in `projectstore`, mark as Open.

## Phase 3: TUI Frame & Tabs
Implement the horizontal border tabs to eliminate indentation and separate domains.
1. **Border Rendering:** Update `titleBox` in `control_pane.go` using `lipgloss` to render `╭─ Projects ─[ Terminals ]─╮`.
2. **State Machine:** Add a `currentTab` state (0: Projects, 1: Terminals).
3. **Tab Keybind:** In `modeNormal`, map the `Tab` key to cycle `currentTab`.
4. **List Filtering:** Modify the `items()` generator so the list only displays items belonging to the active tab.

## Phase 4: TUI Tree Nesting
Refactor the list compositor to render the strict Project hierarchy.
1. **Grouping Logic:** 
   - Group agents and pipelines by their `ProjectID`.
   - Elements without a `ProjectID` (or closed projects) are hidden or parked in a generic "Ungrouped" bucket.
2. **Hierarchy Rendering:**
   ```
   ▼ Project Alpha
       ▼ orchestrator-agent
           ▶ subagent-1
       ▼ test-pipeline
           - job-1
           - job-2
   ▶ Project Beta
   ```
3. **Collapse/Expand:** Use `Left` and `Right` arrow keys to toggle visibility of a Project's children (or an Agent's children). Reserve `Enter` for opening project details/settings in the future.
4. **Closing Projects (IDE-like Hibernation):** 
   - When closing a project with active agents, prompt the user for confirmation.
   - Projects are never deleted from the DB on close; they are just marked as closed and hidden from the active TUI list.
   - Closing a project gracefully `terminates` its active agents (kills the process but keeps the context and worktree). When the project is reopened, those agents are automatically `restored`, picking up right where they left off, exactly like open files in an IDE!

## Phase 5: TUI Open Project Menus
Replace `modeOpenDir` with the new 3-option menu.
1. **Menu Mode (`modeOpenProjectMenu`):** 
   - Renders: `(1) Open Local  (2) Open Remote  (3) New Project`
   - Maps `j/k` for selection, `Enter` to confirm.
2. **Sub-modes:**
   - `modeOpenProjectLocal`: Path autocomplete input.
   - `modeOpenProjectRemote`: URL text input.
   - `modeOpenProjectNew`: Name text input.
3. **API Integration:** Connect the TUI submissions to the Phase 2 Daemon APIs to execute the Git operations asynchronously without blocking the TUI.
