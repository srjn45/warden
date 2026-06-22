package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeLife implements daemon.Lifecycle for route tests.
type fakeLife struct {
	mu             sync.Mutex
	spawned        *store.Session
	lastInput      string
	output         string
	outputErr      error
	classifyResult store.Type
	classified     string
	spawnedCwd     string
	tornDown       string
	restoreErr     error
	restored       string
	terminated     string
	removedWT      string
	removeWTErr    error
	newestClaude   string
	newestErr      error
	adoptResult    *store.Session
	adoptErr       error
	adoptParams    AdoptParams
	lastKey        string
	commitCalls    int
	committedDir   string
	committedMsg   string
	commitResult   bool
	commitErr      error
	lwRepo         string
	lwActive       int
	lwArchived     int
	lwResult       []lifecycle.WorktreeListing
	lwErr          error
	pruneRepo      string
	pruneOpts      lifecycle.PruneOpts
	pruneResult    []lifecycle.PruneResult
	pruneErr       error
}

func (f *fakeLife) Spawn(_ context.Context, req SpawnRequest) (*store.Session, error) {
	f.spawnedCwd = req.Cwd
	freeMode := req.Type == ""
	id := req.Ticket
	if id == "" {
		if freeMode {
			id = "agent-test"
		} else {
			id = req.Type + "-auto"
		}
	}
	typ := store.Type("")
	if !freeMode {
		typ = store.NormalizeType(req.Type)
	}
	f.spawned = &store.Session{
		ID: id, Type: typ, Ticket: req.Ticket, Repo: req.Repo,
		Prompt: req.Prompt, Status: store.StatusSpawning,
		PermissionMode: req.PermissionMode,
	}
	return f.spawned, nil
}

func (f *fakeLife) Classify(_ context.Context, prompt string) (store.Type, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.classified = prompt
	if f.classifyResult == "" {
		return store.TypeOther, nil
	}
	return f.classifyResult, nil
}
func (f *fakeLife) getClassified() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.classified
}
func (f *fakeLife) Terminate(_ context.Context, tmux string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = tmux
	return nil
}
func (f *fakeLife) RemoveWorktree(_ context.Context, sess *store.Session, force, deleteAdoptedBranch bool) error {
	f.removedWT = sess.ID
	return f.removeWTErr
}
func (f *fakeLife) ListWorktrees(_ context.Context, repo string, active, archived []*store.Session) ([]lifecycle.WorktreeListing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lwRepo = repo
	f.lwActive, f.lwArchived = len(active), len(archived)
	return f.lwResult, f.lwErr
}
func (f *fakeLife) PruneWorktrees(_ context.Context, repo string, opts lifecycle.PruneOpts) ([]lifecycle.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruneRepo = repo
	f.pruneOpts = opts
	return f.pruneResult, f.pruneErr
}
func (f *fakeLife) Teardown(_ context.Context, sess *store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tornDown = sess.ID
	return nil
}
func (f *fakeLife) Restore(_ context.Context, sess *store.Session) error {
	f.restored = sess.ID
	return f.restoreErr
}
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
func (f *fakeLife) Input(_ context.Context, s, text string) error { f.lastInput = text; return nil }
func (f *fakeLife) Output(_ context.Context, s string, n int) (string, error) {
	return f.output, f.outputErr
}
func (f *fakeLife) SendKeys(_ context.Context, s, key string) error { f.lastKey = key; return nil }

func (f *fakeLife) TranscriptPath(sess *store.Session) string         { return "" }
func (f *fakeLife) GitBranch(ctx context.Context, dir string) string  { return "" }
func (f *fakeLife) GitNumstat(ctx context.Context, dir string) string { return "" }
func (f *fakeLife) MemoryPressure(_ context.Context) (pressure.Level, error) {
	return pressure.Normal, nil
}

func (f *fakeLife) SpawnJob(_ context.Context, req lifecycle.JobSpawnRequest) (*store.Session, error) {
	id := req.PipelineID + "-" + req.JobID
	branch := ""
	wt := ""
	workdir := req.Repo
	if req.Worktree {
		branch = id
		wt = ".worktrees/" + id
		workdir = req.Repo + "/" + wt
	}
	return &store.Session{
		ID: id, TmuxSession: id, Type: req.Type, Repo: req.Repo,
		Status: store.StatusSpawning, PipelineID: req.PipelineID, JobID: req.JobID,
		Branch: branch, Worktree: wt, Workdir: workdir,
	}, nil
}

