# Auto-Approve Feature Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add automatic approval of yes/no prompts for agent sessions, controlled by global environment variable and per-agent toggle.

**Architecture:** Poller-driven auto-approval that reuses existing approval.Parse() and lifecycle.SendKeys() infrastructure. Global default via WARDEN_AUTO_APPROVE env var, per-agent override stored in session and toggleable via CLI.

**Tech Stack:** Go, existing warden internal packages (config, store, poller, daemon, cli)

---

## File Structure

**Modified Files:**
- `internal/config/config.go` - Add AutoApproveEnabled field and parsing
- `internal/config/config_test.go` - Test WARDEN_AUTO_APPROVE parsing
- `internal/store/types.go` - Add AutoApprove field to Session struct
- `internal/store/store.go` - Add UpdateAutoApprove to Store interface
- `internal/store/file.go` - Implement UpdateAutoApprove method
- `internal/store/file_test.go` - Test AutoApprove persistence
- `internal/poller/poller.go` - Add Deps.SendKeys, tryAutoApprove logic
- `internal/poller/poller_test.go` - Test auto-approve logic
- `internal/daemon/poller_deps.go` - Implement SendKeys in pollerDeps
- `internal/daemon/api.go` - Add PATCH /sessions/{id}/auto-approve route
- `internal/daemon/lifecycle_routes.go` - Add handleSetAutoApprove handler
- `internal/daemon/server.go` - Wire auto-approve config to poller
- `internal/cli/client.go` - Add SetAutoApprove client method
- `internal/cli/root.go` - Register auto-approve command

**New Files:**
- `internal/cli/auto_approve.go` - CLI command for toggling auto-approve

---

### Task 1: Add Config Support for WARDEN_AUTO_APPROVE

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write failing test for autoApproveEnabled() helper**

```go
// Add to internal/config/config_test.go after existing tests

func TestAutoApproveEnabled(t *testing.T) {
	tests := []struct {
		name string
		val  string
		want bool
	}{
		{"empty", "", false},
		{"0", "0", false},
		{"off", "off", false},
		{"OFF", "OFF", false},
		{"false", "false", false},
		{"FALSE", "FALSE", false},
		{"1", "1", true},
		{"on", "on", true},
		{"ON", "ON", true},
		{"true", "true", true},
		{"TRUE", "TRUE", true},
		{"junk", "junk", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("WARDEN_AUTO_APPROVE", tt.val)
			cfg := Load()
			if cfg.AutoApproveEnabled != tt.want {
				t.Errorf("AutoApproveEnabled = %v, want %v", cfg.AutoApproveEnabled, tt.want)
			}
		})
	}
}

func TestAutoApproveEnabledLegacy(t *testing.T) {
	t.Setenv("AGENTCTL_AUTO_APPROVE", "1")
	cfg := Load()
	if !cfg.AutoApproveEnabled {
		t.Error("legacy AGENTCTL_AUTO_APPROVE=1 should enable auto-approve")
	}
}

func TestAutoApproveEnabledPreferNewVar(t *testing.T) {
	t.Setenv("WARDEN_AUTO_APPROVE", "0")
	t.Setenv("AGENTCTL_AUTO_APPROVE", "1")
	cfg := Load()
	if cfg.AutoApproveEnabled {
		t.Error("WARDEN_AUTO_APPROVE should take precedence over AGENTCTL_AUTO_APPROVE")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config -run TestAutoApprove -v`
Expected: FAIL with "undefined: Config.AutoApproveEnabled"

- [ ] **Step 3: Add AutoApproveEnabled field to Config struct**

```go
// In internal/config/config.go, add to Config struct after ApprovalsEnabled

type Config struct {
	Addr                string
	DataDir             string
	ClaudeProjectsDir   string
	NotifyEnabled       bool
	ApprovalsEnabled    bool
	AutoApproveEnabled  bool // WARDEN_AUTO_APPROVE setting
	SpawnGateEnabled    bool
	SpawnGateMaxAgents  int
	MetricsEnabled      bool
	AllowNonLoopback    bool
	TokenGuard          bool
	TokenWarnAlert      bool
	TokenAutoCompact    bool
	TokenWarn           int
	TokenCritical       int
}
```

- [ ] **Step 4: Add autoApproveEnabled() helper function**

```go
// In internal/config/config.go, add after approvalsEnabled()

// autoApproveEnabled reads WARDEN_AUTO_APPROVE (legacy AGENTCTL_AUTO_APPROVE);
// OFF by default (opt-in safety), enabled only for 1/on/true. Gates the
// auto-approval feature (automatic option 1 selection for recognized prompts).
func autoApproveEnabled() bool {
	switch strings.ToLower(env("AUTO_APPROVE")) {
	case "1", "on", "true":
		return true
	}
	return false
}
```

- [ ] **Step 5: Wire autoApproveEnabled() into Load()**

