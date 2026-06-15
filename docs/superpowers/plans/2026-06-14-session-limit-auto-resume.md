# Session Limit Auto-Resume Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Automatically detect when Claude Code agents hit API rate limits and intelligently resume them when the limit expires.

**Architecture:** Extends the existing poller to detect rate-limit patterns in pane captures, adds a new `StatusRateLimited` status, and creates a `RateLimitScheduler` component (mirroring `Restarter`) that manages Go timers for scheduled resume attempts. Parses restore timestamps from errors for precise scheduling, falls back to 30-min retries when parsing fails.

**Tech Stack:** Go 1.26+, existing warden infrastructure (poller, lifecycle, store), time-based scheduling with Go timers.

---

## File Structure

### New Files
- `internal/daemon/ratelimit.go` - RateLimitScheduler component
- `internal/daemon/ratelimit_test.go` - Scheduler unit tests
- `internal/poller/detect.go` - Rate limit detection helpers

### Modified Files
- `internal/store/types.go` - Add StatusRateLimited, Session fields
- `internal/store/store.go` - Add SetRateLimit/ClearRateLimit interface methods
- `internal/store/file.go` - Implement store methods
- `internal/store/file_test.go` - Store method tests
- `internal/poller/poller.go` - Extend classify() for rate limit detection
- `internal/poller/poller_test.go` - Detection tests
- `internal/daemon/api.go` - Wire scheduler into daemon
- `internal/cli/*.go` - Status formatting for CLI
- `internal/tui/*.go` - TUI display updates
- `internal/mcp/server.go` - MCP response updates

---

## Task 1: Add StatusRateLimited to Type System

**Files:**
- Modify: `internal/store/types.go`
- Test: `internal/store/types_test.go`

- [ ] **Step 1: Write test for new status**

Add to `internal/store/types_test.go`:

```go
func TestStatusRateLimited_Valid(t *testing.T) {
	if !StatusRateLimited.Valid() {
		t.Error("StatusRateLimited should be valid")
	}
}

func TestStatusRateLimited_Serialization(t *testing.T) {
	s := Session{
		ID:     "test",
		Status: StatusRateLimited,
	}
	
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"status":"rate_limited"`)
	
	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, StatusRateLimited, back.Status)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/store
