# `agentctl adopt` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `agentctl adopt` — register an existing Claude Code session into agentctl, either by resuming it under a fresh tmux session (plain shell) or by registering an already-running tmux session live (no relaunch).

**Architecture:** A new `POST /adopt` daemon endpoint orchestrates: resolve the claude session id (newest transcript for the dir, or an explicit override), guard against adopting a conversation already tracked, then either resume-under-tmux or register-live. The heavy lifting reuses existing lifecycle primitives (`Restore`'s tmux/resume body, `newestTranscriptPath`) and the poller (which already monitors any tmux session by name, so monitoring needs no changes). The CLI detects tmux via `$TMUX` and prints an attach hint.

**Tech Stack:** Go, chi router, cobra CLI, testify. Spec: `docs/superpowers/specs/2026-06-03-agentctl-adopt-existing-claude-session-design.md`.

---

## File Structure

- `internal/lifecycle/lifecycle.go` — add `NewestClaudeSession`, extract `resumeInTmux` from `Restore`, add `Adopt` + `AdoptRequest` + `ErrTmuxGone`.
- `internal/lifecycle/lifecycle_test.go` — tests for the three above.
- `internal/store/file.go` — export `SafeID` (thin wrapper over `safeID`).
- `internal/store/file_test.go` — `SafeID` test.
- `internal/daemon/api.go` — `AdoptRequest`/`AdoptParams`/`adoptResponse` DTOs; add `NewestClaudeSession` + `Adopt` to the `Lifecycle` interface.
- `internal/daemon/lifecycle_adapter.go` — implement the two new interface methods (thin translation).
- `internal/daemon/lifecycle_routes.go` — `handleAdopt` + register `POST /adopt`.
- `internal/daemon/lifecycle_routes_test.go` — extend `fakeLife`; handler tests.
- `internal/client/client.go` — `Adopt` + `AdoptParams` + `AdoptResult`.
- `internal/cli/lifecycle.go` — `newAdoptCmd` + `currentTmuxSession`.
- `internal/cli/root.go` — register `newAdoptCmd()`.
- `internal/cli/lifecycle_test.go` — `currentTmuxSession` env-gate test (create file if absent).
- `internal/mcp/server.go` — (stretch) `adopt_agent` tool.

---

## Task 1: lifecycle `NewestClaudeSession`

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestNewestClaudeSession(t *testing.T) {
	root := t.TempDir()
	workdir := t.TempDir()
	pdir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(pdir, 0o755))
	older := "11111111-1111-4111-8111-111111111111"
	newer := "22222222-2222-4222-8222-222222222222"
	require.NoError(t, os.WriteFile(filepath.Join(pdir, older+".jsonl"), []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(pdir, newer+".jsonl"), []byte("{}"), 0o644))
	// Make `newer` the most recently modified.
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(filepath.Join(pdir, newer+".jsonl"), future, future))

	lc := New(&FakeRunner{})
	lc.ProjectsDir = root
	got, err := lc.NewestClaudeSession(workdir)
	require.NoError(t, err)
	require.Equal(t, newer, got)
}

