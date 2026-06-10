# Auto-restart of errored agents — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When an opted-in agent reaches `errored`, the daemon automatically resumes its Claude conversation (kill any surviving tmux shell, then `Restore`), bounded by a per-agent retry cap that resets after sustained health.

**Architecture:** A new `Restarter` (in `internal/daemon/autorestart.go`) is added as a third callback on the daemon's existing `OnTransition` hook. On a qualifying `errored` edge (opt-in flag set, not a pipeline job) it consults a pure `decideRestart` function over `(RestartCount, LastRestartAt, now)` and either gives up (stay `errored`, log an event) or restarts (bump the counter, `Terminate` + `Restore`). The opt-in flag is plumbed through the stack exactly like the existing `Supervised` flag.

**Tech Stack:** Go (store/lifecycle/daemon/client/cli). Spec: `docs/superpowers/specs/2026-06-10-warden-auto-restart-design.md`.

---

## File Structure

- `internal/store/types.go` — `Session` gains `AutoRestart bool`, `RestartCount int`, `LastRestartAt time.Time`.
- `internal/store/store.go` + `internal/store/file.go` — `SetRestart(ctx, id, count, at)` store method.
- `internal/lifecycle/lifecycle.go` — `SpawnRequest.AutoRestart`; persisted on the session at the spawn-builder site.
- `internal/daemon/api.go` — `SpawnRequest.AutoRestart` (wire DTO).
- `internal/daemon/lifecycle_adapter.go` — map `AutoRestart` into `lifecycle.SpawnRequest`.
- `internal/client/client.go` — `SpawnParams.AutoRestart` + request body key.
- `internal/cli/lifecycle.go` — `--auto-restart` flag on `start`, both spawn paths.
- `internal/daemon/autorestart.go` (new) — `decideRestart` pure fn + `Restarter.OnTransition`.
- `internal/cli/daemon.go` — construct `Restarter`, add to the `OnTransition` closure.

The `--supervised` flag is the exact plumbing template; mirror it at every layer.

---

### Task 1: Store model fields

**Files:**
- Modify: `internal/store/types.go` (the `Session` struct — after the `Supervised` field, line ~97)
- Test: `internal/store/types_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/types_test.go`:

```go
func TestSessionAutoRestartFieldsJSON(t *testing.T) {
	at := time.Date(2026, 6, 10, 1, 2, 3, 0, time.UTC)
	s := Session{ID: "a", AutoRestart: true, RestartCount: 2, LastRestartAt: at}
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"auto_restart":true`)
	require.Contains(t, string(b), `"restart_count":2`)

	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.True(t, back.AutoRestart)
	require.Equal(t, 2, back.RestartCount)
	require.Equal(t, at, back.LastRestartAt.UTC())

	// zero values omit (omitempty) for the count/timestamp; auto_restart false also omits.
	b2, err := json.Marshal(Session{ID: "b"})
	require.NoError(t, err)
	require.NotContains(t, string(b2), "restart_count")
	require.NotContains(t, string(b2), "last_restart_at")
	require.NotContains(t, string(b2), "auto_restart")
}
```

Ensure the test file imports `time`, `encoding/json`, `github.com/stretchr/testify/require` (check; most already present).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSessionAutoRestartFieldsJSON`
Expected: FAIL (compile error: fields undefined).

- [ ] **Step 3: Add the fields**

In `internal/store/types.go`, in the `Session` struct immediately after the `Supervised bool ...` line, add:

```go
	AutoRestart     bool      `json:"auto_restart,omitempty"`    // opt-in: auto-resume this agent when it errors (capped)
	RestartCount    int       `json:"restart_count,omitempty"`   // consecutive auto-restart attempts since last sustained-healthy run
	LastRestartAt   time.Time `json:"last_restart_at,omitempty"` // when the most recent auto-restart fired
```

Match the struct's gofmt alignment.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestSessionAutoRestartFieldsJSON` → PASS. Then `go test ./internal/store/` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/types.go internal/store/types_test.go
git commit -m "feat(store): add Session.AutoRestart/RestartCount/LastRestartAt"
```

---

### Task 2: `store.SetRestart`

Records a restart attempt's counter + timestamp in one atomic write (mirrors the existing `mutate`-based setters like `UpdatePane`).

**Files:**
- Modify: `internal/store/store.go` (interface, after `UpdatePane`), `internal/store/file.go` (impl, near the other `mutate` setters ~line 214-225)
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/file_test.go`:

```go
func TestSetRestart(t *testing.T) {
	fs := newFileStore(t)
	ctx := context.Background()
	require.NoError(t, fs.Insert(ctx, &Session{ID: "x", Status: StatusErrored}))

	at := time.Date(2026, 6, 10, 1, 2, 3, 0, time.UTC)
	require.NoError(t, fs.SetRestart(ctx, "x", 1, at))

	got, err := fs.Get(ctx, "x")
	require.NoError(t, err)
	require.Equal(t, 1, got.RestartCount)
	require.Equal(t, at, got.LastRestartAt.UTC())
	require.Equal(t, StatusErrored, got.Status) // SetRestart does not touch status
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSetRestart` → FAIL (`fs.SetRestart` undefined).

- [ ] **Step 3a: Add to the `Store` interface**

In `internal/store/store.go`, after the `UpdatePane` method line, add:

```go
	// SetRestart records an auto-restart attempt's counter and timestamp.
	SetRestart(ctx context.Context, id string, count int, at time.Time) error
```

(If `store.go` does not already import `time`, add it.)

- [ ] **Step 3b: Implement in `file.go`**

Next to the other `mutate`-based setters (e.g. after `UpdatePane`), add:

```go
func (fs *FileStore) SetRestart(ctx context.Context, id string, count int, at time.Time) error {
	return fs.mutate(id, func(s *Session) { s.RestartCount = count; s.LastRestartAt = at })
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/store/ -run TestSetRestart` → PASS. Then `go test ./internal/store/` → PASS.
The new interface method must be satisfied by every `store.Store` implementation — if the daemon's `fakeStore` (in `internal/daemon/api_test.go`) fails to compile, add a minimal stub there:
```go
func (f *fakeStore) SetRestart(_ context.Context, id string, count int, at time.Time) error {
	if s := f.sessions[id]; s != nil { s.RestartCount = count; s.LastRestartAt = at }
	return nil
}
```
(Adapt to `fakeStore`'s actual field layout — read it first; only add if compilation requires it. Report if you did.)

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/file.go internal/store/file_test.go internal/daemon/api_test.go
git commit -m "feat(store): SetRestart records auto-restart count + timestamp"
```

---

### Task 3: Plumb `AutoRestart` through spawn (lifecycle → daemon → client → CLI)

Pure flag-threading, mirroring `Supervised`. No behavior yet.

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (`SpawnRequest` ~line 211, the session builder ~line 547)
- Modify: `internal/daemon/api.go` (`SpawnRequest` ~line 41)
- Modify: `internal/daemon/lifecycle_adapter.go` (`Spawn` mapping ~line 35)
- Modify: `internal/client/client.go` (`SpawnParams` ~line 162, body map ~line 171)
- Modify: `internal/cli/lifecycle.go` (flag + both spawn paths)
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestSpawnPersistsAutoRestart(t *testing.T) {
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	l.ExitsDir = t.TempDir()
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: "do x", Cwd: "/work/p", AutoRestart: true})
	require.NoError(t, err)
	require.True(t, s.AutoRestart, "AutoRestart must be persisted on the session")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestSpawnPersistsAutoRestart` → FAIL (`SpawnRequest` has no `AutoRestart`).

- [ ] **Step 3: Thread the field at every layer**

(a) `internal/lifecycle/lifecycle.go`, in `SpawnRequest` (after the `Supervised bool` field, ~line 212):
```go
	AutoRestart bool // opt-in: auto-resume this agent when it errors (capped)
```
In the session-builder where `Supervised: req.Supervised` is set (~line 547), add on the next line:
```go
		AutoRestart: req.AutoRestart,
```

(b) `internal/daemon/api.go`, in `SpawnRequest` (after the `Supervised bool ... json:"supervised"` field, line ~41):
```go
	AutoRestart bool `json:"auto_restart"` // opt-in: auto-resume on error (capped)
```

(c) `internal/daemon/lifecycle_adapter.go`, in `Spawn`'s `lifecycle.SpawnRequest{...}` literal (after `Supervised: req.Supervised,` ~line 35):
```go
		AutoRestart: req.AutoRestart,
```

(d) `internal/client/client.go`, in `SpawnParams` (after `Supervised bool` ~line 162):
```go
	AutoRestart bool
```
and in the `Spawn` body map (alongside `"supervised": p.Supervised,` ~line 171):
```go
		"auto_restart": p.AutoRestart,
```

(e) `internal/cli/lifecycle.go`:
- Register the flag next to `--supervised` (~line 106):
  ```go
  cmd.Flags().Bool("auto-restart", false, "auto-resume this agent if it crashes (errored), capped at a few attempts")
  ```
- In the **free-form** spawn path (~line 43-45), read it and pass it:
  ```go
  autoRestart, _ := cmd.Flags().GetBool("auto-restart")
  s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{Prompt: prompt, Cwd: dir, Supervised: supervised, AutoRestart: autoRestart, Force: force})
  ```
- In the **typed** spawn path (~line 74-85), read it and add `AutoRestart: autoRestart` to the `client.SpawnParams{...}`:
  ```go
  autoRestart, _ := cmd.Flags().GetBool("auto-restart")
  s, err := clientFor(cmd).Spawn(cmd.Context(), client.SpawnParams{
      Type: typ, Ticket: ticket, Repo: repo, Branch: branch, PR: pr, Worktree: worktree, Supervised: supervised, AutoRestart: autoRestart, Force: force,
  })
  ```
  (Confirm the `start` command registers ONE flag set used by both paths, or that the flag is registered on whatever cmd each path belongs to — read the file; register `--auto-restart` wherever `--supervised` is registered.)

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/lifecycle/ -run TestSpawnPersistsAutoRestart` → PASS. Then `go build ./...` → clean, and `go test ./internal/lifecycle/ ./internal/daemon/ ./internal/client/ ./internal/cli/` → PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/lifecycle/lifecycle.go internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/client/client.go internal/cli/lifecycle.go internal/lifecycle/lifecycle_test.go
git add internal/lifecycle/lifecycle.go internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/client/client.go internal/cli/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat: plumb --auto-restart spawn flag through the stack"
```

---

### Task 4: `decideRestart` pure decision function

**Files:**
- Create: `internal/daemon/autorestart.go`
- Test: `internal/daemon/autorestart_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/autorestart_test.go`:

```go
package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDecideRestart(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	const max = 3
	const reset = 5 * time.Minute

	// First-ever crash: no prior restart -> restart, count 1.
	act, next := decideRestart(0, time.Time{}, now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Recent restart, below cap -> restart, count+1.
	act, next = decideRestart(1, now.Add(-30*time.Second), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 2, next)

	// At cap, recent -> give up.
	act, _ = decideRestart(3, now.Add(-30*time.Second), now, max, reset)
	require.Equal(t, actionGiveUp, act)

	// At cap but sustained-healthy (>= reset since last) -> reset -> restart, count 1.
	act, next = decideRestart(3, now.Add(-6*time.Minute), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Boundary: exactly reset elapsed -> resets.
	act, next = decideRestart(3, now.Add(-5*time.Minute), now, max, reset)
	require.Equal(t, actionRestart, act)
	require.Equal(t, 1, next)

	// Boundary: just under reset, at cap -> give up.
	act, _ = decideRestart(3, now.Add(-(5*time.Minute - time.Second)), now, max, reset)
	require.Equal(t, actionGiveUp, act)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestDecideRestart` → FAIL (undefined).

- [ ] **Step 3: Implement the pure function**

Create `internal/daemon/autorestart.go` with (just the types + pure fn for now):

```go
package daemon

import "time"

type restartAction int

const (
	actionGiveUp restartAction = iota
	actionRestart
)

// decideRestart decides whether an errored, auto-restart-enabled agent should be
// restarted, and the counter to persist if so. A restart that happened >= reset
// ago (or never) means the prior run was sustained-healthy, so the counter resets
// to 0 — "a successful run resets the counter", defined as sustained health so a
// resume->instant-crash loop cannot evade the cap by briefly reaching working.
func decideRestart(count int, lastRestartAt, now time.Time, max int, reset time.Duration) (restartAction, int) {
	effective := count
	if lastRestartAt.IsZero() || now.Sub(lastRestartAt) >= reset {
		effective = 0
	}
	if effective >= max {
		return actionGiveUp, effective
	}
	return actionRestart, effective + 1
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestDecideRestart` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/autorestart.go internal/daemon/autorestart_test.go
git commit -m "feat(daemon): decideRestart — capped retry w/ sustained-health reset"
```

---

### Task 5: `Restarter.OnTransition`

The component that acts on the `errored` edge. Depends on the daemon's existing `Lifecycle` interface (which already exposes `Terminate(ctx, tmuxSession)` and `Restore(ctx, sess)`) and `store.Store`.

**Files:**
- Modify: `internal/daemon/autorestart.go`
- Test: `internal/daemon/autorestart_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/daemon/autorestart_test.go`. These use small fakes local to the test (do NOT reuse the package's larger fakes unless they already implement exactly `Terminate`+`Restore` and a settable store — a focused fake is clearer here):

```go
// fakeRestartLife records Terminate/Restore calls; restoreErr forces a failure.
type fakeRestartLife struct {
	Lifecycle // embed so unused interface methods are nil (never called here)
	terminated []string
	restored   []string
	restoreErr error
}

func (f *fakeRestartLife) Terminate(_ context.Context, tmux string) error {
	f.terminated = append(f.terminated, tmux)
	return nil
}
func (f *fakeRestartLife) Restore(_ context.Context, sess *store.Session) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.restored = append(f.restored, sess.ID)
	return nil
}

