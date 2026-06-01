package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeLife implements daemon.Lifecycle for route tests.
type fakeLife struct {
	spawned   *store.Session
	cleaned   string
	lastInput string
	output    string
}

func (f *fakeLife) Spawn(_ context.Context, req SpawnRequest) (*store.Session, error) {
	id := req.Ticket
	if id == "" {
		id = req.Type + "-auto"
	}
	f.spawned = &store.Session{
		ID: id, Type: store.NormalizeType(req.Type), Ticket: req.Ticket,
		Repo: req.Repo, Status: store.StatusSpawning,
	}
	return f.spawned, nil
}
func (f *fakeLife) Cleanup(_ context.Context, id string, force, hard bool) error {
	f.cleaned = id
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

func TestPostSpawnRequiresTypeAndRepo(t *testing.T) {
	ts := lifeServer(t, newFakeStore(), &fakeLife{})
	defer ts.Close()
	body, _ := json.Marshal(SpawnRequest{Ticket: "A-1"}) // no type, no repo
	resp, err := http.Post(ts.URL+"/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
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
