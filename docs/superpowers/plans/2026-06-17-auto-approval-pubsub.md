# Auto-Approval Pub-Sub System Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add event-driven auto-approval system to fix bug where prompts are missed when agent is already in waiting_for_input status.

**Architecture:** Add ApprovalEvent channel to Poller, publish events on status transitions AND pane changes while waiting_for_input, consume via background worker goroutine.

**Tech Stack:** Go 1.21+, tmux (existing), existing warden poller/store/lifecycle infrastructure

## Global Constraints

- Buffer size: 100 events (non-blocking publish, drop on full)
- Event triggers: (1) status transition to waiting_for_input, (2) pane change while already in waiting_for_input
- Worker goroutine: tracked by existing Poller.wg for clean shutdown
- Reuse existing `tryAutoApprove()` method (no duplication)
- Non-blocking publish (select with default case)
- All code in `main/internal/poller/poller.go` and `main/internal/poller/poller_test.go`

---

### Task 1: Add ApprovalEvent Type and Channel

**Files:**
- Modify: `main/internal/poller/poller.go:1-120` (add type, modify Poller struct and New())

**Interfaces:**
- Consumes: None
- Produces: `type ApprovalEvent struct { Session *store.Session; Pane string }`, `Poller.ApprovalEvents chan ApprovalEvent`

- [ ] **Step 1: Write test for ApprovalEvent channel initialization**

```go
// Add to main/internal/poller/poller_test.go
func TestPollerApprovalEventChannelInitialized(t *testing.T) {
	p := New(&FakeDeps{}, 30*time.Second)
	require.NotNil(t, p.ApprovalEvents, "ApprovalEvents channel should be initialized")
	
	// Verify channel is buffered with capacity 100
	require.Equal(t, 100, cap(p.ApprovalEvents), "ApprovalEvents should have buffer capacity of 100")
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd main
go test ./internal/poller/... -run TestPollerApprovalEventChannelInitialized -v
```

Expected: FAIL with "p.ApprovalEvents undefined"

- [ ] **Step 3: Add ApprovalEvent type**

Add after the Poller struct definition in `main/internal/poller/poller.go` (around line 80):

```go
// ApprovalEvent represents a potential auto-approval opportunity.
type ApprovalEvent struct {
	Session *store.Session // snapshot at event time
	Pane    string         // pane content that triggered the event
}
```

- [ ] **Step 4: Add ApprovalEvents field to Poller struct**

Add to the Poller struct in `main/internal/poller/poller.go` (around line 110):

```go
// ApprovalEvents is a buffered channel for approval opportunities.
// Published when: (1) status transitions to waiting_for_input, OR
// (2) pane changes while already in waiting_for_input.
// Consumed by the approval worker goroutine.
ApprovalEvents chan ApprovalEvent
```

- [ ] **Step 5: Initialize channel in New() constructor**

Modify the New() function in `main/internal/poller/poller.go` (around line 122):

```go
func New(d Deps, stuckAfter time.Duration) *Poller {
	return &Poller{
		deps:            d,
		stuckAfter:      stuckAfter,
		SummarizeAfter:  2 * time.Minute,
		lastSummary:     map[string]time.Time{},
		inflight:        map[string]struct{}{},
		lastCtxCheck:    map[string]time.Time{},
		CheckEvery:      20 * time.Second,
		CompactCooldown: 2 * time.Minute,
		ApprovalEvents:  make(chan ApprovalEvent, 100), // NEW LINE
	}
}
```

- [ ] **Step 6: Run test to verify it passes**

```bash
cd main
go test ./internal/poller/... -run TestPollerApprovalEventChannelInitialized -v
```

Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add main/internal/poller/poller.go main/internal/poller/poller_test.go
git commit -m "feat(poller): add ApprovalEvent type and buffered channel

Add ApprovalEvent struct to carry session + pane content for auto-approval.
Initialize ApprovalEvents channel with 100-event buffer in New().

Part of auto-approval pub-sub system.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 2: Add Non-Blocking Event Publisher

**Files:**
- Modify: `main/internal/poller/poller.go:375-390` (add publishApprovalEvent method after lastLines)