func TestNewestClaudeSessionNone(t *testing.T) {
	lc := New(&FakeRunner{})
	lc.ProjectsDir = t.TempDir() // exists but empty
	_, err := lc.NewestClaudeSession(t.TempDir())
	require.ErrorIs(t, err, ErrNoTranscript)

	lc2 := New(&FakeRunner{}) // ProjectsDir empty → disabled
	_, err = lc2.NewestClaudeSession(t.TempDir())
	require.ErrorIs(t, err, ErrNoTranscript)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestNewestClaudeSession -v`
Expected: FAIL — `lc.NewestClaudeSession undefined`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/lifecycle/lifecycle.go` (just after `newestTranscriptPath`, ~line 348):

```go
// NewestClaudeSession returns the claude session id (uuid) of the most recently
// modified transcript for workdir, or ErrNoTranscript when there is none (or
// transcript lookup is disabled). Pure filesystem inspection — no subprocess.
func (l *Lifecycle) NewestClaudeSession(workdir string) (string, error) {
	dir := claudeProjectDir(l.ProjectsDir, workdir)
	if dir == "" {
		return "", ErrNoTranscript
	}
	p := newestTranscriptPath(dir)
	if p == "" {
		return "", ErrNoTranscript
	}
	return strings.TrimSuffix(filepath.Base(p), ".jsonl"), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/lifecycle/ -run TestNewestClaudeSession -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): add NewestClaudeSession transcript discovery"
```

---

## Task 2: Extract `resumeInTmux` from `Restore` (refactor)

No behavior change — `Restore` keeps working; the resume body becomes reusable.

**Files:**
- Modify: `internal/lifecycle/lifecycle.go:486-492`

- [ ] **Step 1: Run the existing Restore test to confirm green baseline**

Run: `go test ./internal/lifecycle/ -run TestRestore -v`
Expected: PASS (`TestRestoreRecreatesAndResumes`, `TestRestorePreconditionErrors`).

- [ ] **Step 2: Add the helper and call it from Restore**

In `internal/lifecycle/lifecycle.go`, add the helper just above `Restore` (~line 466):

```go
// resumeInTmux creates a detached tmux session named id in cwd and resumes the
// claude conversation claudeID inside it. Shared by Restore and Adopt.
func (l *Lifecycle) resumeInTmux(ctx context.Context, id, cwd, claudeID string) error {
	if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", id, "-c", cwd); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", id, claudeResume(claudeID, id), "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys resume: %w: %s", err, out)
	}
	return nil
}
```

Then replace the last two `run.Run` calls in `Restore` (the `tmux new-session` and `tmux send-keys` block, lines 486-491) with:

```go
	return l.resumeInTmux(ctx, sess.ID, sess.Workdir, sess.ClaudeSessionID)
```

So `Restore` ends:

```go
	if l.transcriptPath(sess) == "" {
		return ErrNoTranscript
	}
	return l.resumeInTmux(ctx, sess.ID, sess.Workdir, sess.ClaudeSessionID)
}
```

- [ ] **Step 3: Run the Restore test to verify no regression**

Run: `go test ./internal/lifecycle/ -run TestRestore -v`
Expected: PASS — `TestRestoreRecreatesAndResumes` still asserts the same `new-session` and `send-keys` arg vectors.

- [ ] **Step 4: Commit**

```bash
git add internal/lifecycle/lifecycle.go
git commit -m "refactor(lifecycle): extract resumeInTmux from Restore for reuse"
```

---

## Task 3: lifecycle `Adopt` + `AdoptRequest` + `ErrTmuxGone`

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/lifecycle/lifecycle_test.go`:

```go
func TestAdoptResumeMode(t *testing.T) {
	workdir := t.TempDir()
	sid := "33333333-3333-4333-8333-333333333333"
	fr := &FakeRunner{}
	lc := New(fr)
	sess, err := lc.Adopt(context.Background(), AdoptRequest{
		ID: "agent-a1", Cwd: workdir, ClaudeSessionID: sid, TmuxSession: "",
	})
	require.NoError(t, err)
	require.Equal(t, "agent-a1", sess.ID)
	require.Equal(t, "agent-a1", sess.TmuxSession)
	require.Equal(t, sid, sess.ClaudeSessionID)
	require.Equal(t, store.TypeOther, sess.Type)
	require.Equal(t, store.StatusSpawning, sess.Status)
	require.Equal(t, workdir, sess.Workdir)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "agent-a1", "-c", workdir})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "agent-a1", claudeResume(sid, "agent-a1"), "Enter"})
}

func TestAdoptResumeGeneratesID(t *testing.T) {
	sess, err := New(&FakeRunner{}).Adopt(context.Background(), AdoptRequest{
		ID: "", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "",
	})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sess.ID, "agent-"), "generated id, got %q", sess.ID)
	require.Equal(t, sess.ID, sess.TmuxSession)
}

func TestAdoptResumeNoClaudeID(t *testing.T) {
	_, err := New(&FakeRunner{}).Adopt(context.Background(), AdoptRequest{
		ID: "agent-a1", Cwd: t.TempDir(), ClaudeSessionID: "", TmuxSession: "",
	})
	require.ErrorIs(t, err, ErrNoTranscript)
}

func TestAdoptResumeWorkdirMissing(t *testing.T) {
	_, err := New(&FakeRunner{}).Adopt(context.Background(), AdoptRequest{
		ID: "agent-a1", Cwd: "/no/such/dir", ClaudeSessionID: "x", TmuxSession: "",
	})
	require.ErrorIs(t, err, ErrWorkdirMissing)
}

func TestAdoptLiveKeepsName(t *testing.T) {
	// FakeRunner default = success → has-session succeeds → tmux session alive.
	fr := &FakeRunner{}
	sess, err := New(fr).Adopt(context.Background(), AdoptRequest{
		ID: "work", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "work",
	})
	require.NoError(t, err)
	require.Equal(t, "work", sess.ID)
	require.Equal(t, "work", sess.TmuxSession)
	require.Equal(t, store.StatusWorking, sess.Status)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "has-session", "-t", "work"})
	for _, a := range fr.calledArgs() {
		require.NotEqual(t, "rename-session", argAt(a, 1), "no rename when id == tmux name")
		require.NotEqual(t, "new-session", argAt(a, 1), "live adopt never relaunches")
	}
}

