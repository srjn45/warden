---
title: Session Limit Detection and Auto-Resume
date: 2026-06-14
status: draft
author: Claude Code (via brainstorming)
---

# Session Limit Detection and Auto-Resume

## Overview

Warden will automatically detect when a Claude Code agent hits API rate limits, pause it with a new `rate_limited` status, and intelligently schedule resume attempts based on parsed restore timestamps.

**Key Features:**
- Pane-based detection of rate limit errors (reuses existing poller infrastructure)
- New `StatusRateLimited` visible in CLI/TUI/web/MCP
- Intelligent scheduling: parse restore time from error → schedule resume at `time + 1min buffer`
- Fallback retry: 30-min intervals when timestamp parsing fails
- Persistent state: survives daemon restarts
- No retry cap: self-healing until limit expires or user intervenes

## Problem Statement

When a Claude Code agent hits the API session limit, it currently:
- Appears stuck in `working` or `idle` status
- Requires manual detection and intervention
- Loses context if the user doesn't notice and resume in time

This wastes agent resources and breaks long-running workflows. Warden should detect limits automatically and resume agents when the limit expires.

## Requirements

### Must Have
1. Detect rate limits from Claude Code's pane output
2. Transition agent to new `rate_limited` status
3. Parse restore timestamp from error message when available
4. Schedule automatic resume at parsed time + 1min buffer
5. Fallback to 30-min retry interval when timestamp unavailable
6. On resume failure, re-parse error for updated restore time
7. Persist rate limit state across daemon restarts
8. Display rate limited status in all UIs (CLI, TUI, web, MCP)

### Nice to Have
1. Parse rate limit info from Claude transcript JSON (if/when available)
2. Aggregate rate limit patterns across agents
3. Proactive slowdown when approaching known limits
4. Optional desktop notifications (currently opted out)

## Architecture

### Component Overview

```
┌─────────────────────────────────────────────────────────────┐
│                         Daemon                              │
│                                                             │
│  ┌──────────────┐         ┌─────────────────────┐          │
│  │   Poller     │────────▶│ RateLimitScheduler  │          │
│  │              │         │                     │          │
│  │ - classify() │ OnTrans │ - OnTransition()    │          │
│  │ - detect     │ ition   │ - scheduleResume()  │          │
│  │   RateLimit()│         │ - attemptResume()   │          │
│  └──────┬───────┘         └──────────┬──────────┘          │
│         │                            │                     │
│         │                            │                     │
│         ▼                            ▼                     │
│  ┌──────────────────────────────────────────┐              │
│  │            FileStore                     │              │
│  │                                          │              │
│  │  Session {                               │              │
│  │    Status: rate_limited                  │              │
│  │    RateLimitedAt                         │              │
│  │    RateLimitRestoreAt                    │              │
│  │    RateLimitRetryCount                   │              │
│  │  }                                       │              │
│  └──────────────────────────────────────────┘              │
│                            │                               │
│                            │                               │
│                            ▼                               │
│                     ┌─────────────┐                        │
│                     │  Lifecycle  │                        │
│                     │             │                        │
│                     │  Restore()  │                        │
│                     └─────────────┘                        │
└─────────────────────────────────────────────────────────────┘
```

### New Components

#### 1. StatusRateLimited

**Location:** `internal/store/types.go`

```go
const (
    // ... existing statuses ...
    StatusRateLimited Status = "rate_limited"
)
```

- Non-terminal status (like `idle`, not like `done`)
- Visible in all UIs
- Color: yellow/amber (warning but recoverable)

#### 2. RateLimitScheduler

**Location:** `internal/daemon/ratelimit.go`

Manages scheduled resume attempts for rate-limited agents.

**Structure:**
```go
type RateLimitScheduler struct {
    life  Lifecycle
    store store.Store
    
    mu     sync.Mutex
    timers map[string]*time.Timer // sessionID → active timer
}
```

**Key Methods:**
- `OnTransition(sess, from, to)` - callback when agent enters `rate_limited`
- `scheduleResume(sessionID, at)` - creates Go timer for resume attempt
- `attemptResume(sessionID)` - fires when timer triggers
- `ReconstructTimers(ctx)` - rebuilds timers on daemon restart
- `CancelTimer(sessionID)` - cleanup when agent terminated

**Lifecycle:**
1. Wired into poller's `OnTransition` callback (like `Restarter`)
2. Receives notification when any agent → `rate_limited`
3. Parses restore time, creates timer
4. Timer fires → calls `Lifecycle.Restore()`
5. On success → status back to `spawning`
6. On failure → re-parse, reschedule

#### 3. Session Fields

**Location:** `internal/store/types.go`

```go
type Session struct {
    // ... existing fields ...
    
    RateLimitedAt       *time.Time `json:"rate_limited_at,omitempty"`       // when limit was first hit
    RateLimitRestoreAt  *time.Time `json:"rate_limit_restore_at,omitempty"` // scheduled resume time
    RateLimitRetryCount int        `json:"rate_limit_retry_count,omitempty"` // number of retry attempts
}
```

**Persistence:**
- Stored in session JSON file
- Survives daemon restarts
- Cleared on successful resume via `ClearRateLimit()`

### Modified Components

#### 1. Poller (`internal/poller/poller.go`)

**New Function: `detectRateLimit()`**

```go
// detectRateLimit checks if pane content indicates a rate limit hit.
// Returns (isLimited, restoreTime, ok) where:
//   - isLimited: true if rate limit detected
//   - restoreTime: parsed restore timestamp (zero if not found)
//   - ok: true if restoreTime was successfully parsed
func detectRateLimit(pane string) (bool, time.Time, bool) {
    // Pattern 1: Look for common rate limit keywords
    limitKeywords := []string{
        "rate limit",
        "usage limit",
        "session limit",
        "quota exceeded",
    }
    
    hasLimit := false
    for _, kw := range limitKeywords {
        if strings.Contains(strings.ToLower(pane), kw) {
            hasLimit = true
            break
        }
    }
    
    if !hasLimit {
        return false, time.Time{}, false
    }
    
    // Pattern 2: Try to parse restore time
    restoreTime, ok := parseRestoreTime(pane)
    return true, restoreTime, ok
}

func parseRestoreTime(pane string) (time.Time, bool) {
    // TODO: Implement once exact error message format is known
    // Will support multiple patterns:
    //   "Try again at 3:45 PM"
    //   "Available again at 15:45"
    //   "Reset at 2024-06-14 15:45:00"
    //   "retry_after: 1718380800" (unix timestamp)
    
    // Placeholder implementation
    return time.Time{}, false
}
```

**Modified: `classify()`**

```go
func classify(s *store.Session, pane string, sessionAlive bool, sinceUpdate, stuckAfter time.Duration) store.Status {
    if !sessionAlive {
        return store.StatusOrphaned
    }
    
    // Check for rate limit BEFORE other classifications
    // (prevents misclassification as waiting_for_input)
    if isLimited, _, _ := detectRateLimit(pane); isLimited {
        return store.StatusRateLimited
    }
    
    // Existing classification logic unchanged...
    if strings.Contains(pane, "esc to interrupt") {
        return store.StatusWorking
    }
    if strings.Contains(pane, "❯") || strings.Contains(pane, "Do you want") {
        return store.StatusWaitingForInput
    }
    if s.Status == store.StatusWorking && stuckAfter > 0 && sinceUpdate >= stuckAfter {
        return store.StatusIdle
    }
    return s.Status
}
```

**Why check rate limit first?**
- Rate limit errors might still show the prompt (`❯`) underneath
- Prevents misclassification as `waiting_for_input` or `idle`
- Terminal detection state - once limited, agent stays there until resumed

#### 2. Lifecycle (`internal/lifecycle/lifecycle.go`)

No changes needed - `Restore()` method already exists and will be reused.

May add helper for error parsing:
```go
func parseRestoreTimeFromError(err error) (time.Time, bool) {
    return parseRestoreTime(err.Error())
}
```

