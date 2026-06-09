# Crash Detection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Classify an agent that dies without a clean `SessionEnd` hook into a precise 3-way outcome — `done` (exit 0), `errored` (non-zero/crash, with exit code recorded), or `orphaned` (window gone, no exit info) — via an exit-code sentinel file written by the agent's shell.

**Architecture:** Each `claude` invocation is typed into the agent's tmux shell with a trailing `; printf '%s' "$?" > <exitsdir>/<id>` suffix. The poller, each tick, reads that file for non-terminal sessions and finalizes the status (CAS so a `SessionEnd` hook that already set `done` wins). The exit code is stored on the session; an `errored` transition appends an event. `orphaned` is now reached only when no exit-file exists, because the poller short-circuits when one does.

**Tech Stack:** Go (daemon/poller/lifecycle/store), TypeScript/React (web), Bubbletea/lipgloss (TUI). Spec: `docs/superpowers/specs/2026-06-09-warden-crash-detection-design.md`.

---

## File Structure

- `internal/store/types.go` — add `ExitCode *int` to `Session`.
- `internal/store/store.go` — add `FinalizeExit` to the `Store` interface.
- `internal/store/file.go` — implement `FinalizeExit` (CAS + set ExitCode + conditional event) and `signalName` helper.
- `internal/lifecycle/lifecycle.go` — add `ExitsDir` field; `exitSuffix`, `ReadExit`, `ClearExit` methods; append the suffix at the 4 launch sites.
- `internal/poller/poller.go` — extend `Deps`; read the exit-file first in `tick`.
- `internal/daemon/poller_deps.go` — implement the 3 new `Deps` methods.
- `internal/daemon/harden.go` — add `"exits"` to `hardenedSubdirs`.
- `internal/cli/daemon.go` — set `lc.ExitsDir`.
- `web/src/lib/types.ts` + `web/src/lib/status.ts` — `exit_code` field + show it on the error badge.
- `internal/tui/styles.go` — show exit code on the errored badge.

---

### Task 1: `Session.ExitCode` field

**Files:**
- Modify: `internal/store/types.go:77-99`
- Test: `internal/store/types_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/types_test.go`:

```go
func TestSessionExitCodeJSONRoundTrip(t *testing.T) {
	code := 137
	s := Session{ID: "a", Status: StatusErrored, ExitCode: &code}
	b, err := json.Marshal(s)
	require.NoError(t, err)
	require.Contains(t, string(b), `"exit_code":137`)

	// nil ExitCode is omitted entirely (omitempty).
	b2, err := json.Marshal(Session{ID: "b", Status: StatusWorking})
	require.NoError(t, err)
	require.NotContains(t, string(b2), "exit_code")

	var back Session
	require.NoError(t, json.Unmarshal(b, &back))
	require.NotNil(t, back.ExitCode)
	require.Equal(t, 137, *back.ExitCode)
}
```

Ensure the test file imports `encoding/json` and `github.com/stretchr/testify/require`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSessionExitCodeJSONRoundTrip`
Expected: FAIL — `s.ExitCode` undefined (compile error).

- [ ] **Step 3: Add the field**

In `internal/store/types.go`, inside the `Session` struct, after the `PID` line (`PID int json:"pid"`):

```go
	ExitCode        *int      `json:"exit_code,omitempty"` // process exit status when recovered: nil=unknown (orphaned/pre-feature), 0=clean, non-zero=crash
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestSessionExitCodeJSONRoundTrip`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/types.go internal/store/types_test.go
git commit -m "feat(store): add Session.ExitCode (nil/0/non-zero)"
```

---

### Task 2: `store.FinalizeExit` + `signalName`

CAS on status (like `UpdateStatusIf`) while also setting `ExitCode` and, for a non-zero code, appending a `session exited` event — all in one atomic write.