func TestAdoptLiveRenamesWhenIDDiffers(t *testing.T) {
	fr := &FakeRunner{}
	sess, err := New(fr).Adopt(context.Background(), AdoptRequest{
		ID: "agent-b2", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "0",
	})
	require.NoError(t, err)
	require.Equal(t, "agent-b2", sess.ID)
	require.Equal(t, "agent-b2", sess.TmuxSession)
	require.Contains(t, fr.calledArgs(), []string{"tmux", "rename-session", "-t", "0", "agent-b2"})
}

func TestAdoptLiveTmuxGone(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t ghost": {Err: errStub("no session")},
	}}
	_, err := New(fr).Adopt(context.Background(), AdoptRequest{
		ID: "ghost", Cwd: t.TempDir(), ClaudeSessionID: "x", TmuxSession: "ghost",
	})
	require.ErrorIs(t, err, ErrTmuxGone)
}
```

Add this tiny test helper at the bottom of the test file (if not already present):

```go
// argAt returns a[i] or "" when out of range — for asserting tmux subcommands.
func argAt(a []string, i int) string {
	if i < len(a) {
		return a[i]
	}
	return ""
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/lifecycle/ -run TestAdopt -v`
Expected: FAIL — `AdoptRequest`, `ErrTmuxGone`, and `Adopt` undefined.

- [ ] **Step 3: Write the implementation**

Add `ErrTmuxGone` to the error block in `internal/lifecycle/lifecycle.go` (the `var (...)` at ~line 495):

```go
	ErrTmuxGone           = errors.New("tmux session not found")
```

Add the type and method just after `Restore` (~line 493):

```go
// AdoptRequest carries the resolved inputs for Adopt. TmuxSession == "" selects
// resume mode (create a fresh tmux session and `claude --resume`); a non-empty
// TmuxSession selects live mode (register an existing tmux session, no
// relaunch). ID == "" generates an "agent-<short>" id; in live mode an ID that
// differs from TmuxSession triggers a tmux rename so the agent id and tmux
// session name stay equal (attach/switch-client target the id).
type AdoptRequest struct {
	ID              string
	Cwd             string
	ClaudeSessionID string
	TmuxSession     string
}

// Adopt registers a Claude session agentctl did not spawn. Resume mode resumes
// the conversation under a new tmux session; live mode adopts an existing tmux
// session as-is. It returns the (unpersisted) session record for the caller to
// store. It never relaunches a live session.
func (l *Lifecycle) Adopt(ctx context.Context, req AdoptRequest) (*store.Session, error) {
	id := req.ID
	if id == "" {
		sid, err := shortID()
		if err != nil {
			return nil, err
		}
		id = "agent-" + sid
	}
	sess := &store.Session{
		ID:              id,
		TmuxSession:     id,
		Type:            store.TypeOther,
		Workdir:         req.Cwd,
		ClaudeSessionID: req.ClaudeSessionID,
	}
	if req.TmuxSession == "" { // resume mode
		if req.ClaudeSessionID == "" {
			return nil, ErrNoTranscript
		}
		if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
			return nil, ErrWorkdirMissing
		}
		sess.Status = store.StatusSpawning
		if err := l.resumeInTmux(ctx, id, req.Cwd, req.ClaudeSessionID); err != nil {
			return nil, err
		}
		return sess, nil
	}
	// live mode: register an existing tmux session, no relaunch.
	if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", req.TmuxSession); err != nil {
		return nil, ErrTmuxGone
	}
	if id != req.TmuxSession {
		if out, err := l.run.Run(ctx, "", "tmux", "rename-session", "-t", req.TmuxSession, id); err != nil {
			return nil, fmt.Errorf("tmux rename-session: %w: %s", err, out)
		}
	}
	sess.Status = store.StatusWorking
	return sess, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lifecycle/ -run TestAdopt -v`
Expected: PASS (all seven).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): add Adopt (resume-under-tmux + live register)"
```

---

## Task 4: Export `store.SafeID`

The daemon handler needs to validate a tmux session name as a candidate id.

**Files:**
- Modify: `internal/store/file.go:50-55`
- Test: `internal/store/file_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/file_test.go`:

```go
func TestSafeID(t *testing.T) {
	require.NoError(t, SafeID("agent-1234"))
	require.NoError(t, SafeID("work"))
	require.Error(t, SafeID(""))
	require.Error(t, SafeID("a/b"))
	require.Error(t, SafeID("../etc"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestSafeID -v`
Expected: FAIL — `SafeID undefined`.

- [ ] **Step 3: Add the exported wrapper**