**Interfaces:**
- Consumes: `Poller.ApprovalEvents chan ApprovalEvent`, `*store.Session`, `string` (pane)
- Produces: `func (p *Poller) publishApprovalEvent(s *store.Session, pane string)`

- [ ] **Step 1: Write test for non-blocking publish**

```go
// Add to main/internal/poller/poller_test.go
func TestPublishApprovalEventNonBlocking(t *testing.T) {
	p := New(&FakeDeps{}, 30*time.Second)
	sess := &store.Session{ID: "agent-123", Status: store.StatusWaitingForInput}
	pane := "some pane content"
	
	// Publish should succeed
	p.publishApprovalEvent(sess, pane)
	
	// Verify event was queued
	select {
	case event := <-p.ApprovalEvents:
		require.Equal(t, "agent-123", event.Session.ID)
		require.Equal(t, pane, event.Pane)
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected event to be published")
	}
}

func TestPublishApprovalEventDropsWhenFull(t *testing.T) {
	p := New(&FakeDeps{}, 30*time.Second)
	sess := &store.Session{ID: "agent-123", Status: store.StatusWaitingForInput}
	
	// Fill the channel to capacity
	for i := 0; i < 100; i++ {
		p.ApprovalEvents <- ApprovalEvent{Session: sess, Pane: "fill"}
	}
	
	// Capture log output to verify drop message
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	
	// Publish should drop (not block)
	done := make(chan struct{})
	go func() {
		p.publishApprovalEvent(sess, "should drop")
		close(done)
	}()
	
	select {
	case <-done:
		// Success - didn't block
		require.Contains(t, buf.String(), "approval event dropped for agent-123")
		require.Contains(t, buf.String(), "channel full")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("publishApprovalEvent blocked when channel was full")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd main
go test ./internal/poller/... -run "TestPublishApprovalEvent" -v
```

Expected: FAIL with "p.publishApprovalEvent undefined"

- [ ] **Step 3: Implement publishApprovalEvent method**

Add after the lastLines function in `main/internal/poller/poller.go` (around line 375):

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

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd main
go test ./internal/poller/... -run "TestPublishApprovalEvent" -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add main/internal/poller/poller.go main/internal/poller/poller_test.go
git commit -m "feat(poller): add non-blocking approval event publisher

Implement publishApprovalEvent() with select/default to prevent blocking.
Logs dropped events when channel is full (graceful degradation).

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 3: Add Approval Worker Goroutine

**Files:**
- Modify: `main/internal/poller/poller.go:180-200` (add runApprovalWorker method before tick)
- Modify: `main/internal/poller/poller.go:351-367` (modify Run to start worker)

**Interfaces:**
- Consumes: `Poller.ApprovalEvents chan ApprovalEvent`, `Poller.tryAutoApprove(ctx, *store.Session, string)`
- Produces: `func (p *Poller) runApprovalWorker(ctx context.Context)`

- [ ] **Step 1: Write test for approval worker**

```go
// Add to main/internal/poller/poller_test.go
func TestApprovalWorkerConsumesEvents(t *testing.T) {
	deps := &FakeDeps{
		sessions: []*store.Session{
			{ID: "agent-123", Status: store.StatusWaitingForInput, TmuxSession: "tmux-123"},
		},
	}
	p := New(deps, 30*time.Second)
	p.AutoApproveGlobal = true
	
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	// Start worker
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.runApprovalWorker(ctx)
	}()
	
	// Publish an event with recognizable prompt
	sess := deps.sessions[0]
	pane := "Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	p.ApprovalEvents <- ApprovalEvent{Session: sess, Pane: pane}
	
	// Give worker time to process
	time.Sleep(50 * time.Millisecond)
	
	// Verify SendKeys was called with "1"
	require.Contains(t, deps.calledArgs(), []string{"SendKeys", "tmux-123", "1"})
	
	// Stop worker
	cancel()
	wg.Wait()
}

func TestApprovalWorkerStopsOnContextCancel(t *testing.T) {
	p := New(&FakeDeps{}, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	
	done := make(chan struct{})
	go func() {
		p.runApprovalWorker(ctx)
		close(done)
	}()
	
	// Cancel context
	cancel()
	
	// Verify worker stops
	select {
	case <-done:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("worker did not stop after context cancellation")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd main
go test ./internal/poller/... -run "TestApprovalWorker" -v
```

