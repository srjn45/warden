package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory store.Store for handler tests.
type fakeStore struct {
	data map[string]*store.Session
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[string]*store.Session{}} }

func (f *fakeStore) Insert(_ context.Context, s *store.Session) error {
	if _, ok := f.data[s.ID]; ok {
		return store.ErrExists
	}
	f.data[s.ID] = s
	return nil
}
func (f *fakeStore) Get(_ context.Context, id string) (*store.Session, error) {
	s, ok := f.data[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}
func (f *fakeStore) List(_ context.Context) ([]*store.Session, error) {
	out := make([]*store.Session, 0, len(f.data))
	for _, s := range f.data {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeStore) UpdateStatus(_ context.Context, id string, st store.Status) error {
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Status = st
	return nil
}
func (f *fakeStore) AppendEvent(_ context.Context, id string, ev store.Event) error {
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Events = append(s.Events, ev)
	return nil
}
func (f *fakeStore) UpdatePane(_ context.Context, id, ex string) error { return nil }
func (f *fakeStore) Archive(_ context.Context, id string) error        { delete(f.data, id); return nil }
func (f *fakeStore) Delete(_ context.Context, id string) error         { delete(f.data, id); return nil }
func (f *fakeStore) Ping(_ context.Context) error                      { return nil }
func (f *fakeStore) Close(_ context.Context) error                     { return nil }

func testServer(t *testing.T, fs *fakeStore) *httptest.Server {
	t.Helper()
	srv := &Server{store: fs}
	return httptest.NewServer(srv.router())
}

func TestHealthz(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestGetSessions(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	ts := testServer(t, fs)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body sessionsResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Sessions, 1)
	require.Equal(t, "A-1", body.Sessions[0].ID)
}

func TestGetSessionNotFound(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/sessions/missing")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPostEventUpdatesStatusAndAppends(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusSpawning}
	ts := testServer(t, fs)
	defer ts.Close()

	body, _ := json.Marshal(EventRequest{Session: "A-1", Type: "Notification", Detail: "Allow Bash?"})
	resp, err := http.Post(ts.URL+"/events", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := fs.data["A-1"]
	require.Equal(t, store.StatusWaitingForInput, got.Status)
	require.Len(t, got.Events, 1)
}

func TestPostEventUnknownSessionSoftOK(t *testing.T) {
	ts := testServer(t, newFakeStore())
	defer ts.Close()
	body, _ := json.Marshal(EventRequest{Session: "ghost", Type: "Stop"})
	resp, err := http.Post(ts.URL+"/events", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	// Hooks must fail soft: unknown session is accepted (204), never 5xx.
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}