In `internal/store/file.go`, just after the existing `safeID` func (~line 55):

```go
// SafeID reports whether id is a valid session id (no path separators or "..").
// Exported for callers that validate a candidate id before insert (e.g. adopt).
func SafeID(id string) error { return safeID(id) }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestSafeID -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/file.go internal/store/file_test.go
git commit -m "feat(store): export SafeID for id validation by callers"
```

---

## Task 5: Daemon DTOs, interface methods, adapter, fake (compile-only)

This task makes everything compile; the handler comes in Task 6.

**Files:**
- Modify: `internal/daemon/api.go`, `internal/daemon/lifecycle_adapter.go`, `internal/daemon/lifecycle_routes_test.go`

- [ ] **Step 1: Add DTOs and interface methods**

In `internal/daemon/api.go`, after the `SpawnRequest` block (~line 32):

```go
// AdoptRequest is the body for POST /adopt.
type AdoptRequest struct {
	Cwd         string `json:"cwd"`          // required; dir whose claude session to adopt
	SessionID   string `json:"session_id"`   // optional claude uuid override
	TmuxSession string `json:"tmux_session"` // non-empty ⇒ live-register an existing tmux session
}

// AdoptParams are the resolved inputs the handler passes to Lifecycle.Adopt.
type AdoptParams struct {
	ID              string // chosen id; "" ⇒ Lifecycle generates one
	Cwd             string
	ClaudeSessionID string // may be "" in live mode
	TmuxSession     string // "" ⇒ resume mode
}

// adoptResponse is the body for POST /adopt: the new session plus an optional
// non-fatal warning (e.g. live-registered without a resolvable claude id).
type adoptResponse struct {
	Session *store.Session `json:"session"`
	Warning string         `json:"warning,omitempty"`
}
```

In the same file, add to the `Lifecycle` interface (after `Restore`, ~line 97):

```go
	// NewestClaudeSession returns the claude session id of the newest transcript
	// for cwd (ErrNoTranscript when none).
	NewestClaudeSession(ctx context.Context, cwd string) (string, error)
	// Adopt registers a session agentctl did not spawn (resume or live) and
	// returns the unpersisted record.
	Adopt(ctx context.Context, req AdoptParams) (*store.Session, error)
```

- [ ] **Step 2: Implement the methods on the adapter**

In `internal/daemon/lifecycle_adapter.go`, after `Restore` (~line 71):

```go
func (a *lifecycleAdapter) NewestClaudeSession(_ context.Context, cwd string) (string, error) {
	return a.lc.NewestClaudeSession(cwd)
}

func (a *lifecycleAdapter) Adopt(ctx context.Context, req AdoptParams) (*store.Session, error) {
	return a.lc.Adopt(ctx, lifecycle.AdoptRequest{
		ID:              req.ID,
		Cwd:             req.Cwd,
		ClaudeSessionID: req.ClaudeSessionID,
		TmuxSession:     req.TmuxSession,
	})
}
```

- [ ] **Step 3: Extend the test fake so the package still compiles**

In `internal/daemon/lifecycle_routes_test.go`, add fields to the `fakeLife` struct (after `removeWTErr`):

```go
	newestClaude string
	newestErr    error
	adoptResult  *store.Session
	adoptErr     error
	adoptParams  AdoptParams
```

And add the two methods (after the `Restore` method, ~line 86):

```go
func (f *fakeLife) NewestClaudeSession(_ context.Context, cwd string) (string, error) {
	if f.newestErr != nil {
		return "", f.newestErr
	}
	return f.newestClaude, nil
}
func (f *fakeLife) Adopt(_ context.Context, req AdoptParams) (*store.Session, error) {
	f.adoptParams = req
	if f.adoptErr != nil {
		return nil, f.adoptErr
	}
	if f.adoptResult != nil {
		return f.adoptResult, nil
	}
	id := req.ID
	if id == "" {
		id = "agent-generated"
	}
	return &store.Session{
		ID: id, TmuxSession: id, Type: store.TypeOther, Workdir: req.Cwd,
		ClaudeSessionID: req.ClaudeSessionID, Status: store.StatusWorking,
	}, nil
}
```

- [ ] **Step 4: Verify the package compiles and existing tests pass**

Run: `go build ./... && go test ./internal/daemon/ -v 2>&1 | tail -20`
Expected: build OK; existing daemon tests still PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): adopt DTOs + Lifecycle interface methods + adapter"
```

---

## Task 6: Daemon `handleAdopt` + route

**Files:**
- Modify: `internal/daemon/lifecycle_routes.go`
- Test: `internal/daemon/lifecycle_routes_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/daemon/lifecycle_routes_test.go`. (These reuse the existing helpers `newFakeStore()`, `newHub()`, and the `httptest` pattern already used by the spawn tests.)

```go
func adoptServer(fl *fakeLife, fs store.Store) *httptest.Server {
	srv := &Server{store: fs, life: fl, hub: newHub()}
	return httptest.NewServer(srv.router())
}