// restartStore records SetRestart/AppendEvent/UpdateStatus; minimal store fake.
type restartStore struct {
	store.Store
	count   int
	at      time.Time
	events  []string
	status  store.Status
}

func (s *restartStore) SetRestart(_ context.Context, _ string, count int, at time.Time) error {
	s.count, s.at = count, at
	return nil
}
func (s *restartStore) AppendEvent(_ context.Context, _ string, ev store.Event) error {
	s.events = append(s.events, ev.Detail)
	return nil
}
func (s *restartStore) UpdateStatus(_ context.Context, _ string, st store.Status) error {
	s.status = st
	return nil
}

func newRestarter(life Lifecycle, st store.Store) *Restarter {
	return &Restarter{life: life, store: st, max: 3, reset: 5 * time.Minute}
}

func TestRestarterIgnoresNonQualifying(t *testing.T) {
	life, st := &fakeRestartLife{}, &restartStore{}
	r := newRestarter(life, st)
	now := time.Now()
	// not errored
	r.onTransitionAt(&store.Session{ID: "a", AutoRestart: true}, store.StatusWorking, store.StatusIdle, now)
	// errored but flag off
	r.onTransitionAt(&store.Session{ID: "b"}, store.StatusWorking, store.StatusErrored, now)
	// errored, flag on, but pipeline job
	r.onTransitionAt(&store.Session{ID: "c", AutoRestart: true, PipelineID: "p1"}, store.StatusWorking, store.StatusErrored, now)
	require.Empty(t, life.restored)
	require.Empty(t, life.terminated)
}