**Files:**
- Modify: `internal/store/store.go:24` (interface), `internal/store/file.go` (after `UpdateStatusIf`, ~line 209)
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/file_test.go`. The file already has `func newFileStore(t *testing.T) *FileStore` (line 24) and inserts via `fs.Insert(ctx, &Session{...})` — use both:

```go
func TestFinalizeExitErroredSetsCodeAndEvent(t *testing.T) {
	fs := newFileStore(t) // existing helper in this file
	ctx := context.Background()
	require.NoError(t, fs.Insert(ctx, &Session{ID: "x", Status: StatusWorking}))

	ok, err := fs.FinalizeExit(ctx, "x", StatusWorking, StatusErrored, 137)
	require.NoError(t, err)
	require.True(t, ok)

	got, err := fs.Get(ctx, "x")
	require.NoError(t, err)
	require.Equal(t, StatusErrored, got.Status)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, 137, *got.ExitCode)
	require.Len(t, got.Events, 1)
	require.Contains(t, got.Events[0].Detail, "code 137")
	require.Contains(t, got.Events[0].Detail, "SIGKILL")
}

func TestFinalizeExitCleanSetsCodeNoEvent(t *testing.T) {
	fs := newFileStore(t)
	ctx := context.Background()
	require.NoError(t, fs.Insert(ctx, &Session{ID: "y", Status: StatusWorking}))

	ok, err := fs.FinalizeExit(ctx, "y", StatusWorking, StatusDone, 0)
	require.NoError(t, err)
	require.True(t, ok)

	got, _ := fs.Get(ctx, "y")
	require.Equal(t, StatusDone, got.Status)
	require.NotNil(t, got.ExitCode)
	require.Equal(t, 0, *got.ExitCode)
	require.Empty(t, got.Events) // clean exit logs no event
}

