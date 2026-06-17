# Auto-Approval Pub-Sub System Design

**Date:** 2026-06-17  
**Status:** Approved  
**Author:** Claude Code (Sonnet 4.5)

## Problem Statement

Auto-approval currently only triggers on status transitions to `waiting_for_input`. When an agent is already in `waiting_for_input` and a new prompt appears (pane changes but status stays the same), auto-approval is not triggered. This causes prompts to sit indefinitely despite `WARDEN_AUTO_APPROVE=on` being configured.

**Evidence from logs:**
```
2026/06/17 01:23:54 auto-approved agent-03ecf506 -> option 1: Yes
2026/06/17 01:24:54 auto-approved agent-03ecf506 -> option 1: Yes
[agent shows new prompt while still in waiting_for_input status]
[no auto-approval triggered - prompt sits indefinitely]
```

## Goals

1. **Fix the bug:** Auto-approve prompts even when no status transition occurs
2. **Non-blocking:** Never stall the poller tick loop
3. **Simple:** Use Go channels (ephemeral is acceptable)
4. **Reliable:** Handle edge cases gracefully (duplicates, channel full, state changes)

## Architecture Overview

Add a dedicated approval event channel to the poller, allowing auto-approval to react to prompts asynchronously without relying solely on status transitions.

### Key Components

1. **ApprovalEvent** - Carries session snapshot + pane content
2. **ApprovalEventChan** - Buffered Go channel (capacity 100)
3. **Approval worker goroutine** - Consumes events and calls `tryAutoApprove`
4. **Event publishing** - Two triggers:
   - Status transition to `waiting_for_input` (existing path)
   - Pane change while already in `waiting_for_input` (new path)

### Flow Diagram

```
Poller tick → Detect pane change + waiting_for_input → Publish ApprovalEvent
                                                              ↓
                                                       Channel buffer (100)
                                                              ↓
                                                   Approval worker goroutine
                                                              ↓
                                                    tryAutoApprove(session, pane)
```

## Data Structures

### New Types

**File:** `main/internal/poller/poller.go`

```go
// ApprovalEvent represents a potential auto-approval opportunity.
type ApprovalEvent struct {
    Session *store.Session  // snapshot at event time
    Pane    string          // pane content that triggered the event
}
```

### Changes to Poller Struct

```go
type Poller struct {
    // ... existing fields ...
    
    // ApprovalEvents is a buffered channel for approval opportunities.
    // Published when: (1) status transitions to waiting_for_input, OR
    // (2) pane changes while already in waiting_for_input.
    // Consumed by the approval worker goroutine.
    ApprovalEvents chan ApprovalEvent
}
```

### Constructor Changes

```go
func New(d Deps, stuckAfter time.Duration) *Poller {
    return &Poller{
        // ... existing fields ...
        ApprovalEvents: make(chan ApprovalEvent, 100),  // buffer size
    }
}
```

**Buffer size reasoning:**
- 100 events handles bursts when many agents prompt simultaneously
- Non-blocking publish means we never stall the poller tick
- If buffer fills (unlikely), events are dropped with logging (graceful degradation)

## Event Publishing

### Trigger 1: Status Transition to waiting_for_input

**Location:** `poller.tick()` around line 257

**Change:** Replace direct `tryAutoApprove()` call with event publishing

```go
// Around line 257 in poller.go
if next == store.StatusWaitingForInput && pane != "" {
    // Publish event instead of calling tryAutoApprove directly
    p.publishApprovalEvent(s, pane)
}
```

### Trigger 2: Pane Change While Already waiting_for_input

**Location:** `poller.tick()` after pane change detection (around line 238)

**New code:**

```go
// After pane change detection (around line 238)
if paneChanged {
    _ = p.deps.UpdatePane(ctx, s.ID, excerpt)
    changed = true
    
    // NEW: publish approval event if already waiting
    if s.Status == store.StatusWaitingForInput && pane != "" {
        p.publishApprovalEvent(s, pane)
    }
}
```

### Publishing Method

**New method:**

```go
// publishApprovalEvent sends an event to the approval worker.
// Non-blocking: if the channel is full, the event is dropped (logged).
func (p *Poller) publishApprovalEvent(s *store.Session, pane string) {
    select {
    case p.ApprovalEvents <- ApprovalEvent{Session: s, Pane: pane}:
        // Event queued successfully
    default:
        // Channel full - drop event and log
        log.Printf("poller: approval event dropped for %s (channel full)", s.ID)
    }
}
```

**Key behaviors:**
- Non-blocking select ensures poller tick never stalls
- Dropped events are logged (helps diagnose capacity issues)
- Both triggers share the same publishing logic (DRY)

## Approval Worker Goroutine

### Worker Implementation

**New method in `poller.go`:**