func TestRestarterRestartsQualifying(t *testing.T) {
	life, st := &fakeRestartLife{}, &restartStore{}
	r := newRestarter(life, st)
	now := time.Now()
	sess := &store.Session{ID: "x", TmuxSession: "x", AutoRestart: true, Status: store.StatusErrored, RestartCount: 0}
	r.onTransitionAt(sess, store.StatusWorking, store.StatusErrored, now)

	require.Equal(t, []string{"x"}, life.terminated) // killed surviving shell first
	require.Equal(t, []string{"x"}, life.restored)   // then resumed
	require.Equal(t, 1, st.count)                     // counter bumped
	require.Equal(t, now, st.at)
	require.Equal(t, store.StatusSpawning, st.status) // status reset for the poller to re-pick-up
	require.Len(t, st.events, 1)
	require.Contains(t, st.events[0], "attempt 1/3")
}

func TestRestarterGivesUpAtCap(t *testing.T) {
	life, st := &fakeRestartLife{}, &restartStore{}
	r := newRestarter(life, st)
	now := time.Now()
	sess := &store.Session{ID: "x", TmuxSession: "x", AutoRestart: true, Status: store.StatusErrored,
		RestartCount: 3, LastRestartAt: now.Add(-30 * time.Second)}
	r.onTransitionAt(sess, store.StatusWorking, store.StatusErrored, now)

	require.Empty(t, life.restored)        // did NOT restart
	require.Equal(t, store.Status(""), st.status) // status untouched (stays errored)
	require.Len(t, st.events, 1)
	require.Contains(t, st.events[0], "giving up after 3")
}