#### 3. Store (`internal/store/`)

**New Interface Methods:**

```go
// In internal/store/store.go
type Store interface {
    // ... existing methods ...
    
    // SetRateLimit records rate limit state and next resume time
    SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error
    
    // ClearRateLimit removes rate limit metadata (after successful resume)
    ClearRateLimit(ctx context.Context, id string) error
}
```

**Implementation:**

```go
// In internal/store/file.go
func (s *FileStore) SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error {
    return s.update(ctx, id, func(sess *Session) error {
        now := time.Now()
        if sess.RateLimitedAt == nil {
            sess.RateLimitedAt = &now
        }
        sess.RateLimitRestoreAt = &restoreAt
        sess.RateLimitRetryCount = retryCount
        
        // Append event for tracking
        sess.Events = append(sess.Events, Event{
            TS:     now,
            Type:   "rate-limit",
            Detail: fmt.Sprintf("scheduled resume at %s (retry %d)", 
                restoreAt.Format(time.RFC3339), retryCount),
        })
        
        return nil
    })
}

func (s *FileStore) ClearRateLimit(ctx context.Context, id string) error {
    return s.update(ctx, id, func(sess *Session) error {
        sess.RateLimitedAt = nil
        sess.RateLimitRestoreAt = nil
        sess.RateLimitRetryCount = 0
        
        sess.Events = append(sess.Events, Event{
            TS:     time.Now(),
            Type:   "rate-limit-resumed",
            Detail: "successfully resumed after rate limit",
        })
        
        return nil
    })
}
```

## Detection Logic

### Pattern Matching

The poller captures the tmux pane output every poll interval (already happens for context monitoring). The `detectRateLimit()` function scans for:

**Primary Keywords:**
- "rate limit"
- "usage limit"
- "session limit"
- "quota exceeded"

(Case-insensitive match)

**Time Parsing Patterns:**

Once the user provides the exact Claude Code error message, we'll implement regex patterns like:

```go
// Example patterns (actual implementation pending exact message format)
patterns := []struct {
    regex  *regexp.Regexp
    layout string
}{
    {
        regex:  regexp.MustCompile(`Try again at (\d{1,2}):(\d{2}) (AM|PM)`),
        layout: "3:04 PM",
    },
    {
        regex:  regexp.MustCompile(`Reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`),
        layout: "2006-01-02 15:04:05",
    },
    {
        regex:  regexp.MustCompile(`retry_after:\s*(\d+)`),
        layout: "unix", // special case: parse as unix timestamp
    },
}
```

**Sanity Checks:**

```go
func parseRestoreTime(pane string) (time.Time, bool) {
    t, ok := /* ... parsing logic ... */
    
    if !ok {
        return time.Time{}, false
    }
    
    // Sanity check: restore time shouldn't be in distant past/future
    now := time.Now()
    if t.Before(now.Add(-1*time.Hour)) || t.After(now.Add(24*time.Hour)) {
        // Likely parse error or clock skew
        return time.Time{}, false
    }
    
    return t, true
}
```

**Fallback Behavior:**

If keyword detected but time parsing fails:
- Still transition to `rate_limited` (correct status)
- Schedule retry in 30 minutes (fallback interval)
- Log warning with pane excerpt for debugging

## Scheduling & Retry Logic

### Initial Detection Flow

```
Poller captures pane
    ↓
detectRateLimit() → (isLimited=true, restoreTime, ok)
    ↓
classify() → StatusRateLimited
    ↓
UpdateStatusIf(current, StatusRateLimited)
    ↓
OnTransition callback → RateLimitScheduler.OnTransition()
    ↓
if ok && restoreTime.After(now):
    scheduleAt = restoreTime + 1min
else:
    scheduleAt = now + 30min
    ↓
store.SetRateLimit(sessionID, scheduleAt, retryCount=0)
    ↓
scheduleResume(sessionID, scheduleAt)
```

### Resume Attempt Flow

```go
func (r *RateLimitScheduler) scheduleResume(sessionID string, at time.Time) {
    delay := time.Until(at)
    if delay < 0 {
        delay = 0 // resume immediately if time already passed
    }
    
    r.mu.Lock()
    defer r.mu.Unlock()
    
    // Cancel existing timer if any
    if existing := r.timers[sessionID]; existing != nil {
        existing.Stop()
    }
    
    // Create new timer
    r.timers[sessionID] = time.AfterFunc(delay, func() {
        r.attemptResume(sessionID)
    })
}
```

### Retry Logic

```go
func (r *RateLimitScheduler) attemptResume(sessionID string) {
    ctx := context.Background()
    
    sess, err := r.store.Get(ctx, sessionID)
    if err != nil {
        return // session gone
    }
    
    // Only resume if still rate limited
    if sess.Status != store.StatusRateLimited {
        return
    }
    
    // Attempt resume (reuses existing Restore logic)
    err = r.life.Restore(ctx, sess)
    
    if err == nil {
        // SUCCESS: transition back to spawning
        _, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusSpawning)
        _ = r.store.ClearRateLimit(ctx, sess.ID)
        
        // Clean up timer
        r.mu.Lock()
        delete(r.timers, sess.ID)
        r.mu.Unlock()
        
        return
    }
    
    // FAILURE: check if error has new restore time
    errMsg := err.Error()
    newRestoreTime, ok := parseRestoreTime(errMsg)
    
    if ok && newRestoreTime.After(time.Now()) {
        // Found updated time - schedule for that + buffer (STOP RETRIES)
        scheduleAt := newRestoreTime.Add(1 * time.Minute)
        _ = r.store.SetRateLimit(ctx, sess.ID, scheduleAt, sess.RateLimitRetryCount+1)
        r.scheduleResume(sess.ID, scheduleAt)
        
        _ = r.store.AppendEvent(ctx, sess.ID, Event{
            Type:   "rate-limit-retry",
            Detail: fmt.Sprintf("parsed new restore time: %s", scheduleAt.Format(time.RFC3339)),
        })
    } else {
        // No time found - retry in 30min
        scheduleAt := time.Now().Add(30 * time.Minute)
        _ = r.store.SetRateLimit(ctx, sess.ID, scheduleAt, sess.RateLimitRetryCount+1)
        r.scheduleResume(sess.ID, scheduleAt)
        
        _ = r.store.AppendEvent(ctx, sess.ID, Event{
            Type:   "rate-limit-retry",
            Detail: fmt.Sprintf("no time parsed, retrying in 30m (attempt %d)", sess.RateLimitRetryCount+1),
        })
    }
}
```

**Key Points:**

1. **Intelligent vs. Blind Retry:**
   - If we parse a time on first detection → schedule for exactly that time (no retries needed)
   - If we parse a time on retry → stop the retry loop, schedule for parsed time
   - If we never parse a time → keep retrying every 30min until success

2. **No Retry Cap:**
   - Unlike auto-restart (which caps at 3 attempts), rate limits have no cap
   - Rationale: rate limits are temporary and external; infinite retry is correct behavior
   - User can always manually terminate if needed

3. **1-Minute Buffer:**
   - Parsed times get +1min added for safety
   - Prevents edge case where we resume exactly at reset and still hit limit

### Daemon Restart Handling

When the daemon restarts while agents are rate-limited:

```go
// Called in daemon startup (daemon.go)
func (r *RateLimitScheduler) ReconstructTimers(ctx context.Context) error {
    sessions, err := r.store.List(ctx)
    if err != nil {
        return err
    }
    
    for _, sess := range sessions {
        if sess.Status == store.StatusRateLimited && sess.RateLimitRestoreAt != nil {
            r.scheduleResume(sess.ID, *sess.RateLimitRestoreAt)
        }
    }
    
    return nil
}
```

**Behavior:**
- If scheduled time hasn't passed yet → timer recreated with remaining duration
- If scheduled time already passed → `time.Until()` returns negative → `delay = 0` → resume triggers immediately
- No state lost, seamless recovery