```go
// In internal/config/config.go, modify Load() function to add AutoApproveEnabled

func Load() Config {
	tWarn := envInt("TOKEN_WARN", 200000)
	tCrit := envInt("TOKEN_CRITICAL", 400000)
	if tCrit <= tWarn { // inverted/degenerate config → defaults (warning must be reachable)
		tWarn, tCrit = 200000, 400000
	}
	return Config{
		Addr:               envOr2("ADDR", "127.0.0.1:8765"),
		DataDir:            envOr2("DATA_DIR", defaultDataDir()),
		ClaudeProjectsDir:  envOr("CLAUDE_PROJECTS_DIR", defaultClaudeProjectsDir()),
		NotifyEnabled:      notifyEnabled(),
		ApprovalsEnabled:   approvalsEnabled(),
		AutoApproveEnabled: autoApproveEnabled(), // Add this line
		SpawnGateEnabled:   spawnGateEnabled(),
		SpawnGateMaxAgents: spawnGateMaxAgents(),
		MetricsEnabled:     metricsEnabled(),
		AllowNonLoopback:   allowNonLoopback(),
		TokenGuard:         onByDefault("TOKEN_GUARD"),
		TokenWarnAlert:     onByDefault("TOKEN_WARN_ALERT"),
		TokenAutoCompact:   onByDefault("TOKEN_AUTO_COMPACT"),
		TokenWarn:          tWarn,
		TokenCritical:      tCrit,
	}
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/config -run TestAutoApprove -v`
Expected: PASS (all 3 tests)

- [ ] **Step 7: Run all config tests**

Run: `go test ./internal/config -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add WARDEN_AUTO_APPROVE config option

Add AutoApproveEnabled field to Config and autoApproveEnabled() helper.
Reads WARDEN_AUTO_APPROVE env var (legacy AGENTCTL_AUTO_APPROVE fallback).
Off by default, enabled for 1/on/true.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 2: Add AutoApprove Field to Session Store

**Files:**
- Modify: `internal/store/types.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/file.go`
- Modify: `internal/store/file_test.go`

- [ ] **Step 1: Write failing test for AutoApprove field persistence**

```go
// Add to internal/store/file_test.go after existing tests

func TestAutoApproveFieldPersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()

	// Insert session with AutoApprove = true
	s1 := &Session{
		ID:          "test-auto-approve-1",
		TmuxSession: "tmux-1",
		Repo:        "/repo",
		Status:      StatusWorking,
		AutoApprove: true,
	}
	require.NoError(t, st.Insert(ctx, s1))

	// Retrieve and verify
	got, err := st.Get(ctx, "test-auto-approve-1")
	require.NoError(t, err)
	require.True(t, got.AutoApprove, "AutoApprove should be true")

	// Insert session with AutoApprove = false (default)
	s2 := &Session{
		ID:          "test-auto-approve-2",
		TmuxSession: "tmux-2",
		Repo:        "/repo",
		Status:      StatusWorking,
		AutoApprove: false,
	}
	require.NoError(t, st.Insert(ctx, s2))

	got2, err := st.Get(ctx, "test-auto-approve-2")
	require.NoError(t, err)
	require.False(t, got2.AutoApprove, "AutoApprove should be false")
}

func TestUpdateAutoApprove(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()

	// Insert session with AutoApprove = false
	s := &Session{
		ID:          "test-update-auto",
		TmuxSession: "tmux-1",
		Repo:        "/repo",
		Status:      StatusWorking,
		AutoApprove: false,
	}
	require.NoError(t, st.Insert(ctx, s))

	// Update to true
	err = st.UpdateAutoApprove(ctx, "test-update-auto", true)
	require.NoError(t, err)

	got, err := st.Get(ctx, "test-update-auto")
	require.NoError(t, err)
	require.True(t, got.AutoApprove, "AutoApprove should be updated to true")

	// Update to false
	err = st.UpdateAutoApprove(ctx, "test-update-auto", false)
	require.NoError(t, err)

	got, err = st.Get(ctx, "test-update-auto")
	require.NoError(t, err)
	require.False(t, got.AutoApprove, "AutoApprove should be updated to false")
}