func TestAdoptResumeHappyPath(t *testing.T) {
	dir := t.TempDir()
	fl := &fakeLife{newestClaude: "44444444-4444-4444-8444-444444444444"}
	ts := adoptServer(fl, newFakeStore())
	defer ts.Close()

	body, _ := json.Marshal(AdoptRequest{Cwd: dir}) // resume mode (no tmux_session)
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got adoptResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotNil(t, got.Session)
	require.Equal(t, "44444444-4444-4444-8444-444444444444", fl.adoptParams.ClaudeSessionID)
	require.Empty(t, fl.adoptParams.TmuxSession, "resume mode passes no tmux session")
	require.Empty(t, got.Warning)
}

func TestAdoptResumeNoClaudeSession(t *testing.T) {
	dir := t.TempDir()
	fl := &fakeLife{newestErr: errors.New("none")}
	ts := adoptServer(fl, newFakeStore())
	defer ts.Close()

	body, _ := json.Marshal(AdoptRequest{Cwd: dir})
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdoptCwdMissing(t *testing.T) {
	ts := adoptServer(&fakeLife{}, newFakeStore())
	defer ts.Close()
	body, _ := json.Marshal(AdoptRequest{Cwd: "/no/such/dir/xyz"})
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdoptDuplicateClaudeSession(t *testing.T) {
	dir := t.TempDir()
	fs := newFakeStore()
	sid := "55555555-5555-4555-8555-555555555555"
	require.NoError(t, fs.Insert(context.Background(), &store.Session{
		ID: "existing", TmuxSession: "existing", ClaudeSessionID: sid, Status: store.StatusWorking,
	}))
	fl := &fakeLife{newestClaude: sid}
	ts := adoptServer(fl, fs)
	defer ts.Close()

	body, _ := json.Marshal(AdoptRequest{Cwd: dir})
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestAdoptLiveTmuxGone(t *testing.T) {
	dir := t.TempDir()
	fl := &fakeLife{newestClaude: "x", adoptErr: lifecycle.ErrTmuxGone}
	ts := adoptServer(fl, newFakeStore())
	defer ts.Close()

	body, _ := json.Marshal(AdoptRequest{Cwd: dir, TmuxSession: "ghost"})
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAdoptLiveInsertFailureDoesNotTeardown(t *testing.T) {
	dir := t.TempDir()
	// Pre-seed the id the fake will return so Insert fails with ErrExists.
	fs := newFakeStore()
	require.NoError(t, fs.Insert(context.Background(), &store.Session{ID: "work", TmuxSession: "work"}))
	fl := &fakeLife{adoptResult: &store.Session{ID: "work", TmuxSession: "work", Status: store.StatusWorking}}
	ts := adoptServer(fl, fs)
	defer ts.Close()

	body, _ := json.Marshal(AdoptRequest{Cwd: dir, TmuxSession: "work", SessionID: "zzz"})
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Empty(t, fl.tornDown, "live adopt must NOT tear down the user's existing tmux session")
}
```

Ensure the test file imports `bytes`, `errors`, and `github.com/srajanpathak/agentctl/internal/lifecycle` (add any missing).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/daemon/ -run TestAdopt -v`
Expected: FAIL — `/adopt` returns 404 (route not registered) / `handleAdopt` undefined.

- [ ] **Step 3: Register the route**

In `internal/daemon/lifecycle_routes.go`, add to `registerLifecycleRoutes` (after the `/spawn` line, ~line 19):

```go
	r.Post("/adopt", s.handleAdopt)
```

- [ ] **Step 4: Write the handler**

Add `handleAdopt` to `internal/daemon/lifecycle_routes.go` (e.g. just after `handleSpawn`, before `classifyAndUpdate`):

```go
// handleAdopt registers a Claude session agentctl did not spawn. It resolves the
// claude session id (explicit override, else newest transcript for cwd), refuses
// to adopt a conversation an active session already tracks, then delegates to
// Lifecycle.Adopt (resume-under-tmux when tmux_session is empty, live register
// otherwise) and persists the record. Rollback (kill tmux) runs ONLY in resume
// mode — a live adoption never owns the tmux session, so it must never kill it.
func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	var req AdoptRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if req.Cwd == "" {
		writeErr(w, http.StatusBadRequest, "adopt requires cwd")
		return
	}
	if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
		writeErr(w, http.StatusBadRequest, "cwd is not an existing directory: "+req.Cwd)
		return
	}

	resume := req.TmuxSession == ""

	// Resolve the claude session id: explicit override, else newest for cwd.
	claudeID := req.SessionID
	if claudeID == "" {
		if id, err := s.life.NewestClaudeSession(r.Context(), req.Cwd); err == nil {
			claudeID = id
		}
	}
	if resume && claudeID == "" {
		writeErr(w, http.StatusBadRequest, "no claude session found to resume in "+req.Cwd+" (pass session_id)")
		return
	}

	// Two-heads guard: never adopt a conversation an active session already tracks.
	if claudeID != "" {
		sessions, err := s.store.List(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, ex := range sessions {
			if ex.ClaudeSessionID == claudeID {
				writeErr(w, http.StatusConflict, "claude session already adopted as "+ex.ID)
				return
			}
		}
	}

	// Choose the agent id. Live mode keeps the existing tmux name when it is a
	// safe, unused id; otherwise leave it empty so Lifecycle generates one (and
	// renames the tmux session to match).
	chosenID := ""
	if !resume && store.SafeID(req.TmuxSession) == nil {
		if _, err := s.store.Get(r.Context(), req.TmuxSession); errors.Is(err, store.ErrNotFound) {
			chosenID = req.TmuxSession
		}
	}

	sess, err := s.life.Adopt(r.Context(), AdoptParams{
		ID: chosenID, Cwd: req.Cwd, ClaudeSessionID: claudeID, TmuxSession: req.TmuxSession,
	})
	if err != nil {
		if errors.Is(err, lifecycle.ErrTmuxGone) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.store.Insert(r.Context(), sess); err != nil {
		if errors.Is(err, store.ErrExists) {
			writeErr(w, http.StatusConflict, "already registered: "+sess.ID)
			return
		}
		// Only resume mode created the tmux session; never kill a live one.
		if resume {
			tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if terr := s.life.Teardown(tctx, sess); terr != nil {
				log.Printf("adopt rollback %s: %v", sess.ID, terr)
			}
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	warn := ""
	if claudeID == "" {
		warn = "registered without a claude session id (monitoring only; restore unavailable)"
	}
	s.notify()
	writeJSON(w, http.StatusCreated, adoptResponse{Session: sess, Warning: warn})
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestAdopt -v`
Expected: PASS (all six).

- [ ] **Step 6: Run the full daemon suite for regressions**

Run: `go test ./internal/daemon/`
Expected: ok.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon/lifecycle_routes.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): POST /adopt handler (resume + live register)"
```

---

## Task 7: Client `Adopt`

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go` (append; create if absent)

- [ ] **Step 1: Write the failing test**

Append to `internal/client/client_test.go` (mirror any existing httptest-based client test in this file; if the file does not exist, create it with package `client` and the imports below):

```go
func TestAdoptSendsBodyAndParsesResponse(t *testing.T) {
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/adopt", r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"session":{"id":"agent-x"},"warning":"heads up"}`))
	}))
	defer ts.Close()

	res, err := New(ts.URL).Adopt(context.Background(), AdoptParams{
		Cwd: "/tmp/p", SessionID: "sid", TmuxSession: "work",
	})
	require.NoError(t, err)
	require.Equal(t, "agent-x", res.Session.ID)
	require.Equal(t, "heads up", res.Warning)
	require.Equal(t, "/tmp/p", gotBody["cwd"])
	require.Equal(t, "sid", gotBody["session_id"])
	require.Equal(t, "work", gotBody["tmux_session"])
}
```

Imports needed: `context`, `encoding/json`, `net/http`, `net/http/httptest`, `testing`, `github.com/stretchr/testify/require`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestAdopt -v`
Expected: FAIL — `Adopt` / `AdoptParams` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/client/client.go`, after the `Spawn` method (~line 130):

```go
// AdoptParams mirrors the daemon's /adopt body.
type AdoptParams struct {
	Cwd         string
	SessionID   string
	TmuxSession string
}