func (f *fakeLife) CommitWorktree(_ context.Context, dir, message string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCalls++
	f.committedDir, f.committedMsg = dir, message
	return f.commitResult, f.commitErr
}

func lifeServer(t *testing.T, fs *fakeStore, fl *fakeLife) *httptest.Server {
	t.Helper()
	srv := &Server{store: fs, life: fl}
	return httptest.NewServer(srv.router())
}

func TestPostSpawn(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "A-1", fl.spawned.ID)
	require.Equal(t, store.TypeDevelopment, fl.spawned.Type)
}

func TestPostSpawnRejectsUnsafeTicket(t *testing.T) {
	// A ticket flows straight into the session id, which becomes a filesystem
	// path component (the prompt file) and a tmux session name. It must be
	// validated BEFORE Spawn runs — otherwise "../../x" escapes the prompts dir
	// and ":"/"/" break tmux targeting. safeID gates this at Insert, but Spawn
	// runs first, so the handler must reject up front.
	for _, bad := range []string{"../../etc/passwd", "a/b", "..", "work:1"} {
		fl := &fakeLife{}
		ts := lifeServer(t, newFakeStore(), fl)
		body, _ := json.Marshal(SpawnRequest{Prompt: "do x", Cwd: "/tmp", Ticket: bad})
		resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "ticket %q must be rejected", bad)
		require.Nil(t, fl.spawned, "Spawn must not run for unsafe ticket %q", bad)
		resp.Body.Close()
		ts.Close()
	}
}

func TestPostSpawnRollsBackWhenInsertFails(t *testing.T) {
	fl := &fakeLife{}
	fs := newFakeStore()
	fs.insertErr = errors.New("mongo down")
	ts := lifeServer(t, fs, fl)
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	require.Equal(t, "A-1", fl.tornDown, "tmux/worktree must be torn down when the doc can't be persisted")
}

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

func TestHandleDeleteHardClearsMailbox(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusDone})
	mb, _ := mailbox.New(t.TempDir())
	mb.Append(mailbox.Message{To: "A-1", From: "B-2", Body: "hi"})
	srv := &Server{store: fs, life: &fakeLife{}, mbox: mb, hub: newHub(), done: make(chan struct{})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/sessions/A-1/delete", "application/json", strings.NewReader(`{"hard":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	msgs, err := mb.Messages("A-1")
	require.NoError(t, err)
	require.Empty(t, msgs, "hard delete should clear the agent's inbox")
}