func TestUpdateAutoApproveNotFound(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	require.NoError(t, err)
	ctx := context.Background()

	err = st.UpdateAutoApprove(ctx, "nonexistent", true)
	require.ErrorIs(t, err, ErrNotFound)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store -run "TestAutoApprove|TestUpdateAutoApprove" -v`
Expected: FAIL with "undefined: Session.AutoApprove" and "undefined: Store.UpdateAutoApprove"

- [ ] **Step 3: Add AutoApprove field to Session struct**

```go
// In internal/store/types.go, add AutoApprove field after AutoRestart

type Session struct {
	ID              string     `json:"id"`
	Name            string     `json:"name,omitempty"` // optional human-friendly alias (max 32 chars, alphanumeric + hyphens/underscores)
	Type            Type       `json:"type"`
	Ticket          string     `json:"ticket"` // optional
	TmuxSession     string     `json:"tmux_session"`
	ClaudeSessionID string     `json:"claude_session_id"` // pinned claude --session-id (UUID); deterministic transcript + future --resume
	Repo            string     `json:"repo"`
	Worktree        string     `json:"worktree"` // optional (empty = no worktree)
	Branch          string     `json:"branch"`   // optional
	PR              string     `json:"pr"`       // optional (pr-review)
	Prompt          string     `json:"prompt"`   // initial prompt (prompt-spawned agents)
	Workdir         string     `json:"workdir"`  // absolute cwd of the tmux session
	Subject         string     `json:"subject"`  // one-line auto summary of what it's doing
	Status          Status     `json:"status"`
	PID             int        `json:"pid"`
	ExitCode        *int       `json:"exit_code,omitempty"` // process exit status when recovered: nil=unknown (orphaned/pre-feature), 0=clean, non-zero=crash
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	Events          []Event    `json:"events"`
	LastPaneExcerpt string     `json:"last_pane_excerpt"`
	Supervised      bool       `json:"supervised"`                // launched with --permission-mode acceptEdits (prompts) instead of bypass
	AutoRestart     bool       `json:"auto_restart,omitempty"`    // opt-in: auto-resume this agent when it errors (capped)
	RestartCount    int        `json:"restart_count,omitempty"`   // consecutive auto-restart attempts since last sustained-healthy run
	LastRestartAt   *time.Time `json:"last_restart_at,omitempty"` // when the most recent auto-restart fired
	AutoApprove     bool       `json:"auto_approve,omitempty"`    // opt-in: auto-approve yes/no prompts (always option 1)
	PipelineID      string     `json:"pipeline_id,omitempty"`     // set for pipeline jobs (back-ref)
	JobID           string     `json:"job_id,omitempty"`          // set for pipeline jobs (back-ref)

	ContextTokens    int        `json:"context_tokens,omitempty"`     // latest context-window fill; 0 = unknown (no model turn yet)
	ContextState     string     `json:"context_state,omitempty"`      // "" | ok | warning | critical
	ContextCheckedAt time.Time  `json:"context_checked_at,omitempty"` // when ContextTokens was last refreshed
	LastCompactAt    *time.Time `json:"last_compact_at,omitempty"`    // when warden last auto-sent /compact (cooldown guard)

	// Rate limit fields
	RateLimitedAt       *time.Time `json:"rate_limited_at,omitempty"`        // when limit was first hit
	RateLimitRestoreAt  *time.Time `json:"rate_limit_restore_at,omitempty"`  // scheduled resume time
	RateLimitRetryCount int        `json:"rate_limit_retry_count,omitempty"` // number of retry attempts
}
```

- [ ] **Step 4: Add UpdateAutoApprove to Store interface**

```go
// In internal/store/store.go, add UpdateAutoApprove to Store interface

type Store interface {
	Insert(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	GetByNameOrID(ctx context.Context, nameOrID string) (*Session, error)
	List(ctx context.Context) ([]*Session, error)
	Delete(ctx context.Context, id string) error
	Update(ctx context.Context, id string, fn func(*Session)) error
	UpdateStatusIf(ctx context.Context, id string, expected, next Status) (bool, error)
	UpdatePane(ctx context.Context, id, excerpt string) error
	UpdateSubject(ctx context.Context, id, subject string) error
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	StampCompact(ctx context.Context, id string) error
	UpdateAutoApprove(ctx context.Context, id string, enabled bool) error // Add this line
	FinalizeExit(ctx context.Context, id string, expected, next Status, code int) (bool, error)
	Rename(ctx context.Context, id, newName string) error
	Archive(ctx context.Context, id string) error
	ListArchived(ctx context.Context) ([]*Session, error)
	RestoreArchived(ctx context.Context, id string) error
	UpdateRateLimit(ctx context.Context, id string, fn func(*Session)) error
}
```

- [ ] **Step 5: Implement UpdateAutoApprove in FileStore**

```go
// In internal/store/file.go, add UpdateAutoApprove method after StampCompact

func (fs *FileStore) UpdateAutoApprove(ctx context.Context, id string, enabled bool) error {
	return fs.Update(ctx, id, func(s *Session) {
		s.AutoApprove = enabled
	})
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/store -run "TestAutoApprove|TestUpdateAutoApprove" -v`
Expected: PASS (all 3 tests)

- [ ] **Step 7: Run all store tests**

Run: `go test ./internal/store -v`
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/store/types.go internal/store/store.go internal/store/file.go internal/store/file_test.go
git commit -m "feat: add AutoApprove field to Session store

Add AutoApprove bool field to Session struct for per-agent auto-approval.
Add UpdateAutoApprove() method to Store interface and FileStore.
Field persists in session JSON files.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 3: Add SendKeys to Poller Deps

**Files:**
- Modify: `internal/poller/poller.go`
- Modify: `internal/daemon/poller_deps.go`

- [ ] **Step 1: Add SendKeys to Deps interface**

```go
// In internal/poller/poller.go, add SendKeys to Deps interface after Compact

type Deps interface {
	List(ctx context.Context) ([]*store.Session, error)
	// UpdateStatusIf swaps status from expected→next, reporting whether it took
	// effect. The poller uses the CAS form so it never overwrites a status a hook
	// changed between this tick's List and its write.
	UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error)
	UpdatePane(ctx context.Context, id, excerpt string) error
	UpdateSubject(ctx context.Context, id, subject string) error
	SessionAlive(ctx context.Context, tmuxName string) bool
	CapturePane(ctx context.Context, tmuxName string) (string, error)
	Summarize(ctx context.Context, s *store.Session) (string, error)
	// ExitCode returns the exit status recorded for the agent's shell, if any.
	ExitCode(ctx context.Context, id string) (code int, present bool)
	// FinalizeExit transitions the session to its terminal status from the exit
	// code (CAS on expected), recording the code (+ event for crashes).
	FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error)
	// ClearExit removes the consumed exit-file so it can't be re-read.
	ClearExit(ctx context.Context, id string)
	// ContextTokens returns the agent's current context-window occupancy read
	// from its transcript. ok=false when no model turn has been recorded yet.
	ContextTokens(ctx context.Context, s *store.Session) (tokens int, ok bool)
	// UpdateContext persists the gauge (tokens + state band).
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	// Compact sends "/compact" to the agent (only called when it is idle/waiting).
	Compact(ctx context.Context, s *store.Session) error
	// StampCompact records that /compact was just sent (cooldown guard).
	StampCompact(ctx context.Context, id string) error
	// SendKeys sends a single key (e.g. numbered menu option) to the agent's tmux pane.
	SendKeys(ctx context.Context, tmuxSession, keys string) error
}
```

- [ ] **Step 2: Implement SendKeys in pollerDeps**

```go
// In internal/daemon/poller_deps.go, add SendKeys method after StampCompact

func (d *pollerDeps) StampCompact(ctx context.Context, id string) error {
	return d.store.StampCompact(ctx, id)
}

func (d *pollerDeps) SendKeys(ctx context.Context, tmuxSession, keys string) error {
	return d.lc.SendKeys(ctx, tmuxSession, keys)
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./internal/poller ./internal/daemon`
Expected: Success (no errors)

- [ ] **Step 4: Commit**

```bash
git add internal/poller/poller.go internal/daemon/poller_deps.go
git commit -m "feat: add SendKeys to poller Deps interface

Add SendKeys() to Deps interface for auto-approval support.
Implement in pollerDeps by delegating to lifecycle.SendKeys().

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 4: Implement Auto-Approval Logic in Poller

**Files:**
- Modify: `internal/poller/poller.go`
- Modify: `internal/poller/poller_test.go`

- [ ] **Step 1: Write failing test for tryAutoApprove**

```go
// Add to internal/poller/poller_test.go after existing tests

func TestTryAutoApprove(t *testing.T) {
	// Mock deps that track SendKeys calls
	var sendKeysLog []struct{ session, keys string }
	deps := &mockDeps{
		sendKeys: func(ctx context.Context, sess, keys string) error {
			sendKeysLog = append(sendKeysLog, struct{ session, keys string }{sess, keys})
			return nil
		},
	}

	p := New(deps, 0)
	p.AutoApproveGlobal = true
	ctx := context.Background()

	// Test 1: Auto-approve disabled globally and per-session
	sess := &store.Session{
		ID:          "test-1",
		TmuxSession: "tmux-1",
		AutoApprove: false,
	}
	p.AutoApproveGlobal = false
	p.tryAutoApprove(ctx, sess, "Do you want?\n❯ 1. Yes\n  2. No")
	if len(sendKeysLog) != 0 {
		t.Error("should not auto-approve when disabled")
	}

	// Test 2: Auto-approve enabled globally, recognized prompt
	p.AutoApproveGlobal = true
	p.tryAutoApprove(ctx, sess, "Bash(ls)\nDo you want to proceed?\n❯ 1. Yes\n  2. No")
	if len(sendKeysLog) != 1 || sendKeysLog[0].keys != "1" {
		t.Errorf("should auto-approve, got sendKeys log: %+v", sendKeysLog)
	}

	// Test 3: Auto-approve enabled per-session, recognized prompt
	sendKeysLog = nil
	p.AutoApproveGlobal = false
	sess.AutoApprove = true
	p.tryAutoApprove(ctx, sess, "Edit(file.go)\nDo you want to proceed?\n❯ 1. Yes\n  2. No\n  3. Other")
	if len(sendKeysLog) != 1 || sendKeysLog[0].keys != "1" {
		t.Errorf("should auto-approve with per-session override, got: %+v", sendKeysLog)
	}

	// Test 4: Unrecognized prompt (no options)
	sendKeysLog = nil
	p.AutoApproveGlobal = true
	p.tryAutoApprove(ctx, sess, "What would you like me to do?")
	if len(sendKeysLog) != 0 {
		t.Error("should skip unrecognized prompt")
	}

	// Test 5: SendKeys fails
	sendKeysLog = nil
	deps.sendKeys = func(ctx context.Context, sess, keys string) error {
		return fmt.Errorf("tmux not responding")
	}
	p.tryAutoApprove(ctx, sess, "Do you want?\n❯ 1. Yes\n  2. No")
	// Should not panic, just log error (we verify no panic by reaching here)
}

// mockDeps for testing
type mockDeps struct {
	list             func(context.Context) ([]*store.Session, error)
	updateStatusIf   func(context.Context, string, store.Status, store.Status) (bool, error)
	updatePane       func(context.Context, string, string) error
	updateSubject    func(context.Context, string, string) error
	sessionAlive     func(context.Context, string) bool
	capturePane      func(context.Context, string) (string, error)
	summarize        func(context.Context, *store.Session) (string, error)
	exitCode         func(context.Context, string) (int, bool)
	finalizeExit     func(context.Context, string, store.Status, store.Status, int) (bool, error)
	clearExit        func(context.Context, string)
	contextTokens    func(context.Context, *store.Session) (int, bool)
	updateContext    func(context.Context, string, int, string) error
	compact          func(context.Context, *store.Session) error
	stampCompact     func(context.Context, string) error
	sendKeys         func(context.Context, string, string) error
}

func (m *mockDeps) List(ctx context.Context) ([]*store.Session, error) {
	if m.list != nil {
		return m.list(ctx)
	}
	return nil, nil
}

func (m *mockDeps) UpdateStatusIf(ctx context.Context, id string, exp, next store.Status) (bool, error) {
	if m.updateStatusIf != nil {
		return m.updateStatusIf(ctx, id, exp, next)
	}
	return false, nil
}

func (m *mockDeps) UpdatePane(ctx context.Context, id, ex string) error {
	if m.updatePane != nil {
		return m.updatePane(ctx, id, ex)
	}
	return nil
}

func (m *mockDeps) UpdateSubject(ctx context.Context, id, subj string) error {
	if m.updateSubject != nil {
		return m.updateSubject(ctx, id, subj)
	}
	return nil
}

func (m *mockDeps) SessionAlive(ctx context.Context, name string) bool {
	if m.sessionAlive != nil {
		return m.sessionAlive(ctx, name)
	}
	return true
}

func (m *mockDeps) CapturePane(ctx context.Context, name string) (string, error) {
	if m.capturePane != nil {
		return m.capturePane(ctx, name)
	}
	return "", nil
}

func (m *mockDeps) Summarize(ctx context.Context, s *store.Session) (string, error) {
	if m.summarize != nil {
		return m.summarize(ctx, s)
	}
	return "", nil
}

func (m *mockDeps) ExitCode(ctx context.Context, id string) (int, bool) {
	if m.exitCode != nil {
		return m.exitCode(ctx, id)
	}
	return 0, false
}

func (m *mockDeps) FinalizeExit(ctx context.Context, id string, exp, next store.Status, code int) (bool, error) {
	if m.finalizeExit != nil {
		return m.finalizeExit(ctx, id, exp, next, code)
	}
	return false, nil
}

func (m *mockDeps) ClearExit(ctx context.Context, id string) {
	if m.clearExit != nil {
		m.clearExit(ctx, id)
	}
}

func (m *mockDeps) ContextTokens(ctx context.Context, s *store.Session) (int, bool) {
	if m.contextTokens != nil {
		return m.contextTokens(ctx, s)
	}
	return 0, false
}

func (m *mockDeps) UpdateContext(ctx context.Context, id string, tokens int, state string) error {
	if m.updateContext != nil {
		return m.updateContext(ctx, id, tokens, state)
	}
	return nil
}

func (m *mockDeps) Compact(ctx context.Context, s *store.Session) error {
	if m.compact != nil {
		return m.compact(ctx, s)
	}
	return nil
}

func (m *mockDeps) StampCompact(ctx context.Context, id string) error {
	if m.stampCompact != nil {
		return m.stampCompact(ctx, id)
	}
	return nil
}

func (m *mockDeps) SendKeys(ctx context.Context, sess, keys string) error {
	if m.sendKeys != nil {
		return m.sendKeys(ctx, sess, keys)
	}
	return nil
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller -run TestTryAutoApprove -v`
Expected: FAIL with "undefined: Poller.AutoApproveGlobal" and "undefined: Poller.tryAutoApprove"

- [ ] **Step 3: Add AutoApproveGlobal field to Poller struct**

```go
// In internal/poller/poller.go, add AutoApproveGlobal field to Poller struct

type Poller struct {
	deps           Deps
	stuckAfter     time.Duration
	SummarizeAfter time.Duration        // throttle for subject refresh (0 = every change)
	lastSummary    map[string]time.Time // touched only by the tick goroutine
	// OnChange, if set, is called once after a tick that changed any session
	// (status or pane), and again from a summarizer worker when it refreshes a
	// subject. The daemon wires this to hub.publish for SSE.
	OnChange func()

	// OnTransition, if set, is called once per successful status swap with the
	// session and its old/new status (edge-triggered — once per transition, not
	// per tick). The daemon wires this to fire user notifications.
	OnTransition func(sess *store.Session, from, to store.Status)

	// Context-size guard config + hooks (set by the daemon after New). When
	// TokenGuard is false the whole check is skipped. CompactCooldown bounds how
	// often /compact may be auto-sent to one agent.
	TokenGuard      bool
	TokenWarn       int
	TokenCrit       int
	WarnAlert       bool
	AutoCompact     bool
	CompactCooldown time.Duration
	CheckEvery      time.Duration // throttle for the per-agent transcript read
	// OnContextAlert, if set, fires once per upward threshold crossing.
	OnContextAlert func(sess *store.Session, state ctxtokens.State, tokens int)

	// AutoApproveGlobal is the global default for auto-approval (from config).
	// Per-session AutoApprove overrides this.
	AutoApproveGlobal bool

	lastCtxCheck map[string]time.Time // last context read per session (tick goroutine only)

	// Summarization runs `claude -p`, which is slow, so it is dispatched to
	// background workers rather than blocking the tick loop. mu guards inflight;
	// wg tracks live workers so Run can drain them on shutdown.
	mu       sync.Mutex
	inflight map[string]struct{} // session ids with a summarizer currently running
	wg       sync.WaitGroup
}
```

- [ ] **Step 4: Add tryAutoApprove method to Poller**

```go
// In internal/poller/poller.go, add tryAutoApprove method after checkContext

// tryAutoApprove attempts to auto-approve a recognized yes/no prompt by sending
// option 1. It is called after the session transitions to StatusWaitingForInput.
// Only attempts auto-approval if:
// - AutoApproveGlobal OR session.AutoApprove is true
// - The pane content parses as a recognized prompt (approval.Parse ok=true)
// - Parsed options list has at least one option
// Logs all attempts (success/skip/failure) for auditing.
func (p *Poller) tryAutoApprove(ctx context.Context, s *store.Session, pane string) {
	// Check if auto-approve enabled (global OR per-session)
	if !p.AutoApproveGlobal && !s.AutoApprove {
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

- [ ] **Step 5: Add approval import to poller.go**

```go
// In internal/poller/poller.go, add to imports at top of file

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/approval"  // Add this line
	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
)
```

- [ ] **Step 6: Integrate tryAutoApprove into tick loop**

```go
// In internal/poller/poller.go, find the tick() method and add tryAutoApprove call
// after UpdateStatusIf for StatusWaitingForInput transitions.
// Look for the section that handles status updates around line 150-200.
// Add after the UpdateStatusIf call that transitions to StatusWaitingForInput:

// Around line 170 in tick(), after the classify() call and UpdateStatusIf:
if newStatus != s.Status {
	took, err := p.deps.UpdateStatusIf(ctx, s.ID, s.Status, newStatus)
	if err != nil {
		log.Printf("poller: update status %s: %v", s.ID, err)
		continue
	}
	if !took {
		continue
	}
	changed = true
	old := s.Status
	s.Status = newStatus

	// Fire OnTransition callback (notifications, executor reconcile, etc.)
	if p.OnTransition != nil {
		p.OnTransition(s, old, newStatus)
	}

	// Auto-approve if transitioned to waiting_for_input (add this block)
	if newStatus == store.StatusWaitingForInput && pane != "" {
		p.tryAutoApprove(ctx, s, pane)
	}
}
```

- [ ] **Step 7: Run test to verify it passes**

Run: `go test ./internal/poller -run TestTryAutoApprove -v`
Expected: PASS

- [ ] **Step 8: Run all poller tests**

Run: `go test ./internal/poller -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "feat: implement auto-approval logic in poller

Add tryAutoApprove() method that auto-selects option 1 for recognized prompts.
Integrate into tick() loop after StatusWaitingForInput transition.
Add AutoApproveGlobal field to Poller for global default.
Log all auto-approval attempts (success/skip/failure).

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 5: Wire Auto-Approve Config to Poller in Daemon

**Files:**
- Modify: `internal/daemon/server.go`

- [ ] **Step 1: Find where Poller is initialized in Server**

Read: `internal/daemon/server.go` and locate the `NewServer` or poller initialization code

- [ ] **Step 2: Set AutoApproveGlobal from config**

```go
// In internal/daemon/server.go, find where poller is created (likely in NewServer)
// Add line to set AutoApproveGlobal from cfg.AutoApproveEnabled
// Example location (adjust based on actual code):

func NewServer(cfg config.Config, ...) (*Server, error) {
	// ... existing code ...
	
	poll := poller.New(pollerDeps, stuckAfter)
	poll.AutoApproveGlobal = cfg.AutoApproveEnabled  // Add this line
	poll.TokenGuard = cfg.TokenGuard
	poll.TokenWarn = cfg.TokenWarn
	// ... rest of poller config ...
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./cmd/warden`
Expected: Success (no errors)

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/server.go
git commit -m "feat: wire auto-approve config to poller

Set Poller.AutoApproveGlobal from Config.AutoApproveEnabled.
Enables global auto-approval default from WARDEN_AUTO_APPROVE env var.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 6: Add Daemon API Route for SetAutoApprove

**Files:**
- Modify: `internal/daemon/lifecycle_routes.go`
- Modify: `internal/daemon/api.go`

- [ ] **Step 1: Add SetAutoApproveRequest type and handler**

```go
// In internal/daemon/lifecycle_routes.go, add after other route handlers

// SetAutoApproveRequest is the body for PATCH /sessions/{id}/auto-approve.
type SetAutoApproveRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetAutoApprove updates a session's AutoApprove flag.
func (s *Server) handleSetAutoApprove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req SetAutoApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}

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

- [ ] **Step 2: Add route to API router**

```go
// In internal/daemon/api.go, add route to sessions router
// Find the sessions router setup (likely in setupRoutes or similar method)

func (s *Server) routes() chi.Router {
	// ... existing routes ...
	
	r.Route("/sessions/{id}", func(r chi.Router) {
		r.Get("/", s.handleGetSession)
		r.Delete("/", s.handleDeleteSession)
		r.Post("/approve", s.handleApprove)
		r.Patch("/auto-approve", s.handleSetAutoApprove)  // Add this line
		r.Patch("/rename", s.handleRenameSession)
		// ... other session routes ...
	})
	
	// ... rest of routes ...
}
```

- [ ] **Step 3: Verify compilation**

Run: `go build ./internal/daemon`
Expected: Success (no errors)

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/lifecycle_routes.go internal/daemon/api.go
git commit -m "feat: add PATCH /sessions/{id}/auto-approve API route

Add handleSetAutoApprove handler that updates session AutoApprove field.
Returns 404 if session not found, 200 with updated value on success.
Triggers SSE notification on update.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 7: Add CLI Command for Auto-Approve

**Files:**
- Create: `internal/cli/auto_approve.go`
- Modify: `internal/cli/client.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Create auto_approve.go with command**

```go
// Create internal/cli/auto_approve.go

package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAutoApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "auto-approve <agent-id> <on|off>",
		Short: "Enable or disable auto-approval for an agent's prompts",
		Long: `Enable or disable automatic approval of yes/no prompts for a specific agent.

When auto-approve is enabled, the daemon automatically selects option 1 for
recognized yes/no tool-permission prompts. Unrecognized prompts, multi-select,
and text-entry fields are skipped and require manual approval.

Examples:
  warden auto-approve abc123 on   # Enable auto-approve for agent abc123
  warden auto-approve abc123 off  # Disable auto-approve for agent abc123

Global default is controlled by WARDEN_AUTO_APPROVE environment variable.
Per-agent setting overrides the global default.`,
		Args: cobra.ExactArgs(2),
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

- [ ] **Step 2: Add SetAutoApprove to Client**

```go
// In internal/cli/client.go, add SetAutoApprove method after other methods

func (c *Client) SetAutoApprove(ctx context.Context, id string, enabled bool) error {
	body := map[string]bool{"enabled": enabled}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "PATCH",
		c.base+"/sessions/"+id+"/auto-approve",
		bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("session %s not found", id)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server error: %s", body)
	}
	return nil
}
```

- [ ] **Step 3: Add imports to client.go if needed**

```go
// In internal/cli/client.go, ensure these imports are present:

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	// ... other imports ...
)
```

- [ ] **Step 4: Register command in root.go**

```go
// In internal/cli/root.go, add to newRootCmd() where commands are registered