```go
// runApprovalWorker consumes approval events and attempts auto-approval.
// Runs until ctx is cancelled.
func (p *Poller) runApprovalWorker(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case event := <-p.ApprovalEvents:
            p.tryAutoApprove(ctx, event.Session, event.Pane)
        }
    }
}
```

### Starting the Worker

**Changes to `Poller.Run()`:**

```go
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
    // Start approval worker
    p.wg.Add(1)
    go func() {
        defer p.wg.Done()
        p.runApprovalWorker(ctx)
    }()
    
    // Existing tick loop
    t := time.NewTicker(interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            p.wg.Wait()  // drain worker + summarizers
            return
        case <-t.C:
            if err := p.tick(ctx); err != nil {
                log.Printf("poller tick: %v", err)
            }
        }
    }
}
```

**Key behaviors:**
- Worker tracked by existing `p.wg` for clean shutdown
- Consumes events sequentially (one at a time)
- Gracefully stops on context cancellation
- Reuses existing `tryAutoApprove()` method (no duplication)

## Error Handling & Edge Cases

### Edge Case 1: Duplicate Events for Same Prompt

**Problem:** If the pane doesn't change after auto-approval sends a key, the next tick might re-publish the same prompt.

**Solution:** `tryAutoApprove()` already handles this gracefully:
- Parses the pane content fresh each time
- If the prompt is unrecognized or already gone, logs "auto-approve skipped"
- Idempotent - safe to call multiple times on the same prompt

### Edge Case 2: Channel Buffer Full

**Problem:** 100+ agents all prompt simultaneously.

**Solution:**
- Non-blocking publish drops the event (logged)
- Manual approval still works (approval inbox remains functional)
- User can still approve via TUI/CLI/MCP
- Logged: `"poller: approval event dropped for %s (channel full)"`

### Edge Case 3: Session State Changed Between Event Publish and Consumption

**Problem:** Session transitions away from `waiting_for_input` before worker processes event.

**Solution:**
- `tryAutoApprove()` checks `AutoApproveGlobal` and `session.AutoApprove` flags
- If session is no longer waiting, `SendKeys()` is harmless (tmux handles gracefully)
- Approval parser will fail if prompt is gone (safe no-op)

### Edge Case 4: Daemon Restart

**Problem:** Buffered events lost.

**Solution:**
- Acceptable - prompts are ephemeral, user agreed to this trade-off
- After restart, poller tick will detect `waiting_for_input` status and re-publish
- User can always manually approve via TUI/CLI/MCP

### Error Logging

All error conditions are logged for observability:

- Channel full: `"poller: approval event dropped for %s (channel full)"`
- Auto-approve skip: `"auto-approve skipped for %s: unrecognized prompt"` (existing)
- Auto-approve success: `"auto-approved %s -> option 1: %s"` (existing)

## Testing Strategy

### Unit Tests

**Event Publishing Tests** (`main/internal/poller/poller_test.go`):
- Pane change + `waiting_for_input` status → verify event published
- Status transition to `waiting_for_input` → verify event published
- Channel full → verify event dropped + logged
- Pane change + non-waiting status → verify no event published

**Worker Tests** (`main/internal/poller/poller_test.go`):
- Publish event → verify `tryAutoApprove()` called with correct session/pane
- Context cancelled → verify worker stops gracefully
- Multiple events → verify processed sequentially
- Use existing `FakeDeps` pattern for mocking

### Integration Test

**End-to-end scenario:**
1. Spawn supervised agent with `WARDEN_AUTO_APPROVE=on`
2. Trigger first prompt → verify auto-approved
3. Trigger second prompt (while still `waiting_for_input`) → verify auto-approved
4. Validates the bug fix works end-to-end

### Manual Testing

**Validation steps:**
1. Run daemon with `WARDEN_AUTO_APPROVE=on`
2. Spawn agent that prompts multiple times sequentially (e.g., find -exec sed)
3. Verify all prompts auto-approved (check `/tmp/warden.daemon.err` logs)
4. Verify no "auto-approve skipped" for recognized prompts
5. Verify agent completes work without manual intervention

## Implementation Plan

See separate implementation plan in `docs/superpowers/plans/2026-06-17-auto-approval-pubsub-plan.md` (generated by writing-plans skill).

## Files Modified

1. `main/internal/poller/poller.go` - Add ApprovalEvent, channel, worker, publishing logic
2. `main/internal/poller/poller_test.go` - Add unit tests for new behavior

## Non-Goals

- **Persistence:** Events are ephemeral (lost on daemon restart)
- **Distributed:** Single-process daemon only
- **General event bus:** Focused on auto-approval, not reusable for other features
- **Backpressure:** Channel full = drop events (acceptable degradation)

## Future Enhancements

If this pattern proves useful, consider:
- Generalize to an event bus for other features (logging, metrics, debugging)
- Add metrics (events published, dropped, processed)
- Configurable buffer size via `WARDEN_APPROVAL_EVENT_BUFFER`
- Per-agent auto-approval policies (allow/deny lists)