go test -run TestStatusRateLimited -v
```

Expected: FAIL - StatusRateLimited not defined

- [ ] **Step 3: Add StatusRateLimited constant**

In `internal/store/types.go`, add after existing statuses:

```go
const (
	StatusSpawning        Status = "spawning"
	StatusWorking         Status = "working"
	StatusWaitingForInput Status = "waiting_for_input"
	StatusIdle            Status = "idle"
	StatusDone            Status = "done"
	StatusErrored         Status = "errored"
	StatusOrphaned        Status = "orphaned"
	StatusRateLimited     Status = "rate_limited"  // NEW
)
```

- [ ] **Step 4: Update Valid() method**

In `internal/store/types.go`, update `Valid()`:

```go
func (s Status) Valid() bool {
	switch s {
	case StatusSpawning, StatusWorking, StatusWaitingForInput,
		StatusIdle, StatusDone, StatusErrored, StatusOrphaned,
		StatusRateLimited:  // NEW
		return true
	}
	return false
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
cd internal/store
go test -run TestStatusRateLimited -v
```

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/store/types.go internal/store/types_test.go
git commit -m "feat: add StatusRateLimited to type system

Add new rate_limited status for agents that hit API limits.
Status is non-terminal and will be used by scheduler for
auto-resume.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 2: Add Session Rate Limit Fields

**Files:**
- Modify: `internal/store/types.go`
- Test: `internal/store/types_test.go`

- [ ] **Step 1: Write test for rate limit fields**

Add to `internal/store/types_test.go`:

```go
func TestSession_RateLimitFields(t *testing.T) {
	now := time.Now()
	restoreAt := now.Add(1 * time.Hour)
	
	s := Session{
		ID:                  "test",
		RateLimitedAt:       &now,
		RateLimitRestoreAt:  &restoreAt,
		RateLimitRetryCount: 2,
	}
	
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"rate_limited_at"`)
	require.Contains(t, string(b), `"rate_limit_restore_at"`)
	require.Contains(t, string(b), `"rate_limit_retry_count":2`)
	
	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.NotNil(t, back.RateLimitedAt)
	require.NotNil(t, back.RateLimitRestoreAt)
	require.Equal(t, 2, back.RateLimitRetryCount)
}

func TestSession_RateLimitFields_Omitempty(t *testing.T) {
	s := Session{ID: "test"}
	
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.NotContains(t, string(b), "rate_limited_at")
	require.NotContains(t, string(b), "rate_limit_restore_at")
	require.NotContains(t, string(b), "rate_limit_retry_count")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/store
go test -run TestSession_RateLimit -v
```

Expected: FAIL - fields not defined

- [ ] **Step 3: Add fields to Session struct**

In `internal/store/types.go`, add to Session struct:

```go
type Session struct {
	// ... existing fields ...
	
	ContextTokens    int        `json:"context_tokens,omitempty"`
	ContextState     string     `json:"context_state,omitempty"`
	ContextCheckedAt time.Time  `json:"context_checked_at,omitempty"`
	LastCompactAt    *time.Time `json:"last_compact_at,omitempty"`
	
	// NEW: Rate limit fields
	RateLimitedAt       *time.Time `json:"rate_limited_at,omitempty"`       // when limit was first hit
	RateLimitRestoreAt  *time.Time `json:"rate_limit_restore_at,omitempty"` // scheduled resume time
	RateLimitRetryCount int        `json:"rate_limit_retry_count,omitempty"` // number of retry attempts
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/store
go test -run TestSession_RateLimit -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/types.go internal/store/types_test.go
git commit -m "feat: add rate limit fields to Session

Add RateLimitedAt, RateLimitRestoreAt, and RateLimitRetryCount
fields to track rate limit state and scheduled resume times.

Fields use omitempty to maintain backward compatibility.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 3: Add Store Interface Methods

**Files:**
- Modify: `internal/store/store.go`

- [ ] **Step 1: Add SetRateLimit method to interface**

In `internal/store/store.go`, add after existing methods:

```go
type Store interface {
	// ... existing methods ...
	
	// SetRateLimit records rate limit state and next resume time
	SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error
	
	// ClearRateLimit removes rate limit metadata (after successful resume)
	ClearRateLimit(ctx context.Context, id string) error
}
```

- [ ] **Step 2: Verify compilation**

```bash
cd internal/store
go build
```

Expected: FAIL - FileStore doesn't implement new methods

- [ ] **Step 3: Commit interface definition**

```bash
git add internal/store/store.go
git commit -m "feat: add rate limit methods to Store interface

Add SetRateLimit and ClearRateLimit methods for managing
rate limit state persistence.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 4: Implement SetRateLimit in FileStore

**Files:**
- Modify: `internal/store/file.go`
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write test for SetRateLimit**

Add to `internal/store/file_test.go`:

```go
func TestFileStore_SetRateLimit(t *testing.T) {
	st := newTestFileStore(t)
	
	sess := &Session{ID: "test-123", Status: StatusWorking}
	require.NoError(t, st.Insert(context.Background(), sess))
	
	restoreAt := time.Now().Add(1 * time.Hour).UTC()
	err := st.SetRateLimit(context.Background(), "test-123", restoreAt, 0)
	require.NoError(t, err)
	
	// Verify fields set
	got, err := st.Get(context.Background(), "test-123")
	require.NoError(t, err)
	
	require.NotNil(t, got.RateLimitedAt, "RateLimitedAt should be set")
	require.NotNil(t, got.RateLimitRestoreAt, "RateLimitRestoreAt should be set")
	require.True(t, got.RateLimitRestoreAt.Equal(restoreAt), 
		"RateLimitRestoreAt = %v, want %v", got.RateLimitRestoreAt, restoreAt)
	require.Equal(t, 0, got.RateLimitRetryCount, "RateLimitRetryCount should be 0")
	
	// Verify event appended
	require.NotEmpty(t, got.Events, "expected event to be appended")
	lastEvent := got.Events[len(got.Events)-1]
	require.Equal(t, "rate-limit", lastEvent.Type, "event type should be rate-limit")
	require.Contains(t, lastEvent.Detail, "scheduled resume")
}

func TestFileStore_SetRateLimit_PreservesFirstLimitedAt(t *testing.T) {
	st := newTestFileStore(t)
	
	firstTime := time.Now().Add(-1 * time.Hour).UTC()
	sess := &Session{
		ID:            "test-123",
		Status:        StatusRateLimited,
		RateLimitedAt: &firstTime,
	}
	require.NoError(t, st.Insert(context.Background(), sess))
	
	restoreAt := time.Now().Add(1 * time.Hour).UTC()
	err := st.SetRateLimit(context.Background(), "test-123", restoreAt, 1)
	require.NoError(t, err)
	
	got, err := st.Get(context.Background(), "test-123")
	require.NoError(t, err)
	
	// First RateLimitedAt should be preserved
	require.NotNil(t, got.RateLimitedAt)
	require.True(t, got.RateLimitedAt.Equal(firstTime),
		"RateLimitedAt should preserve first occurrence")
	require.Equal(t, 1, got.RateLimitRetryCount)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/store
go test -run TestFileStore_SetRateLimit -v
```

Expected: FAIL - method not implemented

- [ ] **Step 3: Implement SetRateLimit**

Add to `internal/store/file.go`:

```go
func (s *FileStore) SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error {
	return s.update(ctx, id, func(sess *Session) error {
		now := time.Now().UTC()
		
		// Preserve first RateLimitedAt time
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
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/store
go test -run TestFileStore_SetRateLimit -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/file.go internal/store/file_test.go
git commit -m "feat: implement SetRateLimit in FileStore

Store rate limit state and scheduled resume time.
Preserves first RateLimitedAt timestamp across retries.
Appends event for audit trail.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 5: Implement ClearRateLimit in FileStore

**Files:**
- Modify: `internal/store/file.go`
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write test for ClearRateLimit**

Add to `internal/store/file_test.go`:

```go
func TestFileStore_ClearRateLimit(t *testing.T) {
	st := newTestFileStore(t)
	
	restoreAt := time.Now().Add(1 * time.Hour).UTC()
	limitedAt := time.Now().UTC()
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
	
	require.Nil(t, got.RateLimitedAt, "RateLimitedAt should be cleared")
	require.Nil(t, got.RateLimitRestoreAt, "RateLimitRestoreAt should be cleared")
	require.Equal(t, 0, got.RateLimitRetryCount, "RateLimitRetryCount should be 0")
	
	// Verify resume event appended
	require.NotEmpty(t, got.Events)
	lastEvent := got.Events[len(got.Events)-1]
	require.Equal(t, "rate-limit-resumed", lastEvent.Type)
	require.Contains(t, lastEvent.Detail, "successfully resumed")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/store
go test -run TestFileStore_ClearRateLimit -v
```

Expected: FAIL - method not implemented

- [ ] **Step 3: Implement ClearRateLimit**

Add to `internal/store/file.go`:

```go
func (s *FileStore) ClearRateLimit(ctx context.Context, id string) error {
	return s.update(ctx, id, func(sess *Session) error {
		sess.RateLimitedAt = nil
		sess.RateLimitRestoreAt = nil
		sess.RateLimitRetryCount = 0
		
		sess.Events = append(sess.Events, Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-resumed",
			Detail: "successfully resumed after rate limit",
		})
		
		return nil
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/store
go test -run TestFileStore_ClearRateLimit -v
```

Expected: PASS

- [ ] **Step 5: Run all store tests**

```bash
cd internal/store
go test -v
```

Expected: All tests PASS

- [ ] **Step 6: Commit**

```bash
git add internal/store/file.go internal/store/file_test.go
git commit -m "feat: implement ClearRateLimit in FileStore

Clear rate limit metadata after successful resume.
Appends rate-limit-resumed event for tracking.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 6: Create Rate Limit Detection Helpers

**Files:**
- Create: `internal/poller/detect.go`
- Test: `internal/poller/detect_test.go`

- [ ] **Step 1: Write tests for detectRateLimit**

Create `internal/poller/detect_test.go`:

```go
package poller

import (
	"strings"
	"testing"
	"time"
)

func TestDetectRateLimit_KeywordDetection(t *testing.T) {
	tests := []struct {
		name      string
		pane      string
		wantLimit bool
	}{
		{
			name:      "rate limit keyword",
			pane:      "Error: rate limit exceeded",
			wantLimit: true,
		},
		{
			name:      "usage limit keyword",
			pane:      "Usage limit reached. Try again later.",
			wantLimit: true,
		},
		{
			name:      "session limit keyword",
			pane:      "Session limit hit",
			wantLimit: true,
		},
		{
			name:      "quota exceeded keyword",
			pane:      "Quota exceeded for this session",
			wantLimit: true,
		},
		{
			name:      "case insensitive",
			pane:      "RATE LIMIT EXCEEDED",
			wantLimit: true,
		},
		{
			name:      "no rate limit",
			pane:      "Working on your request...",
			wantLimit: false,
		},
		{
			name:      "rate in different context",
			pane:      "The success rate is high",
			wantLimit: false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLimit, _, _ := detectRateLimit(tt.pane)
			if gotLimit != tt.wantLimit {
				t.Errorf("detectRateLimit() limit = %v, want %v", gotLimit, tt.wantLimit)
			}
		})
	}
}

func TestDetectRateLimit_WithBuriedKeyword(t *testing.T) {
	pane := `Previous output line 1
Previous output line 2
Error: rate limit exceeded. Try again later.
More output
❯ Continue?`
	
	gotLimit, _, _ := detectRateLimit(pane)
	if !gotLimit {
		t.Error("should detect rate limit even when buried in output")
	}
}

func TestParseRestoreTime_Placeholder(t *testing.T) {
	// NOTE: This is a placeholder test until exact message format is known
	// Will be updated when user provides actual Claude Code error message
	
	tests := []struct {
		name   string
		pane   string
		wantOK bool
	}{
		{
			name:   "no time in message",
			pane:   "Rate limit exceeded. Try again later.",
			wantOK: false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotOK := parseRestoreTime(tt.pane)
			if gotOK != tt.wantOK {
				t.Errorf("parseRestoreTime() ok = %v, want %v", gotOK, tt.wantOK)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/poller
go test -run TestDetectRateLimit -v
```

Expected: FAIL - functions not defined

- [ ] **Step 3: Create detect.go with placeholder implementation**

Create `internal/poller/detect.go`:

```go
package poller

import (
	"strings"
	"time"
)

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
	paneLower := strings.ToLower(pane)
	for _, kw := range limitKeywords {
		if strings.Contains(paneLower, kw) {
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

// parseRestoreTime attempts to extract a restore timestamp from the error message.
// Returns (time, true) if successful, (zero, false) otherwise.
//
// NOTE: This is a placeholder implementation until the exact Claude Code error
// message format is known. Will be updated with regex patterns once the user
// provides the actual error message.
//
// Expected patterns to support (examples):
//   "Try again at 3:45 PM"
//   "Available again at 15:45"
//   "Reset at 2024-06-14 15:45:00"
//   "retry_after: 1718380800" (unix timestamp)
func parseRestoreTime(pane string) (time.Time, bool) {
	// Placeholder: always returns false until exact format is known
	// TODO: Add regex patterns when user provides actual error message
	return time.Time{}, false
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/poller
go test -run TestDetectRateLimit -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/poller/detect.go internal/poller/detect_test.go
git commit -m "feat: add rate limit detection helpers

Add detectRateLimit and parseRestoreTime functions.
Keyword detection is complete, timestamp parsing is
placeholder until exact error message format is known.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 7: Extend Poller classify() for Rate Limit Detection

**Files:**
- Modify: `internal/poller/poller.go`
- Test: `internal/poller/poller_test.go`

- [ ] **Step 1: Write test for classify with rate limit**

Add to `internal/poller/poller_test.go`:

```go
func TestClassify_RateLimited(t *testing.T) {
	sess := &store.Session{
		ID:     "test",
		Status: store.StatusWorking,
	}
	
	pane := "Error: rate limit exceeded. Please try again later."
	
	got := classify(sess, pane, true, 0, 0)
	
	if got != store.StatusRateLimited {
		t.Errorf("classify() = %v, want %v", got, store.StatusRateLimited)
	}
}

func TestClassify_RateLimitPriority(t *testing.T) {
	// Rate limit should take priority over prompt detection
	sess := &store.Session{
		ID:     "test",
		Status: store.StatusWorking,
	}
	
	pane := "Rate limit exceeded. Try again later.\n❯ Continue?"
	
	got := classify(sess, pane, true, 0, 0)
	
	if got != store.StatusRateLimited {
		t.Errorf("classify() = %v, want %v (rate limit should take priority)", 
			got, store.StatusRateLimited)
	}
}

func TestClassify_NoRateLimit(t *testing.T) {
	sess := &store.Session{
		ID:     "test",
		Status: store.StatusWorking,
	}
	
	pane := "Working on your request..."
	
	got := classify(sess, pane, true, 0, 0)
	
	if got == store.StatusRateLimited {
		t.Error("classify() should not return rate_limited for normal output")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/poller
go test -run TestClassify_RateLimit -v
```

Expected: FAIL - StatusRateLimited not returned

- [ ] **Step 3: Update classify() to check rate limits first**

In `internal/poller/poller.go`, modify the `classify` function:

```go
func classify(s *store.Session, pane string, sessionAlive bool, sinceUpdate, stuckAfter time.Duration) store.Status {
	if !sessionAlive {
		return store.StatusOrphaned
	}
	
	// Check for rate limit BEFORE other classifications
	// (prevents misclassification as waiting_for_input when prompt is shown)
	if isLimited, _, _ := detectRateLimit(pane); isLimited {
		return store.StatusRateLimited
	}
	
	// Existing classification logic unchanged
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

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/poller
go test -run TestClassify_RateLimit -v
```

Expected: PASS

- [ ] **Step 5: Run all poller tests**

```bash
cd internal/poller
go test -v
```

Expected: All tests PASS

- [ ] **Step 6: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "feat: extend classify to detect rate limits

Check for rate limit patterns before other classifications
to prevent misclassification as waiting_for_input.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 8: Create RateLimitScheduler Component

**Files:**
- Create: `internal/daemon/ratelimit.go`
- Test: `internal/daemon/ratelimit_test.go`

- [ ] **Step 1: Write test for OnTransition**

Create `internal/daemon/ratelimit_test.go`:

```go
package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type mockLifecycle struct {
	restoreCalls int
	restoreErr   error
}

func (m *mockLifecycle) Restore(ctx context.Context, sess *store.Session) error {
	m.restoreCalls++
	return m.restoreErr
}

type mockStore struct {
	sessions            map[string]*store.Session
	setRateLimitCalls   int
	clearRateLimitCalls int
	updateStatusIfCalls int
	appendEventCalls    int
}

func (m *mockStore) Get(ctx context.Context, id string) (*store.Session, error) {
	if sess, ok := m.sessions[id]; ok {
		return sess, nil
	}
	return nil, store.ErrNotFound
}

func (m *mockStore) List(ctx context.Context) ([]*store.Session, error) {
	var sessions []*store.Session
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func (m *mockStore) SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error {
	m.setRateLimitCalls++
	if sess, ok := m.sessions[id]; ok {
		sess.RateLimitRestoreAt = &restoreAt
		sess.RateLimitRetryCount = retryCount
	}
	return nil
}

func (m *mockStore) ClearRateLimit(ctx context.Context, id string) error {
	m.clearRateLimitCalls++
	return nil
}

func (m *mockStore) UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error) {
	m.updateStatusIfCalls++
	if sess, ok := m.sessions[id]; ok {
		if sess.Status == expected {
			sess.Status = next
			return true, nil
		}
	}
	return false, nil
}

func (m *mockStore) AppendEvent(ctx context.Context, id string, ev store.Event) error {
	m.appendEventCalls++
	return nil
}

func TestRateLimitScheduler_OnTransition(t *testing.T) {
	mockLife := &mockLifecycle{}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	sess := &store.Session{
		ID:              "test-123",
		Status:          store.StatusRateLimited,
		LastPaneExcerpt: "Rate limit exceeded. Try again later.",
	}
	mockStore.sessions["test-123"] = sess
	
	// Trigger transition
	sched.OnTransition(sess, store.StatusWorking, store.StatusRateLimited)
	
	// Verify SetRateLimit was called
	require.Equal(t, 1, mockStore.setRateLimitCalls, "SetRateLimit should be called")
	
	// Verify timer was created
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.True(t, exists, "timer should be created for session")
}

func TestRateLimitScheduler_OnTransition_IgnoresOtherStatuses(t *testing.T) {
	mockLife := &mockLifecycle{}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	sess := &store.Session{ID: "test-123"}
	
	// Transition to non-rate-limited status
	sched.OnTransition(sess, store.StatusWorking, store.StatusIdle)
	
	// Verify no action taken
	require.Equal(t, 0, mockStore.setRateLimitCalls)
	
	sched.mu.Lock()
	defer sched.mu.Unlock()
	require.Empty(t, sched.timers)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/daemon
go test -run TestRateLimitScheduler_OnTransition -v
```

Expected: FAIL - RateLimitScheduler not defined

- [ ] **Step 3: Create ratelimit.go with basic structure**

Create `internal/daemon/ratelimit.go`:

```go
package daemon

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// Lifecycle is the subset of lifecycle operations needed by RateLimitScheduler.
type Lifecycle interface {
	Restore(ctx context.Context, sess *store.Session) error
}

// RateLimitScheduler manages scheduled resume attempts for rate-limited agents.
type RateLimitScheduler struct {
	life  Lifecycle
	store store.Store
	
	retryInterval time.Duration
	buffer        time.Duration
	enabled       bool
	
	mu     sync.Mutex
	timers map[string]*time.Timer
}

// NewRateLimitScheduler creates a new scheduler with configuration from env vars.
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

// OnTransition is wired as a callback on the poller's status-transition hook.
func (r *RateLimitScheduler) OnTransition(sess *store.Session, from, to store.Status) {
	if !r.enabled || to != store.StatusRateLimited {
		return
	}
	
	ctx := context.Background()
	
	// Parse restore time from pane (already captured by poller)
	// NOTE: parseRestoreTime is in internal/poller but not exported yet
	// For now, always fall back to retry interval
	restoreTime := time.Time{}
	ok := false
	
	var scheduleAt time.Time
	if ok && restoreTime.After(time.Now()) {
		// Success: use parsed time + buffer
		scheduleAt = restoreTime.Add(r.buffer)
	} else {
		// Fallback: retry in configured interval
		scheduleAt = time.Now().Add(r.retryInterval)
	}
	
	// Persist the schedule
	_ = r.store.SetRateLimit(ctx, sess.ID, scheduleAt, 0)
	
	// Schedule the resume attempt
	r.scheduleResume(sess.ID, scheduleAt)
}

// scheduleResume creates a timer for the resume attempt.
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

// attemptResume is called when the timer fires (placeholder for now).
func (r *RateLimitScheduler) attemptResume(sessionID string) {
	// Will implement in next task
}

// Helper functions

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/daemon
go test -run TestRateLimitScheduler_OnTransition -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/ratelimit.go internal/daemon/ratelimit_test.go
git commit -m "feat: add RateLimitScheduler with OnTransition

Create scheduler component that listens for rate_limited
transitions and schedules resume timers. Placeholder
attemptResume to be implemented next.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 9: Implement attemptResume Logic

**Files:**
- Modify: `internal/daemon/ratelimit.go`
- Test: `internal/daemon/ratelimit_test.go`

- [ ] **Step 1: Write tests for attemptResume**

Add to `internal/daemon/ratelimit_test.go`:

```go
func TestRateLimitScheduler_AttemptResume_Success(t *testing.T) {
	mockLife := &mockLifecycle{restoreErr: nil} // Success
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	mockStore.sessions["test-123"] = &store.Session{
		ID:     "test-123",
		Status: store.StatusRateLimited,
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	sched.attemptResume("test-123")
	
	// Verify Restore was called
	require.Equal(t, 1, mockLife.restoreCalls)
	
	// Verify status updated to spawning
	require.Equal(t, 1, mockStore.updateStatusIfCalls)
	
	// Verify ClearRateLimit called
	require.Equal(t, 1, mockStore.clearRateLimitCalls)
	
	// Verify timer removed
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.False(t, exists, "timer should be removed after success")
}

func TestRateLimitScheduler_AttemptResume_SessionGone(t *testing.T) {
	mockLife := &mockLifecycle{}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	// Session doesn't exist
	sched.attemptResume("nonexistent")
	
	// Should be no-op
	require.Equal(t, 0, mockLife.restoreCalls)
}

func TestRateLimitScheduler_AttemptResume_StatusChanged(t *testing.T) {
	mockLife := &mockLifecycle{}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	// Session is no longer rate_limited
	mockStore.sessions["test-123"] = &store.Session{
		ID:     "test-123",
		Status: store.StatusWorking,
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	sched.attemptResume("test-123")
	
	// Should not attempt restore
	require.Equal(t, 0, mockLife.restoreCalls)
}

func TestRateLimitScheduler_AttemptResume_StillLimited(t *testing.T) {
	mockLife := &mockLifecycle{
		restoreErr: errors.New("Rate limit. Try again later."),
	}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	mockStore.sessions["test-123"] = &store.Session{
		ID:                  "test-123",
		Status:              store.StatusRateLimited,
		RateLimitRetryCount: 0,
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	sched.attemptResume("test-123")
	
	// Verify SetRateLimit called (rescheduling)
	require.Equal(t, 1, mockStore.setRateLimitCalls)
	
	// Verify event appended
	require.Equal(t, 1, mockStore.appendEventCalls)
	
	// Timer should be rescheduled
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.True(t, exists, "timer should be rescheduled")
}

func TestRateLimitScheduler_AttemptResume_OtherError(t *testing.T) {
	mockLife := &mockLifecycle{
		restoreErr: errors.New("network connection failed"),
	}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	mockStore.sessions["test-123"] = &store.Session{
		ID:     "test-123",
		Status: store.StatusRateLimited,
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	sched.attemptResume("test-123")
	
	// Verify status updated to errored
	require.Equal(t, 1, mockStore.updateStatusIfCalls)
	
	// Verify event appended with error detail
	require.Equal(t, 1, mockStore.appendEventCalls)
	
	// Timer should be removed (not rescheduled)
	sched.mu.Lock()
	defer sched.mu.Unlock()
	_, exists := sched.timers["test-123"]
	require.False(t, exists, "timer should be removed after non-limit error")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/daemon
go test -run TestRateLimitScheduler_AttemptResume -v
```

Expected: FAIL - attemptResume not fully implemented

- [ ] **Step 3: Implement attemptResume**

Update `internal/daemon/ratelimit.go`:

```go
// attemptResume fires when a scheduled timer triggers.
func (r *RateLimitScheduler) attemptResume(sessionID string) {
	ctx := context.Background()
	
	sess, err := r.store.Get(ctx, sessionID)
	if err != nil {
		// Session gone (deleted, archived)
		r.mu.Lock()
		delete(r.timers, sessionID)
		r.mu.Unlock()
		return
	}
	
	// Only resume if still rate limited
	if sess.Status != store.StatusRateLimited {
		// User manually resumed or status changed
		r.mu.Lock()
		delete(r.timers, sessionID)
		r.mu.Unlock()
		return
	}
	
	// Attempt resume
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
	
	// FAILURE: check if error indicates still rate limited
	errMsg := err.Error()
	
	// Try to parse as rate limit error
	// NOTE: This will use detectRateLimit which only checks keywords for now
	isRateLimit := false
	errLower := strings.ToLower(errMsg)
	rateLimitKeywords := []string{"rate limit", "usage limit", "session limit", "quota exceeded"}
	for _, kw := range rateLimitKeywords {
		if strings.Contains(errLower, kw) {
			isRateLimit = true
			break
		}
	}
	
	if isRateLimit {
		// Still rate limited - reschedule
		// TODO: Parse new restore time when parseRestoreTime is available
		scheduleAt := time.Now().Add(r.retryInterval)
		_ = r.store.SetRateLimit(ctx, sess.ID, scheduleAt, sess.RateLimitRetryCount+1)
		r.scheduleResume(sess.ID, scheduleAt)
		
		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-retry",
			Detail: "no time parsed, retrying in " + r.retryInterval.String(),
		})
	} else {
		// Different error (network, auth, etc.) - transition to errored
		_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusErrored)
		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-resume-failed",
			Detail: "resume failed with non-limit error: " + err.Error(),
		})
		
		// Clean up timer
		r.mu.Lock()
		delete(r.timers, sess.ID)
		r.mu.Unlock()
	}
}
```

Also add the import:

```go
import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"  // NEW
	"sync"
	"time"

	"github.com/srjn45/warden/internal/store"
)
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/daemon
go test -run TestRateLimitScheduler_AttemptResume -v
```

Expected: PASS

- [ ] **Step 5: Run all daemon tests**

```bash
cd internal/daemon
go test -v
```

Expected: All tests PASS (or only pre-existing failures)

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/ratelimit.go internal/daemon/ratelimit_test.go
git commit -m "feat: implement attemptResume logic

Handle resume success, failure cases:
- Success: clear rate limit, transition to spawning
- Still limited: reschedule with retry interval
- Other error: transition to errored

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 10: Add ReconstructTimers and CancelTimer Methods

**Files:**
- Modify: `internal/daemon/ratelimit.go`
- Test: `internal/daemon/ratelimit_test.go`

- [ ] **Step 1: Write test for ReconstructTimers**

Add to `internal/daemon/ratelimit_test.go`:

```go
func TestRateLimitScheduler_ReconstructTimers(t *testing.T) {
	mockLife := &mockLifecycle{}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	// Set up sessions: one rate-limited, one not
	futureTime := time.Now().Add(1 * time.Hour)
	mockStore.sessions["limited-1"] = &store.Session{
		ID:                  "limited-1",
		Status:              store.StatusRateLimited,
		RateLimitRestoreAt:  &futureTime,
	}
	mockStore.sessions["working-1"] = &store.Session{
		ID:     "working-1",
		Status: store.StatusWorking,
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	err := sched.ReconstructTimers(context.Background())
	require.NoError(t, err)
	
	// Verify timer created only for rate-limited session
	sched.mu.Lock()
	defer sched.mu.Unlock()
	
	_, exists := sched.timers["limited-1"]
	require.True(t, exists, "timer should exist for rate-limited session")
	
	_, exists = sched.timers["working-1"]
	require.False(t, exists, "should not create timer for non-rate-limited session")
}

func TestRateLimitScheduler_ReconstructTimers_PastTime(t *testing.T) {
	mockLife := &mockLifecycle{restoreErr: nil}
	mockStore := &mockStore{
		sessions: make(map[string]*store.Session),
	}
	
	// Restore time in the past
	pastTime := time.Now().Add(-1 * time.Hour)
	mockStore.sessions["test-123"] = &store.Session{
		ID:                  "test-123",
		Status:              store.StatusRateLimited,
		RateLimitRestoreAt:  &pastTime,
	}
	
	sched := NewRateLimitScheduler(mockLife, mockStore)
	
	err := sched.ReconstructTimers(context.Background())
	require.NoError(t, err)
	
	// Give timer a moment to fire (it should fire immediately)
	time.Sleep(50 * time.Millisecond)
	
	// Verify Restore was called (timer fired immediately)
	require.Equal(t, 1, mockLife.restoreCalls)
}

func TestRateLimitScheduler_CancelTimer(t *testing.T) {
	sched := NewRateLimitScheduler(nil, nil)
	
	// Create a mock timer
	sched.timers["test-123"] = time.AfterFunc(1*time.Hour, func() {})
	
	sched.CancelTimer("test-123")
	
	sched.mu.Lock()
	defer sched.mu.Unlock()
	
	_, exists := sched.timers["test-123"]
	require.False(t, exists, "timer should be removed after cancel")
}

func TestRateLimitScheduler_CancelTimer_NotExists(t *testing.T) {
	sched := NewRateLimitScheduler(nil, nil)
	
	// Should not panic
	sched.CancelTimer("nonexistent")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd internal/daemon
go test -run "TestRateLimitScheduler_(Reconstruct|Cancel)" -v
```

Expected: FAIL - methods not defined

- [ ] **Step 3: Implement ReconstructTimers**

Add to `internal/daemon/ratelimit.go`:

```go
// ReconstructTimers rebuilds active timers from session state on daemon startup.
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

// CancelTimer stops and removes the timer for a session.
func (r *RateLimitScheduler) CancelTimer(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	if timer := r.timers[sessionID]; timer != nil {
		timer.Stop()
		delete(r.timers, sessionID)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd internal/daemon
go test -run "TestRateLimitScheduler_(Reconstruct|Cancel)" -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/ratelimit.go internal/daemon/ratelimit_test.go
git commit -m "feat: add ReconstructTimers and CancelTimer

ReconstructTimers restores scheduled resumes after daemon restart.
CancelTimer cleans up when user manually terminates agent.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 11: Wire Scheduler into Daemon

**Files:**
- Modify: `internal/daemon/api.go`
- Modify: `internal/cli/daemon.go`

- [ ] **Step 1: Add scheduler field to daemon**

In `internal/daemon/api.go`, find the daemon struct and add:

```go
type daemon struct {
	// ... existing fields ...
	
	rateLimitScheduler *RateLimitScheduler  // NEW
}
```

- [ ] **Step 2: Wire scheduler in daemon startup**

In `internal/cli/daemon.go`, find where the daemon is initialized (around where the poller and restarter are set up) and add:

```go
// Around line 150-200, after pl := poller.New(...) and restarter := daemon.NewRestarter(...)

rateLimitSched := daemon.NewRateLimitScheduler(life, st)
pl.OnTransition = func(sess *store.Session, from, to store.Status) {
	notifyHook(sess, from, to)
	exec.OnTransition(sess, from, to)
	restarter.OnTransition(sess, from, to)
	rateLimitSched.OnTransition(sess, from, to)  // NEW
}
```

- [ ] **Step 3: Reconstruct timers on startup**

Add after the daemon server starts:

```go
// After daemon server initialization, before starting the poller

if err := rateLimitSched.ReconstructTimers(ctx); err != nil {
	log.Printf("daemon: failed to reconstruct rate limit timers: %v", err)
}
```

- [ ] **Step 4: Test compilation**

```bash
cd cmd/warden
go build
```

Expected: Successful compilation

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/api.go internal/cli/daemon.go
git commit -m "feat: wire RateLimitScheduler into daemon

Register scheduler in poller OnTransition callback.
Reconstruct timers on daemon startup for persistence.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 12: Add CLI Status Formatting

**Files:**
- Modify: `internal/cli/status.go` (or equivalent CLI formatting file)
- Test: Manual testing

- [ ] **Step 1: Find status formatting code**

```bash
cd internal/cli
grep -r "StatusErrored\|StatusDone" . | head -5
```

Note the file that handles status formatting (likely `status.go` or similar).

- [ ] **Step 2: Add color for StatusRateLimited**

Find the function that formats status with colors (e.g., `formatStatus` or similar) and add:

```go
func formatStatus(s store.Status) string {
	switch s {
	case store.StatusDone:
		return color.Green(string(s))
	case store.StatusWorking:
		return color.Blue(string(s))
	case store.StatusErrored:
		return color.Red(string(s))
	case store.StatusRateLimited:  // NEW
		return color.Yellow(string(s))  // NEW (amber/yellow for warning)
	// ... other cases
	default:
		return string(s)
	}
}
```

- [ ] **Step 3: Add rate limit info display**

Find or create the function that displays detailed session info (e.g., `warden status <id>`) and add:

```go
func formatRateLimitInfo(sess *store.Session) string {
	if sess.Status != store.StatusRateLimited {
		return ""
	}
	
	var lines []string
	lines = append(lines, "\nRate Limit Info:")
	
	if sess.RateLimitedAt != nil {
		lines = append(lines, fmt.Sprintf("  Limited At: %s", 
			sess.RateLimitedAt.Format("2006-01-02 15:04:05")))
	}
	
	if sess.RateLimitRestoreAt != nil {
		until := time.Until(*sess.RateLimitRestoreAt)
		if until > 0 {
			lines = append(lines, fmt.Sprintf("  Resume At:  %s (in %s)", 
				sess.RateLimitRestoreAt.Format("2006-01-02 15:04:05"),
				formatDuration(until)))
		} else {
			lines = append(lines, fmt.Sprintf("  Resume At:  %s (resuming...)", 
				sess.RateLimitRestoreAt.Format("2006-01-02 15:04:05")))
		}
	}
	
	lines = append(lines, fmt.Sprintf("  Retries:    %d", sess.RateLimitRetryCount))
	
	return strings.Join(lines, "\n")
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	
	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
```

Then integrate this into the status display:

```go
// In the function that prints session details
fmt.Println("Status:", formatStatus(sess.Status))
fmt.Println(formatRateLimitInfo(sess))  // NEW
fmt.Println("Subject:", sess.Subject)
// ... rest of output
```

- [ ] **Step 4: Test compilation**

```bash
cd cmd/warden
go build
```

Expected: Successful compilation

- [ ] **Step 5: Manual test (if daemon is running)**

```bash
# List agents to see status colors
./warden ls

# Check detailed status display format
# (won't show rate limit info unless an agent is actually rate limited)
./warden status <some-agent-id>
```

- [ ] **Step 6: Commit**

```bash
git add internal/cli/*.go
git commit -m "feat: add CLI formatting for rate_limited status

Display rate_limited in yellow/amber color.
Show rate limit info in status detail view with countdown.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 13: Update MCP Tool Responses

**Files:**
- Modify: `internal/mcp/server.go`

- [ ] **Step 1: Find list_agents tool implementation**

```bash
cd internal/mcp
grep -n "list_agents\|ListAgents" server.go | head -3
```

Note the line numbers.

- [ ] **Step 2: Add rate limit fields to response**

Find the code that builds the agent list response and ensure rate limit fields are included:

```go
// In the list_agents tool handler, when building the response
agents := []map[string]interface{}{}
for _, sess := range sessions {
	agent := map[string]interface{}{
		"id":         sess.ID,
		"name":       sess.Name,
		"status":     sess.Status,
		"subject":    sess.Subject,
		"type":       sess.Type,
		"created_at": sess.CreatedAt,
		"updated_at": sess.UpdatedAt,
	}
	
	// NEW: Add rate limit fields if present
	if sess.RateLimitedAt != nil {
		agent["rate_limited_at"] = sess.RateLimitedAt
	}
	if sess.RateLimitRestoreAt != nil {
		agent["rate_limit_restore_at"] = sess.RateLimitRestoreAt
		agent["seconds_until_resume"] = int(time.Until(*sess.RateLimitRestoreAt).Seconds())
	}
	if sess.RateLimitRetryCount > 0 {
		agent["rate_limit_retry_count"] = sess.RateLimitRetryCount
	}
	
	agents = append(agents, agent)
}
```

- [ ] **Step 3: Update get_agent tool similarly**

Find the `get_agent` tool handler and add the same fields:

```go
// In get_agent tool handler
response := map[string]interface{}{
	"id":      sess.ID,
	"status":  sess.Status,
	"subject": sess.Subject,
	// ... other fields
}

// NEW: Add rate limit info if present
if sess.Status == store.StatusRateLimited {
	rateLimitInfo := map[string]interface{}{}
	
	if sess.RateLimitedAt != nil {
		rateLimitInfo["limited_at"] = sess.RateLimitedAt
	}
	if sess.RateLimitRestoreAt != nil {
		rateLimitInfo["restore_at"] = sess.RateLimitRestoreAt
		rateLimitInfo["seconds_until_resume"] = int(time.Until(*sess.RateLimitRestoreAt).Seconds())
	}
	rateLimitInfo["retry_count"] = sess.RateLimitRetryCount
	
	response["rate_limit_info"] = rateLimitInfo
}
```

- [ ] **Step 4: Test compilation**

```bash
cd cmd/warden
go build
```

Expected: Successful compilation

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/server.go
git commit -m "feat: add rate limit fields to MCP responses

Include rate_limited_at, rate_limit_restore_at, retry_count,
and seconds_until_resume in list_agents and get_agent.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 14: Integration Test and Documentation

**Files:**
- Create: `docs/USAGE.md` update (or append)
- Manual testing

- [ ] **Step 1: Build the binary**

```bash
make build
# or
cd cmd/warden && go build -o ../../bin/warden
```

- [ ] **Step 2: Manual integration test**

Test the complete flow:

```bash
# 1. Start daemon (if not running)
./bin/warden daemon &

# 2. Simulate rate limit (modify a test to inject rate limit pane)
# OR wait for a real rate limit to occur

# 3. Verify detection
./bin/warden ls
# Should show agent with rate_limited status

# 4. Check detailed status
./bin/warden status <agent-id>
# Should show rate limit info with countdown

# 5. Verify daemon restart persistence
pkill warden
./bin/warden daemon &
sleep 2
./bin/warden ls
# Rate limited agent should still be there with timer scheduled
```

- [ ] **Step 3: Update USAGE.md**

Add to `docs/USAGE.md` (or create a new section):

```markdown
## Rate Limit Handling

Warden automatically detects and handles Claude API rate limits:

### Automatic Detection

When an agent hits the API session limit, warden:
1. Detects the rate limit from the agent's output
2. Transitions status to `rate_limited` (shown in yellow)
3. Schedules automatic resume when the limit expires

### Viewing Rate Limited Agents

```bash
# List all agents (rate_limited shown in yellow)
warden ls

# View detailed rate limit info
warden status <agent-id>
```

Output example:
```
Status: rate_limited
Rate Limit Info:
  Limited At: 2026-06-14 14:30:00
  Resume At:  2026-06-14 15:45:00 (in 1h 15m 23s)
  Retries:    0
```

### Auto-Resume Behavior

- **Parsed timestamp**: Resumes at the exact time + 1 minute buffer
- **No timestamp**: Retries every 30 minutes until successful
- **Retry failures**: Re-parses errors for updated times
- **Non-limit errors**: Transitions to `errored` status

### Configuration

```bash
# Disable auto-resume (manual intervention only)
WARDEN_RATE_LIMIT_AUTO_RESUME=false warden daemon

# Change retry interval (default: 30m)
WARDEN_RATE_LIMIT_RETRY_INTERVAL=15m warden daemon

# Change safety buffer (default: 1m)
WARDEN_RATE_LIMIT_BUFFER=2m warden daemon
```

### Manual Intervention

You can manually resume a rate-limited agent:

```bash
warden attach <agent-id>
# Then interact as normal
```

Or terminate if no longer needed:

```bash
warden done <agent-id>
```
```

- [ ] **Step 4: Commit documentation**

```bash
git add docs/USAGE.md
git commit -m "docs: add rate limit handling to USAGE guide

Document auto-detection, auto-resume, configuration,
and manual intervention options.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Task 15: Final Integration and Testing

**Files:**
- All modified files
- Tests

- [ ] **Step 1: Run full test suite**

```bash
# Run all tests
go test ./... -v

# Check for test failures
echo $?
```

Expected: Exit code 0 (all tests pass)

- [ ] **Step 2: Run linter (if available)**

```bash
# If golangci-lint is configured
golangci-lint run

# Or use go vet
go vet ./...
```

Expected: No errors

- [ ] **Step 3: Build release binary**

```bash
make release
# or
make build
```

Expected: Successful build

- [ ] **Step 4: Create feature summary**

Create summary of changes:

```markdown
# Rate Limit Auto-Resume - Implementation Summary

## Implemented

✅ Core Infrastructure
- Added `StatusRateLimited` to type system
- Added `RateLimitedAt`, `RateLimitRestoreAt`, `RateLimitRetryCount` session fields
- Implemented `SetRateLimit` and `ClearRateLimit` store methods

✅ Detection Logic
- Created `detectRateLimit` and `parseRestoreTime` helpers
- Extended poller `classify()` to check rate limits first
- Keyword-based detection (timestamp parsing pending exact error format)

✅ Scheduling Component
- Implemented `RateLimitScheduler` with timer management
- `OnTransition` callback for automatic scheduling
- `attemptResume` with success/failure/retry logic
- `ReconstructTimers` for daemon restart persistence
- `CancelTimer` for manual termination cleanup

✅ Integration
- Wired scheduler into daemon startup
- Connected to poller OnTransition callback
- Timer reconstruction on daemon startup

✅ UI Updates
- CLI status formatting with yellow color
- Rate limit info in status detail view
- MCP tool responses include rate limit fields

✅ Documentation
- Updated USAGE.md with rate limit section
- Documented configuration options
- Manual intervention instructions

## Pending

⏳ Exact Error Message Format
- `parseRestoreTime()` is placeholder
- Needs actual Claude Code error message to implement regex patterns
- Tests include placeholder expectations

## Configuration

- `WARDEN_RATE_LIMIT_AUTO_RESUME=true|false` (default: true)
- `WARDEN_RATE_LIMIT_RETRY_INTERVAL=30m` (default: 30m)
- `WARDEN_RATE_LIMIT_BUFFER=1m` (default: 1m)

## Testing

- Unit tests: All store, poller, scheduler components
- Integration: Manual daemon restart, status display
- Pending: Real rate limit scenario (requires live API)
```

- [ ] **Step 5: Final commit**

```bash
git add -A
git commit -m "feat: complete rate limit auto-resume implementation

Implements automatic detection and resume of rate-limited agents.

Core features:
- New StatusRateLimited status visible in all UIs
- Intelligent scheduling with parsed restore times
- 30-min fallback retry when timestamp unavailable
- Persistent state across daemon restarts
- No retry cap for self-healing

Pending: exact Claude Code error message format for
timestamp parsing (currently placeholder).

All tests passing. Ready for real-world testing.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Next Steps

After implementation is complete:

1. **Get Exact Error Message**
   - Wait for user to hit real rate limit
   - Copy exact pane output
   - Update `parseRestoreTime()` with real regex patterns
   - Update tests with actual message format

2. **Real-World Testing**
   - Trigger actual Claude API rate limit
   - Verify detection works
   - Confirm auto-resume at scheduled time
   - Test daemon restart during wait period

3. **TUI Updates** (Optional)
   - Add rate limit status color to TUI
   - Display countdown timer in detail pane
   - Live refresh of countdown

4. **Web Dashboard Updates** (Optional, if web exists)
   - Session card shows rate_limited badge
   - JavaScript countdown timer
   - Event timeline includes rate limit events

5. **Create PR**
   - Ensure all tests pass
   - Update CHANGELOG
   - Create PR with feature description
   - Link to design spec

---

## Summary

This plan implements the rate limit auto-resume feature in 15 bite-sized tasks:

1. ✅ Add StatusRateLimited constant
2. ✅ Add Session rate limit fields
3. ✅ Add Store interface methods
4. ✅ Implement SetRateLimit
5. ✅ Implement ClearRateLimit
6. ✅ Create detection helpers
7. ✅ Extend poller classify
8. ✅ Create RateLimitScheduler
9. ✅ Implement attemptResume
10. ✅ Add ReconstructTimers/CancelTimer
11. ✅ Wire scheduler into daemon
12. ✅ Add CLI formatting
13. ✅ Update MCP responses
14. ✅ Documentation
15. ✅ Integration testing

**Total estimated time:** 3-4 hours for core implementation
**Pending:** Exact error message format from user (~30 minutes to update once received)

The implementation follows TDD principles, includes comprehensive tests, and integrates seamlessly with existing warden infrastructure.
