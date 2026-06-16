# Auto-Approve Feature — Design

**Date:** 2026-06-16
**Status:** Approved (brainstorm), pending implementation plan

## Problem

The existing approvals inbox feature (2026-06-03-approvals-inbox-design.md) allows users to manually approve tool-permission prompts from the CLI or TUI without attaching to each agent. However, for fully autonomous agent workflows or unattended pipelines, even manual approval interrupts the flow.

Users need a way to automatically approve yes/no prompts so agents can proceed without human intervention. This is particularly useful for:
- Long-running pipelines that spawn multiple agents
- Overnight/unattended agent workflows
- Development workflows where the user trusts all tool permissions in a specific directory
- Batch processing scenarios where prompts are expected and should auto-proceed

## Goals

- Automatically approve recognized yes/no prompts when enabled
- Support global default setting via environment variable
- Support per-agent override that can be toggled while agent is running
- Maintain safety: only auto-approve recognized prompts, always select option 1
- Reuse existing approval parsing and SendKeys infrastructure

## Non-Goals

- Auto-approving multi-select prompts (skip, wait for manual intervention)
- Auto-approving text-entry fields (skip, wait for manual intervention)
- Auto-approving unrecognized/freeform prompts (skip, wait for manual intervention)
- Configurable option selection (always option 1, not configurable per prompt type)
- Retry logic for failed auto-approvals (if SendKeys fails, leave for manual handling)

## Requirements

### R1: Global Configuration
- `WARDEN_AUTO_APPROVE` environment variable controls default behavior
- Off by default (opt-in safety), enabled with `1/on/true`
- Read at daemon startup in `config.Load()`
- Requires `WARDEN_APPROVALS=on` (auto-approve is an extension of approvals feature)

### R2: Per-Agent Override
- Each agent session has an `AutoApprove` boolean field
- Default value inherits from global `WARDEN_AUTO_APPROVE` at spawn time
- Persisted in session store (survives daemon restarts)
- Modifiable while agent is running via CLI command

### R3: CLI Control
- `warden auto-approve <agent-id> on` - enable auto-approve for specific agent
- `warden auto-approve <agent-id> off` - disable auto-approve for specific agent
- Command validates session exists before updating
- Returns confirmation of new state

### R4: Auto-Approval Logic
- Triggered in poller's `tick()` after detecting `StatusWaitingForInput`
- Only attempts auto-approval if:
  - `WARDEN_APPROVALS` is enabled (base feature gate)
  - Global `WARDEN_AUTO_APPROVE` is enabled OR session's `AutoApprove` is true
  - `approval.Parse()` returns `ok=true` (recognized prompt)
  - Parsed options list has at least one option
- Always selects option 1 (the first option)
- Logs every auto-approval attempt (success/failure) for auditing

### R5: Safety Guards
- Skip auto-approval if `approval.Parse()` returns `ok=false`
- Skip if options list is empty or malformed
- Skip if `SendKeys()` fails (log error, prompt remains for manual handling)
- Never retry failed auto-approvals automatically
- Session status remains `StatusWaitingForInput` if auto-approval fails

## Architecture

### Data Model Changes

**`internal/store/types.go`:**
```go
type Session struct {
    // ... existing fields ...
    AutoApprove bool `json:"auto_approve"` // per-agent auto-approve override
}
```

**Database Migration:**
- Add `auto_approve BOOLEAN DEFAULT 0` column to sessions table
- Backfill existing sessions with `false` (conservative default)

**`internal/config/config.go`:**
```go
type Config struct {
    // ... existing fields ...
    AutoApproveEnabled bool // WARDEN_AUTO_APPROVE setting
}
```

### Auto-Approval Flow

```
┌─────────────────────────────────────────────────────────────┐
│ Poller Tick Loop                                            │
├─────────────────────────────────────────────────────────────┤
│ 1. Classify session status from pane capture                │
│ 2. If new status = StatusWaitingForInput:                   │
│    a. Update session status in store                        │
│    b. Check if auto-approve should trigger:                 │
│       - WARDEN_APPROVALS enabled?                           │
│       - (Global WARDEN_AUTO_APPROVE OR session.AutoApprove)?│
│    c. If yes, call tryAutoApprove(session, pane)            │
└─────────────────────────────────────────────────────────────┘
                           │
                           ▼
┌─────────────────────────────────────────────────────────────┐
│ tryAutoApprove(session, pane)                               │
├─────────────────────────────────────────────────────────────┤
│ 1. Parse pane with approval.Parse()                         │
│ 2. If !ok or len(Options) == 0:                             │
│    - Log "skipped: unrecognized prompt"                     │
│    - Return (leave waiting_for_input)                       │
│ 3. Send "1" via SendKeys(session.TmuxSession, "1")          │
│ 4. If SendKeys fails:                                       │
│    - Log error                                              │
│    - Return (leave waiting_for_input)                       │
│ 5. Log success: "auto-approved {session.ID} -> option 1"    │
│ 6. Notify SSE subscribers (prompt was answered)             │
└─────────────────────────────────────────────────────────────┘
```