func TestRestarterRestoreFailureLeavesErrored(t *testing.T) {
	life := &fakeRestartLife{restoreErr: errors.New("workdir gone")}
	st := &restartStore{}
	r := newRestarter(life, st)
	now := time.Now()
	sess := &store.Session{ID: "x", TmuxSession: "x", AutoRestart: true, Status: store.StatusErrored}
	r.onTransitionAt(sess, store.StatusWorking, store.StatusErrored, now)

	require.Equal(t, 1, st.count)                 // attempt was counted
	require.Equal(t, store.Status(""), st.status) // NOT set to spawning (restore failed)
	require.Len(t, st.events, 2)                  // "attempt 1/3" + "restore failed"
	require.Contains(t, st.events[1], "restore failed")
}
```

Ensure the test imports `context`, `errors`, `time`, `store`, `require`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestRestarter` → FAIL (`Restarter` / `onTransitionAt` undefined).

- [ ] **Step 3: Implement `Restarter`**

Append to `internal/daemon/autorestart.go`:

```go
import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/store"
)
```
(Merge with the existing `import "time"` at the top — make it a single grouped import block.)

```go
// Restarter auto-resumes an opted-in agent that reaches errored, bounded by a
// per-agent retry cap that resets after sustained health.
type Restarter struct {
	life  Lifecycle
	store store.Store
	max   int
	reset time.Duration
}

// NewRestarter builds a Restarter. The cap and reset window are tunable via
// WARDEN_AUTO_RESTART_MAX (default 3) and WARDEN_AUTO_RESTART_RESET (a Go
// duration, default 5m); the feature itself is opt-in per agent (Session.AutoRestart).
func NewRestarter(life Lifecycle, st store.Store) *Restarter {
	return &Restarter{
		life:  life,
		store: st,
		max:   envInt("WARDEN_AUTO_RESTART_MAX", 3),
		reset: envDuration("WARDEN_AUTO_RESTART_RESET", 5*time.Minute),
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// OnTransition is wired as a callback on the poller's status-transition hook.
func (r *Restarter) OnTransition(sess *store.Session, _ store.Status, to store.Status) {
	r.onTransitionAt(sess, store.Status(""), to, time.Now().UTC())
}

// onTransitionAt is the testable core (now injected). It restarts a qualifying
// errored agent or records a give-up.
func (r *Restarter) onTransitionAt(sess *store.Session, _ store.Status, to store.Status, now time.Time) {
	if to != store.StatusErrored || !sess.AutoRestart || sess.PipelineID != "" {
		return
	}
	ctx := context.Background()
	act, next := decideRestart(sess.RestartCount, sess.LastRestartAt, now, r.max, r.reset)
	if act == actionGiveUp {
		r.appendEvent(ctx, sess.ID, fmt.Sprintf("auto-restart: giving up after %d attempts", r.max))
		return
	}
	if err := r.store.SetRestart(ctx, sess.ID, next, now); err != nil {
		log.Printf("auto-restart: set restart %s: %v", sess.ID, err)
		return
	}
	r.appendEvent(ctx, sess.ID, fmt.Sprintf("auto-restart: attempt %d/%d", next, r.max))
	// Kill any surviving shell (errored = claude died, shell alive) so Restore's
	// has-session guard passes; best-effort (Terminate ignores an already-dead session).
	_ = r.life.Terminate(ctx, sess.TmuxSession)
	if err := r.life.Restore(ctx, sess); err != nil {
		r.appendEvent(ctx, sess.ID, fmt.Sprintf("auto-restart: restore failed: %v", err))
		return
	}
	if err := r.store.UpdateStatus(ctx, sess.ID, store.StatusSpawning); err != nil {
		log.Printf("auto-restart: status %s: %v", sess.ID, err)
	}
}

func (r *Restarter) appendEvent(ctx context.Context, id, detail string) {
	if err := r.store.AppendEvent(ctx, id, store.Event{Type: "auto-restart", Detail: detail}); err != nil {
		log.Printf("auto-restart: event %s: %v", id, err)
	}
}
```