Expected: FAIL with "p.runApprovalWorker undefined"

- [ ] **Step 3: Implement runApprovalWorker method**

Add before the tick method in `main/internal/poller/poller.go` (around line 180):

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

- [ ] **Step 4: Wire worker startup in Run method**

Modify the Run method in `main/internal/poller/poller.go` (around line 351) to start the worker:

```go
func (p *Poller) Run(ctx context.Context, interval time.Duration) {
	// Start approval worker
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runApprovalWorker(ctx)
	}()
	
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Drain in-flight summarizers + approval worker; ctx cancellation
			// already aborts their work, so this returns promptly.
			p.wg.Wait()
			return
		case <-t.C:
			if err := p.tick(ctx); err != nil {
				log.Printf("poller tick: %v", err)
			}
		}
	}
}
```

- [ ] **Step 5: Run tests to verify they pass**

```bash
cd main
go test ./internal/poller/... -run "TestApprovalWorker" -v
```

Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add main/internal/poller/poller.go main/internal/poller/poller_test.go
git commit -m "feat(poller): add approval worker goroutine

Implement runApprovalWorker() that consumes ApprovalEvents and calls
tryAutoApprove(). Starts in Run() and tracked by existing wg for
clean shutdown on context cancellation.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 4: Publish Events on Status Transition

**Files:**
- Modify: `main/internal/poller/poller.go:257-260` (replace tryAutoApprove call with publishApprovalEvent)

**Interfaces:**
- Consumes: `Poller.publishApprovalEvent(s *store.Session, pane string)`
- Produces: Event published on transition to waiting_for_input

- [ ] **Step 1: Write test for event publishing on status transition**

```go
// Add to main/internal/poller/poller_test.go
func TestPublishEventOnStatusTransitionToWaitingForInput(t *testing.T) {
	pane := "Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	deps := &FakeDeps{
		sessions: []*store.Session{
			{ID: "agent-123", Status: store.StatusWorking, TmuxSession: "tmux-123", UpdatedAt: time.Now()},
		},
		paneContent: map[string]string{"tmux-123": pane},
	}
	
	p := New(deps, 30*time.Second)
	p.AutoApproveGlobal = true
	
	ctx := context.Background()
	
	// First tick: session is working
	err := p.tick(ctx)
	require.NoError(t, err)
	
	// Simulate session now shows waiting prompt
	deps.sessions[0].Status = store.StatusWorking // will transition to waiting_for_input
	deps.paneContent["tmux-123"] = pane + "\n\n❯ " // has ❯ cursor = waiting
	
	// Tick should detect transition and publish event
	err = p.tick(ctx)
	require.NoError(t, err)
	
	// Verify event was published
	select {
	case event := <-p.ApprovalEvents:
		require.Equal(t, "agent-123", event.Session.ID)
		require.Contains(t, event.Pane, "Do you want to proceed")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected approval event to be published on status transition")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd main
go test ./internal/poller/... -run TestPublishEventOnStatusTransitionToWaitingForInput -v
```

Expected: FAIL - no event published (currently calls tryAutoApprove directly)

- [ ] **Step 3: Replace tryAutoApprove call with publishApprovalEvent**

Modify the tick method in `main/internal/poller/poller.go` (around line 257):

```go
// Around line 257 - REPLACE the existing block:
// if next == store.StatusWaitingForInput && pane != "" {
//     p.tryAutoApprove(ctx, s, pane)
// }

// WITH:
if next == store.StatusWaitingForInput && pane != "" {
	p.publishApprovalEvent(s, pane)
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
cd main
go test ./internal/poller/... -run TestPublishEventOnStatusTransitionToWaitingForInput -v
```

Expected: PASS

- [ ] **Step 5: Run full poller test suite**

```bash
cd main
go test ./internal/poller/... -v
```

Expected: All tests PASS (verify existing auto-approval tests still work)

- [ ] **Step 6: Commit**

