# Layered Teardown Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Replace the monolithic `done`/cleanup (kill tmux + remove worktree + archive, in one call) with three composable operations — **terminate** (kill tmux, keep record+worktree), **delete** (clear the record), **remove-worktree** (always explicit) — where worktree removal is never implicit.

**Architecture:** Split `lifecycle.Cleanup` into `Terminate` + `RemoveWorktree`; expose three daemon endpoints + client methods + CLI verbs + MCP tools; `delete` is store-only (archive/hard). `done` becomes a convenience (terminate+delete, never worktree). Worktree removal refuses if the agent is still alive or has uncommitted/unpushed work (unless `--force`) and clears the record's worktree fields after.

**Tech Stack:** Go 1.26, stdlib + testify; chi; MCP Go SDK.

**Design spec:** `docs/superpowers/specs/2026-06-02-agentctl-teardown-design.md`

**Ordering (each commit builds):** store method first (Task 1), lifecycle new methods keeping `Cleanup` (Task 2), daemon switch to the new methods + drop `/cleanup` (Task 3), client+cli (Task 4, keeps `client.Cleanup` for mcp), mcp+skill (Task 5), then delete the now-dead `lifecycle.Cleanup` + `client.Cleanup` + old tests (Task 6).

**Worktree:** all work happens in `/Users/srajan.pathak/workspace/personal/agentctl-teardown` (branch `teardown`); a parallel session uses the main checkout — never touch it.

---

### Task 1: `store.ClearWorktree`

**Files:** `internal/store/store.go`, `internal/store/file.go`, `internal/store/file_test.go`, `internal/daemon/api_test.go` (fakeStore).

- [ ] **Step 1: Write the failing test.** Append to `internal/store/file_test.go`:
```go
func TestFileClearWorktree(t *testing.T) {
	ctx := context.Background()
	st := newFileStore(t)
	s := sample() // has Worktree + Branch set
	require.NoError(t, st.Insert(ctx, s))
	require.NoError(t, st.ClearWorktree(ctx, s.ID))
	got, err := st.Get(ctx, s.ID)
	require.NoError(t, err)
	require.Empty(t, got.Worktree, "worktree cleared")
	require.Empty(t, got.Branch, "branch cleared")
	require.ErrorIs(t, st.ClearWorktree(ctx, "nope"), ErrNotFound)
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/store/ -run TestFileClearWorktree` → FAIL (undefined `ClearWorktree`).

- [ ] **Step 3: Add to the `Store` interface** in `internal/store/store.go` (after `UpdatePane`):
```go
	// ClearWorktree blanks the Worktree and Branch fields (after the worktree is
	// removed from disk), so the record no longer points at a gone worktree.
	ClearWorktree(ctx context.Context, id string) error
```

- [ ] **Step 4: Implement on FileStore** in `internal/store/file.go` (next to `UpdatePane`):
```go
func (fs *FileStore) ClearWorktree(ctx context.Context, id string) error {
	return fs.mutate(id, func(s *Session) { s.Worktree = ""; s.Branch = "" })
}
```