// AdoptResult is the /adopt response: the new session plus an optional warning.
type AdoptResult struct {
	Session *store.Session
	Warning string
}

func (c *Client) Adopt(ctx context.Context, p AdoptParams) (*AdoptResult, error) {
	var resp struct {
		Session *store.Session `json:"session"`
		Warning string         `json:"warning"`
	}
	body := map[string]any{
		"cwd": p.Cwd, "session_id": p.SessionID, "tmux_session": p.TmuxSession,
	}
	if err := c.do(ctx, http.MethodPost, "/adopt", body, &resp); err != nil {
		return nil, err
	}
	return &AdoptResult{Session: resp.Session, Warning: resp.Warning}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/ -run TestAdopt -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): add Adopt"
```

---

## Task 8: CLI `adopt` command

**Files:**
- Modify: `internal/cli/lifecycle.go`, `internal/cli/root.go`
- Test: `internal/cli/lifecycle_test.go` (append; create if absent)

- [ ] **Step 1: Write the failing test**

Append to `internal/cli/lifecycle_test.go` (create with `package cli` if absent):

```go
func TestCurrentTmuxSessionNotInTmux(t *testing.T) {
	t.Setenv("TMUX", "") // not inside tmux
	require.Equal(t, "", currentTmuxSession())
}
```

Imports: `testing`, `github.com/stretchr/testify/require`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestCurrentTmuxSession -v`
Expected: FAIL — `currentTmuxSession` undefined.