```bash
git add main/internal/poller/poller.go main/internal/poller/poller_test.go
git commit -m "feat(poller): publish approval event on status transition

Replace direct tryAutoApprove() call with publishApprovalEvent() when
transitioning to waiting_for_input. Maintains existing behavior via
async worker consumption.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 5: Publish Events on Pane Change While Waiting

**Files:**
- Modify: `main/internal/poller/poller.go:238-240` (add event publishing after pane update)

**Interfaces:**
- Consumes: `Poller.publishApprovalEvent(s *store.Session, pane string)`, `store.StatusWaitingForInput`
- Produces: Event published when pane changes while in waiting_for_input

- [ ] **Step 1: Write test for event publishing on pane change**

```go
// Add to main/internal/poller/poller_test.go
func TestPublishEventOnPaneChangeWhileWaiting(t *testing.T) {
	firstPrompt := "Do you want to proceed?\n ❯ 1. Yes\n   2. No"
	secondPrompt := "Another prompt?\n ❯ 1. Yes\n   2. No"
	
	deps := &FakeDeps{
		sessions: []*store.Session{
			{
				ID:          "agent-123",
				Status:      store.StatusWaitingForInput,
				TmuxSession: "tmux-123",
				UpdatedAt:   time.Now(),
				LastPaneExcerpt: lastLines(firstPrompt, 20),
			},
		},
		paneContent: map[string]string{"tmux-123": firstPrompt},
	}
	
	p := New(deps, 30*time.Second)
	p.AutoApproveGlobal = true
	
	ctx := context.Background()
	
	// First tick: already waiting with first prompt, no pane change
	err := p.tick(ctx)
	require.NoError(t, err)
	
	// Drain any initial event from the channel
	select {
	case <-p.ApprovalEvents:
	default:
	}
	
	// Simulate pane changes to show second prompt (still waiting_for_input)
	deps.paneContent["tmux-123"] = secondPrompt
	
	// Tick should detect pane change and publish event
	err = p.tick(ctx)
	require.NoError(t, err)
	
	// Verify event was published
	select {
	case event := <-p.ApprovalEvents:
		require.Equal(t, "agent-123", event.Session.ID)
		require.Contains(t, event.Pane, "Another prompt")
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected approval event on pane change while waiting_for_input")
	}
}

