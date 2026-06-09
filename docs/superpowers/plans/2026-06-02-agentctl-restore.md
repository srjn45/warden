# Session Restore Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `agentctl restore <id>` (CLI + daemon + MCP) that recreates a lost agent's tmux session in its original dir and resumes the same Claude conversation via `claude --resume <ClaudeSessionID>`.

**Architecture:** A new `lifecycle.Restore(ctx, *store.Session)` validates resume-only preconditions, recreates the tmux session, and relaunches claude with `--resume`. Detection of "lost" already exists (the poller marks dead sessions `orphaned`), so no poller work. Wired through the daemon `Lifecycle` interface → `POST /sessions/{id}/restore` → `internal/client` → CLI → MCP `restore_agent`.

**Tech Stack:** Go 1.26, stdlib + testify; chi router; MCP Go SDK.

**Design spec:** `docs/superpowers/specs/2026-06-02-agentctl-restore-design.md`

**Verified (empirical):** `claude --resume <uuid>` continues the same conversation and keeps the same `<uuid>.jsonl` transcript.

**Ordering:** lifecycle core first (Task 1), then daemon wiring (Task 2 — adds `Restore` to the `Lifecycle` interface, so `fakeLife` and the adapter must gain it together), then client+CLI (Task 3), then MCP+skill (Task 4). Build green at every commit.

---

### Task 1: `lifecycle.Restore` + `claudeResume` builder + sentinels

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing tests.** Append to `internal/lifecycle/lifecycle_test.go`:

```go
func TestRestoreRecreatesAndResumes(t *testing.T) {
	root := t.TempDir()
	workdir := t.TempDir()
	sid := "66666666-6666-4666-8666-666666666666"
	pdir := claudeProjectDir(root, workdir)
	require.NoError(t, os.MkdirAll(pdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pdir, sid+".jsonl"), []byte("{}"), 0o644))

	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t agent-r1": {Err: errStub("no session")}, // dead
	}}
	lc := New(fr)
	lc.ProjectsDir = root
	sess := &store.Session{ID: "agent-r1", TmuxSession: "agent-r1", Workdir: workdir, ClaudeSessionID: sid}

	require.NoError(t, lc.Restore(context.Background(), sess))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "new-session", "-d", "-s", "agent-r1", "-c", workdir})
	require.Contains(t, fr.calledArgs(), []string{"tmux", "send-keys", "-t", "agent-r1", claudeResume(sid, "agent-r1"), "Enter"})
}

func TestRestorePreconditionErrors(t *testing.T) {
	sid := "66666666-6666-4666-8666-666666666666"
	dead := func() *FakeRunner {
		return &FakeRunner{Responses: map[string]FakeResp{"tmux has-session -t a": {Err: errStub("dead")}}}
	}

	// no pinned session id (checked before any tmux call)
	require.ErrorIs(t, New(&FakeRunner{}).Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: t.TempDir()}), ErrNoSessionID)

	// already running: has-session succeeds (FakeRunner default = success = alive)
	require.ErrorIs(t, New(&FakeRunner{}).Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: t.TempDir(), ClaudeSessionID: sid}), ErrAlreadyRunning)

	// workdir gone
	require.ErrorIs(t, New(dead()).Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: "/no/such/dir", ClaudeSessionID: sid}), ErrWorkdirMissing)

	// no transcript: dead, workdir exists, empty ProjectsDir
	lc := New(dead())
	lc.ProjectsDir = t.TempDir()
	require.ErrorIs(t, lc.Restore(context.Background(),
		&store.Session{ID: "a", TmuxSession: "a", Workdir: t.TempDir(), ClaudeSessionID: sid}), ErrNoTranscript)
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/lifecycle/ -run TestRestore`
Expected: FAIL (compile: `claudeResume`, `ErrNoSessionID`, `ErrAlreadyRunning`, `ErrWorkdirMissing`, `ErrNoTranscript`, `Restore` undefined).

- [ ] **Step 3: Add the sentinels.** In `internal/lifecycle/lifecycle.go`, next to the existing `ErrDirtyWorktree`/`ErrUnpushedCommits` block, add:
```go
var (
	ErrAlreadyRunning = errors.New("agent is already running (use send/attach)")
	ErrNoSessionID    = errors.New("no pinned claude session id; re-spawn instead")
	ErrWorkdirMissing = errors.New("agent workdir is gone; re-spawn instead")
	ErrNoTranscript   = errors.New("no transcript to resume")
)
```

- [ ] **Step 4: Add the resume builder.** Just below `claudeLaunch`, add:
```go
// claudeResume builds the invocation that resumes an existing agent conversation
// by its pinned session id (continues the same transcript). --name re-applies the
// display label so the resumed session still reads as the agent id.
func claudeResume(sessionID, name string) string {
	return claudeCmd + " --resume " + sessionID + " --name " + shellQuoteArg(name)
}
```