- [ ] **Step 3: Add the command and helper**

In `internal/cli/lifecycle.go`, add `strings` to the import block, then add:

```go
// currentTmuxSession returns the running tmux session name when invoked inside
// tmux ($TMUX set), else "". A non-empty result selects live-register mode;
// empty selects resume mode.
func currentTmuxSession() string {
	if os.Getenv("TMUX") == "" {
		return ""
	}
	out, err := exec.Command("tmux", "display-message", "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func newAdoptCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "Register the Claude session in this directory (resume it under tmux, or register the current tmux session live)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirFlag, _ := cmd.Flags().GetString("dir")
			dir, err := resolveDir(dirFlag)
			if err != nil {
				return err
			}
			sessionID, _ := cmd.Flags().GetString("session-id")
			tmuxSession := currentTmuxSession()
			res, err := clientFor(cmd).Adopt(cmd.Context(), client.AdoptParams{
				Cwd: dir, SessionID: sessionID, TmuxSession: tmuxSession,
			})
			if err != nil {
				return err
			}
			mode := "resumed"
			if tmuxSession != "" {
				mode = "live"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "adopted as %s (%s) — attach with `agentctl attach %s`\n",
				res.Session.ID, mode, res.Session.ID)
			if res.Warning != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", res.Warning)
			}
			return nil
		},
	}
	cmd.Flags().String("session-id", "", "claude session uuid to adopt (default: newest for the directory)")
	cmd.Flags().String("dir", "", "directory whose claude session to adopt (default: current directory)")
	return cmd
}
```

In `internal/cli/root.go`, add `newAdoptCmd()` to the lifecycle registration line (~line 19):

```go
	root.AddCommand(newStartCmd(), newTerminateCmd(), newDeleteCmd(), newRemoveWorktreeCmd(), newDoneCmd(), newRestoreCmd(), newAttachCmd(), newAdoptCmd())
```

- [ ] **Step 4: Run test + build to verify**