func TestHandleDeleteSoftKeepsMailbox(t *testing.T) {
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{ID: "A-1", TmuxSession: "A-1", Status: store.StatusDone})
	mb, _ := mailbox.New(t.TempDir())
	mb.Append(mailbox.Message{To: "A-1", From: "B-2", Body: "hi"})
	srv := &Server{store: fs, life: &fakeLife{}, mbox: mb, hub: newHub(), done: make(chan struct{})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	// Default (archive) delete must NOT touch the inbox.
	resp, err := http.Post(ts.URL+"/sessions/A-1/delete", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	msgs, err := mb.Messages("A-1")
	require.NoError(t, err)
	require.Len(t, msgs, 1, "soft/archive delete must leave the inbox intact")
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

func TestHandlePruneMissingRepo(t *testing.T) {
	srv := lifeServer(t, newFakeStore(), &fakeLife{})
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/prune", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a missing repo is an HTTP error")
}

func TestHandlePruneReturnsSkipsNotErrors(t *testing.T) {
	fl := &fakeLife{pruneResult: []lifecycle.PruneResult{
		{Path: ".worktrees/a", Action: lifecycle.PruneSkip, State: "dirty", Reason: "dirty (use --force)"},
		{Path: ".worktrees/b", Action: lifecycle.PruneRemove, BranchDeleted: true},
	}}
	srv := lifeServer(t, newFakeStore(), fl)
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/prune", "application/json", strings.NewReader(`{"repo":"/repo","force":true,"include_archived":true}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "dirty/unpushed skips are results, not HTTP errors")
	var body struct {
		Results []lifecycle.PruneResult `json:"results"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Results, 2)
	require.Equal(t, lifecycle.PruneSkip, body.Results[0].Action)
	require.Equal(t, "/repo", fl.pruneRepo)
	require.True(t, fl.pruneOpts.Force)
	require.True(t, fl.pruneOpts.IncludeArchived)
}

func TestHandleListWorktrees(t *testing.T) {
	fl := &fakeLife{lwResult: []lifecycle.WorktreeListing{
		{Path: ".worktrees/a", Owner: "a", Lifecycle: "live", State: "clean"},
	}}
	srv := lifeServer(t, newFakeStore(), fl)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/worktrees?repo=/repo")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body struct {
		Worktrees []lifecycle.WorktreeListing `json:"worktrees"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Worktrees, 1)
	require.Equal(t, "/repo", fl.lwRepo)
}

func TestHandleListWorktreesMissingRepo(t *testing.T) {
	srv := lifeServer(t, newFakeStore(), &fakeLife{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/worktrees")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPostSpawnRequiresTypeAndRepo(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Ticket: "A-1"}) // no type, no repo
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPostSpawnRejectsUnknownType(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "bogus", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "unknown types are rejected, not collapsed to other")
}

func TestPostSpawnRejectsInvalidPermissionMode(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	// A permission_mode carrying shell syntax must be rejected at the boundary,
	// not concatenated into the claude launch line typed into the tmux pane.
	body, _ := json.Marshal(SpawnRequest{Type: "debug-ci", Repo: "/repo", PermissionMode: "auto; rm -rf ~"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "an unknown permission mode is rejected")
}

func TestPostSpawnNoTicketIsAllowed(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "debug-ci", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotEmpty(t, fl.spawned.ID)
}

func TestPostInput(t *testing.T) {
	fl := &fakeLife{}
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", TmuxSession: "A-1"}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()
	body, _ := json.Marshal(InputRequest{Text: "hello agent"})
	resp, err := http.Post(ts.URL+"/sessions/A-1/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "hello agent", fl.lastInput)
}

func TestGetOutput(t *testing.T) {
	fl := &fakeLife{output: "pane text"}
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", TmuxSession: "A-1"}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/sessions/A-1/output")
	require.NoError(t, err)
	var out OutputResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "pane text", out.Output)
}

// An existing session whose tmux pane can't be captured (still spawning, or
// just terminated) must yield an empty 200 — not a 500 — so the live grid's
// thumbnail polling stays quiet.
func TestGetOutputUncapturablePaneReturnsEmpty200(t *testing.T) {
	fl := &fakeLife{outputErr: errors.New("tmux capture-pane: exit status 1: can't find session")}
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", TmuxSession: "A-1"}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/sessions/A-1/output")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out OutputResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, "", out.Output)
}

func TestPostInputNotFound(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	body, _ := json.Marshal(InputRequest{Text: "hi"})
	resp, err := http.Post(ts.URL+"/sessions/ghost/input", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestGetOutputNotFound(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/sessions/ghost/output")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPostSpawnDuplicateConflict(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1"}
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestSpawnNotifiesSubscribers(t *testing.T) {
	fl := &fakeLife{}
	srv := &Server{store: newFakeStore(), life: fl, hub: newHub()}
	ch, unsub := srv.hub.subscribe()
	defer unsub()

	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("spawn did not notify SSE subscribers")
	}
}

func promptServer(t *testing.T, fs *fakeStore, fl *fakeLife) *Server {
	t.Helper()
	return &Server{store: fs, life: fl, hub: newHub()}
}

func TestPostSpawnPromptMode(t *testing.T) {
	fl := &fakeLife{}
	srv := promptServer(t, newFakeStore(), fl)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	dir := t.TempDir()
	body, _ := json.Marshal(SpawnRequest{Prompt: "research SSE reconnection", Cwd: dir})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.ID)
	require.Equal(t, store.Type(""), got.Type, "type empty at creation (classifying)")
	require.Equal(t, "research SSE reconnection", got.Prompt)
	require.Equal(t, dir, fl.spawnedCwd, "caller cwd passed to spawn")
}

func TestPostSpawnPromptThenClassifies(t *testing.T) {
	fs := newFakeStore()
	fl := &fakeLife{classifyResult: store.TypeAnalysis}
	srv := promptServer(t, fs, fl)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(SpawnRequest{Prompt: "investigate flaky test", Cwd: t.TempDir()})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))

	// Background classification updates the type shortly after.
	require.Eventually(t, func() bool {
		s, err := fs.Get(context.Background(), created.ID)
		return err == nil && s.Type == store.TypeAnalysis
	}, 2*time.Second, 10*time.Millisecond)
	require.Equal(t, "investigate flaky test", fl.getClassified())
}