## Error Handling & Edge Cases

### Edge Case 1: Daemon Restart During Wait

**Scenario:** Agent rate-limited at 14:30, scheduled resume at 15:45, daemon restarts at 15:00

**Handling:**
- On startup, `ReconstructTimers()` reads all sessions
- Finds session with `Status=rate_limited` and `RateLimitRestoreAt=15:45`
- Creates timer with 45-min delay
- Resume happens on schedule

### Edge Case 2: User Manually Terminates Rate-Limited Agent

**Scenario:** User runs `warden done <id>` on rate-limited agent

**Handling:**
```go
// In Lifecycle.Terminate()
func (l *Lifecycle) Terminate(ctx context.Context, tmuxSession string) error {
    // ... existing termination logic ...
    
    // Clean up rate limit timer if exists
    if l.scheduler != nil {
        l.scheduler.CancelTimer(sessionID)
    }
    
    return nil
}

// In RateLimitScheduler
func (r *RateLimitScheduler) CancelTimer(sessionID string) {
    r.mu.Lock()
    defer r.mu.Unlock()
    
    if timer := r.timers[sessionID]; timer != nil {
        timer.Stop()
        delete(r.timers, sessionID)
    }
}
```

### Edge Case 3: Parse Failures

**Scenario:** Rate limit detected but timestamp in unknown format

**Handling:**
- `parseRestoreTime()` returns `(time.Time{}, false)`
- Status still changes to `rate_limited` (correct)
- Fallback to 30-min retry
- Warning logged: `"rate-limit: session abc123: could not parse restore time from pane"`
- Event recorded: `"rate-limit: could not parse restore time, retrying in 30m"`

### Edge Case 4: Resume Fails with Non-Rate-Limit Error

**Scenario:** Resume attempt gets network error, auth error, or other non-limit failure

**Handling:**
```go
if err != nil {
    errMsg := err.Error()
    
    // Try to parse as rate limit error first
    newRestoreTime, ok := parseRestoreTime(errMsg)
    if ok {
        // Still rate limited, reschedule
        // ... existing logic ...
    } else {
        // Different error (network, auth, etc.)
        // Transition to errored instead of retrying indefinitely
        _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusErrored)
        _ = r.store.AppendEvent(ctx, sess.ID, Event{
            Type:   "rate-limit-resume-failed",
            Detail: fmt.Sprintf("resume failed with non-limit error: %v", err),
        })
        
        // Clean up timer
        r.mu.Lock()
        delete(r.timers, sess.ID)
        r.mu.Unlock()
        
        return
    }
}
```

This prevents infinite retry loops when the problem isn't a rate limit.

### Edge Case 5: Agent Deleted While Timer Active

**Scenario:** Timer fires but session was deleted/archived

**Handling:**
```go
func (r *RateLimitScheduler) attemptResume(sessionID string) {
    sess, err := r.store.Get(ctx, sessionID)
    if err != nil {
        // Session gone (deleted, archived, or never existed)
        r.mu.Lock()
        delete(r.timers, sessionID)
        r.mu.Unlock()
        return
    }
    // ... rest of logic ...
}
```

Timer cleaned up automatically, no-op.

### Edge Case 6: Clock Skew / Invalid Parse Times

**Scenario:** Parsed time is nonsensical (distant past/future)

**Handling:**
```go
func parseRestoreTime(pane string) (time.Time, bool) {
    t, ok := /* ... parsing logic ... */
    
    if !ok {
        return time.Time{}, false
    }
    
    // Sanity check: restore time shouldn't be in distant past/future
    now := time.Now()
    if t.Before(now.Add(-1*time.Hour)) {
        // More than 1 hour in the past - likely parse error
        return time.Time{}, false
    }
    if t.After(now.Add(24*time.Hour)) {
        // More than 24 hours in future - unlikely for session limits
        return time.Time{}, false
    }
    
    return t, true
}
```

Treats invalid times as parse failures, falls back to 30-min retry.

### Edge Case 7: Multiple Agents Rate-Limited Simultaneously

**Scenario:** 5 agents all hit limit at same time

**Handling:**
- Each gets its own timer in `scheduler.timers` map
- All fire independently
- No interaction between agents
- Scheduler is thread-safe (mutex-protected map)

### Edge Case 8: Status Changes During Wait

**Scenario:** Agent is `rate_limited`, user manually resumes it before timer fires

**Handling:**
```go
func (r *RateLimitScheduler) attemptResume(sessionID string) {
    sess, err := r.store.Get(ctx, sessionID)
    // ...
    
    // Only resume if STILL rate limited
    if sess.Status != store.StatusRateLimited {
        r.mu.Lock()
        delete(r.timers, sessionID)
        r.mu.Unlock()
        return // no-op, user already handled it
    }
    
    // ... proceed with resume ...
}
```

Timer fires but no-ops if status already changed.

## UI & Display

### CLI (`warden ls`)

**List View:**

```
ID       NAME          STATUS          SUBJECT
abc123   feature-auth  rate_limited    Implementing OAuth flow
def456   bug-fix       working         Fixing login validation
ghi789   docs          rate_limited    Updating API docs
```

**Detail View (`warden status <id>`):**

```
Session: abc123 (feature-auth)
Type: development
Status: rate_limited
Subject: Implementing OAuth flow

Rate Limit Info:
  Limited At: 2026-06-14 14:30:00
  Resume At:  2026-06-14 15:45:00 (in 1h 15m)
  Retries:    0

Created: 2026-06-14 12:00:00
Updated: 2026-06-14 14:30:15
Branch: feature-auth-oauth
Worktree: /path/to/repo/.worktrees/feature-auth-abc123

Recent Events:
  14:30:15 rate-limit      scheduled resume at 2026-06-14T15:45:00Z (retry 0)
  14:29:58 status-change   working → rate_limited
  12:05:00 spawned         agent started
```

**Formatting:**

```go
// In CLI formatting code
func formatStatus(s store.Status) string {
    switch s {
    case store.StatusDone:
        return color.Green(string(s))
    case store.StatusWorking:
        return color.Blue(string(s))
    case store.StatusErrored:
        return color.Red(string(s))
    case store.StatusRateLimited:
        return color.Yellow(string(s)) // Amber/yellow for warning-but-recoverable
    // ... other statuses
    }
}

func formatRateLimitInfo(sess *store.Session) string {
    if sess.Status != store.StatusRateLimited {
        return ""
    }
    
    var lines []string
    lines = append(lines, "Rate Limit Info:")
    
    if sess.RateLimitedAt != nil {
        lines = append(lines, fmt.Sprintf("  Limited At: %s", sess.RateLimitedAt.Format(time.RFC3339)))
    }
    
    if sess.RateLimitRestoreAt != nil {
        until := time.Until(*sess.RateLimitRestoreAt)
        lines = append(lines, fmt.Sprintf("  Resume At:  %s (in %s)", 
            sess.RateLimitRestoreAt.Format(time.RFC3339),
            formatDuration(until)))
    }
    
    lines = append(lines, fmt.Sprintf("  Retries:    %d", sess.RateLimitRetryCount))
    
    return strings.Join(lines, "\n")
}
```

### TUI (Cockpit)

**Session List:**

```
┌─ Agents ────────────────────────────────────────────────────┐
│ ID      Name          Status        Subject                 │
├─────────────────────────────────────────────────────────────┤
│ abc123  feature-auth  rate_limited  Implementing OAuth      │ (yellow highlight)
│ def456  bug-fix       working       Fixing validation       │ (blue highlight)
│ ghi789  docs          rate_limited  Updating API docs       │ (yellow highlight)
└─────────────────────────────────────────────────────────────┘
```

**Detail Pane:**

