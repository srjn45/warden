# Monitoring + Notifications Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Fire a macOS desktop notification when an agent transitions into a state that needs the user (`waiting_for_input`, `idle`/stuck, `orphaned`, `errored`).

**Architecture:** A `notify.Notifier` (osascript on darwin, log fallback otherwise). The poller gains an edge-triggered `OnTransition(sess, from, to)` hook; the daemon wires it to fire the notifier (async, best-effort) for actionable states only. No new monitoring loop — the existing poller tick is the monitor.

**Tech Stack:** Go 1.26 stdlib (`os/exec`, `runtime`, `strconv`), testify.

**Design spec:** `docs/superpowers/specs/2026-06-02-agentctl-monitoring-design.md`

**Worktree:** all work in `/Users/srajan.pathak/workspace/personal/agentctl-monitor` (branch `monitor`); a parallel session may use the main checkout — never touch it.

**Ordering (each commit builds green):** config flag (1), notify package (2), poller hook (3), daemon hook (4, imports notify), cli wiring + README (5, imports daemon hook). Each adds unused-but-compiling surface until Task 5 wires it.

---

### Task 1: config `NotifyEnabled`

**Files:** `internal/config/config.go`, `internal/config/config_test.go`.

- [ ] **Step 1: Write the failing tests.** Append to `internal/config/config_test.go`:
```go
func TestNotifyEnabledDefaultOn(t *testing.T) {
	t.Setenv("AGENTCTL_NOTIFY", "")
	require.True(t, Load().NotifyEnabled, "notifications on by default")
}

func TestNotifyDisabledFromEnv(t *testing.T) {
	for _, v := range []string{"0", "off", "false", "OFF"} {
		t.Setenv("AGENTCTL_NOTIFY", v)
		require.False(t, Load().NotifyEnabled, "AGENTCTL_NOTIFY=%q disables", v)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/config/ -run TestNotify` → FAIL (`NotifyEnabled` undefined).

- [ ] **Step 3: Implement.** In `internal/config/config.go`:
- Add `NotifyEnabled bool` to the `Config` struct.
- Add a helper (uses `strings`; add the import if missing):
```go
// notifyEnabled reads AGENTCTL_NOTIFY; on by default, off for 0/off/false.
func notifyEnabled() bool {
	switch strings.ToLower(os.Getenv("AGENTCTL_NOTIFY")) {
	case "0", "off", "false":
		return false
	}
	return true
}
```
- In `Load()`, add the field: `NotifyEnabled: notifyEnabled(),`.

- [ ] **Step 4: Verify** — `go test ./internal/config/ -run TestNotify` → PASS; `go build ./... && go vet ./internal/config/`; `gofmt -w internal/config/config.go internal/config/config_test.go` then `gofmt -l` → empty.

- [ ] **Step 5: Commit**
```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): NotifyEnabled from AGENTCTL_NOTIFY (default on)"
```

---

### Task 2: `internal/notify` package

**Files:** Create `internal/notify/notify.go`, `internal/notify/notify_test.go`.

- [ ] **Step 1: Write the failing tests.** Create `internal/notify/notify_test.go`:
```go
package notify

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOSANotifierBuildsScript(t *testing.T) {
	var gotName string
	var gotArgs []string
	n := osaNotifier{run: func(name string, args ...string) error { gotName = name; gotArgs = args; return nil }}
	n.Notify("agentctl — needs input", `agent-a1b2: review auth`)
	require.Equal(t, "osascript", gotName)
	require.Len(t, gotArgs, 2)
	require.Equal(t, "-e", gotArgs[0])
	require.Contains(t, gotArgs[1], "display notification")
	require.Contains(t, gotArgs[1], "with title")
	require.Contains(t, gotArgs[1], "needs input")
	require.Contains(t, gotArgs[1], "agent-a1b2")
}

func TestNewSelectsByPlatformAndEnabled(t *testing.T) {
	require.IsType(t, logNotifier{}, New(false), "disabled → log notifier")
	if runtime.GOOS == "darwin" {
		require.IsType(t, osaNotifier{}, New(true), "darwin+enabled → osa notifier")
	} else {
		require.IsType(t, logNotifier{}, New(true), "non-darwin → log notifier")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/notify/` → FAIL (package/types undefined).

- [ ] **Step 3: Implement.** Create `internal/notify/notify.go`:
```go
// Package notify delivers short "an agent needs you" alerts to the user.
package notify

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strconv"
)

// Notifier delivers a short attention message to the user.
type Notifier interface {
	Notify(title, body string)
}

// New returns the platform notifier: a macOS desktop notifier when enabled on
// darwin, else a log-only notifier (non-darwin, or notifications disabled).
func New(enabled bool) Notifier {
	if enabled && runtime.GOOS == "darwin" {
		return osaNotifier{run: execRun}
	}
	return logNotifier{}
}

func execRun(name string, args ...string) error { return exec.Command(name, args...).Run() }

// osaNotifier shows a macOS notification via osascript. Best-effort: a failure
// is logged, never propagated, so it can't disrupt the poll loop.
type osaNotifier struct {
	run func(name string, args ...string) error
}

func (o osaNotifier) Notify(title, body string) {
	// body/title become AppleScript string literals; strconv.Quote escapes the
	// quotes and newlines (subjects are short plain text, so this is sufficient).
	script := fmt.Sprintf("display notification %s with title %s", strconv.Quote(body), strconv.Quote(title))
	if err := o.run("osascript", "-e", script); err != nil {
		log.Printf("notify: osascript: %v", err)
	}
}

// logNotifier writes the notification to the log — the fallback when desktop
// notifications aren't available (non-darwin) or are disabled.
type logNotifier struct{}

func (logNotifier) Notify(title, body string) { log.Printf("notify: %s — %s", title, body) }
```