- [ ] **Step 5: Add to the daemon `fakeStore`** in `internal/daemon/api_test.go` (mirror `UpdateSubject`'s map-mutation style):
```go
func (f *fakeStore) ClearWorktree(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Worktree = ""
	s.Branch = ""
	return nil
}
```
(If `fakeStore` has no mutex/field names matching, follow the exact pattern of the existing `UpdateSubject` method in that file — same lock + map lookup + `store.ErrNotFound`.)

- [ ] **Step 6: Verify** — `go test ./internal/store/ -run TestFileClearWorktree` → PASS; `go build ./... && go vet ./internal/store/ ./internal/daemon/`; `gofmt -w` the four files; `gofmt -l` them → empty.

- [ ] **Step 7: Commit**
```bash
git add internal/store/store.go internal/store/file.go internal/store/file_test.go internal/daemon/api_test.go
git commit -m "feat(store): ClearWorktree — blank worktree/branch on the record"
```

---

### Task 2: `lifecycle.Terminate` + `lifecycle.RemoveWorktree` (keep `Cleanup`)

**Files:** `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`.

- [ ] **Step 1: Write the failing tests.** Append to `internal/lifecycle/lifecycle_test.go`:
```go
func TestTerminateKillsTmuxOnly(t *testing.T) {
	fr := &FakeRunner{}
	require.NoError(t, New(fr).Terminate(context.Background(), "A-1"))
	require.Contains(t, fr.calledArgs(), []string{"tmux", "kill-session", "-t", "A-1"})
	for _, a := range fr.calledArgs() {
		require.NotEqual(t, "git", a[0], "terminate touches no git")
	}
}

func TestRemoveWorktreeRefusesIfAlive(t *testing.T) {
	// has-session succeeds (FakeRunner default) → agent alive → refuse.
	fr := &FakeRunner{}
	err := New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), false)
	require.ErrorIs(t, err, ErrWorktreeAgentAlive)
	for _, a := range fr.calledArgs() {
		require.NotEqual(t, "git", a[0], "must not touch git while the agent is alive")
	}
}

func TestRemoveWorktreeGuardsDirty(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t A-1":                              {Err: errStub("dead")},
		"git -C /repo/.worktrees/A-1 status --porcelain":       {Out: " M f.go\n"},
	}}
	require.ErrorIs(t, New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), false), ErrDirtyWorktree)
}

func TestRemoveWorktreeForceProceeds(t *testing.T) {
	fr := &FakeRunner{} // has-session would say alive, but force skips the checks
	require.NoError(t, New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), true))
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", "--force", ".worktrees/A-1"})
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "branch", "-D", "A-1"})
}

func TestRemoveWorktreeCleanProceeds(t *testing.T) {
	fr := &FakeRunner{Responses: map[string]FakeResp{
		"tmux has-session -t A-1":                        {Err: errStub("dead")},
		"git -C /repo/.worktrees/A-1 status --porcelain": {Out: ""},
		"git -C /repo/.worktrees/A-1 log @{u}.. --oneline": {Out: ""},
	}}
	require.NoError(t, New(fr).RemoveWorktree(context.Background(), cleanupInput("A-1"), false))
	require.Contains(t, fr.calledArgs(), []string{"git", "-C", "/repo", "worktree", "remove", ".worktrees/A-1"})
}

func TestRemoveWorktreeNoWorktreeErrors(t *testing.T) {
	tgt := CleanupTarget{ID: "x", TmuxSession: "x"} // no Worktree
	require.ErrorIs(t, New(&FakeRunner{}).RemoveWorktree(context.Background(), tgt, false), ErrNoWorktree)
}
```
(`cleanupInput(id)` already exists in the test file and returns a `CleanupTarget{ID:id, Repo:"/repo", Worktree:".worktrees/"+id, Branch:id, TmuxSession:id}` — reuse it.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/lifecycle/ -run 'TestTerminate|TestRemoveWorktree'` → FAIL (undefined `Terminate`/`RemoveWorktree`/`ErrWorktreeAgentAlive`/`ErrNoWorktree`).

- [ ] **Step 3: Add sentinels** in `internal/lifecycle/lifecycle.go` (next to `ErrDirtyWorktree`):
```go
	ErrNoWorktree         = errors.New("session has no worktree")
	ErrWorktreeAgentAlive = errors.New("agent is still running; terminate it before removing its worktree")
```
(Add inside the existing `var ( … )` block that holds `ErrDirtyWorktree`/`ErrUnpushedCommits`, or a new `var (...)` block.)

- [ ] **Step 4: Add `Terminate` and `RemoveWorktree`** (place near `Cleanup`):
```go
// Terminate kills the agent's tmux session (which kills the claude process
// inside it). It is idempotent: killing an already-gone session is not an error.
// It touches no git and leaves the record and any worktree intact.
func (l *Lifecycle) Terminate(ctx context.Context, tmuxSession string) error {
	// tmux kill-session errors if the session is already gone; that is the
	// desired end state, so the error is ignored.
	_, _ = l.run.Run(ctx, "", "tmux", "kill-session", "-t", tmuxSession)
	return nil
}

// RemoveWorktree removes the session's git worktree and branch. It is always an
// explicit, separate step. Unless force is set, it refuses when the agent's tmux
// session is still alive (terminate first) and when the worktree has uncommitted
// or unpushed work (the guard). Sessions with no worktree return ErrNoWorktree.
func (l *Lifecycle) RemoveWorktree(ctx context.Context, t CleanupTarget, force bool) error {
	if t.Worktree == "" {
		return ErrNoWorktree
	}
	if !force {
		if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", t.TmuxSession); err == nil {
			return ErrWorktreeAgentAlive
		}
		if err := l.guard(ctx, t); err != nil {
			return err
		}
	}
	removeArgs := []string{"-C", t.Repo, "worktree", "remove", t.Worktree}
	if force {
		removeArgs = []string{"-C", t.Repo, "worktree", "remove", "--force", t.Worktree}
	}
	if out, err := l.run.Run(ctx, "", "git", removeArgs...); err != nil {
		return fmt.Errorf("git worktree remove: %w: %s", err, out)
	}
	if t.Branch != "" {
		if out, err := l.run.Run(ctx, "", "git", "-C", t.Repo, "branch", "-D", t.Branch); err != nil {
			return fmt.Errorf("git branch -D: %w: %s", err, out)
		}
	}
	return nil
}
```
(Leave the existing `Cleanup` in place for now — Task 6 removes it once nothing calls it.)

- [ ] **Step 5: Verify** — `go test ./internal/lifecycle/ -run 'TestTerminate|TestRemoveWorktree'` → PASS, then full `go test ./internal/lifecycle/` → PASS (old `TestCleanup*` still pass). `go build ./... && go vet ./internal/lifecycle/`; `gofmt -l` clean.

- [ ] **Step 6: Commit**
```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): Terminate + RemoveWorktree (alive/guard checks; explicit)"
```

---

### Task 3: daemon — three endpoints, interface, adapter (drop `/cleanup`)

**Files:** `internal/daemon/api.go` (interface), `internal/daemon/lifecycle_adapter.go`, `internal/daemon/lifecycle_routes.go`, `internal/daemon/lifecycle_routes_test.go`.

- [ ] **Step 1: Write the failing tests.** In `internal/daemon/lifecycle_routes_test.go`: extend `fakeLife` and replace the cleanup handler tests.

Add to the `fakeLife` struct + methods (and remove its `Cleanup` method):
```go
	// fields
	terminated    string
	removedWT     string
	removeWTErr   error