func TestFinalizeExitCASLoses(t *testing.T) {
	fs := newFileStore(t)
	ctx := context.Background()
	require.NoError(t, fs.Insert(ctx, &Session{ID: "z", Status: StatusDone})) // hook already finished it

	ok, err := fs.FinalizeExit(ctx, "z", StatusWorking, StatusErrored, 1)
	require.NoError(t, err)
	require.False(t, ok) // expected!=stored → no-op
	got, _ := fs.Get(ctx, "z")
	require.Equal(t, StatusDone, got.Status)
	require.Nil(t, got.ExitCode)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestFinalizeExit`
Expected: FAIL — `fs.FinalizeExit` undefined.

- [ ] **Step 3: Add to the `Store` interface**

In `internal/store/store.go`, after the `UpdateStatusIf` method (line 24), add:

```go
	// FinalizeExit is a compare-and-swap like UpdateStatusIf that also records the
	// process exit code and, for a non-zero code, appends a "session exited" event
	// — all in one atomic write. The poller uses it to finalize an agent from its
	// exit-file without clobbering a status a SessionEnd hook already set.
	FinalizeExit(ctx context.Context, id string, expected, next Status, code int) (bool, error)
```

- [ ] **Step 4: Implement in `file.go`**

In `internal/store/file.go`, immediately after the `UpdateStatusIf` method (ends ~line 209), add:

```go
// FinalizeExit sets status next (CAS on expected), records ExitCode=code, and
// for a non-zero code appends a "session exited" event — in one atomic write.
func (fs *FileStore) FinalizeExit(ctx context.Context, id string, expected, next Status, code int) (bool, error) {
	if err := safeID(id); err != nil {
		return false, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	s, err := readSession(fs.activePath(id))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if s.Status != expected {
		return false, nil
	}
	s.Status = next
	c := code
	s.ExitCode = &c
	if code != 0 {
		s.Events = append(s.Events, Event{
			TS:     time.Now().UTC(),
			Type:   "exit",
			Detail: exitDetail(code),
		})
	}
	s.UpdatedAt = time.Now().UTC()
	if err := atomicWriteJSON(fs.activePath(id), s); err != nil {
		return false, err
	}
	return true, nil
}

// exitDetail renders a human-readable exit reason. A code in the shell's
// "killed by signal" range (128 < code <= 128+64) names the signal.
func exitDetail(code int) string {
	if sig := signalName(code - 128); code > 128 && code <= 192 && sig != "" {
		return fmt.Sprintf("session exited: code %d (%s)", code, sig)
	}
	return fmt.Sprintf("session exited: code %d", code)
}

// signalName maps the common termination signals to their names; "" for others.
func signalName(sig int) string {
	switch sig {
	case 2:
		return "SIGINT"
	case 6:
		return "SIGABRT"
	case 9:
		return "SIGKILL"
	case 11:
		return "SIGSEGV"
	case 15:
		return "SIGTERM"
	}
	return ""
}
```

Ensure `internal/store/file.go` imports `"fmt"` (add it to the import block if absent).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -run TestFinalizeExit`
Expected: PASS (all three)

Also run the full store package so the interface addition didn't break a mock: `go test ./internal/store/`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/file.go internal/store/file_test.go
git commit -m "feat(store): FinalizeExit — atomic CAS + exit code + exit event"
```

---

### Task 3: Lifecycle `ExitsDir` + `exitSuffix` / `ReadExit` / `ClearExit`

**Files:**
- Modify: `internal/lifecycle/lifecycle.go` (struct ~line 182; new helpers near `writePromptFile` ~line 991)
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestExitSuffixAndReadClear(t *testing.T) {
	dir := t.TempDir()
	l := New(ExecRunner{})
	l.ExitsDir = dir

	suffix := l.exitSuffix("agent-1")
	require.Contains(t, suffix, `printf '%s' "$?"`)
	require.Contains(t, suffix, filepath.Join(dir, "agent-1"))

	// No file yet → not present.
	_, ok := l.ReadExit("agent-1")
	require.False(t, ok)

	// Simulate the shell writing the exit code.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-1"), []byte("137"), 0o600))
	code, ok := l.ReadExit("agent-1")
	require.True(t, ok)
	require.Equal(t, 137, code)

	// Malformed body → treated as absent.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-1"), []byte("nope"), 0o600))
	_, ok = l.ReadExit("agent-1")
	require.False(t, ok)

	// Clear removes the file.
	l.ClearExit("agent-1")
	_, err := os.Stat(filepath.Join(dir, "agent-1"))
	require.True(t, os.IsNotExist(err))
}

func TestExitSuffixEmptyWhenDirUnset(t *testing.T) {
	l := New(ExecRunner{}) // ExitsDir == ""
	require.Equal(t, "", l.exitSuffix("agent-1"))
}

func TestExitSuffixClearsStaleFile(t *testing.T) {
	dir := t.TempDir()
	l := New(ExecRunner{})
	l.ExitsDir = dir
	require.NoError(t, os.WriteFile(filepath.Join(dir, "agent-1"), []byte("9"), 0o600))
	_ = l.exitSuffix("agent-1") // building the suffix at spawn must clear a prior run's file
	_, err := os.Stat(filepath.Join(dir, "agent-1"))
	require.True(t, os.IsNotExist(err))
}
```

Ensure the test imports `os`, `path/filepath`, and `require`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestExitSuffix`
Expected: FAIL — `l.ExitsDir` / `l.exitSuffix` undefined.

- [ ] **Step 3: Add the field and helpers**

In `internal/lifecycle/lifecycle.go`, add to the `Lifecycle` struct (after `PromptsDir`, ~line 192):

```go
	// ExitsDir is a shared dir (the daemon sets it, e.g. ~/.warden/exits) where
	// each agent's shell records claude's exit status, keyed by agent id. Empty
	// (tests) disables exit capture — agents then fall back to orphaned-only
	// classification. Never the dir the agent runs in.
	ExitsDir string
```

Then add these helpers near `writePromptFile` (~line 991). Ensure the file imports `os`, `strconv`, `path/filepath`, and `log` (most are already present — add any missing to the import block):

```go
// exitSuffix ensures ExitsDir exists, clears any stale exit-file for id (from a
// reused id), and returns the shell suffix that records claude's exit status to
// it. Returns "" (no capture) when ExitsDir is unset or the dir can't be made —
// best-effort, consistent with the other launch-time side effects.
func (l *Lifecycle) exitSuffix(id string) string {
	if l.ExitsDir == "" {
		return ""
	}
	if err := os.MkdirAll(l.ExitsDir, 0o700); err != nil {
		log.Printf("exit-capture: mkdir %s: %v", l.ExitsDir, err)
		return ""
	}
	path := filepath.Join(l.ExitsDir, id)
	_ = os.Remove(path) // clear a prior run's file so the poller can't consume it
	return " ; printf '%s' \"$?\" > " + shellQuoteArg(path)
}

// ReadExit returns the exit code recorded for id and whether one is present.
// A missing or malformed file reports (0, false) — treat as "not yet recorded".
func (l *Lifecycle) ReadExit(id string) (int, bool) {
	if l.ExitsDir == "" {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(l.ExitsDir, id))
	if err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return code, true
}

// ClearExit removes id's exit-file (best-effort) once the poller has consumed it.
func (l *Lifecycle) ClearExit(id string) {
	if l.ExitsDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(l.ExitsDir, id))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lifecycle/ -run TestExitSuffix`
Expected: PASS (all three)

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): ExitsDir + exitSuffix/ReadExit/ClearExit helpers"
```

---

### Task 4: Append the exit suffix at the launch sites

Four sites type a `claude` line via `send-keys`. Append `l.exitSuffix(id)` to each `launch` string.

**Files:**
- Modify: `internal/lifecycle/lifecycle.go:593` (spawnFreeForm), `:625` (spawnTyped), `:1058` (SpawnJob), `:737` (resumeInTmux)
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

The lifecycle tests use `fr := &FakeRunner{}` + `New(fr).Spawn(...)` and assert the exact recorded command via `require.Contains(t, fr.calledArgs(), []string{...})` (see `TestSpawnPromptModeLaunchesFromCwd`, ~line 458). Mirror that exactly, with the expected `launch` now carrying the deterministic exit suffix:

```go
func TestSpawnAppendsExitSuffix(t *testing.T) {
	t.Setenv("WARDEN_NO_PIPELINE_HINT", "")  // anchor: hint on, matches production
	t.Setenv("AGENTCTL_NO_PIPELINE_HINT", "")
	fr := &FakeRunner{}
	l := New(fr)
	l.PromptsDir = "/state/prompts"
	l.ExitsDir = t.TempDir()
	prompt := "do a thing"
	s, err := l.Spawn(context.Background(), SpawnRequest{Prompt: prompt, Cwd: "/work/project"})
	require.NoError(t, err)

	promptFile := "/state/prompts/" + s.ID
	launch := claudeLaunch(s.ClaudeSessionID, s.ID, false) + pipelineHint() +
		` "$(cat ` + shellQuoteArg(promptFile) + `)"` +
		" ; printf '%s' \"$?\" > " + shellQuoteArg(filepath.Join(l.ExitsDir, s.ID))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", s.ID, launch, "Enter"})
}
```

This is the exact production launch string with `exitSuffix` appended — `exitSuffix` is deterministic given `ExitsDir` and id. Ensure the test file imports `path/filepath` (it is used elsewhere in the file already).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestSpawnAppendsExitSuffix`
Expected: FAIL — the suffix is not yet appended.

- [ ] **Step 3: Append the suffix at all four sites**

`spawnFreeForm` (line 593):

```go
	launch := claudeLaunch(sess.ClaudeSessionID, sess.ID, req.Supervised) + pipelineHint() + launchPrompt + l.exitSuffix(sess.ID)
```

`spawnTyped` (line 625):

```go
	launch := claudeLaunch(sess.ClaudeSessionID, sess.ID, req.Supervised) + pipelineHint() + l.exitSuffix(sess.ID)
```

`SpawnJob` (line 1058):

```go
	launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"` + l.exitSuffix(id)
```

`resumeInTmux` (line 737) — the send-keys uses the result of `claudeResume(...)` inline; pull it into a variable so the suffix can be appended:

```go
	resume := claudeResume(claudeID, id, supervised) + l.exitSuffix(id)
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, resume, "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys resume: %w: %s", err, out)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lifecycle/`
Expected: PASS (new test green; existing spawn tests still green — when `ExitsDir` is unset in those, `exitSuffix` returns "" so the recorded command is byte-identical to before).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): append exit-capture suffix at all launch sites"
```

---

### Task 5: Extend poller `Deps` + implement in daemon

**Files:**
- Modify: `internal/poller/poller.go:40-51` (Deps interface)
- Modify: `internal/daemon/poller_deps.go`
- Modify: `internal/poller/poller_test.go` (stubDeps must satisfy the wider interface)

- [ ] **Step 1: Extend the `Deps` interface**

In `internal/poller/poller.go`, inside the `Deps` interface (after `Summarize`, line 50), add:

```go
	// ExitCode returns the exit status recorded for the agent's shell, if any.
	ExitCode(ctx context.Context, id string) (code int, present bool)
	// FinalizeExit transitions the session to its terminal status from the exit
	// code (CAS on expected), recording the code (+ event for crashes).
	FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error)
	// ClearExit removes the consumed exit-file so it can't be re-read.
	ClearExit(ctx context.Context, id string)
```

- [ ] **Step 2: Implement in `poller_deps.go`**

Append to `internal/daemon/poller_deps.go`:

```go
func (d *pollerDeps) ExitCode(_ context.Context, id string) (int, bool) {
	return d.lc.ReadExit(id)
}
func (d *pollerDeps) FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error) {
	return d.store.FinalizeExit(ctx, id, expected, next, code)
}
func (d *pollerDeps) ClearExit(_ context.Context, id string) {
	d.lc.ClearExit(id)
}
```

- [ ] **Step 3: Make `stubDeps` satisfy the interface**

In `internal/poller/poller_test.go`, add fields to `stubDeps` and method stubs so it still compiles. Add to the struct:

```go
	exitCodes  map[string]int  // id → recorded exit code (presence = in map)
	finalized  map[string]store.Status // records FinalizeExit successful swaps
	finalCode  map[string]int          // records the code passed to FinalizeExit
	cleared    map[string]bool         // records ClearExit calls
```

And the methods:

```go
func (d *stubDeps) ExitCode(_ context.Context, id string) (int, bool) {
	c, ok := d.exitCodes[id]
	return c, ok
}
func (d *stubDeps) FinalizeExit(_ context.Context, id string, expected, next store.Status, code int) (bool, error) {
	if d.lastExpected == nil {
		d.lastExpected = map[string]store.Status{}
	}
	d.lastExpected[id] = expected
	if d.casFail[id] {
		return false, nil
	}
	if d.finalized == nil {
		d.finalized = map[string]store.Status{}
		d.finalCode = map[string]int{}
	}
	d.finalized[id] = next
	d.finalCode[id] = code
	return true, nil
}
func (d *stubDeps) ClearExit(_ context.Context, id string) {
	if d.cleared == nil {
		d.cleared = map[string]bool{}
	}
	d.cleared[id] = true
}
```

- [ ] **Step 4: Verify it compiles (tests unchanged still pass)**

Run: `go test ./internal/poller/ ./internal/daemon/`
Expected: PASS — interface widened, stub satisfies it, no behavior change yet.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/poller.go internal/daemon/poller_deps.go internal/poller/poller_test.go
git commit -m "feat(poller): widen Deps with ExitCode/FinalizeExit/ClearExit"
```

---

### Task 6: Poller reads the exit-file first in `tick`

**Files:**
- Modify: `internal/poller/poller.go:96-152` (the per-session loop in `tick`)
- Test: `internal/poller/poller_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/poller/poller_test.go`. These drive a full `tick` (not just `classify`). Mirror any existing `tick`-level test in the file for setup; the assertions are:

```go
func TestTickFinalizesFromExitFile(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		present  bool
		alive    bool
		wantNext store.Status
		wantFin  bool // FinalizeExit expected to run
	}{
		{"clean exit hook missed", 0, true, true, store.StatusDone, true},
		{"crash with code", 137, true, true, store.StatusErrored, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &stubDeps{
				sessions:  []*store.Session{{ID: "A", Status: store.StatusWorking}},
				alive:     map[string]bool{"A": tc.alive},
				panes:     map[string]string{},
				updates:   map[string]store.Status{},
				exitCodes: map[string]int{},
			}
			if tc.present {
				d.exitCodes["A"] = tc.code
			}
			p := New(d, 5*time.Minute)
			require.NoError(t, p.tick(context.Background()))
			require.Equal(t, tc.wantNext, d.finalized["A"])
			require.Equal(t, tc.code, d.finalCode["A"])
			require.True(t, d.cleared["A"]) // file consumed
		})
	}
}