func TestPostSpawnRejectsEmptyRequest(t *testing.T) {
	srv := promptServer(t, newFakeStore(), &fakeLife{})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{}) // no type, no repo, no cwd → no launch dir
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

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

func TestPostSpawnForwardsCwd(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	dir := t.TempDir()
	body, _ := json.Marshal(SpawnRequest{Prompt: "do X", Cwd: dir})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, dir, fl.spawnedCwd, "cwd is forwarded to the lifecycle")
}

func TestPostSpawnRejectsMissingCwd(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Prompt: "do X", Cwd: "/no/such/dir/xyz123"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "a cwd that isn't an existing dir is rejected")
}

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

func TestAdoptLiveHappyPath(t *testing.T) {
	dir := t.TempDir()
	fl := &fakeLife{newestClaude: "66666666-6666-4666-8666-666666666666"}
	ts := adoptServer(fl, newFakeStore())
	defer ts.Close()

	body, _ := json.Marshal(AdoptRequest{Cwd: dir, TmuxSession: "mysess"})
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got adoptResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotNil(t, got.Session)
	require.Equal(t, "mysess", fl.adoptParams.TmuxSession, "live mode forwards the tmux session")
	require.Empty(t, got.Warning, "claude id resolved → no warning")
}

func TestAdoptLiveNoClaudeIDWarns(t *testing.T) {
	dir := t.TempDir()
	fl := &fakeLife{newestErr: errors.New("none")} // no claude id resolvable
	ts := adoptServer(fl, newFakeStore())
	defer ts.Close()

	body, _ := json.Marshal(AdoptRequest{Cwd: dir, TmuxSession: "mysess2"})
	resp, err := http.Post(ts.URL+"/adopt", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got adoptResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.Warning, "live register without a claude id must warn")
	require.Empty(t, fl.adoptParams.ClaudeSessionID, "claude id stays empty")
}

func TestPostSpawnInteractiveNoPrompt(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	dir := t.TempDir() // cwd must be an existing directory
	body, _ := json.Marshal(SpawnRequest{Cwd: dir})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "empty prompt + cwd is a valid interactive spawn")
	require.Equal(t, dir, fl.spawnedCwd)
	require.Equal(t, "", fl.spawned.Prompt)
}

func TestPostSpawnWithPermissionMode(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo", PermissionMode: "acceptEdits"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Equal(t, "acceptEdits", fl.spawned.PermissionMode)
}
func TestPatchPermissionMode(t *testing.T) {
	fs := newFakeStore()
	fs.data["abc123"] = &store.Session{ID: "abc123", PermissionMode: "auto"}
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"permission_mode": "acceptEdits"})
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/sessions/abc123/permission-mode", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify the store was updated
	sess, _ := fs.Get(context.Background(), "abc123")
	require.Equal(t, "acceptEdits", sess.PermissionMode)
}

func TestPatchPermissionModeInvalidMode(t *testing.T) {
	fs := newFakeStore()
	fs.data["abc123"] = &store.Session{ID: "abc123", PermissionMode: "auto"}
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"permission_mode": "invalid"})
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/sessions/abc123/permission-mode", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPatchPermissionModeNotFound(t *testing.T) {
	fs := newFakeStore()
	ts := lifeServer(t, fs, &fakeLife{})
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"permission_mode": "acceptEdits"})
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/sessions/missing/permission-mode", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