- [ ] **Step 4: Verify** — `go test ./internal/notify/` → PASS; `go build ./...`; `gofmt -l internal/notify/` → empty.

- [ ] **Step 5: Commit**
```bash
git add internal/notify/notify.go internal/notify/notify_test.go
git commit -m "feat(notify): Notifier — macOS osascript with log fallback"
```

---

### Task 3: poller `OnTransition` hook

**Files:** `internal/poller/poller.go`, `internal/poller/poller_test.go`.

- [ ] **Step 1: Write the failing tests.** Append to `internal/poller/poller_test.go`:
```go
func TestTickFiresOnTransition(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking}},
		alive:    map[string]bool{"A-1": false}, // dead → orphaned
		panes:    map[string]string{},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	var gotFrom, gotTo store.Status
	var gotID string
	n := 0
	p.OnTransition = func(s *store.Session, from, to store.Status) {
		gotID, gotFrom, gotTo, n = s.ID, from, to, n+1
	}
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, 1, n, "fired once on the transition")
	require.Equal(t, "A-1", gotID)
	require.Equal(t, store.StatusWorking, gotFrom)
	require.Equal(t, store.StatusOrphaned, gotTo)
}

func TestTickNoTransitionForTerminalStatus(t *testing.T) {
	d := &stubDeps{
		sessions: []*store.Session{{ID: "A-1", TmuxSession: "A-1", Status: store.StatusDone}},
		alive:    map[string]bool{"A-1": false},
		panes:    map[string]string{},
		updates:  map[string]store.Status{},
	}
	p := New(d, 5*time.Minute)
	fired := false
	p.OnTransition = func(*store.Session, store.Status, store.Status) { fired = true }
	require.NoError(t, p.tick(context.Background()))
	require.False(t, fired, "terminal status is skipped → no transition")
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/poller/ -run TestTick.*Transition` → FAIL (`OnTransition` undefined).

- [ ] **Step 3: Implement.** In `internal/poller/poller.go`:
- Add a field to the `Poller` struct, right after the `OnChange func()` field:
```go
	// OnTransition, if set, is called once per successful status swap with the
	// session and its old/new status (edge-triggered — once per transition, not
	// per tick). The daemon wires this to fire user notifications.
	OnTransition func(sess *store.Session, from, to store.Status)
```
- In `tick`, in the swap block, extend the `else if ok` branch (currently `} else if ok { changed = true }`):
```go
				} else if ok {
					changed = true
					if p.OnTransition != nil {
						p.OnTransition(s, s.Status, next)
					}
				}
```
(`s.Status` is the pre-swap snapshot value; `next` is the new status.)

- [ ] **Step 4: Verify** — `go test ./internal/poller/` → PASS (new + existing); `go build ./... && go vet ./internal/poller/`; `gofmt -l internal/poller/` → empty.

- [ ] **Step 5: Commit**
```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "feat(poller): OnTransition hook (edge-triggered per status swap)"
```

---

### Task 4: daemon notify hook (`NotifyOnTransition` + `notifyMessage`)

**Files:** Create `internal/daemon/notify_hook.go`, `internal/daemon/notify_hook_test.go`.

- [ ] **Step 1: Write the failing tests.** Create `internal/daemon/notify_hook_test.go`:
```go
package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func TestNotifyMessageActionable(t *testing.T) {
	s := &store.Session{ID: "agent-x", Subject: "review auth"}
	cases := []struct {
		to         store.Status
		wantTitle  string
		wantInBody string
	}{
		{store.StatusWaitingForInput, "agentctl — needs input", "review auth"},
		{store.StatusIdle, "agentctl — stuck", "went idle"},
		{store.StatusOrphaned, "agentctl — agent lost", "tmux gone"},
		{store.StatusErrored, "agentctl — errored", "agent-x"},
	}
	for _, tc := range cases {
		title, body, ok := notifyMessage(s, tc.to)
		require.True(t, ok, tc.to)
		require.Equal(t, tc.wantTitle, title)
		require.Contains(t, body, tc.wantInBody)
	}
}

func TestNotifyMessageNonActionable(t *testing.T) {
	s := &store.Session{ID: "agent-x"}
	for _, st := range []store.Status{store.StatusWorking, store.StatusSpawning, store.StatusDone} {
		_, _, ok := notifyMessage(s, st)
		require.False(t, ok, st)
	}
}

func TestNotifyMessageSubjectFallsBackToID(t *testing.T) {
	_, body, ok := notifyMessage(&store.Session{ID: "agent-x"}, store.StatusWaitingForInput)
	require.True(t, ok)
	require.Contains(t, body, "agent-x")
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeNotifier) Notify(title, body string) { f.mu.Lock(); f.calls++; f.mu.Unlock() }
func (f *fakeNotifier) count() int                { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestNotifyOnTransitionFiresForActionableOnly(t *testing.T) {
	fn := &fakeNotifier{}
	hook := NotifyOnTransition(fn)
	hook(&store.Session{ID: "a"}, store.StatusWorking, store.StatusWaitingForInput) // actionable → fires
	hook(&store.Session{ID: "a"}, store.StatusWaitingForInput, store.StatusWorking) // not actionable
	require.Eventually(t, func() bool { return fn.count() == 1 }, time.Second, 5*time.Millisecond)
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/daemon/ -run Notify` → FAIL (`notifyMessage`/`NotifyOnTransition` undefined).

