# Agent Name/Alias Feature Design

**Date**: 2026-06-12  
**Status**: Approved  
**Author**: Claude Code (warden agent)

## Overview

Allow users to give agents memorable names instead of only using auto-generated IDs like `agent-a1b2`. Names are optional, human-friendly identifiers that make it easier to reference agents in commands and UI.

## Goals

1. Support optional naming of agents at spawn time via `--name` flag
2. Enable name-based lookup in all CLI commands (`attach`, `send`, `status`, etc.)
3. Display names prominently in `warden ls`, TUI, and status output
4. Maintain backward compatibility: agents without names work exactly as before
5. Ensure name uniqueness among active sessions to prevent confusion

## Non-Goals

- Renaming agents after creation (names are immutable)
- Auto-generating friendly names (users must explicitly provide them)
- Name-based lookup for archived sessions (only active sessions)

## Requirements Summary

From user discussion:
- Names are **unique among active sessions only** (not globally)
- Display format: **name first, then ID** in tables
- Lookup strategy: **match name first**, fall back to ID
- Validation: **alphanumeric + hyphens/underscores**, **max 32 chars**, **case-sensitive**
- Mutability: **immutable** once set at spawn time

---

## Design

### 1. Data Model

#### Session struct changes

Add a `Name` field to `store.Session` in `internal/store/types.go`:

```go
type Session struct {
    ID              string     `json:"id"`
    Name            string     `json:"name,omitempty"` // optional human-friendly alias (max 32 chars)
    Type            Type       `json:"type"`
    Ticket          string     `json:"ticket"` // optional
    TmuxSession     string     `json:"tmux_session"`
    // ... rest unchanged
}
```

**Properties:**
- **Optional**: `omitempty` tag means empty names don't bloat JSON
- **Immutable**: set once at spawn, never updated
- **Validated at insert**: format, length, and uniqueness checks
- **Case-sensitive**: "MyAgent" and "myagent" are distinct

#### Validation rules

Applied in `FileStore.Insert()`:

1. **Format**: Must match `^[a-zA-Z0-9_-]+$` (alphanumeric, hyphens, underscores only)
2. **Length**: 1-32 characters when non-empty
3. **Uniqueness**: No other active session can have the same name (exact case-sensitive match)
4. **Empty allowed**: An empty/missing name is valid (backwards compatibility)

**Why these rules?**
- Alphanumeric + hyphens/underscores keeps names shell-safe (no escaping needed)
- 32-char limit prevents line-wrapping in table views
- Case-sensitive matching respects user intent and avoids ambiguity

---

### 2. Store Layer

#### New Store interface method

Add to `internal/store/store.go`:

```go
type Store interface {
    // ... existing methods ...
    Get(ctx context.Context, id string) (*Session, error)
    
    // GetByNameOrID looks up a session by name first (exact case-sensitive match
    // among active sessions), falling back to ID lookup if no name matches.
    // Returns ErrNotFound if neither name nor ID match any active session.
    GetByNameOrID(ctx context.Context, nameOrID string) (*Session, error)
    
    List(ctx context.Context) ([]*Session, error)
    // ... rest unchanged ...
}
```

**Why a new method?**  
Centralizes lookup logic in the store layer where session data lives. All callers (CLI, MCP, TUI) get consistent name-first lookup without duplicating the scanning logic.

#### FileStore.GetByNameOrID implementation

Algorithm:
1. Call `List()` to get all active sessions
2. Scan for an exact name match (`s.Name == nameOrID`, case-sensitive)
3. If found, return that session immediately
4. If not found, fall back to `Get(nameOrID)` for ID-based lookup
5. Return `ErrNotFound` if both fail

**Performance:** O(n) name scan is acceptable since active session counts are typically small (<50 agents).

#### Name uniqueness validation

Enhance `FileStore.Insert()`:

1. If `s.Name` is non-empty:
   - Validate format using regex `^[a-zA-Z0-9_-]{1,32}$`
   - Return `ErrInvalidName` if format check fails
   - Call `List()` to get active sessions
   - Scan for any session with `s.Name == existing.Name` (case-sensitive)
   - Return `ErrNameExists` if a duplicate is found
2. Proceed with existing Insert logic (ID uniqueness, file write, etc.)

#### New error types

Add to `internal/store/store.go`:

```go
var ErrNotFound = errors.New("session not found")
var ErrExists = errors.New("session already exists")
var ErrNameExists = errors.New("agent name already exists")
var ErrInvalidName = errors.New("invalid agent name: must be 1-32 alphanumeric chars, hyphens, or underscores")
```

**How to apply:** Use these errors for user-facing messages in CLI/MCP layers.

---

### 3. Client Layer

#### SpawnParams changes

Add `Name` field to `client.SpawnParams` in `internal/client/client.go`:

```go
type SpawnParams struct {
    Type        string
    Ticket      string
    Name        string // new: optional human-friendly alias
    Repo        string
    Branch      string
    PR          string
    // ... rest unchanged
}
```

The client passes this through to the daemon's POST `/spawn` endpoint, which validates and stores it.