// methods
func (f *fakeLife) Terminate(_ context.Context, tmux string) error { f.terminated = tmux; return nil }
func (f *fakeLife) RemoveWorktree(_ context.Context, sess *store.Session, force bool) error {
	f.removedWT = sess.ID
	return f.removeWTErr
}
```
(Delete the existing `func (f *fakeLife) Cleanup(...)` method.)

Replace any existing `TestHandleCleanup*` tests with:
```go
func TestHandleTerminate(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusWorking})
	fl := &fakeLife{}
	srv := lifeServer(t, fs, fl)
	resp, err := http.Post(srv.URL+"/sessions/A-1/terminate", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "A-1", fl.terminated)
	got, _ := fs.Get(context.Background(), "A-1")
	require.Equal(t, store.StatusDone, got.Status, "terminate marks the record done")
}

func TestHandleDeleteArchivesByDefault(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusDone})
	srv := lifeServer(t, fs, &fakeLife{})
	resp, err := http.Post(srv.URL+"/sessions/A-1/delete", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, err = fs.Get(context.Background(), "A-1")
	require.ErrorIs(t, err, store.ErrNotFound, "record removed from active store")
}

func TestHandleRemoveWorktreeGuardConflict(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Worktree: ".worktrees/A-1", Repo: "/repo", Status: store.StatusDone})
	fl := &fakeLife{removeWTErr: lifecycle.ErrWorktreeAgentAlive}
	srv := lifeServer(t, fs, fl)
	resp, err := http.Post(srv.URL+"/sessions/A-1/remove-worktree", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestHandleRemoveWorktreeClearsRecord(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Worktree: ".worktrees/A-1", Branch: "A-1", Repo: "/repo", Status: store.StatusDone})
	fl := &fakeLife{}
	srv := lifeServer(t, fs, fl)
	resp, err := http.Post(srv.URL+"/sessions/A-1/remove-worktree", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "A-1", fl.removedWT)
	got, _ := fs.Get(context.Background(), "A-1")
	require.Empty(t, got.Worktree, "record's worktree cleared after removal")
}
```
(Add `strings` to the test imports if needed.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/daemon/ -run 'TestHandleTerminate|TestHandleDelete|TestHandleRemoveWorktree'` → FAIL (fakeLife no longer matches interface / handlers+routes missing).