- [ ] **Step 3: Implement.** Create `internal/daemon/notify_hook.go`:
```go
package daemon

import (
	"github.com/srajanpathak/agentctl/internal/notify"
	"github.com/srajanpathak/agentctl/internal/store"
)

// notifyMessage builds the notification for a transition into status `to`. It
// returns actionable=false for states that don't need the user's attention.
func notifyMessage(sess *store.Session, to store.Status) (title, body string, actionable bool) {
	subj := sess.Subject
	if subj == "" {
		subj = sess.ID
	}
	switch to {
	case store.StatusWaitingForInput:
		return "agentctl — needs input", sess.ID + ": " + subj, true
	case store.StatusIdle:
		return "agentctl — stuck", sess.ID + " went idle: " + subj, true
	case store.StatusOrphaned:
		return "agentctl — agent lost", sess.ID + " tmux gone: " + subj, true
	case store.StatusErrored:
		return "agentctl — errored", sess.ID + ": " + subj, true
	}
	return "", "", false
}

// NotifyOnTransition returns a poller OnTransition hook that fires the notifier
// (best-effort, async) when an agent enters a state that needs the user.
func NotifyOnTransition(n notify.Notifier) func(*store.Session, store.Status, store.Status) {
	return func(sess *store.Session, _ store.Status, to store.Status) {
		title, body, ok := notifyMessage(sess, to)
		if !ok {
			return
		}
		go n.Notify(title, body)
	}
}
```

- [ ] **Step 4: Verify** — `go test ./internal/daemon/ -run Notify` → PASS, then `go test ./internal/daemon/` → PASS; `go build ./... && go vet ./internal/daemon/`; `gofmt -l internal/daemon/notify_hook.go internal/daemon/notify_hook_test.go` → empty.

- [ ] **Step 5: Commit**
```bash
git add internal/daemon/notify_hook.go internal/daemon/notify_hook_test.go
git commit -m "feat(daemon): NotifyOnTransition + notifyMessage (actionable-state alerts)"
```

---

### Task 5: wire it in the daemon CLI + README

**Files:** `internal/cli/daemon.go`, `README.md`.

- [ ] **Step 1: Wire the hook.** In `internal/cli/daemon.go`, add the `notify` import:
```go
	"github.com/srajanpathak/agentctl/internal/notify"
```
and, immediately after `pl := poller.New(pd, 5*time.Minute)`, add:
```go
			pl.OnTransition = daemon.NotifyOnTransition(notify.New(cfg.NotifyEnabled))
```

- [ ] **Step 2: Document in README.** Add `AGENTCTL_NOTIFY` to the env-var table (mirror the existing rows):
```
| `AGENTCTL_NOTIFY` | `on` | macOS desktop notifications when an agent needs attention (`off`/`0`/`false` to disable) |
```
And add a short note near the daemon/launchd section:
> **Notifications:** the daemon posts a macOS notification when an agent enters `waiting_for_input`, `idle` (stuck), `orphaned`, or `errored`. These appear only when the daemon runs in your GUI login session (a terminal, or a launchd **user agent**); a headless/system daemon logs them instead. Disable with `AGENTCTL_NOTIFY=off`.

- [ ] **Step 3: Verify** — `go build ./... && go vet ./internal/cli/`; `gofmt -l internal/cli/daemon.go` → empty. (No unit test for the cli wiring, consistent with the rest of `cli`; the hook + message are covered by Tasks 3–4.)

- [ ] **Step 4: Commit**
```bash
git add internal/cli/daemon.go README.md
git commit -m "feat: wire agent notifications into the daemon; document AGENTCTL_NOTIFY"
```

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test -race ./...` — all green.
- [ ] Live smoke (darwin, in your GUI session): `make build`, run the daemon, spawn a prompt agent, and when it transitions to `waiting_for_input` (or kill its tmux to force `orphaned`) confirm a macOS notification appears naming the agent. Then `AGENTCTL_NOTIFY=off ./bin/agentctl daemon` and confirm it logs instead of notifying. Clean up.

Then proceed to **superpowers:finishing-a-development-branch**.