- [ ] **Step 5: Add `Restore`.** Place it after `Spawn` (and near `transcriptPath`, which it reuses):
```go
// Restore recreates a lost agent's tmux session in its original workdir and
// resumes the same claude conversation (claude --resume). It is resume-only: it
// validates that the session is actually gone, has a pinned id, its workdir
// still exists, and its transcript is present — returning a specific sentinel
// otherwise — and never silently starts a fresh conversation.
func (l *Lifecycle) Restore(ctx context.Context, sess *store.Session) error {
	if sess.ClaudeSessionID == "" {
		return ErrNoSessionID
	}
	// Refuse if the tmux session is still alive (avoid a double-launch).
	if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", sess.TmuxSession); err == nil {
		return ErrAlreadyRunning
	}
	if fi, err := os.Stat(sess.Workdir); err != nil || !fi.IsDir() {
		return ErrWorkdirMissing
	}
	if l.transcriptPath(sess) == "" {
		return ErrNoTranscript
	}
	if out, err := l.run.Run(ctx, "", "tmux", "new-session", "-d", "-s", sess.ID, "-c", sess.Workdir); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	if out, err := l.run.Run(ctx, "", "tmux", "send-keys", "-t", sess.ID, claudeResume(sess.ClaudeSessionID, sess.ID), "Enter"); err != nil {
		return fmt.Errorf("tmux send-keys resume: %w: %s", err, out)
	}
	return nil
}
```
(`os`, `errors`, `fmt` are already imported.)

- [ ] **Step 6: Run to verify pass** — `go test ./internal/lifecycle/ -run TestRestore` → PASS, then full `go test ./internal/lifecycle/` → PASS. `go build ./... && go vet ./internal/lifecycle/`; `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**
```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): Restore — recreate tmux + claude --resume (resume-only preconditions)"
```

---

### Task 2: daemon wiring — interface, adapter, handler, route

**Files:**
- Modify: `internal/daemon/lifecycle_routes.go` (interface + handler + route)
- Modify: `internal/daemon/lifecycle_adapter.go`
- Test: `internal/daemon/lifecycle_routes_test.go`

- [ ] **Step 1: Write the failing tests.** In `internal/daemon/lifecycle_routes_test.go`: (a) add a `lifecycle` import if absent; (b) extend `fakeLife` with restore tracking; (c) add the handler tests.

Add fields to the `fakeLife` struct and a method:
```go
// (add to the fakeLife struct definition)
	restoreErr error
	restored   string

// (add as a new method on *fakeLife)
func (f *fakeLife) Restore(_ context.Context, sess *store.Session) error {
	f.restored = sess.ID
	return f.restoreErr
}
```

Add the tests:
```go
func TestHandleRestoreSucceeds(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusOrphaned})
	fl := &fakeLife{}
	srv := lifeServer(t, fs, fl)

	resp, err := http.Post(srv.URL+"/sessions/A-1/restore", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "A-1", fl.restored)
	got, _ := fs.Get(context.Background(), "A-1")
	require.Equal(t, store.StatusSpawning, got.Status)
}