```
┌─ Session: abc123 (feature-auth) ────────────────────────────┐
│ Status: rate_limited                                         │
│                                                              │
│ Rate Limited: 14:30:00                                       │
│ Resume At:    15:45:00 (in 1h 15m 23s)                      │ (live countdown)
│ Retries:      0                                              │
│                                                              │
│ Subject: Implementing OAuth flow                             │
│ Branch:  feature-auth-oauth                                  │
│                                                              │
│ Recent Activity:                                             │
│   14:30:15  rate-limit scheduled resume                      │
│   14:29:58  status → rate_limited                            │
│   12:05:00  spawned                                          │
└──────────────────────────────────────────────────────────────┘
```

**Live Countdown:**

The TUI's existing refresh loop (already handles live updates for pane excerpts) will update the countdown timer in real-time:

```go
// In TUI render logic
if sess.RateLimitRestoreAt != nil {
    until := time.Until(*sess.RateLimitRestoreAt)
    if until > 0 {
        fmt.Fprintf(w, "Resume At:    %s (in %s)\n", 
            sess.RateLimitRestoreAt.Format("15:04:05"),
            formatDuration(until)) // "1h 15m 23s" → "1h 15m 22s" → ...
    } else {
        fmt.Fprintf(w, "Resume At:    %s (resuming...)\n",
            sess.RateLimitRestoreAt.Format("15:04:05"))
    }
}
```

### Web Dashboard

**Session Card:**

```html
<div class="session-card rate-limited">
  <div class="status-badge yellow">rate_limited</div>
  <h3>feature-auth</h3>
  <p class="subject">Implementing OAuth flow</p>
  
  <div class="rate-limit-info">
    <div class="countdown">
      <span class="label">Resume in:</span>
      <span class="time" id="countdown-abc123">1h 15m 23s</span>
    </div>
    <div class="details">
      <span>Limited at 14:30:00</span>
      <span>Resume at 15:45:00</span>
      <span>Retries: 0</span>
    </div>
  </div>
  
  <div class="actions">
    <button onclick="attach('abc123')">Attach</button>
    <button onclick="terminate('abc123')">Terminate</button>
  </div>
</div>
```

**JavaScript Countdown:**

```javascript
// Live countdown timer (updates every second)
function updateCountdown(sessionId, restoreAt) {
  const el = document.getElementById(`countdown-${sessionId}`);
  const now = Date.now();
  const until = new Date(restoreAt) - now;
  
  if (until <= 0) {
    el.textContent = "resuming...";
    return;
  }
  
  const hours = Math.floor(until / 3600000);
  const mins = Math.floor((until % 3600000) / 60000);
  const secs = Math.floor((until % 60000) / 1000);
  
  el.textContent = `${hours}h ${mins}m ${secs}s`;
}

// Update every second for all rate-limited sessions
setInterval(() => {
  document.querySelectorAll('.session-card.rate-limited').forEach(card => {
    const sessionId = card.dataset.sessionId;
    const restoreAt = card.dataset.restoreAt;
    updateCountdown(sessionId, restoreAt);
  });
}, 1000);
```

**Event Timeline:**

```
Timeline:
  [14:30:15] rate-limit: scheduled resume at 15:45:00 (retry 0)
  [14:29:58] status: working → rate_limited
  [12:05:00] spawned
```

### MCP Tools

**`list_agents` Response:**

```json
{
  "agents": [
    {
      "id": "abc123",
      "name": "feature-auth",
      "status": "rate_limited",
      "subject": "Implementing OAuth flow",
      "type": "development",
      "rate_limited_at": "2026-06-14T14:30:00Z",
      "rate_limit_restore_at": "2026-06-14T15:45:00Z",
      "rate_limit_retry_count": 0,
      "created_at": "2026-06-14T12:00:00Z",
      "updated_at": "2026-06-14T14:30:15Z"
    }
  ]
}
```

**`get_agent` Response:**

```json
{
  "id": "abc123",
  "name": "feature-auth",
  "status": "rate_limited",
  "rate_limit_info": {
    "limited_at": "2026-06-14T14:30:00Z",
    "restore_at": "2026-06-14T15:45:00Z",
    "retry_count": 0,
    "seconds_until_resume": 4523
  },
  "events": [
    {
      "ts": "2026-06-14T14:30:15Z",
      "type": "rate-limit",
      "detail": "scheduled resume at 2026-06-14T15:45:00Z (retry 0)"
    },
    {
      "ts": "2026-06-14T14:29:58Z",
      "type": "status-change",
      "detail": "working → rate_limited"
    }
  ]
}
```

### Status Colors/Icons

Following existing warden conventions:

| Status          | Color        | Use Case                    |
|-----------------|--------------|----------------------------- |
| `done`          | Green        | Successfully completed       |
| `working`       | Blue         | Actively processing          |
| `idle`          | Gray         | Waiting for input/idle       |
| `errored`       | Red          | Crashed or failed            |
| `orphaned`      | Dark Red     | Tmux session died            |
| `rate_limited`  | Yellow/Amber | Hit API limit, auto-resuming |

**Rationale for Yellow:**
- Yellow/amber traditionally signals "warning but not critical"
- Agent isn't broken (red), just temporarily paused
- Will auto-recover (unlike `errored` which may need intervention)
- Visually distinct from blue (`working`) and gray (`idle`)

## Data Persistence

### Session JSON Structure

**Before Rate Limit:**

```json
{
  "id": "abc123",
  "name": "feature-auth",
  "status": "working",
  "subject": "Implementing OAuth flow",
  "created_at": "2026-06-14T12:00:00Z",
  "updated_at": "2026-06-14T14:29:58Z",
  "events": [
    {
      "ts": "2026-06-14T12:00:00Z",
      "type": "spawned",
      "detail": ""
    }
  ]
}
```

**After Rate Limit Detection:**

```json
{
  "id": "abc123",
  "name": "feature-auth",
  "status": "rate_limited",
  "subject": "Implementing OAuth flow",
  "created_at": "2026-06-14T12:00:00Z",
  "updated_at": "2026-06-14T14:30:15Z",
  "rate_limited_at": "2026-06-14T14:30:00Z",
  "rate_limit_restore_at": "2026-06-14T15:45:00Z",
  "rate_limit_retry_count": 0,
  "events": [
    {
      "ts": "2026-06-14T14:30:15Z",
      "type": "rate-limit",
      "detail": "scheduled resume at 2026-06-14T15:45:00Z (retry 0)"
    },
    {
      "ts": "2026-06-14T14:29:58Z",
      "type": "status-change",
      "detail": "working → rate_limited"
    },
    {
      "ts": "2026-06-14T12:00:00Z",
      "type": "spawned",
      "detail": ""
    }
  ]
}
```

**After Successful Resume:**

```json
{
  "id": "abc123",
  "name": "feature-auth",
  "status": "spawning",
  "subject": "Implementing OAuth flow",
  "created_at": "2026-06-14T12:00:00Z",
  "updated_at": "2026-06-14T15:46:05Z",
  "events": [
    {
      "ts": "2026-06-14T15:46:05Z",
      "type": "rate-limit-resumed",
      "detail": "successfully resumed after rate limit"
    },
    {
      "ts": "2026-06-14T14:30:15Z",
      "type": "rate-limit",
      "detail": "scheduled resume at 2026-06-14T15:45:00Z (retry 0)"
    },
    {
      "ts": "2026-06-14T14:29:58Z",
      "type": "status-change",
      "detail": "working → rate_limited"
    },
    {
      "ts": "2026-06-14T12:00:00Z",
      "type": "spawned",
      "detail": ""
    }
  ]
}
```

Note: `rate_limited_at`, `rate_limit_restore_at`, and `rate_limit_retry_count` fields are cleared after successful resume (via `ClearRateLimit()`).

### Event Types

New event types added:

| Event Type               | When                                | Detail Format                                               |
|--------------------------|-------------------------------------|-------------------------------------------------------------|
| `rate-limit`             | Limit detected, resume scheduled    | `"scheduled resume at <RFC3339> (retry N)"`                 |
| `rate-limit-retry`       | Resume attempt failed, rescheduling | `"parsed new restore time: <RFC3339>"` or `"no time parsed, retrying in 30m (attempt N)"` |
| `rate-limit-resumed`     | Resume successful                   | `"successfully resumed after rate limit"`                   |
| `rate-limit-resume-failed` | Resume failed with non-limit error | `"resume failed with non-limit error: <error>"`             |

### Backwards Compatibility

**Old Sessions:**
- New fields (`RateLimitedAt`, `RateLimitRestoreAt`, `RateLimitRetryCount`) use `omitempty` tag
- Existing session JSON files missing these fields unmarshal correctly (fields remain `nil`/`0`)
- No migration needed

**New Status Enum:**
- `StatusRateLimited` added to the `Valid()` check
- Old code that doesn't know about this status will treat it as unknown (but won't break)
- CLI/TUI/web all check `switch s.Status` with default cases

## Configuration

### Environment Variables

Tunable parameters (similar to auto-restart's `WARDEN_AUTO_RESTART_*`):

```bash
# Fallback retry interval when time parsing fails (default: 30m)
WARDEN_RATE_LIMIT_RETRY_INTERVAL=30m

# Safety buffer added to parsed restore time (default: 1m)
WARDEN_RATE_LIMIT_BUFFER=1m

# Enable/disable rate limit auto-resume entirely (default: true)
WARDEN_RATE_LIMIT_AUTO_RESUME=true
```

**Implementation:**

```go
// In daemon/ratelimit.go
type RateLimitScheduler struct {
    // ... existing fields ...
    
    retryInterval time.Duration
    buffer        time.Duration
    enabled       bool
}

func NewRateLimitScheduler(life Lifecycle, st store.Store) *RateLimitScheduler {
    return &RateLimitScheduler{
        life:          life,
        store:         st,
        timers:        make(map[string]*time.Timer),
        retryInterval: envDuration("WARDEN_RATE_LIMIT_RETRY_INTERVAL", 30*time.Minute),
        buffer:        envDuration("WARDEN_RATE_LIMIT_BUFFER", 1*time.Minute),
        enabled:       envBool("WARDEN_RATE_LIMIT_AUTO_RESUME", true),
    }
}

func (r *RateLimitScheduler) OnTransition(sess *store.Session, from, to store.Status) {
    if !r.enabled || to != store.StatusRateLimited {
        return
    }
    // ... rest of logic using r.retryInterval and r.buffer ...
}
```

**Disabling Auto-Resume:**

```bash
WARDEN_RATE_LIMIT_AUTO_RESUME=false warden daemon
```

When disabled:
- Poller still detects rate limits and transitions to `rate_limited` status
- Scheduler's `OnTransition` no-ops (no timers created)
- User must manually resume with `warden attach <id>` or via Lifecycle.Restore

Useful for debugging or environments where manual control is preferred.

## Implementation Plan

### Phase 1: Core Infrastructure

**Files to Modify:**
- `internal/store/types.go`
- `internal/store/store.go`
- `internal/store/file.go`

**Tasks:**
1. Add `StatusRateLimited` constant to status enum
2. Update `Status.Valid()` to include new status
3. Add Session fields:
   - `RateLimitedAt *time.Time`
   - `RateLimitRestoreAt *time.Time`
   - `RateLimitRetryCount int`
4. Add Store interface methods:
   - `SetRateLimit(ctx, id, restoreAt, retryCount) error`
   - `ClearRateLimit(ctx, id) error`
5. Implement methods in FileStore
6. Write unit tests for store operations

**Deliverable:** Session struct and store methods ready for use

---

### Phase 2: Detection Logic

**Files to Create/Modify:**
- `internal/poller/poller.go` (modify)
- `internal/poller/detect.go` (new - detection helpers)
- `internal/poller/poller_test.go` (modify)

**Tasks:**
1. Implement `detectRateLimit(pane string) (bool, time.Time, bool)`
   - Keyword matching for rate limit errors
   - Placeholder `parseRestoreTime()` (returns `false` until exact message known)
2. Modify `classify()` to check rate limit first
3. Write unit tests for detection logic:
   - Rate limit with parseable time
   - Rate limit without time
   - No rate limit
   - Edge cases (clock skew, invalid formats)
4. Add sanity checks for parsed times

**Deliverable:** Poller can detect rate limits and transition status

**Note:** `parseRestoreTime()` will be a placeholder until the user provides the exact Claude Code error message format. Tests will use mock messages.

---

### Phase 3: Scheduling Component

**Files to Create:**
- `internal/daemon/ratelimit.go` (new)
- `internal/daemon/ratelimit_test.go` (new)

**Files to Modify:**
- `internal/daemon/api.go` (wire scheduler into daemon)

**Tasks:**
1. Implement `RateLimitScheduler` struct
2. Implement core methods:
   - `OnTransition()` - callback when agent enters `rate_limited`
   - `scheduleResume()` - creates timer
   - `attemptResume()` - timer callback, handles success/failure/retry
   - `ReconstructTimers()` - daemon restart recovery
   - `CancelTimer()` - cleanup on termination
3. Wire scheduler into daemon:
   - Create instance in daemon startup
   - Register as poller `OnTransition` callback
   - Call `ReconstructTimers()` after daemon starts
4. Implement configuration:
   - Environment variable parsing
   - Default values
5. Write unit tests:
   - Scheduling logic (with mocked time)
   - Retry logic (success, still limited, other error)
   - Timer reconstruction
   - Multiple agents simultaneously

**Deliverable:** Scheduler component working and tested

---

### Phase 4: UI Updates

**Files to Modify:**
- `internal/cli/*.go` (status formatting, detail view)
- `internal/tui/*.go` (status colors, detail pane)
- `web/src/*.tsx` (if web dashboard exists)
- `internal/mcp/server.go` (MCP tool responses)

**Tasks:**
1. **CLI:**
   - Add yellow/amber color for `rate_limited` status
   - Extend `warden status` to show rate limit info
   - Format countdown ("in 1h 15m")
2. **TUI:**
   - Add status color highlighting
   - Detail pane shows rate limit metadata
   - Live countdown timer in refresh loop
3. **Web Dashboard:**
   - Session card shows `rate_limited` badge
   - Countdown timer (JavaScript)
   - Event timeline includes rate limit events
4. **MCP:**
   - `list_agents` includes rate limit fields
   - `get_agent` returns rate limit info
5. Write integration tests:
   - CLI output formatting
   - MCP response structure

**Deliverable:** All UIs display rate limit status correctly

---

### Phase 5: Integration & Polish

**Files to Modify:**
- `internal/lifecycle/lifecycle.go` (error parsing helper)
- `internal/poller/detect.go` (finalize `parseRestoreTime()`)
- Documentation files

**Tasks:**
1. **Finalize Detection Patterns:**
   - User provides exact Claude Code rate limit message
   - Implement regex patterns in `parseRestoreTime()`
   - Add real-world tests with actual message formats
   - Update tests with correct expectations
2. **Error Parsing:**
   - Add helper in lifecycle for parsing Restore() errors
   - Integrate into scheduler's retry logic
3. **Integration Testing:**
   - End-to-end: detection → scheduling → resume
   - Daemon restart during wait
   - Multiple agents simultaneously
   - Manual termination during wait
4. **Documentation:**
   - Update `docs/USAGE.md` with rate limit info
   - Update `docs/FEATURES.md` catalog
   - Add troubleshooting section
   - Update README if needed
5. **Code Review & Cleanup:**
   - Remove debug logging
   - Finalize error messages
   - Ensure consistent naming

**Deliverable:** Feature complete, tested, and documented

---

### Phase 6: Release

**Tasks:**
1. Create feature branch `feature/rate-limit-auto-resume`
2. Open PR with:
   - Full test suite passing
   - Documentation updates
   - Changelog entry
3. Manual testing in real environment:
   - Trigger real rate limits
   - Verify auto-resume works
   - Test UI across CLI/TUI/web
4. Merge to main
5. Tag release (e.g., `v1.2.0`)
6. Update GitHub releases with feature announcement

**Deliverable:** Feature shipped and available to users

## Testing Strategy

### Unit Tests

#### 1. Detection Logic (`internal/poller/poller_test.go`)

```go
func TestDetectRateLimit(t *testing.T) {
    tests := []struct {
        name      string
        pane      string
        wantLimit bool
        wantTime  time.Time
        wantOK    bool
    }{
        {
            name:      "rate limit with parseable time",
            pane:      "Rate limit exceeded. Try again at 3:45 PM",
            wantLimit: true,
            wantTime:  parseTime("15:45"),
            wantOK:    true,
        },
        {
            name:      "session limit with 24h format",
            pane:      "Session limit reached. Available at 15:45:00",
            wantLimit: true,
            wantTime:  parseTime("15:45:00"),
            wantOK:    true,
        },
        {
            name:      "quota exceeded without time",
            pane:      "Quota exceeded. Please try again later.",
            wantLimit: true,
            wantTime:  time.Time{},
            wantOK:    false,
        },
        {
            name:      "no rate limit",
            pane:      "Working on your request...",
            wantLimit: false,
            wantTime:  time.Time{},
            wantOK:    false,
        },
        {
            name:      "rate limit buried in output",
            pane:      "Previous output\nRate limit exceeded\nMore output",
            wantLimit: true,
            wantTime:  time.Time{},
            wantOK:    false,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            gotLimit, gotTime, gotOK := detectRateLimit(tt.pane)
            
            if gotLimit != tt.wantLimit {
                t.Errorf("detectRateLimit() limit = %v, want %v", gotLimit, tt.wantLimit)
            }
            if gotOK != tt.wantOK {
                t.Errorf("detectRateLimit() ok = %v, want %v", gotOK, tt.wantOK)
            }
            if tt.wantOK && !gotTime.Equal(tt.wantTime) {
                t.Errorf("detectRateLimit() time = %v, want %v", gotTime, tt.wantTime)
            }
        })
    }
}

func TestParseRestoreTime_SanityChecks(t *testing.T) {
    tests := []struct {
        name     string
        pane     string
        wantOK   bool
        checkMsg string
    }{
        {
            name:     "time in distant past",
            pane:     "Try again at 2020-01-01 10:00:00",
            wantOK:   false,
            checkMsg: "should reject times >1h in past",
        },
        {
            name:     "time in distant future",
            pane:     "Try again at 2030-01-01 10:00:00",
            wantOK:   false,
            checkMsg: "should reject times >24h in future",
        },
        {
            name:     "reasonable time",
            pane:     fmt.Sprintf("Try again at %s", time.Now().Add(2*time.Hour).Format("15:04")),
            wantOK:   true,
            checkMsg: "should accept time 2h from now",
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            _, gotOK := parseRestoreTime(tt.pane)
            if gotOK != tt.wantOK {
                t.Errorf("%s: got ok=%v, want ok=%v", tt.checkMsg, gotOK, tt.wantOK)
            }
        })
    }
}

func TestClassify_RateLimitPriority(t *testing.T) {
    // Rate limit detection should happen BEFORE prompt detection
    pane := "Rate limit exceeded. Try again later.\n❯ Continue?"
    
    sess := &store.Session{Status: store.StatusWorking}
    got := classify(sess, pane, true, 0, 0)
    
    if got != store.StatusRateLimited {
        t.Errorf("classify() = %v, want %v (rate limit should take priority over prompt)", got, store.StatusRateLimited)
    }
}
```

#### 2. Scheduler Logic (`internal/daemon/ratelimit_test.go`)

```go
func TestRateLimitScheduler_OnTransition(t *testing.T) {
    // Mock dependencies
    mockLife := &mockLifecycle{}
    mockStore := &mockStore{}
    
    sched := NewRateLimitScheduler(mockLife, mockStore)
    
    sess := &store.Session{
        ID:              "test-123",
        Status:          store.StatusRateLimited,
        LastPaneExcerpt: "Rate limit. Try again at 15:45",
        RateLimitedAt:   ptr(time.Now()),
    }
    
    // Trigger transition
    sched.OnTransition(sess, store.StatusWorking, store.StatusRateLimited)
    
    // Verify SetRateLimit was called
    if mockStore.setRateLimitCalls != 1 {
        t.Errorf("expected SetRateLimit called once, got %d", mockStore.setRateLimitCalls)
    }
    
    // Verify timer was created
    sched.mu.Lock()
    defer sched.mu.Unlock()
    if _, exists := sched.timers["test-123"]; !exists {
        t.Error("expected timer to be created for session")
    }
}

func TestRateLimitScheduler_AttemptResume_Success(t *testing.T) {
    mockLife := &mockLifecycle{restoreErr: nil} // Success
    mockStore := &mockStore{}
    
    sched := NewRateLimitScheduler(mockLife, mockStore)
    
    mockStore.sessions["test-123"] = &store.Session{
        ID:     "test-123",
        Status: store.StatusRateLimited,
    }
    
    sched.attemptResume("test-123")
    
    // Verify status updated to spawning
    if mockStore.updateStatusIfCalls != 1 {
        t.Error("expected UpdateStatusIf called")
    }
    
    // Verify ClearRateLimit called
    if mockStore.clearRateLimitCalls != 1 {
        t.Error("expected ClearRateLimit called")
    }
    
    // Verify timer removed
    sched.mu.Lock()
    defer sched.mu.Unlock()
    if _, exists := sched.timers["test-123"]; exists {
        t.Error("expected timer to be removed after success")
    }
}

func TestRateLimitScheduler_AttemptResume_StillLimited(t *testing.T) {
    mockLife := &mockLifecycle{
        restoreErr: errors.New("Rate limit. Try again at 16:30"),
    }
    mockStore := &mockStore{}
    
    sched := NewRateLimitScheduler(mockLife, mockStore)
    
    mockStore.sessions["test-123"] = &store.Session{
        ID:                  "test-123",
        Status:              store.StatusRateLimited,
        RateLimitRetryCount: 0,
    }
    
    sched.attemptResume("test-123")
    
    // Verify SetRateLimit called with new time
    if mockStore.setRateLimitCalls != 1 {
        t.Error("expected SetRateLimit called with new restore time")
    }
    
    // Verify timer rescheduled
    sched.mu.Lock()
    defer sched.mu.Unlock()
    if _, exists := sched.timers["test-123"]; !exists {
        t.Error("expected timer to be rescheduled")
    }
}

func TestRateLimitScheduler_AttemptResume_OtherError(t *testing.T) {
    mockLife := &mockLifecycle{
        restoreErr: errors.New("network connection failed"),
    }
    mockStore := &mockStore{}
    
    sched := NewRateLimitScheduler(mockLife, mockStore)
    
    mockStore.sessions["test-123"] = &store.Session{
        ID:     "test-123",
        Status: store.StatusRateLimited,
    }
    
    sched.attemptResume("test-123")
    
    // Verify status updated to errored (not rescheduled)
    if mockStore.updateStatusIfCalls != 1 {
        t.Error("expected UpdateStatusIf called to transition to errored")
    }
    
    // Verify AppendEvent called with failure detail
    if mockStore.appendEventCalls != 1 {
        t.Error("expected AppendEvent called with error detail")
    }
    
    // Verify timer removed (not rescheduled)
    sched.mu.Lock()
    defer sched.mu.Unlock()
    if _, exists := sched.timers["test-123"]; exists {
        t.Error("expected timer to be removed after non-limit error")
    }
}

func TestRateLimitScheduler_ReconstructTimers(t *testing.T) {
    mockLife := &mockLifecycle{}
    mockStore := &mockStore{}
    
    // Set up sessions: one rate-limited, one not
    futureTime := time.Now().Add(1 * time.Hour)
    mockStore.sessions = map[string]*store.Session{
        "limited-1": {
            ID:                  "limited-1",
            Status:              store.StatusRateLimited,
            RateLimitRestoreAt:  &futureTime,
        },
        "working-1": {
            ID:     "working-1",
            Status: store.StatusWorking,
        },
    }
    
    sched := NewRateLimitScheduler(mockLife, mockStore)
    
    err := sched.ReconstructTimers(context.Background())
    if err != nil {
        t.Fatalf("ReconstructTimers() error = %v", err)
    }
    
    // Verify timer created only for rate-limited session
    sched.mu.Lock()
    defer sched.mu.Unlock()
    
    if _, exists := sched.timers["limited-1"]; !exists {
        t.Error("expected timer for rate-limited session")
    }
    if _, exists := sched.timers["working-1"]; exists {
        t.Error("should not create timer for non-rate-limited session")
    }
}

func TestRateLimitScheduler_CancelTimer(t *testing.T) {
    sched := NewRateLimitScheduler(nil, nil)
    
    // Create a mock timer
    sched.timers["test-123"] = time.AfterFunc(1*time.Hour, func() {})
    
    sched.CancelTimer("test-123")
    
    sched.mu.Lock()
    defer sched.mu.Unlock()
    
    if _, exists := sched.timers["test-123"]; exists {
        t.Error("expected timer to be removed after cancel")
    }
}
```

#### 3. Store Methods (`internal/store/file_test.go`)

```go
func TestFileStore_SetRateLimit(t *testing.T) {
    st := newTestFileStore(t)
    
    sess := &Session{ID: "test-123", Status: StatusWorking}
    require.NoError(t, st.Insert(context.Background(), sess))
    
    restoreAt := time.Now().Add(1 * time.Hour)
    err := st.SetRateLimit(context.Background(), "test-123", restoreAt, 0)
    require.NoError(t, err)
    
    // Verify fields set
    got, err := st.Get(context.Background(), "test-123")
    require.NoError(t, err)
    
    if got.RateLimitedAt == nil {
        t.Error("expected RateLimitedAt to be set")
    }
    if got.RateLimitRestoreAt == nil || !got.RateLimitRestoreAt.Equal(restoreAt) {
        t.Errorf("RateLimitRestoreAt = %v, want %v", got.RateLimitRestoreAt, restoreAt)
    }
    if got.RateLimitRetryCount != 0 {
        t.Errorf("RateLimitRetryCount = %d, want 0", got.RateLimitRetryCount)
    }
    
    // Verify event appended
    if len(got.Events) == 0 {
        t.Error("expected event to be appended")
    }
    lastEvent := got.Events[len(got.Events)-1]
    if lastEvent.Type != "rate-limit" {
        t.Errorf("event type = %s, want 'rate-limit'", lastEvent.Type)
    }
}

func TestFileStore_ClearRateLimit(t *testing.T) {
    st := newTestFileStore(t)
    
    restoreAt := time.Now().Add(1 * time.Hour)
    limitedAt := time.Now()
    sess := &Session{
        ID:                  "test-123",
        Status:              StatusRateLimited,
        RateLimitedAt:       &limitedAt,
        RateLimitRestoreAt:  &restoreAt,
        RateLimitRetryCount: 2,
    }
    require.NoError(t, st.Insert(context.Background(), sess))
    
    err := st.ClearRateLimit(context.Background(), "test-123")
    require.NoError(t, err)
    
    // Verify fields cleared
    got, err := st.Get(context.Background(), "test-123")
    require.NoError(t, err)
    
    if got.RateLimitedAt != nil {
        t.Error("expected RateLimitedAt to be cleared")
    }
    if got.RateLimitRestoreAt != nil {
        t.Error("expected RateLimitRestoreAt to be cleared")
    }
    if got.RateLimitRetryCount != 0 {
        t.Errorf("RateLimitRetryCount = %d, want 0", got.RateLimitRetryCount)
    }
    
    // Verify resume event appended
    lastEvent := got.Events[len(got.Events)-1]
    if lastEvent.Type != "rate-limit-resumed" {
        t.Errorf("event type = %s, want 'rate-limit-resumed'", lastEvent.Type)
    }
}
```

### Integration Tests

#### 1. End-to-End Flow

```go
func TestRateLimitFlow_EndToEnd(t *testing.T) {
    // Set up test daemon with all components
    daemon := setupTestDaemon(t)
    defer daemon.Shutdown()
    
    // Spawn an agent
    sess := daemon.spawnAgent("test-agent", "development")
    
    // Simulate rate limit in pane
    daemon.mockPaneCapture(sess.ID, "Rate limit exceeded. Try again at 15:45")
    
    // Wait for poller tick
    time.Sleep(100 * time.Millisecond)
    
    // Verify status changed to rate_limited
    got, _ := daemon.store.Get(context.Background(), sess.ID)
    if got.Status != store.StatusRateLimited {
        t.Errorf("status = %v, want rate_limited", got.Status)
    }
    
    // Verify timer scheduled
    if !daemon.scheduler.HasTimer(sess.ID) {
        t.Error("expected timer to be scheduled")
    }
    
    // Fast-forward time to trigger resume
    daemon.advanceTime(2 * time.Hour)
    
    // Verify resume attempted
    if daemon.lifecycle.restoreCalls != 1 {
        t.Errorf("Restore() calls = %d, want 1", daemon.lifecycle.restoreCalls)
    }
    
    // Verify status back to spawning
    got, _ = daemon.store.Get(context.Background(), sess.ID)
    if got.Status != store.StatusSpawning {
        t.Errorf("status = %v, want spawning", got.Status)
    }
}
```

#### 2. Daemon Restart Test

```go
func TestRateLimitFlow_DaemonRestart(t *testing.T) {
    // Start daemon
    daemon := setupTestDaemon(t)
    
    // Create rate-limited session with future restore time
    restoreAt := time.Now().Add(30 * time.Minute)
    sess := &store.Session{
        ID:                  "test-123",
        Status:              store.StatusRateLimited,
        RateLimitRestoreAt:  &restoreAt,
    }
    daemon.store.Insert(context.Background(), sess)
    
    // Shutdown daemon
    daemon.Shutdown()
    
    // Restart daemon
    daemon2 := setupTestDaemon(t)
    defer daemon2.Shutdown()
    
    // Verify timer was reconstructed
    if !daemon2.scheduler.HasTimer("test-123") {
        t.Error("expected timer to be reconstructed after restart")
    }
    
    // Fast-forward past restore time
    daemon2.advanceTime(35 * time.Minute)
    
    // Verify resume attempted
    if daemon2.lifecycle.restoreCalls != 1 {
        t.Error("resume should have been attempted after restart")
    }
}
```

### Manual Testing Checklist

- [ ] **Detection:**
  - [ ] Agent hits real Claude API rate limit
  - [ ] Warden detects within one poll interval (~10 seconds)
  - [ ] Status changes to `rate_limited`
  - [ ] Event logged with scheduled resume time

- [ ] **UI Display:**
  - [ ] `warden ls` shows `rate_limited` status in yellow
  - [ ] `warden status <id>` shows rate limit info (time, retries)
  - [ ] TUI cockpit displays countdown timer
  - [ ] Web dashboard shows rate limit badge and countdown
  - [ ] MCP `list_agents` returns rate limit fields

- [ ] **Auto-Resume:**
  - [ ] Agent resumes automatically at scheduled time
  - [ ] Status transitions `rate_limited` → `spawning` → `working`
  - [ ] Resume event logged
  - [ ] Rate limit fields cleared

- [ ] **Retry Logic:**
  - [ ] Parse fails → fallback to 30-min retry
  - [ ] Resume fails with same limit → reschedule with new time
  - [ ] Resume fails with other error → transition to `errored`

- [ ] **Edge Cases:**
  - [ ] Daemon restart during wait → timer reconstructed
  - [ ] User terminates rate-limited agent → timer cancelled
  - [ ] Multiple agents rate-limited simultaneously → each handled independently
  - [ ] Manual resume before timer fires → timer no-ops correctly

- [ ] **Configuration:**
  - [ ] `WARDEN_RATE_LIMIT_AUTO_RESUME=false` disables auto-resume
  - [ ] `WARDEN_RATE_LIMIT_RETRY_INTERVAL=15m` changes fallback interval
  - [ ] `WARDEN_RATE_LIMIT_BUFFER=2m` changes safety buffer

## Observability

### Logging

**Daemon Logs:**

```
[ratelimit] session abc123: detected rate limit, scheduled resume at 2026-06-14T15:45:00Z
[ratelimit] session abc123: attempting resume (retry 0)
[ratelimit] session abc123: resume successful
```

```
[ratelimit] session def456: could not parse restore time from pane, retrying in 30m
[ratelimit] session def456: attempting resume (retry 1)
[ratelimit] session def456: still rate limited, parsed new time: 2026-06-14T16:15:00Z
[ratelimit] session def456: resume successful
```

```
[ratelimit] session ghi789: attempting resume (retry 2)
[ratelimit] session ghi789: resume failed with non-limit error: network timeout
[ratelimit] session ghi789: transitioned to errored
```

**Event Log (per session):**

Visible in `warden status <id>`, TUI, web dashboard, and MCP:

```
Events:
  [15:46:05] rate-limit-resumed: successfully resumed after rate limit
  [14:30:15] rate-limit: scheduled resume at 2026-06-14T15:45:00Z (retry 0)
  [14:29:58] status-change: working → rate_limited
```

### Metrics (Future)

If warden adds Prometheus/metrics support later:

```
# Total rate limit detections
warden_rate_limit_detections_total{} 42

# Total resume attempts
warden_rate_limit_resume_attempts_total{result="success"} 38
warden_rate_limit_resume_attempts_total{result="still_limited"} 3
warden_rate_limit_resume_attempts_total{result="error"} 1

# Current rate-limited sessions
warden_rate_limited_sessions{} 2

# Parse success rate
warden_rate_limit_parse_success_total{} 35
warden_rate_limit_parse_failure_total{} 7
```

## Future Enhancements

### 1. Transcript JSON Parsing

**When Available:**
If Claude Code adds structured rate limit info to `.jsonl` transcript files:

```json
{
  "type": "error",
  "error": {
    "type": "rate_limit_error",
    "message": "Session limit exceeded",
    "retry_after": 1718380800,
    "reset_at": "2026-06-14T15:45:00Z"
  }
}
```

**Implementation:**
- Add secondary detection path in poller
- Parse `retry_after` or `reset_at` from transcript
- Higher confidence than pane scraping
- Can deprecate pane parsing or use as fallback

### 2. Per-User Rate Limit Tracking

**Goal:** Aggregate rate limit patterns across all agents to predict upcoming limits.

**Design:**
- Track rate limit events globally (not just per-session)
- Detect patterns: "every day around 14:00" or "after ~1000 requests"
- Warn user proactively: "High rate limit risk - consider spacing agents"
- Display in dashboard: "Daily limit: 80% used"

**Use Cases:**
- Users running many agents in parallel
- Predictive scheduling: delay new agents when close to limit
- Budget management for paid API tiers

### 3. Proactive Throttling

**Goal:** Slow down agent activity when approaching known rate limits.

**Design:**
- Monitor global request count (if trackable)
- When nearing limit (e.g., 90% of daily quota):
  - Delay spawning new agents
  - Add artificial delays between agent actions
  - Notify user: "Rate limit throttling active"
- Automatic recovery when limit resets

**Challenges:**
- Requires visibility into Claude API usage (may not be available)
- Hard to predict without official quota APIs

### 4. Notification Opt-In

**Goal:** Allow users to enable desktop notifications for rate limit events.

**Design:**
- Config flag: `notify_on_rate_limit: true`
- Notifications:
  - "Agent 'feature-auth' rate limited, resuming at 3:45 PM"
  - "Agent 'feature-auth' resumed successfully"
- Uses existing `internal/notify/notify.go` infrastructure
- Controlled via `WARDEN_NOTIFY_ENABLED` env var

**Current Decision:** Opted out for initial implementation (status visibility sufficient).

### 5. User-Configured Retry Strategy

**Goal:** Let users customize retry behavior per project.

**Design:**
```yaml
# .warden.yml
rate_limit:
  auto_resume: true
  retry_interval: 20m
  buffer: 2m
  max_retries: 10  # optional cap
```

**Benefits:**
- Different projects have different tolerance for retries
- Testing environments might want aggressive retries
- Production might want manual intervention after N failures

## Open Questions

### 1. Exact Error Message Format

**Status:** Pending user input

**Needed:**
- Exact text Claude Code displays when hitting session limits
- Format of timestamp in error message (if any)
- Whether it varies by error type (rate limit vs. quota vs. other)

**Impact:**
- Determines regex patterns in `parseRestoreTime()`
- Affects detection confidence
- May need multiple patterns for different error types

**Action:** User will trigger a real rate limit and provide the exact pane output.

---

### 2. Claude Code Error Codes

**Question:** Does Claude Code exit with a specific error code when rate limited?

**Options:**
- **Yes:** Could use exit code as secondary detection signal (similar to crash detection)
- **No:** Rely solely on pane capture

**Impact:**
- If yes, adds another detection path (more robust)
- Exit code might be more reliable than pane scraping

**Investigation:** Check `claude` exit codes during rate limit (manual test).

---

### 3. Restore Behavior on Persistent Limits

**Question:** What happens if the limit is longer than expected (e.g., account suspended)?

**Options:**
- **A:** Keep retrying indefinitely (current design)
- **B:** Add absolute timeout (e.g., stop after 7 days)
- **C:** Escalate to user after N retries (notification override)

**Current Decision:** Option A (no cap), but can revisit if real-world usage shows issues.

---

### 4. Multi-User Rate Limits

**Question:** If warden is used in multi-user environments, are rate limits per-user or per-API-key?

**Impact:**
- Per-user: each user's agents tracked separately
- Per-API-key: global tracking needed across all users

**Current Scope:** Single-user assumption (most warden deployments are personal).

**Future:** If multi-user support added, may need per-user rate limit tracking.

## Summary

This design adds intelligent session limit detection and auto-resume to warden:

**Core Mechanism:**
- Poller detects rate limits via pane capture (keyword + optional timestamp parsing)
- New `StatusRateLimited` status makes limits visible in all UIs
- `RateLimitScheduler` manages Go timers for scheduled resume attempts
- Parse restore time from error → schedule at `time + 1min buffer`
- Fallback to 30-min retries when parsing fails
- On retry failure, re-parse for updated time or reschedule

**User Experience:**
- Agents automatically resume when limits expire
- Status visible in CLI (`warden ls`), TUI (cockpit), web dashboard, and MCP
- No manual intervention needed unless resume fails with non-limit error
- Survives daemon restarts (timers reconstructed from persisted state)

**Design Principles:**
- Reuse existing infrastructure (poller, lifecycle, store)
- Mirror proven patterns (auto-restart, context monitoring)
- Fail gracefully (parse failures fall back to blind retry)
- No retry cap (rate limits are temporary and external)
- Observable (events, logs, UI feedback)

**Implementation Phases:**
1. Core infrastructure (status, store methods)
2. Detection logic (poller extensions)
3. Scheduling component (new `RateLimitScheduler`)
4. UI updates (CLI, TUI, web, MCP)
5. Integration & polish (real message patterns, testing)
6. Release

**Open Items:**
- Exact Claude Code error message format (pending user input)
- Exit code investigation (manual testing)

The feature is scoped to be self-contained, testable, and incrementally deliverable. Once the exact error message format is known, Phase 2 (detection) can be finalized and the implementation can proceed through the remaining phases.