### Implementation Components

**1. Config (`internal/config/config.go`)**
- Add `AutoApproveEnabled bool` to `Config` struct
- Add `autoApproveEnabled()` helper (off by default, on for `1/on/true`)
- Read `WARDEN_AUTO_APPROVE` (with legacy `AGENTCTL_AUTO_APPROVE` fallback) in `Load()`

**2. Store (`internal/store/types.go`, `internal/store/sqlite.go`)**
- Add `AutoApprove bool` field to `Session` struct
- Add database migration to create `auto_approve` column
- Update `Insert()` to set default `AutoApprove` from config or spawn request
- Update `Update()` to allow changing `AutoApprove`

**3. Poller (`internal/poller/poller.go`)**
- Add `autoApproveGlobal bool` field to `Poller` struct (set from config)
- Add `lifecycle` interface dependency for `SendKeys()`
- In `tick()`, after detecting `StatusWaitingForInput`, call `tryAutoApprove()`
- Add `tryAutoApprove(ctx, session, pane)` function:
  ```go
  func (p *Poller) tryAutoApprove(ctx context.Context, s *store.Session, pane string) {
      // Check if auto-approve enabled (global OR per-session)
      if !p.autoApproveGlobal && !s.AutoApprove {
          return
      }
      
      // Parse the approval
      a, ok := approval.Parse(pane)
      if !ok || len(a.Options) == 0 {
          log.Printf("auto-approve skipped for %s: unrecognized prompt", s.ID)
          return
      }
      
      // Send option 1
      if err := p.deps.SendKeys(ctx, s.TmuxSession, "1"); err != nil {
          log.Printf("auto-approve failed for %s: %v", s.ID, err)
          return
      }
      
      log.Printf("auto-approved %s -> option 1: %s", s.ID, a.Options[0])
      if p.OnChange != nil {
          p.OnChange()
      }
  }
  ```

**4. Poller Deps (`internal/poller/poller.go`, `internal/daemon/poller_deps.go`)**
- Add `SendKeys(ctx context.Context, tmuxSession, keys string) error` to `Deps` interface
- Implement in `pollerDeps` by delegating to `lifecycle.SendKeys()`

**5. Daemon Routes (`internal/daemon/api.go`, `internal/daemon/lifecycle_routes.go`)**
- Add `PATCH /sessions/{id}/auto-approve` route
- Add handler `handleSetAutoApprove(w, r)`:
  ```go
  type SetAutoApproveRequest struct {
      Enabled bool `json:"enabled"`
  }
  
  func (s *Server) handleSetAutoApprove(w http.ResponseWriter, r *http.Request) {
      id := chi.URLParam(r, "id")
      var req SetAutoApproveRequest
      if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
          writeErr(w, http.StatusBadRequest, "bad json")
          return
      }
      
      // Update session auto_approve field
      if err := s.store.UpdateAutoApprove(r.Context(), id, req.Enabled); err != nil {
          if errors.Is(err, store.ErrNotFound) {
              writeErr(w, http.StatusNotFound, "session not found")
              return
          }
          writeErr(w, http.StatusInternalServerError, err.Error())
          return
      }
      
      s.notify()
      writeJSON(w, http.StatusOK, map[string]bool{"auto_approve": req.Enabled})
  }
  ```

**6. Store Update Method (`internal/store/sqlite.go`)**
- Add `UpdateAutoApprove(ctx context.Context, id string, enabled bool) error` method

**7. CLI Command (`internal/cli/auto_approve.go`, new file)**
```go
package cli

import (
    "fmt"
    "github.com/spf13/cobra"
)

func newAutoApproveCmd() *cobra.Command {
    return &cobra.Command{
        Use:   "auto-approve <agent-id> <on|off>",
        Short: "Enable or disable auto-approval for an agent's prompts",
        Args:  cobra.ExactArgs(2),
        RunE: func(cmd *cobra.Command, args []string) error {
            id := args[0]
            mode := args[1]
            
            var enabled bool
            switch mode {
            case "on", "1", "true":
                enabled = true
            case "off", "0", "false":
                enabled = false
            default:
                return fmt.Errorf("mode must be 'on' or 'off', got %q", mode)
            }
            
            c := clientFor(cmd)
            if err := c.SetAutoApprove(cmd.Context(), id, enabled); err != nil {
                return err
            }
            
            status := "disabled"
            if enabled {
                status = "enabled"
            }
            fmt.Fprintf(cmd.OutOrStdout(), "auto-approve %s for %s\n", status, id)
            return nil
        },
    }
}
```