func newRootCmd() *cobra.Command {
	// ... existing setup ...
	
	root.AddCommand(
		newListCmd(),
		newSpawnCmd(),
		newAttachCmd(),
		newApprovalsCmd(),
		newApproveCmd(),
		newAutoApproveCmd(),  // Add this line
		newDeleteCmd(),
		// ... other commands ...
	)
	
	return root
}
```

- [ ] **Step 5: Verify compilation**

Run: `go build ./cmd/warden`
Expected: Success (no errors)

- [ ] **Step 6: Test command help**

Run: `go run ./cmd/warden auto-approve --help`
Expected: Help text displays correctly

- [ ] **Step 7: Commit**

```bash
git add internal/cli/auto_approve.go internal/cli/client.go internal/cli/root.go
git commit -m "feat: add warden auto-approve CLI command

Add 'warden auto-approve <id> on|off' command to toggle per-agent auto-approval.
Add Client.SetAutoApprove() that calls PATCH /sessions/{id}/auto-approve.
Command validates mode argument and returns confirmation message.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

### Task 8: Integration Testing and Documentation

**Files:**
- Modify: `README.md` or `docs/FEATURES.md` (if documentation exists)

- [ ] **Step 1: Manual integration test - spawn agent**

Run: `WARDEN_AUTO_APPROVE=on warden daemon`
Then in another terminal: `warden spawn development --prompt "test auto approve"`
Expected: Daemon starts with auto-approve enabled globally