func TestHandleRestoreMapsPreconditionErrors(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1"})
	fl := &fakeLife{restoreErr: lifecycle.ErrAlreadyRunning}
	srv := lifeServer(t, fs, fl)

	resp, err := http.Post(srv.URL+"/sessions/A-1/restore", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}
```
(`http`, `context`, `lifecycle`, `store` imports: add `lifecycle` and `net/http` if not already present in this test file.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/daemon/ -run TestHandleRestore`
Expected: FAIL — `fakeLife` does not implement `Restore` until the interface adds it / compile error on the route + handler.

- [ ] **Step 3: Add `Restore` to the daemon `Lifecycle` interface.** In `internal/daemon/lifecycle_routes.go`, add to the `Lifecycle interface`:
```go
	// Restore recreates and resumes a lost session from its stored doc.
	Restore(ctx context.Context, sess *store.Session) error
```

- [ ] **Step 4: Implement the adapter method.** In `internal/daemon/lifecycle_adapter.go`, add:
```go
func (a *lifecycleAdapter) Restore(ctx context.Context, sess *store.Session) error {
	return a.lc.Restore(ctx, sess)
}
```

- [ ] **Step 5: Register the route + handler.** In `registerLifecycleRoutes`, add:
```go
	r.Post("/sessions/{id}/restore", s.handleRestore)
```
And add the handler (mirrors `handleInput`, maps the sentinels):
```go
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.life.Restore(r.Context(), sess); err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrAlreadyRunning):
			writeErr(w, http.StatusConflict, err.Error())
		case errors.Is(err, lifecycle.ErrNoSessionID),
			errors.Is(err, lifecycle.ErrWorkdirMissing),
			errors.Is(err, lifecycle.ErrNoTranscript):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := s.store.UpdateStatus(r.Context(), id, store.StatusSpawning); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "restoring"})
}
```
(`lifecycle`, `errors`, `store`, `chi` are already imported in this file.)

- [ ] **Step 6: Run to verify pass** — `go test ./internal/daemon/ -run TestHandleRestore` → PASS, then `go test ./internal/daemon/` → PASS. `go build ./... && go vet ./internal/daemon/`; `gofmt -l` clean.

- [ ] **Step 7: Commit**
```bash
git add internal/daemon/lifecycle_routes.go internal/daemon/lifecycle_adapter.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): POST /sessions/{id}/restore wired to lifecycle.Restore"
```

---

### Task 3: client method + CLI `restore` command

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/cli/lifecycle.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Add the client method.** In `internal/client/client.go`, beside `Input`:
```go
func (c *Client) Restore(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/restore", nil, nil)
}
```

- [ ] **Step 2: Add the CLI command.** In `internal/cli/lifecycle.go`, beside `newDoneCmd`:
```go
func newRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <TICKET>",
		Short: "Recreate and resume a lost/orphaned agent (claude --resume)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).Restore(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "restoring %s\n", args[0])
			return nil
		},
	}
}
```

- [ ] **Step 3: Register it.** In `internal/cli/root.go`, add `newRestoreCmd()` to the lifecycle command line:
```go
	root.AddCommand(newStartCmd(), newDoneCmd(), newRestoreCmd(), newAttachCmd())
```

- [ ] **Step 4: Build + vet** — `go build ./... && go vet ./internal/client/ ./internal/cli/`; `gofmt -l internal/client/client.go internal/cli/lifecycle.go internal/cli/root.go` prints nothing. (The CLI/client have no unit tests today; the build is the check, and the route is covered by Task 2.)

- [ ] **Step 5: Commit**
```bash
git add internal/client/client.go internal/cli/lifecycle.go internal/cli/root.go
git commit -m "feat(cli): agentctl restore <id> + client.Restore"
```

---

### Task 4: MCP `restore_agent` tool + skill doc

**Files:**
- Modify: `internal/mcp/server.go`
- Test: `internal/mcp/server_test.go`
- Modify: `skills/agentctl/SKILL.md`

- [ ] **Step 1: Write the failing test.** Append to `internal/mcp/server_test.go`:
```go
func TestRestoreAgentTool(t *testing.T) {
	var hitPath string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/sessions/A-1/restore" {
			hitPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"restoring"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer daemon.Close()

	srv := NewServer(daemon.URL)
	ctx := context.Background()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTransport) }()

	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	defer session.Close()

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "restore_agent",
		Arguments: map[string]any{"ticket": "A-1"},
	})
	require.NoError(t, err)
	require.False(t, res.IsError, textOf(res))
	require.Equal(t, "/sessions/A-1/restore", hitPath)
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/mcp/ -run TestRestoreAgentTool`
Expected: FAIL (tool `restore_agent` not registered → `res.IsError` / path never hit).

- [ ] **Step 3: Register the tool.** In `internal/mcp/server.go`, after the `cleanup_agent` `AddTool` block (before `return s`), add:
```go
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "restore_agent",
		Description: "Recreate and resume a lost/orphaned agent's tmux + claude session (claude --resume). Use only when the agent's tmux session is gone (status orphaned).",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ticketArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Restore(ctx, a.Ticket); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("restoring " + a.Ticket), nil, nil
	})
```

- [ ] **Step 4: Run to verify pass** — `go test ./internal/mcp/ -run TestRestoreAgentTool` → PASS, then `go test ./internal/mcp/` → PASS. `go build ./...`; `gofmt -l internal/mcp/server.go internal/mcp/server_test.go` clean.

- [ ] **Step 5: Document in the skill.** In `skills/agentctl/SKILL.md`, add a row to the intent→tool table:
```
| restore / bring back a lost or orphaned agent | `restore_agent` (id) — only for sessions whose tmux is gone (status `orphaned`); resumes the same conversation |
```
And add a one-line guardrail near the others: "Restore is resume-only and for `orphaned`/dead sessions; if it reports the agent is still running, use `send`/attach instead."

- [ ] **Step 6: Commit**
```bash
git add internal/mcp/server.go internal/mcp/server_test.go skills/agentctl/SKILL.md
git commit -m "feat(mcp): restore_agent tool + skill doc"
```

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test -race ./...` — all green, no Docker.
- [ ] Live smoke: build, run the daemon (throwaway dirs) in a trusted cwd, spawn an agent, `tmux kill-session` it, confirm `agentctl ls` shows `orphaned`, run `agentctl restore <id>`, then confirm the tmux session is back and `claude --resume` reattached the conversation (a recall check), and status returns to working. Clean up the daemon/agent afterward.

Then proceed to **superpowers:finishing-a-development-branch**.