**8. CLI Client (`internal/cli/client.go`)**
- Add `SetAutoApprove(ctx context.Context, id string, enabled bool) error` method

**9. CLI Root (`internal/cli/root.go`)**
- Register `auto-approve` command in `newRootCmd()`

### Logging & Observability

All auto-approval events are logged for debugging and auditing:

```
[INFO] auto-approved abc123 -> option 1: Yes
[WARN] auto-approve skipped for def456: unrecognized prompt
[ERROR] auto-approve failed for ghi789: send keys: session not responding
```

These logs are written to the daemon's standard output (captured by systemd or manual runs).

Future enhancement: expose auto-approval events via metrics endpoint (`/metrics`) for monitoring dashboards.

## Behavior Matrix

| Global Setting | Session Setting | Prompt Type | Behavior |
|---------------|----------------|-------------|----------|
| off | off | yes/no | Manual approval required |
| off | on | yes/no | Auto-approved (option 1) |
| on | off | yes/no | Manual approval required |
| on | on | yes/no | Auto-approved (option 1) |
| on | on | multi-select | Skipped, manual required |
| on | on | text-entry | Skipped, manual required |
| on | on | unrecognized | Skipped, manual required |
| on | on | parse failure | Skipped, manual required |

## Testing Strategy

**Unit Tests:**
- `internal/config/config_test.go`: Test `WARDEN_AUTO_APPROVE` parsing (on/off/1/0/true/false)
- `internal/store/sqlite_test.go`: Test `AutoApprove` field persistence and `UpdateAutoApprove()`
- `internal/poller/poller_test.go`: Test `tryAutoApprove()` logic with mock deps

**Integration Tests:**
- Spawn agent with global auto-approve enabled, verify prompts auto-answered
- Spawn agent with global disabled, toggle per-agent on, verify auto-approval works
- Auto-approve with unrecognized prompt, verify session stays `waiting_for_input`
- SendKeys failure, verify session stays `waiting_for_input` and error logged

**Manual Tests:**
- End-to-end: spawn agent, trigger tool permission, verify auto-approval
- CLI: toggle auto-approve on/off while agent running, verify takes effect on next prompt
- Daemon restart: verify per-agent auto-approve setting survives restart

## Migration Path

**Phase 1: Core Implementation**
1. Add config, store schema, and database migration
2. Implement poller auto-approval logic
3. Add daemon API route
4. Add CLI command

**Phase 2: Observability**
1. Add structured logging for auto-approval events
2. Expose metrics (auto-approvals attempted/succeeded/failed)

**Phase 3: UI Integration (future)**
1. TUI: show auto-approve status in agent detail view
2. TUI: hotkey to toggle auto-approve for selected agent
3. Web UI: toggle switch in agent detail panel

## Security & Safety Considerations

**Safety First:**
- Off by default (opt-in)
- Only recognizes yes/no prompts (skips anything complex)
- Always option 1 (predictable behavior)
- Never retries (fail-safe to manual)

**Audit Trail:**
- All auto-approvals logged with session ID and selected option
- Failed attempts logged with reason
- Logs are timestamped and searchable

**Escape Hatch:**
- User can disable globally with `WARDEN_AUTO_APPROVE=off`
- User can disable per-agent with `warden auto-approve <id> off`
- Disabling approvals feature (`WARDEN_APPROVALS=off`) disables auto-approve too

## Future Enhancements (Out of Scope)

1. **Allowlist/Denylist:** Configure which tools can be auto-approved (e.g., "always auto-approve Read, never auto-approve Bash rm")
2. **Option Selection Rules:** Configure which option to select based on prompt type (e.g., "option 2 for don't-ask-again")
3. **Notification on Auto-Approve:** Send push notification when auto-approval occurs
4. **Auto-Approve Statistics:** Dashboard showing how many prompts were auto-approved vs. manual
5. **Time-Limited Auto-Approve:** Enable auto-approve for a session only for N minutes

## Open Questions

None. Design is ready for implementation planning.