- [ ] **Step 3: Update the daemon `Lifecycle` interface** in `internal/daemon/api.go` — remove the `Cleanup` line, add:
```go
	// Terminate kills the agent's tmux session (keeps record + worktree).
	Terminate(ctx context.Context, tmuxSession string) error
	// RemoveWorktree removes the session's git worktree + branch (explicit).
	RemoveWorktree(ctx context.Context, sess *store.Session, force bool) error
```
Also delete the `CleanupRequest` struct in `api.go` (no longer used) and add:
```go
type deleteRequest struct {
	Hard bool `json:"hard"`
}
type removeWorktreeRequest struct {
	Force bool `json:"force"`
}
```

- [ ] **Step 4: Update the adapter** in `internal/daemon/lifecycle_adapter.go` — replace the `Cleanup` method with `Terminate`/`RemoveWorktree`, and recompose `Teardown` from them:
```go
func (a *lifecycleAdapter) Terminate(ctx context.Context, tmuxSession string) error {
	return a.lc.Terminate(ctx, tmuxSession)
}

func (a *lifecycleAdapter) RemoveWorktree(ctx context.Context, sess *store.Session, force bool) error {
	return a.lc.RemoveWorktree(ctx, lifecycle.CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, TmuxSession: sess.TmuxSession,
	}, force)
}

// Teardown force-cleans the resources Spawn created (spawn rollback): kill tmux,
// then force-remove the worktree if there is one.
func (a *lifecycleAdapter) Teardown(ctx context.Context, sess *store.Session) error {
	_ = a.lc.Terminate(ctx, sess.TmuxSession)
	if sess.Worktree == "" {
		return nil
	}
	return a.lc.RemoveWorktree(ctx, lifecycle.CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, TmuxSession: sess.TmuxSession,
	}, true)
}
```
(Delete the old `func (a *lifecycleAdapter) Cleanup(...)`.)