func TestNoEventOnPaneChangeWhenNotWaiting(t *testing.T) {
	deps := &FakeDeps{
		sessions: []*store.Session{
			{
				ID:          "agent-123",
				Status:      store.StatusWorking, // NOT waiting
				TmuxSession: "tmux-123",
				UpdatedAt:   time.Now(),
				LastPaneExcerpt: "old output",
			},
		},
		paneContent: map[string]string{"tmux-123": "old output"},
	}
	
	p := New(deps, 30*time.Second)
	ctx := context.Background()
	
	// First tick
	err := p.tick(ctx)
	require.NoError(t, err)
	
	// Change pane content
	deps.paneContent["tmux-123"] = "new output"
	
	// Second tick
	err = p.tick(ctx)
	require.NoError(t, err)
	
	// Verify NO event published (not waiting_for_input)
	select {
	case <-p.ApprovalEvents:
		t.Fatal("should not publish event when status is not waiting_for_input")
	case <-time.After(50 * time.Millisecond):
		// Success - no event
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd main
go test ./internal/poller/... -run "TestPublishEventOnPaneChange|TestNoEventOnPaneChange" -v
```

Expected: FAIL - no event published on pane change

- [ ] **Step 3: Add event publishing on pane change**

Modify the tick method in `main/internal/poller/poller.go` (around line 238):

```go
// Around line 238 - FIND the paneChanged block:
if alive && captureOK {
	captured, err := p.deps.CapturePane(ctx, s.TmuxSession)
	if err != nil {
		captureOK = false
	} else {
		pane = captured
		if excerpt := lastLines(pane, 20); excerpt != s.LastPaneExcerpt {
			_ = p.deps.UpdatePane(ctx, s.ID, excerpt)
			changed = true
			paneChanged = true
			
			// NEW: publish approval event if already waiting
			if s.Status == store.StatusWaitingForInput && pane != "" {
				p.publishApprovalEvent(s, pane)
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd main
go test ./internal/poller/... -run "TestPublishEventOnPaneChange|TestNoEventOnPaneChange" -v
```

Expected: PASS

- [ ] **Step 5: Run full poller test suite**

```bash
cd main
go test ./internal/poller/... -v
```

Expected: All tests PASS

- [ ] **Step 6: Commit**

```bash
git add main/internal/poller/poller.go main/internal/poller/poller_test.go
git commit -m "fix(poller): publish approval event on pane change while waiting

Fixes bug where auto-approval only triggers on status transitions.
Now also publishes event when pane changes while already in
waiting_for_input status (new prompt appears).

This is the core bug fix.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 6: Integration Test

**Files:**
- Modify: `main/internal/poller/poller_test.go` (add end-to-end integration test)

**Interfaces:**
- Consumes: All components (channel, publisher, worker, triggers)
- Produces: End-to-end test validating bug fix

- [ ] **Step 1: Write integration test**

```go
// Add to main/internal/poller/poller_test.go
func TestAutoApprovalEndToEnd(t *testing.T) {
	// Scenario: Agent shows first prompt (status transition), gets auto-approved,
	// then shows second prompt (pane change, no status transition), gets auto-approved.
	
	firstPrompt := "First prompt\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	secondPrompt := "Second prompt\nDo you want to continue?\n ❯ 1. Yes\n   2. No"
	
	deps := &FakeDeps{
		sessions: []*store.Session{
			{
				ID:          "agent-123",
				Status:      store.StatusWorking,
				TmuxSession: "tmux-123",
				UpdatedAt:   time.Now(),
			},
		},
		paneContent: map[string]string{"tmux-123": "working..."},
	}
	
	p := New(deps, 30*time.Second)
	p.AutoApproveGlobal = true
	
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	
	// Start worker
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.runApprovalWorker(ctx)
	}()
	
	// === First prompt: status transition to waiting_for_input ===
	
	// Simulate first prompt appearing
	deps.paneContent["tmux-123"] = firstPrompt
	
	// Tick should transition to waiting_for_input and publish event
	err := p.tick(ctx)
	require.NoError(t, err)
	
	// Give worker time to process
	time.Sleep(50 * time.Millisecond)
	
	// Verify SendKeys called for first prompt
	calls := deps.calledArgs()
	require.Contains(t, calls, []string{"SendKeys", "tmux-123", "1"}, "should auto-approve first prompt")
	
	// Clear call history
	deps.calls = nil
	
	// === Second prompt: pane change while already waiting_for_input ===
	
	// Session is now waiting_for_input (from previous transition)
	// Pane changes to show second prompt
	deps.sessions[0].Status = store.StatusWaitingForInput
	deps.sessions[0].LastPaneExcerpt = lastLines(firstPrompt, 20)
	deps.paneContent["tmux-123"] = secondPrompt
	
	// Tick should detect pane change and publish event (NO status change)
	err = p.tick(ctx)
	require.NoError(t, err)
	
	// Give worker time to process
	time.Sleep(50 * time.Millisecond)
	
	// Verify SendKeys called for second prompt (this is the bug fix validation)
	calls = deps.calledArgs()
	require.Contains(t, calls, []string{"SendKeys", "tmux-123", "1"}, "should auto-approve second prompt (bug fix)")
	
	// Stop worker
	cancel()
	p.wg.Wait()
}
```

- [ ] **Step 2: Run integration test**

```bash
cd main
go test ./internal/poller/... -run TestAutoApprovalEndToEnd -v
```

Expected: PASS (validates bug fix works end-to-end)

- [ ] **Step 3: Run full test suite**

```bash
cd main
go test ./internal/poller/... -v
```

Expected: All tests PASS

- [ ] **Step 4: Commit**

```bash
git add main/internal/poller/poller_test.go
git commit -m "test(poller): add end-to-end auto-approval integration test

Validates bug fix: agent shows first prompt (auto-approved via status
transition), then second prompt (auto-approved via pane change while
waiting). Second prompt is the regression test for the bug.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 7: Manual Testing and Documentation

**Files:**
- None (manual testing + log verification)

**Interfaces:**
- Consumes: Complete implementation
- Produces: Validated system behavior

- [ ] **Step 1: Rebuild warden daemon**

```bash
cd main
go build -o ~/.local/bin/warden-dev ./cmd/warden
```

Expected: Clean build with no errors

- [ ] **Step 2: Restart daemon with new binary**

```bash
# Stop existing daemon
systemctl --user stop warden

# Update daemon config to use dev binary temporarily
# Or just run manually:
WARDEN_AUTO_APPROVE=on ~/.local/bin/warden-dev daemon
```

Expected: Daemon starts, logs show "warden daemon listening on..."

- [ ] **Step 3: Test with agent that prompts multiple times**

In a separate terminal:

```bash
# Spawn a supervised agent
warden spawn --type development --repo /home/srajan/Development/warden --prompt "Update all test files in internal/daemon to use lifecycle.New with FakeConfig. Each file update requires approval."
```

Expected: Agent spawns and starts working

- [ ] **Step 4: Monitor daemon logs for auto-approval**

```bash
tail -f /tmp/warden.daemon.err | grep -E "auto-approv"
```

Expected output pattern:
```
2026/06/17 XX:XX:XX auto-approved agent-XXXXXXXX -> option 1: Yes
2026/06/17 XX:XX:XX auto-approved agent-XXXXXXXX -> option 1: Yes
2026/06/17 XX:XX:XX auto-approved agent-XXXXXXXX -> option 1: Yes
```

Multiple auto-approvals for same agent WITHOUT "auto-approve skipped" messages.

- [ ] **Step 5: Verify no missed prompts**

```bash
# Check approvals queue
warden approvals
```

Expected: Empty (all prompts auto-approved)

- [ ] **Step 6: Restore production daemon**

```bash
# Stop dev daemon (Ctrl+C if manual, or systemctl --user stop warden)
# Restore to production binary
systemctl --user start warden
```

Expected: Production daemon running with new feature

- [ ] **Step 7: Document manual test results**

Create summary comment in the final commit:

```
Manual testing validated:
- Multiple sequential prompts all auto-approved ✓
- No "auto-approve skipped" for recognized prompts ✓
- Daemon logs show successful auto-approval events ✓
- Approvals queue remains empty (all handled) ✓
- Bug fix: Second+ prompts now auto-approved (previously missed) ✓
```

- [ ] **Step 8: Final commit**

```bash
git commit --allow-empty -m "test: manual validation of auto-approval pub-sub system

Manually tested with multi-prompt agent workflow.
Confirmed bug fix: subsequent prompts (while waiting_for_input)
now trigger auto-approval via pane change events.

Results:
- Multiple sequential prompts: all auto-approved
- No skipped recognitions for valid prompts
- Approvals queue stayed empty
- Bug fix validated: pane-change events work correctly

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Self-Review Checklist

### 1. Spec Coverage

✅ **Architecture Overview** → Task 1 (types/channel), Task 3 (worker)
✅ **Data Structures** → Task 1 (ApprovalEvent, Poller field, constructor)
✅ **Event Publishing - Trigger 1** → Task 4 (status transition)
✅ **Event Publishing - Trigger 2** → Task 5 (pane change while waiting)
✅ **Publishing Method** → Task 2 (non-blocking publishApprovalEvent)
✅ **Worker Implementation** → Task 3 (runApprovalWorker)
✅ **Worker Startup** → Task 3 (modify Run())
✅ **Error Handling & Edge Cases** → Covered in tests (Task 2: channel full, Task 3: context cancel, Task 5: status check)
✅ **Testing Strategy** → Task 1-5 (unit tests), Task 6 (integration), Task 7 (manual)

### 2. Placeholder Scan

✅ No TBD/TODO/placeholders
✅ All code blocks complete
✅ All commands have expected output
✅ All edge cases have explicit tests

### 3. Type Consistency

✅ `ApprovalEvent` struct fields consistent across all tasks
✅ `publishApprovalEvent(s *store.Session, pane string)` signature consistent
✅ `runApprovalWorker(ctx context.Context)` signature consistent
✅ `Poller.ApprovalEvents chan ApprovalEvent` type consistent

All checks passed.