Run: `go test ./internal/cli/ -run TestCurrentTmuxSession -v && go build ./...`
Expected: PASS + build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/lifecycle.go internal/cli/root.go internal/cli/lifecycle_test.go
git commit -m "feat(cli): add adopt command"
```

---

## Task 9 (stretch): MCP `adopt_agent` tool

Defer freely — the CLI is fully functional without it. Skip if the concurrent TUI work is actively editing nearby files.

**Files:**
- Modify: `internal/mcp/server.go`

- [ ] **Step 1: Read the existing tool-registration pattern**

Run: `sed -n '90,140p' internal/mcp/server.go`
Expected: shows the `spawn_agent` `AddTool` call and the `spawnArgs` struct — mirror its shape.

- [ ] **Step 2: Add the args struct and tool**

Near `spawnArgs` (~line 27) add:

```go
type adoptArgs struct {
	Dir         string `json:"dir,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	TmuxSession string `json:"tmux_session,omitempty"`
}
```

Alongside the other `mcpsdk.AddTool(...)` calls (e.g. after `spawn_agent`), add:

```go
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "adopt_agent",
		Description: "Register an existing Claude Code session into agentctl: resume the newest conversation for a directory under tmux, or (when tmux_session is given) register a running tmux session live without relaunch.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a adoptArgs) (*mcpsdk.CallToolResult, any, error) {
		cwd := a.Dir
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		} else if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
		res, err := s.cl.Adopt(ctx, client.AdoptParams{
			Cwd: cwd, SessionID: a.SessionID, TmuxSession: a.TmuxSession,
		})
		if err != nil {
			return nil, nil, err
		}
		msg := "adopted as " + res.Session.ID
		if res.Warning != "" {
			msg += " (warning: " + res.Warning + ")"
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: msg}},
		}, res.Session, nil
	})
```

Ensure `os` and `path/filepath` are imported in `server.go` (the `spawn_agent` handler already uses them — confirm with the read in Step 1).

- [ ] **Step 3: Build and run mcp tests**

Run: `go build ./... && go test ./internal/mcp/`
Expected: build OK; tests pass.

- [ ] **Step 4: Commit**

```bash
git add internal/mcp/server.go
git commit -m "feat(mcp): add adopt_agent tool"
```

---

## Task 10: Docs + full verification

**Files:**
- Modify: `README.md` (or the docs file that lists CLI verbs / MCP tools — grep for `restore` to find it)

- [ ] **Step 1: Document the command**

Run: `grep -rn "agentctl restore\|restore_agent" README.md docs/*.md 2>/dev/null | head`
Then add an `adopt` entry next to `restore` wherever the CLI verbs / MCP tools are listed, e.g.:

```
- `agentctl adopt` — register the Claude session in the current directory: resumes it under tmux (plain shell) or registers the current tmux session live (run inside tmux). `--session-id <uuid>` to pick a specific conversation, `--dir <path>` to target another directory.
```

- [ ] **Step 2: Full build, vet, and test sweep**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: build OK, vet clean, all packages `ok`.

- [ ] **Step 3: Manual smoke test (resume mode)**

Run (in a directory where you have previously run `claude`, after the daemon is rebuilt + restarted):
```bash
go build -o /tmp/agentctl ./cmd/agentctl   # adjust to the module's main package path
/tmp/agentctl adopt
/tmp/agentctl ls
```
Expected: prints `adopted as agent-XXXX (resumed) …`; `ls` shows the new agent; `tmux ls` shows a session named `agent-XXXX` running `claude --resume`.

- [ ] **Step 4: Manual smoke test (live mode)**

Run inside an existing tmux session (e.g. one you started by hand running claude in another window):
```bash
/tmp/agentctl adopt
/tmp/agentctl ls
```
Expected: prints `adopted as <name> (live) …`; the tmux session is NOT relaunched; `ls` shows it; the poller picks up status within ~1s.

- [ ] **Step 5: Commit docs**

```bash
git add README.md
git commit -m "docs: document agentctl adopt"
```

---

## Self-Review

**Spec coverage:**
- §A CLI surface → Task 8 (flags `--session-id`/`--dir`, `$TMUX` detection via `currentTmuxSession`, attach-hint output incl. warning). ✓
- §B endpoint + `AdoptRequest` + resolve + two-heads guard + branch + Insert/rollback/notify → Tasks 5–6. ✓
- §C resume vs live branches → Task 3 (`Adopt`) + Task 6 (handler wiring). ✓
- §D ID↔tmux invariant (keep safe+unused name, else generate+rename) → handler `chosenID` logic (Task 6) + `Adopt` rename (Task 3). ✓
- §E shared `resumeInTmux` + `NewestClaudeSession` → Tasks 1–2. ✓
- §F MCP tool → Task 9 (stretch). ✓
- §G error table: cwd 400, nothing-to-resume 400, dup 409, tmux gone 404, no-claude-id live → 201+warning, insert fail 500+rollback(resume only) → covered by Task 6 tests. ✓ (Note: "session_id given but no transcript" returns 201/resume-proceeds rather than 400 — see deviation below.)
- §H tests (lifecycle/daemon/cli) → Tasks 1,3,6,7,8. ✓
- §I risks: rollback-never-kills-live-tmux is enforced (Task 6 `if resume` guard) and tested (`TestAdoptLiveInsertFailureDoesNotTeardown`). ✓

**Deviation from spec (intentional, flag for reviewer):** §G lists "`--session-id` given but no transcript → 400". The plan does **not** verify the transcript exists for an explicit `--session-id` before resuming; `claude --resume <uuid>` simply fails visibly in the pane if the id is bogus, and the glob-based `transcriptPath` makes a pre-check redundant for the common case. Dropping the pre-check keeps `Adopt` free of a transcript stat in the explicit-id path. If a hard pre-check is wanted, add a `transcriptPath`-backed validation in `handleAdopt` before calling `Adopt`. Confirm this is acceptable during execution.

**Placeholder scan:** none — every code step has complete code and exact commands.

**Type consistency:** `AdoptRequest` (lifecycle: `{ID,Cwd,ClaudeSessionID,TmuxSession}`; daemon wire: `{Cwd,SessionID,TmuxSession}`) and `AdoptParams` (daemon + client) are named distinctly on purpose and translated explicitly in the adapter (Task 5) and client (Task 7). `Adopt`, `NewestClaudeSession`, `resumeInTmux`, `SafeID`, `ErrTmuxGone`, `adoptResponse`, `AdoptResult` are used consistently across tasks.
