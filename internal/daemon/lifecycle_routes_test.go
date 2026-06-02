package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeLife implements daemon.Lifecycle for route tests.
type fakeLife struct {
	mu             sync.Mutex
	spawned        *store.Session
	cleaned        string
	lastInput      string
	output         string
	classifyResult store.Type
	classified     string
	spawnedWorkdir string
	tornDown       string
	cleanupErr     error // when set, Cleanup fails with it
}

func (f *fakeLife) Spawn(_ context.Context, req SpawnRequest) (*store.Session, error) {
	f.spawnedWorkdir = req.Workdir
	promptMode := req.Prompt != "" && req.Type == ""
	id := req.Ticket
	if id == "" {
		if promptMode {
			id = "agent-test"
		} else {
			id = req.Type + "-auto"
		}
	}
	typ := store.Type("")
	if !promptMode {
		typ = store.NormalizeType(req.Type)
	}
	f.spawned = &store.Session{
		ID: id, Type: typ, Ticket: req.Ticket, Repo: req.Repo,
		Prompt: req.Prompt, Status: store.StatusSpawning,
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
func (f *fakeLife) Cleanup(_ context.Context, id string, force, hard bool) error {
	if f.cleanupErr != nil {
		return f.cleanupErr
	}
	f.cleaned = id
	return nil
}
func (f *fakeLife) Teardown(_ context.Context, sess *store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tornDown = sess.ID
	return nil
}
func (f *fakeLife) Input(_ context.Context, s, text string) error { f.lastInput = text; return nil }
func (f *fakeLife) Output(_ context.Context, s string, n int) (string, error) {
	return f.output, nil
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

func postCleanup(t *testing.T, ts *httptest.Server, id string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(CleanupRequest{ID: id})
	resp, err := http.Post(ts.URL+"/cleanup", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	return resp
}

func TestCleanupArchivesOnSuccess(t *testing.T) {
	fl := &fakeLife{}
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1"}
	ts := lifeServer(t, fs, fl)
	defer ts.Close()
	resp := postCleanup(t, ts, "A-1")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "A-1", fl.cleaned)
	_, exists := fs.data["A-1"]
	require.False(t, exists, "doc must be archived out of the active collection")
}

func TestCleanupGuardFailureIs409(t *testing.T) {
	fl := &fakeLife{cleanupErr: lifecycle.ErrDirtyWorktree}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	resp := postCleanup(t, ts, "A-1")
	require.Equal(t, http.StatusConflict, resp.StatusCode, "guard failures are a client conflict")
}

func TestCleanupOperationalFailureIs500(t *testing.T) {
	fl := &fakeLife{cleanupErr: errors.New("git worktree remove: boom")}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	resp := postCleanup(t, ts, "A-1")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, "tmux/git faults are server errors, not 409")
}

func TestCleanupNotFoundIs404(t *testing.T) {
	fl := &fakeLife{cleanupErr: store.ErrNotFound}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	resp := postCleanup(t, ts, "nope")
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestCleanupArchiveFailureIsSurfaced(t *testing.T) {
	fl := &fakeLife{}
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1"}
	fs.archiveErr = errors.New("mongo down")
	ts := lifeServer(t, fs, fl)
	defer ts.Close()
	resp := postCleanup(t, ts, "A-1")
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"a failed archive must not be reported as a clean success")
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

func TestPostSpawnNoTicketIsAllowed(t *testing.T) {
	fl := &fakeLife{}
	ts := lifeServer(t, newFakeStore(), fl)
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Type: "buildkite-debug", Repo: "/repo"})
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
	return &Server{store: fs, life: fl, hub: newHub(), workdir: "/tmp/agentctl-agents"}
}

func TestPostSpawnPromptMode(t *testing.T) {
	fl := &fakeLife{}
	srv := promptServer(t, newFakeStore(), fl)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(SpawnRequest{Prompt: "research SSE reconnection"})
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var got store.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.ID)
	require.Equal(t, store.Type(""), got.Type, "type empty at creation (classifying)")
	require.Equal(t, "research SSE reconnection", got.Prompt)
	require.Equal(t, "/tmp/agentctl-agents", fl.spawnedWorkdir, "server workdir passed to spawn")
}

func TestPostSpawnPromptThenClassifies(t *testing.T) {
	fs := newFakeStore()
	fl := &fakeLife{classifyResult: store.TypeAnalysis}
	srv := promptServer(t, fs, fl)
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(SpawnRequest{Prompt: "investigate flaky test"})
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

func TestPostSpawnRequiresPromptOrTypeRepo(t *testing.T) {
	srv := promptServer(t, newFakeStore(), &fakeLife{})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{}) // nothing
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