Note: `OnTransition` passes `store.Status("")` as the unused `from` — `onTransitionAt` ignores it (named `_`). The real `from` is irrelevant; the decision keys only on `to == errored`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestRestarter|TestDecideRestart'` → PASS (all). Then `go test ./internal/daemon/` → PASS (full package — the embedded `Lifecycle`/`store.Store` in the test fakes means only the methods actually called need real bodies; if the compiler complains a fake doesn't satisfy an interface because a called method is missing, add it).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/daemon/autorestart.go internal/daemon/autorestart_test.go
git add internal/daemon/autorestart.go internal/daemon/autorestart_test.go
git commit -m "feat(daemon): Restarter — kill-then-Restore an errored opted-in agent"
```

---

### Task 6: Wire the Restarter into the daemon

**Files:**
- Modify: `internal/cli/daemon.go` (the `OnTransition` closure ~line 89-93)

- [ ] **Step 1: Add the construction + callback**

In `internal/cli/daemon.go`, just before the `pl.OnTransition = func(...)` assignment (~line 90), construct the restarter (it needs `life` and `st`, both already in scope):

```go
				restarter := daemon.NewRestarter(life, st)
```

Then extend the closure to call it last:

```go
				notifyHook := daemon.NotifyOnTransition(notify.New(cfg.NotifyEnabled))
				pl.OnTransition = func(sess *store.Session, from, to store.Status) {
					notifyHook(sess, from, to)
					exec.OnTransition(sess, from, to)
					restarter.OnTransition(sess, from, to)
				}
```

(The restarter acts last so its kill-then-Restore mutates tmux/status only after notify and pipeline-reconcile have observed the `errored` edge.)

- [ ] **Step 2: Build + vet**

Run: `go build ./...` → clean. `go vet ./internal/cli/ ./internal/daemon/` → clean.

- [ ] **Step 3: Confirm `life` satisfies the Restarter's needs**

`daemon.NewRestarter(life, st)` takes `life` as `daemon.Lifecycle`. Confirm `life` (the `*lifecycleAdapter`) is that interface type at this call site (it is — `daemon.NewServer` already takes `life Lifecycle`). If the variable's static type differs, pass it as-is; Go will accept the concrete type where the interface is expected.

- [ ] **Step 4: Run the daemon + cli package tests**

Run: `go test ./internal/cli/ ./internal/daemon/` → PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/cli/daemon.go
git add internal/cli/daemon.go
git commit -m "feat(daemon): wire Restarter into the OnTransition hook"
```

---

### Task 7: Full verification

- [ ] **Step 1: Whole Go suite**

Run: `go test ./...`
Expected: PASS (if a heavy package times out on a contended machine, re-run it alone with `-timeout 300s` to confirm it's contention, not a regression).

- [ ] **Step 2: Vet + build**

Run: `go vet ./...` (clean) and `go build ./...` (clean).

- [ ] **Step 3: Hand off for live smoke (left for the user)**

Live smoke (requires `make install` + daemon restart): `warden start --auto-restart --dir /tmp/x`; `kill -9` its claude process → expect an `auto-restart: attempt 1/3` event and the agent back to `working` on the same conversation; repeat the `kill -9` to confirm it stops at 3 with an `auto-restart: giving up after 3 attempts` event and stays `errored`. (Note the `--supervised`/`--auto-restart` daemon-side change means the running daemon must be rebuilt from this branch's merge — see crash-detection's note about the occupied main checkout being stale.)

---

## Notes for the implementer

- **Why no global env enable:** the feature is gated by the per-agent `AutoRestart` flag; `WARDEN_AUTO_RESTART_MAX`/`_RESET` only tune the cap/reset window. Read them in `NewRestarter` (Task 5) — do not add to `config.Config`.
- **`errored` is terminal** (the poller skips it), so after `Restore` the restarter sets status back to `spawning` (exactly as `handleRestore` does) — that re-arms the poller, and the resumed Claude's `SessionStart` hook then drives it to `working`.
- **Counter is bumped before `Restore`** so a failed restore still counts the attempt (no hot-loop on a permanently-unrestorable agent).
- **Pipeline jobs are excluded** via the `sess.PipelineID != ""` guard — the pipeline executor owns job retry/failure.
- **Backward compatible:** old/un-opted agents have `AutoRestart=false` → the guard returns immediately, behaviour unchanged.