#### Client.Get() changes

Modify `Client.Get(ctx, nameOrID)` to call the daemon's GET endpoint with the nameOrID parameter. The daemon internally calls `store.GetByNameOrID()` to handle name-first lookup.

**Why in the client?**  
The client is a thin HTTP wrapper. The actual lookup logic lives in the daemon's store layer, keeping the client simple.

---

### 4. CLI Layer

#### `warden start` command

Add a `--name` flag to `newStartCmd()` in `internal/cli/lifecycle.go`:

```go
cmd.Flags().String("name", "", "optional human-friendly name (max 32 chars, alphanumeric + hyphens/underscores)")
```

**Usage examples:**
```bash
# Free-form mode
warden start "build the frontend" --name my-build

# Typed mode
warden start PROJ-123 --type development --name feature-auth

# No name (backward compatible)
warden start "debug ci"
```

Pass the name to `client.Spawn()` via `SpawnParams.Name`.

#### Error handling

Wrap store errors with user-friendly messages:

- `ErrNameExists` → `"agent name 'foo' is already in use by another active session"`
- `ErrInvalidName` → `"invalid name: must be 1-32 alphanumeric chars, hyphens, or underscores"`

Display these on stderr and exit with non-zero status.

#### `warden ls` command

Modify table output in `newLsCmd()` to add a NAME column:

**Before:**
```
ID              TYPE        STATUS      CONTEXT  AGE  DIR      SUBJECT
agent-a1b2      development working     42k      5m   warden   adding feature X
```

**After:**
```
NAME            ID              TYPE        STATUS      CONTEXT  AGE  DIR      SUBJECT
my-build        agent-a1b2      development working     42k      5m   warden   adding feature X
—               agent-c3d4      analysis    idle        —        2h   app      investigating bug
review-pr       agent-e5f6      pr-review   done        15k      1d   backend  review complete
```

**Column layout:**
- NAME: 15 chars (left-aligned), show `—` when empty
- ID: 14 chars (unchanged)
- Rest: unchanged

**Implementation:** Update the header and row formatting in `newLsCmd()` to include `s.Name` before `s.ID`.

#### Lookup-based commands

Commands that accept an agent identifier now support names:

- `warden attach <name-or-id>`
- `warden send <name-or-id> <message>`
- `warden status <name-or-id>`
- `warden tail <name-or-id>`
- `warden terminate <name-or-id>`
- `warden delete <name-or-id>`
- `warden done <name-or-id>`

**No code changes needed** in these commands — they already call `client.Get(nameOrID)`, which now internally calls `store.GetByNameOrID()`.

#### `warden status` output

Add the name field to the detail view in `newStatusCmd()`:

**Before:**
```
id:         agent-a1b2
type:       development
status:     working
...
```

**After:**
```
id:         agent-a1b2
name:       my-build
type:       development
status:     working
...
```

If name is empty, show `name:       —`

---

### 5. MCP Layer

#### `spawn_agent` tool

Add `name` field to `spawnArgs` in `internal/mcp/server.go`:

```go
type spawnArgs struct {
    Type       string `json:"type,omitempty" jsonschema:"task type: development|analysis|spike|pr-review|code|docs|website|debug-ci|tests|other"`
    Ticket     string `json:"ticket,omitempty" jsonschema:"optional Jira ticket; becomes the session id when present"`
    Name       string `json:"name,omitempty" jsonschema:"optional human-friendly name (max 32 chars, alphanumeric + hyphens/underscores)"`
    Repo       string `json:"repo,omitempty" jsonschema:"absolute path to the repo (managed-worktree mode)"`
    // ... rest unchanged
}
```

**Usage example (MCP tool call):**
```json
{
  "name": "spawn_agent",
  "arguments": {
    "prompt": "build the API",
    "name": "api-builder"
  }
}
```

Pass `args.Name` to `client.Spawn()` via `SpawnParams.Name`.

#### Error responses

When name validation fails, return error text in the tool result:

```go
if errors.Is(err, store.ErrNameExists) {
    return textResult(fmt.Sprintf("agent name '%s' is already in use by another active session", args.Name)), nil
}
if errors.Is(err, store.ErrInvalidName) {
    return textResult("invalid name: must be 1-32 alphanumeric chars, hyphens, or underscores"), nil
}
```

#### Lookup tools

Tools that accept agent identifiers now support name-based lookup:

- `get_agent` — `ticketArgs.Ticket` is really "nameOrID"
- `send_to_agent` — `sendArgs.Ticket` accepts names
- `get_agent_output` — `outputArgs.Ticket` accepts names
- `terminate_agent` — `forceArgs.Ticket` accepts names
- `delete_agent` — `deleteToolArgs.Ticket` accepts names
- `restore_agent` — `ticketArgs.Ticket` accepts names
- `approve` — `approveArgs.Ticket` accepts names

**No parameter renames needed** — the "ticket" field is semantically "agent identifier" and already flows through `client.Get(nameOrID)`.

#### Tool result display

For `get_agent` and `list_agents`, include the name field in JSON output:

```json
{
  "id": "agent-a1b2",
  "name": "my-build",
  "type": "development",
  "status": "working",
  ...
}
```

**Why?** Orchestrator agents can see and reference names when querying agent state.

---

### 6. TUI Layer

#### Agent list display

Modify `renderItemLine()` in `internal/tui/list.go` to show name before ID:

**Before:**
```
agent-a1b2      working     42k      5m   [feat-x]
```

**After:**
```
my-build        agent-a1b2      working     42k      5m   [feat-x]
—               agent-c3d4      idle        —        2h   —
```

**Column format:** `%-15s %-14s ...` (name 15 chars, ID 14 chars)

When `s.Name` is empty, render `—` with `stMuted` style.

#### Detail overlay

Update `detailBody()` to show the name:

**Header title:**
```
─────────────────────────────────────────
         agent-a1b2 (my-build)
─────────────────────────────────────────
```

When name is present, show `id (name)` in the header. Otherwise just `id`.

**Summary section:**
```
id:         agent-a1b2
name:       my-build
type:       development
status:     working
...
```

Add a `name:` row after `id:`. If empty, show `name:       —`.

#### No spawn UI changes

The TUI spawn prompt (`n` key) doesn't need name support in this iteration. Users spawn named agents via CLI or MCP. The TUI can be enhanced later by:
1. Adding a "Name (optional):" text input field to the spawn dialog
2. Passing the name through the spawn flow

**Why defer this?** The spawn dialog is already complex (type selection, dir input, prompt textarea). Adding name support is a nice-to-have that can be layered on once the core feature is validated via CLI/MCP.

---

### 7. Testing Strategy

#### Unit tests

**Store layer (`internal/store/file_test.go`):**
- Name validation: valid formats pass, invalid formats return `ErrInvalidName`
- Length validation: 1-32 chars pass, 0 or 33+ fail
- Uniqueness: inserting duplicate name returns `ErrNameExists`
- Case sensitivity: "foo" and "Foo" are different names
- `GetByNameOrID`: name match takes precedence over ID match
- `GetByNameOrID`: falls back to ID when no name matches
- `GetByNameOrID`: returns `ErrNotFound` when neither matches
- Empty names: allowed and don't conflict with each other

**Client layer (`internal/client/client_test.go`):**
- `SpawnParams.Name` is passed through POST `/spawn`
- `Get(nameOrID)` calls the correct endpoint

**CLI layer (`internal/cli/lifecycle_test.go`, `internal/cli/sessions_test.go`):**
- `--name` flag is parsed and passed to `Spawn()`
- Error messages for `ErrNameExists` and `ErrInvalidName` are user-friendly
- `ls` output includes NAME column
- `status` output includes name field

**MCP layer (`internal/mcp/server_test.go`):**
- `spawn_agent` tool accepts `name` argument
- Name validation errors are returned as tool results
- `list_agents` JSON includes name field

#### Integration tests

**End-to-end flows:**
1. Spawn agent with `--name`, verify it appears in `warden ls`
2. Attach by name: `warden attach my-agent` works
3. Send by name: `warden send my-agent "hello"` works
4. Duplicate name: `warden start --name foo` twice fails on second attempt
5. Name persistence: stop daemon, restart, name survives
6. Case sensitivity: spawn "Foo" and "foo" as different agents
7. Backward compatibility: spawn without `--name`, all commands still work

---

## Backward Compatibility

- **Session JSON**: `name` field is `omitempty`, so existing sessions without names remain unchanged
- **Store interface**: New method `GetByNameOrID` doesn't break existing callers of `Get(id)`
- **CLI**: `--name` flag is optional; omitting it works exactly as before
- **MCP**: `name` field in `spawnArgs` is optional
- **TUI**: Agents without names display `—` in the NAME column

**Migration:** No data migration needed. Existing sessions load with `Name: ""` (zero value).

---

## Future Enhancements

### Renaming agents
Add `warden rename <name-or-id> <new-name>` command:
- Validates new name (format, length, uniqueness)
- Updates `Session.Name` via new `Store.UpdateName()` method
- Maintains name immutability guarantee unless user explicitly renames

### TUI spawn dialog
Enhance `n` key spawn flow to accept optional name input before launching.

### Name auto-completion
Add shell completion for agent names in `warden attach`, `warden send`, etc.

### Name-based filtering
Support `warden ls --name <pattern>` to filter agents by name glob/regex.

### Web dashboard
Show name prominently on agent tiles and in detail views.

---

## Open Questions

None — all requirements clarified during brainstorming.

---

## Summary

This design adds optional, user-friendly names to warden agents while maintaining full backward compatibility. Names are:

- **Optional**: empty names are valid (default behavior unchanged)
- **Unique**: among active sessions only (case-sensitive)
- **Immutable**: set at spawn, never changed
- **First-class**: displayed prominently and used for lookups

The implementation touches five layers (store, client, CLI, MCP, TUI) but keeps changes localized and testable. Name-first lookup is centralized in the store layer, ensuring consistency across all interfaces.