- [ ] **Step 2: Manual integration test - trigger prompt**

In the spawned agent, trigger a tool permission prompt (e.g., by asking Claude to run a bash command).
Expected: Prompt auto-approved (option 1 selected automatically), agent proceeds

- [ ] **Step 3: Manual integration test - per-agent toggle**

Run: `warden auto-approve <agent-id> off`
Trigger another prompt in the same agent.
Expected: Prompt NOT auto-approved, waits for manual intervention

Run: `warden auto-approve <agent-id> on`
Trigger another prompt.
Expected: Prompt auto-approved again

- [ ] **Step 4: Manual integration test - daemon restart**

Stop daemon, restart with `WARDEN_AUTO_APPROVE=off warden daemon`
Check existing agent with AutoApprove=true persisted.
Trigger prompt.
Expected: Per-agent setting survives restart, prompt still auto-approved

- [ ] **Step 5: Add documentation to FEATURES.md**

```markdown
## Auto-Approve

Automatically approve yes/no tool-permission prompts for agents.

**Configuration:**
- `WARDEN_AUTO_APPROVE=on` - Enable auto-approval globally (off by default)
- Per-agent override: `warden auto-approve <id> on|off`

**Behavior:**
- Automatically selects option 1 for recognized yes/no prompts
- Skips multi-select, text-entry, and unrecognized prompts (manual approval required)
- Logs all auto-approval attempts for auditing

**Safety:**
- Off by default (opt-in)
- Only works with recognized prompt grammar
- Always selects option 1 (predictable behavior)
- Never retries on failure (fail-safe to manual approval)

**Examples:**
```bash
# Enable globally
export WARDEN_AUTO_APPROVE=on
warden daemon