func TestTickOrphanedOnlyWhenNoExitFile(t *testing.T) {
	// Window gone, no exit-file → orphaned via the existing classify path.
	d := &stubDeps{
		sessions:  []*store.Session{{ID: "A", Status: store.StatusWorking}},
		alive:     map[string]bool{"A": false},
		panes:     map[string]string{},
		updates:   map[string]store.Status{},
		exitCodes: map[string]int{}, // empty → not present
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Equal(t, store.StatusOrphaned, d.updates["A"]) // via UpdateStatusIf
	require.Empty(t, d.finalized)                          // FinalizeExit not called
}

func TestTickExitFileCASLosesToHook(t *testing.T) {
	d := &stubDeps{
		sessions:  []*store.Session{{ID: "A", Status: store.StatusWorking}},
		alive:     map[string]bool{"A": true},
		panes:     map[string]string{},
		updates:   map[string]store.Status{},
		exitCodes: map[string]int{"A": 1},
		casFail:   map[string]bool{"A": true}, // hook already finalized it
	}
	p := New(d, 5*time.Minute)
	require.NoError(t, p.tick(context.Background()))
	require.Empty(t, d.finalized["A"]) // swap lost
	require.True(t, d.cleared["A"])    // stale file still cleared
}
```

> `stubDeps.SessionAlive` returns `d.alive[name]` and `CapturePane` returns `d.panes[name], nil` (no error on a missing key — confirmed at `poller_test.go:171-176`), so an empty `panes` map is safe. In the finalize cases `tick` `continue`s before `CapturePane` anyway; in the orphaned case `alive=false` skips capture.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/poller/ -run TestTick`
Expected: FAIL — exit-file path not handled (no `finalized` recorded; orphaned still fires even with a code).

- [ ] **Step 3: Add the exit-file branch in `tick`**

In `internal/poller/poller.go`, at the top of the per-session loop body in `tick` — immediately after the `if isTerminal(s.Status) { continue }` guard (line 104-106) — insert:

```go
		// Exit-file is authoritative: if the agent's shell recorded an exit code,
		// finalize from it (CAS so a SessionEnd hook that already set done wins)
		// and skip liveness/pane classification this tick.
		if code, ok := p.deps.ExitCode(ctx, s.ID); ok {
			next := store.StatusDone
			if code != 0 {
				next = store.StatusErrored
			}
			swapped, err := p.deps.FinalizeExit(ctx, s.ID, s.Status, next, code)
			if err != nil {
				log.Printf("poller: finalize %s: %v", s.ID, err)
				continue // leave the file; retry next tick
			}
			p.deps.ClearExit(ctx, s.ID) // consumed (clear even if CAS lost — the file is stale)
			if swapped {
				changed = true
				if p.OnTransition != nil {
					p.OnTransition(s, s.Status, next)
				}
			}
			continue
		}
```

This sits before `alive := p.deps.SessionAlive(...)`. When no exit-file exists, control falls through to the unchanged liveness/pane logic — so the `!alive → orphaned` branch in `classify` is now reached only when no exit-file exists, exactly as designed. `classify` itself needs no change.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/poller/`
Expected: PASS (new tick tests green; all existing classify/summary tests still green)

- [ ] **Step 5: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "feat(poller): finalize agents from exit-file (done/errored), gate orphaned"
```

---

### Task 7: Daemon wiring — set `ExitsDir` + harden the dir

**Files:**
- Modify: `internal/cli/daemon.go:64`
- Modify: `internal/daemon/harden.go:11`
- Test: `internal/daemon/harden_test.go` (if present) — otherwise rely on the build + the existing harden test

- [ ] **Step 1: Write/extend the failing test**

If `internal/daemon/harden_test.go` exists and tests `hardenedSubdirs`, add an assertion that `"exits"` is included; otherwise add:

```go
func TestHardenedSubdirsIncludesExits(t *testing.T) {
	found := false
	for _, s := range hardenedSubdirs {
		if s == "exits" {
			found = true
		}
	}
	require.True(t, found, "exits dir must be hardened to 0o700 like the other data dirs")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestHardenedSubdirsIncludesExits`
Expected: FAIL — `"exits"` not in the list.

- [ ] **Step 3: Make the changes**

In `internal/daemon/harden.go` line 11, add `"exits"`:

```go
var hardenedSubdirs = []string{"sessions", "closed", "context", "inbox", "pipelines", "prompts", "metrics", "exits"}
```

In `internal/cli/daemon.go`, after the `lc.PromptsDir = ...` line (64), add:

```go
				lc.ExitsDir = filepath.Join(cfg.DataDir, "exits")
```

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/daemon/ -run TestHardenedSubdirsIncludesExits && go build ./...`
Expected: PASS + clean build

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/harden.go internal/cli/daemon.go internal/daemon/harden_test.go
git commit -m "feat(daemon): wire ExitsDir (~/.warden/exits) + harden to 0o700"
```

---

### Task 8: Surface the exit code (web + TUI)

**Files:**
- Modify: `web/src/lib/types.ts:36-55` (Session), `web/src/lib/status.ts`, `web/src/components/BusyIdleBadge.tsx` + its 4 call sites (`AgentGrid.tsx:39`, `AgentTab.tsx:35`, `AttentionQueue.tsx:50`, `TabBar.tsx:27`)
- Test: `web/src/lib/status.test.ts`
- Modify: `internal/tui/styles.go:34-35`, `internal/tui/list.go:490`
- Test: `internal/tui/styles_test.go` (create)

- [ ] **Step 1: Write the failing web test**

In `web/src/lib/status.test.ts`, add:

```ts
import { busyIdle } from './status';

it('shows the exit code on an errored badge when present', () => {
  expect(busyIdle('errored', 137).label).toBe('Error (137)');
});
it('errored without a code keeps the plain Error label', () => {
  expect(busyIdle('errored').label).toBe('Error');
});
```

- [ ] **Step 2: Run web test to verify it fails**

Run: `cd web && npx vitest run src/lib/status.test.ts`
Expected: FAIL — `busyIdle` takes one arg / label is `Error`.

- [ ] **Step 3: Update types + status mapping**

In `web/src/lib/types.ts`, add to the `Session` interface (after `supervised: boolean;`):

```ts
  exit_code?: number | null;
```

In `web/src/lib/status.ts`, change the signature and the `errored` case:

```ts
export function busyIdle(status: Status, exitCode?: number | null): Badge {
  switch (status) {
    case 'spawning': return { label: 'Starting', kind: 'busy' };
    case 'working': return { label: 'Busy', kind: 'busy' };
    case 'waiting_for_input': return { label: 'Needs input', kind: 'attention' };
    case 'idle': return { label: 'Idle', kind: 'idle' };
    case 'done': return { label: 'Done', kind: 'idle' };
    case 'errored':
      return { label: exitCode != null && exitCode !== 0 ? `Error (${exitCode})` : 'Error', kind: 'error' };
    case 'orphaned': return { label: 'Orphaned', kind: 'error' };
    default: return { label: status, kind: 'idle' };
  }
}
```

Then thread the code through the badge component. In `web/src/components/BusyIdleBadge.tsx`:

```tsx
import type { Status } from '../lib/types';
import { busyIdle } from '../lib/status';

export default function BusyIdleBadge({ status, exitCode }: { status: Status; exitCode?: number | null }) {
  const b = busyIdle(status, exitCode);
  return <span className={`badge ${b.kind}`} title={status}>{b.label}</span>;
}
```

And pass `exitCode` at each of the 4 call sites (the session var is `s` or `session`):
- `AgentGrid.tsx:39` → `<BusyIdleBadge status={s.status} exitCode={s.exit_code} />`
- `AgentTab.tsx:35` → `<BusyIdleBadge status={session.status} exitCode={session.exit_code} />`
- `AttentionQueue.tsx:50` → `<BusyIdleBadge status={s.status} exitCode={s.exit_code} />`
- `TabBar.tsx:27` → `<BusyIdleBadge status={s.status} exitCode={s.exit_code} />`

The `exitCode` prop is optional, so this is a safe widening.

- [ ] **Step 4: Run web test to verify it passes**

Run: `cd web && npx vitest run src/lib/status.test.ts`
Expected: PASS

- [ ] **Step 5: Write the failing TUI test**

Add a `badge` helper that takes the session (or code) so it can append the code. In `internal/tui/styles_test.go` (create if absent, `package tui`):

```go
package tui

import (
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestBadgeErroredShowsExitCode(t *testing.T) {
	code := 137
	label, _ := badge(store.StatusErrored, &code)
	require.Equal(t, "error 137", label)

	label2, _ := badge(store.StatusErrored, nil)
	require.Equal(t, "error", label2)

	label3, _ := badge(store.StatusDone, nil)
	require.Equal(t, "done", label3)
}
```

- [ ] **Step 6: Run TUI test to verify it fails**

Run: `go test ./internal/tui/ -run TestBadgeErroredShowsExitCode`
Expected: FAIL — `badge` takes one arg.

- [ ] **Step 7: Update `badge` to accept the exit code**

In `internal/tui/styles.go`, change the signature and the errored case:

```go
func badge(s store.Status, exitCode *int) (string, lipgloss.Style) {
	switch s {
	case store.StatusSpawning:
		return "starting", stBusy
	case store.StatusWorking:
		return "busy", stBusy
	case store.StatusWaitingForInput:
		return "needs-input", stAttention
	case store.StatusIdle:
		return "idle", stIdle
	case store.StatusDone:
		return "done", stIdle
	case store.StatusErrored:
		if exitCode != nil && *exitCode != 0 {
			return fmt.Sprintf("error %d", *exitCode), stError
		}
		return "error", stError
	case store.StatusOrphaned:
		return "orphaned", stError
	default:
		if s == "" {
			return "classifying", stMuted
		}
		return string(s), stIdle
	}
}
```

Add `"fmt"` to the `internal/tui/styles.go` import block. There is exactly one caller — `internal/tui/list.go:490` — change it from `badge(s.Status)` to `badge(s.Status, s.ExitCode)` (`s` is the `*store.Session` already in scope there).

- [ ] **Step 8: Run tests + build to verify they pass**

Run: `go test ./internal/tui/ && go build ./...`
Expected: PASS + clean build

- [ ] **Step 9: Commit**

```bash
git add web/src/lib/types.ts web/src/lib/status.ts web/src/lib/status.test.ts internal/tui/styles.go internal/tui/styles_test.go
git commit -m "feat(web,tui): show exit code on the errored badge"
```

---

### Task 9: Full verification

- [ ] **Step 1: Run the whole Go suite**

Run: `go test ./...`
Expected: PASS (note: per memory, heavy tmux/daemon packages can be slow on a contended machine; if a package times out, re-run that package alone with `-timeout 300s` to confirm it's contention, not a regression).

- [ ] **Step 2: Run the web suite + build**

Run: `cd web && npm test && npm run build`
Expected: PASS + dist built (needed so the daemon embeds the updated UI).

- [ ] **Step 3: Vet**

Run: `go vet ./...`
Expected: clean

- [ ] **Step 4: Commit any final fixups, then hand off for live smoke**

Live smoke is left for the user (requires `make install` + **daemon restart**): spawn a supervised probe agent, kill the `claude` process (`pkill -f 'claude --'` inside its pane, or `kill -9` the claude PID) and confirm it flips to `errored` with the exit code on the badge + an `exit` event; separately `tmux kill-session` an agent and confirm it flips to `orphaned` (no exit-file). Record the outcome.

---

## Notes for the implementer

- **Why orphaned needs no code change in `classify`:** the `tick` exit-file branch `continue`s before reaching `SessionAlive`/`classify`, so `classify`'s `!alive → orphaned` is naturally reached only when no exit-file exists.
- **Best-effort posture:** exit capture must never block a spawn. `exitSuffix` returning `""` (dir unset / mkdir fails) cleanly degrades to today's behavior.
- **Backward compatible:** agents spawned before this change have no exit-file → they keep falling to `orphaned`/`idle`; no migration.
- **Rollout:** daemon-side change → `make install` + daemon restart; web change → the embedded `web/dist` rebuild happens via the release/install build.