- [ ] **Step 5: Replace routes + handlers** in `internal/daemon/lifecycle_routes.go`. In `registerLifecycleRoutes`, remove `r.Post("/cleanup", s.handleCleanup)` and add:
```go
	r.Post("/sessions/{id}/terminate", s.handleTerminate)
	r.Post("/sessions/{id}/delete", s.handleDelete)
	r.Post("/sessions/{id}/remove-worktree", s.handleRemoveWorktree)
```
Delete `handleCleanup` and add (a small helper at the top of the new handlers loads the session — mirror `handleRestore`'s load):
```go
// liveStatus reports whether the stored status implies the agent may still be
// running (so delete can warn instead of silently orphaning a live tmux).
func liveStatus(s store.Status) bool {
	switch s {
	case store.StatusSpawning, store.StatusWorking, store.StatusWaitingForInput, store.StatusIdle:
		return true
	}
	return false
}

func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
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
	if err := s.life.Terminate(r.Context(), sess.TmuxSession); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.store.UpdateStatus(r.Context(), id, store.StatusDone); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "terminated"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req deleteRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body ok → archive
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	warn := ""
	if liveStatus(sess.Status) {
		warn = "agent may still be running (status " + string(sess.Status) + "); terminate it first or it becomes untracked"
	}
	var derr error
	if req.Hard {
		derr = s.store.Delete(r.Context(), id)
	} else {
		derr = s.store.Archive(r.Context(), id)
	}
	if derr != nil && !errors.Is(derr, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, derr.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "warning": warn})
}

func (s *Server) handleRemoveWorktree(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req removeWorktreeRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.life.RemoveWorktree(r.Context(), sess, req.Force); err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrNoWorktree):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, lifecycle.ErrWorktreeAgentAlive),
			errors.Is(err, lifecycle.ErrDirtyWorktree),
			errors.Is(err, lifecycle.ErrUnpushedCommits):
			writeErr(w, http.StatusConflict, err.Error())
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	if err := s.store.ClearWorktree(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "worktree removed"})
}
```

- [ ] **Step 6: Verify** — `go test ./internal/daemon/` → PASS; `go build ./... && go vet ./internal/daemon/`; `gofmt -w` the changed files; `gofmt -l` → empty. (`lifecycle.Cleanup` is now unused but still compiles — removed in Task 6.)

- [ ] **Step 7: Commit**
```bash
git add internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/daemon/lifecycle_routes.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): terminate/delete/remove-worktree endpoints (drop /cleanup)"
```

---

### Task 4: client methods + CLI verbs

**Files:** `internal/client/client.go`, `internal/cli/lifecycle.go`, `internal/cli/root.go`.

- [ ] **Step 1: Add client methods** in `internal/client/client.go` (keep the existing `Cleanup` for now — mcp still uses it until Task 5):
```go
func (c *Client) Terminate(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/terminate", nil, nil)
}

func (c *Client) Delete(ctx context.Context, id string, hard bool) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/delete", map[string]bool{"hard": hard}, nil)
}

func (c *Client) RemoveWorktree(ctx context.Context, id string, force bool) error {
	return c.do(ctx, http.MethodPost, "/sessions/"+id+"/remove-worktree", map[string]bool{"force": force}, nil)
}
```

- [ ] **Step 2: Rewrite the CLI teardown commands** in `internal/cli/lifecycle.go`. Replace `newDoneCmd` and add the three verbs:
```go
func newTerminateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "terminate <TICKET>",
		Short: "Stop an agent: kill its tmux+claude session (keeps the record and worktree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).Terminate(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "terminated %s\n", args[0])
			return nil
		},
	}
}

func newDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <TICKET>",
		Short: "Clear an agent's stored record (archives by default; --hard to purge)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			if err := clientFor(cmd).Delete(cmd.Context(), args[0], hard); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("hard", false, "permanently purge the record instead of archiving")
	return cmd
}

func newRemoveWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove-worktree <TICKET>",
		Short: "Remove an agent's git worktree + branch (always asks; --force overrides guards)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			force, _ := cmd.Flags().GetBool("force")
			if !yes {
				fmt.Fprintf(cmd.OutOrStdout(), "Remove the git worktree and branch for %s? This cannot be undone. [y/N]: ", args[0])
				var ans string
				_, _ = fmt.Fscanln(cmd.InOrStdin(), &ans)
				if ans != "y" && ans != "Y" {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}
			if err := clientFor(cmd).RemoveWorktree(cmd.Context(), args[0], force); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed worktree for %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("force", false, "override the alive/uncommitted/unpushed guards")
	cmd.Flags().Bool("yes", false, "skip the confirmation prompt")
	return cmd
}

func newDoneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "done <TICKET>",
		Short: "Terminate an agent and clear its record (does NOT remove the worktree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hard, _ := cmd.Flags().GetBool("hard")
			c := clientFor(cmd)
			if err := c.Terminate(cmd.Context(), args[0]); err != nil {
				return err
			}
			if err := c.Delete(cmd.Context(), args[0], hard); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "done %s (terminated + record cleared; worktree, if any, kept — use remove-worktree)\n", args[0])
			return nil
		},
	}
	cmd.Flags().Bool("hard", false, "purge the record instead of archiving")
	return cmd
}
```

- [ ] **Step 3: Register the new commands** in `internal/cli/root.go` — replace the lifecycle line:
```go
	root.AddCommand(newStartCmd(), newTerminateCmd(), newDeleteCmd(), newRemoveWorktreeCmd(), newDoneCmd(), newRestoreCmd(), newAttachCmd())
```

- [ ] **Step 4: Verify** — `go build ./... && go vet ./internal/client/ ./internal/cli/`; `gofmt -w` + `gofmt -l` the three files → empty. (`agentctl --help` shows terminate/delete/remove-worktree/done/restore.)

- [ ] **Step 5: Commit**
```bash
git add internal/client/client.go internal/cli/lifecycle.go internal/cli/root.go
git commit -m "feat(cli): terminate/delete/remove-worktree verbs; done = terminate+delete"
```

---

### Task 5: MCP tools + skill

**Files:** `internal/mcp/server.go`, `internal/mcp/server_test.go`, `skills/agentctl/SKILL.md`.

- [ ] **Step 1: Write the failing test.** Append to `internal/mcp/server_test.go`:
```go
func TestTeardownTools(t *testing.T) {
	hits := map[string]bool{}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path] = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer daemon.Close()
	srv := NewServer(daemon.URL)
	ctx := context.Background()
	ct, st := mcpsdk.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, st) }()
	cl := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := cl.Connect(ctx, ct, nil)
	require.NoError(t, err)
	defer session.Close()

	for _, tc := range []struct{ tool, path string }{
		{"terminate_agent", "/sessions/A-1/terminate"},
		{"delete_agent", "/sessions/A-1/delete"},
		{"remove_worktree", "/sessions/A-1/remove-worktree"},
	} {
		res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: tc.tool, Arguments: map[string]any{"ticket": "A-1"}})
		require.NoError(t, err)
		require.False(t, res.IsError, textOf(res))
		require.True(t, hits[tc.path], "expected %s to hit %s", tc.tool, tc.path)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/mcp/ -run TestTeardownTools` → FAIL.

- [ ] **Step 3: Replace `cleanup_agent`** in `internal/mcp/server.go`. Delete the `cleanup_agent` `AddTool` block and the `cleanupArgs` type; add a `forceArgs` type near the other arg structs:
```go
type forceArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Force  bool   `json:"force,omitempty" jsonschema:"override the alive/uncommitted/unpushed guards"`
}
type deleteToolArgs struct {
	Ticket string `json:"ticket" jsonschema:"the agent's ticket / session id"`
	Hard   bool   `json:"hard,omitempty" jsonschema:"permanently purge instead of archiving"`
}
```
Then register three tools (where `cleanup_agent` was):
```go
	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "terminate_agent",
		Description: "Stop an agent: kill its tmux+claude session. Keeps the record and worktree (reversible via restore_agent). The default 'stop this agent' action.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a ticketArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Terminate(ctx, a.Ticket); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("terminated " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "delete_agent",
		Description: "Clear an agent's stored record (archives by default; hard=true purges). Does not touch tmux or the worktree.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a deleteToolArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.Delete(ctx, a.Ticket, a.Hard); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("deleted " + a.Ticket), nil, nil
	})

	mcpsdk.AddTool(s.mcp, &mcpsdk.Tool{
		Name:        "remove_worktree",
		Description: "Remove an agent's git worktree + branch. DESTRUCTIVE and always requires explicit user confirmation first. Refuses if the agent is still running (terminate first) or has uncommitted/unpushed work, unless force=true.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, a forceArgs) (*mcpsdk.CallToolResult, any, error) {
		if err := s.cl.RemoveWorktree(ctx, a.Ticket, a.Force); err != nil {
			return textResult("error: " + err.Error()), nil, nil
		}
		return textResult("removed worktree for " + a.Ticket), nil, nil
	})
```

- [ ] **Step 4: Verify** — `go test ./internal/mcp/ -run TestTeardownTools` → PASS, then `go test ./internal/mcp/` → PASS; `go build ./...`; `gofmt -w`+`gofmt -l` the two go files → empty.

- [ ] **Step 5: Update the skill** `skills/agentctl/SKILL.md`:
- Replace the intent→tool row `| terminate / kill / clean up <id> | cleanup_agent — see Guardrails |` with three rows:
```
| stop / terminate / kill <id> | `terminate_agent` (id) — kills tmux+claude, keeps record+worktree; reversible via `restore_agent` |
| clear / delete an agent's record | `delete_agent` (id, hard?) — archives by default |
| remove an agent's worktree | `remove_worktree` (id, force?) — DESTRUCTIVE; **confirm with the user first**; terminate the agent first |
```
- In Guardrails, replace the `cleanup_agent` bullet(s) with: "**`remove_worktree` is destructive — always confirm with the user first** (name the agent + that its worktree and branch will be deleted), and it refuses while the agent is still running or has uncommitted/unpushed work unless `force:true`. `terminate_agent` is the safe default for 'stop this agent' (it's reversible via `restore_agent`). Never bulk-terminate/-delete/-remove without explicit per-agent or explicit 'all' confirmation."
- Update the description-line trigger and the CLI-fallback line: replace any `cleanup_agent` / `agentctl done` mention so they reflect `terminate`/`delete`/`remove-worktree` (the `agentctl done` fallback now = terminate+delete). Update the "Kill the idle ones" example to use `terminate_agent` per confirmed id.

- [ ] **Step 6: Commit**
```bash
git add internal/mcp/server.go internal/mcp/server_test.go skills/agentctl/SKILL.md
git commit -m "feat(mcp): terminate_agent/delete_agent/remove_worktree (replace cleanup_agent); skill update"
```

---

### Task 6: remove the now-dead `Cleanup` paths

**Files:** `internal/lifecycle/lifecycle.go`, `internal/lifecycle/lifecycle_test.go`, `internal/client/client.go`.

- [ ] **Step 1: Confirm nothing references them** — run:
```bash
grep -rn '\.Cleanup(' internal/ --include='*.go' | grep -v _test.go
grep -rn 'func (c \*Client) Cleanup\|func (l \*Lifecycle) Cleanup' internal/
```
Expect the only remaining references to be the definitions themselves and the `lifecycle_test.go` `TestCleanup*` tests.

- [ ] **Step 2: Delete the dead code:**
  - In `internal/lifecycle/lifecycle.go`: delete the `Cleanup` method.
  - In `internal/lifecycle/lifecycle_test.go`: delete `TestCleanupGuardAbortsOnUncommitted`, `TestCleanupGuardAbortsOnUnpushed`, `TestCleanupForceProceedsAndKillsTmuxFirst`, `TestCleanupCleanProceeds`, `TestCleanupNoWorktreeOnlyKillsTmux`, `TestCleanupGuardAbortsWhenNoUpstream`. **Keep** `cleanupInput` (now used by the `RemoveWorktree` tests from Task 2) and `guard`/`worktreeAbs` (used by `RemoveWorktree`).
  - In `internal/client/client.go`: delete the `Cleanup` method.

- [ ] **Step 3: Verify** — `go build ./... && go vet ./... && go test -race ./...` → all green. `gofmt -l .` shows no new issues from these files.

- [ ] **Step 4: Commit**
```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go internal/client/client.go
git commit -m "chore: remove dead Cleanup paths (superseded by terminate/delete/remove-worktree)"
```

---

## Final verification (after all tasks)

- [ ] `go build ./... && go vet ./... && go test -race ./...` — all green.
- [ ] `grep -rn 'cleanup_agent\|handleCleanup\|/cleanup' internal/ skills/` — empty (no stale cleanup surface).
- [ ] Live smoke (in this worktree, trusted cwd): spawn a worktree agent → `agentctl terminate <id>` (tmux gone via `tmux has-session`; record present with status `done`; worktree dir still on disk) → `agentctl restore <id>` resumes it → `terminate` again → `agentctl remove-worktree <id>` (answer `y`; worktree+branch gone; `agentctl status` shows empty worktree) → `agentctl delete <id>` (record archived). Clean up.

Then proceed to **superpowers:finishing-a-development-branch**.