# Toggle for specific agent
warden auto-approve abc123 on
warden auto-approve abc123 off
```
```

- [ ] **Step 6: Run full test suite**

Run: `go test ./...`
Expected: All tests pass

- [ ] **Step 7: Commit documentation**

```bash
git add docs/FEATURES.md
git commit -m "docs: add auto-approve feature documentation

Document WARDEN_AUTO_APPROVE env var and warden auto-approve command.
Explain behavior, safety guarantees, and usage examples.

Co-Authored-By: Claude Sonnet 4.5 <noreply@anthropic.com>"
```

---

## Self-Review Checklist

**Spec Coverage:**
- ✅ R1: Global Configuration - Task 1 (config.go, WARDEN_AUTO_APPROVE)
- ✅ R2: Per-Agent Override - Task 2 (Session.AutoApprove, UpdateAutoApprove)
- ✅ R3: CLI Control - Task 7 (auto-approve command)
- ✅ R4: Auto-Approval Logic - Task 4 (tryAutoApprove in poller)
- ✅ R5: Safety Guards - Task 4 (all guards in tryAutoApprove)

**Placeholder Scan:**
- ✅ No TBD or TODO markers
- ✅ All code blocks complete with actual implementation
- ✅ All test expectations specified
- ✅ All file paths exact

**Type Consistency:**
- ✅ Session.AutoApprove (bool) used consistently
- ✅ Poller.AutoApproveGlobal (bool) matches Config.AutoApproveEnabled
- ✅ UpdateAutoApprove(ctx, id string, enabled bool) signature consistent
- ✅ SetAutoApprove(ctx, id string, enabled bool) signature consistent

**Dependencies:**
- ✅ Task 1 → Task 5 (config flows to server/poller)
- ✅ Task 2 → Task 6, 7 (store UpdateAutoApprove used by API and CLI)
- ✅ Task 3 → Task 4 (SendKeys used by tryAutoApprove)
- ✅ Tasks 1-6 → Task 8 (integration testing after all components)

---

## Plan Complete

All 8 tasks defined with step-by-step TDD approach. Each task produces working, tested code. Plan ready for execution.
