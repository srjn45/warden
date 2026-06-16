package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory store.Store for handler tests.
type fakeStore struct {
	mu         sync.Mutex
	data       map[string]*store.Session
	insertErr  error // when set, Insert fails with it (no doc stored)
	archiveErr error // when set, Archive fails with it (doc left in place)
}

func newFakeStore() *fakeStore { return &fakeStore{data: map[string]*store.Session{}} }

func (f *fakeStore) Insert(_ context.Context, s *store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.insertErr != nil {
		return f.insertErr
	}
	if _, ok := f.data[s.ID]; ok {
		return store.ErrExists
	}
	f.data[s.ID] = s
	return nil
}
func (f *fakeStore) Get(_ context.Context, id string) (*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}
func (f *fakeStore) GetByNameOrID(_ context.Context, nameOrID string) (*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// First check for name match
	for _, s := range f.data {
		if s.Name != "" && s.Name == nameOrID {
			return s, nil
		}
	}
	// Fall back to ID lookup
	s, ok := f.data[nameOrID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}
func (f *fakeStore) List(_ context.Context) ([]*store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*store.Session, 0, len(f.data))
	for _, s := range f.data {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeStore) UpdateStatus(_ context.Context, id string, st store.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Status = st
	return nil
}
func (f *fakeStore) UpdateStatusIf(_ context.Context, id string, expected, next store.Status) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok || s.Status != expected {
		return false, nil
	}
	s.Status = next
	return true, nil
}
func (f *fakeStore) FinalizeExit(_ context.Context, id string, expected, next store.Status, code int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok || s.Status != expected {
		return false, nil
	}
	// NB: does not append the exit event; sufficient for handler-level tests.
	s.Status = next
	c := code
	s.ExitCode = &c
	return true, nil
}
func (f *fakeStore) UpdateType(_ context.Context, id string, t store.Type) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Type = t
	return nil
}
func (f *fakeStore) UpdateSubject(_ context.Context, id, subject string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Subject = subject
	return nil
}
func (f *fakeStore) AppendEvent(_ context.Context, id string, ev store.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Events = append(s.Events, ev)
	return nil
}
func (f *fakeStore) AppendEventStatus(_ context.Context, id string, ev store.Event, status store.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.Events = append(s.Events, ev)
	if status != "" {
		s.Status = status
	}
	return nil
}
func (f *fakeStore) UpdatePane(_ context.Context, id, ex string) error { return nil }
func (f *fakeStore) SetRestart(_ context.Context, id string, count int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.data[id]; s != nil {
		s.RestartCount = count
		t := at
		s.LastRestartAt = &t
	}
	return nil
}
func (f *fakeStore) UpdateContext(_ context.Context, id string, tokens int, state string) error {
	return nil
}
func (f *fakeStore) StampCompact(_ context.Context, id string) error { return nil }
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
func (f *fakeStore) SetRateLimit(_ context.Context, id string, restoreAt time.Time, retryCount int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	now := time.Now().UTC()
	if s.RateLimitedAt == nil {
		s.RateLimitedAt = &now
	}
	s.RateLimitRestoreAt = &restoreAt
	s.RateLimitRetryCount = retryCount
	return nil
}
func (f *fakeStore) ClearRateLimit(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[id]
	if !ok {
		return store.ErrNotFound
	}
	s.RateLimitedAt = nil
	s.RateLimitRestoreAt = nil
	s.RateLimitRetryCount = 0
	return nil
}
func (f *fakeStore) Archive(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.archiveErr != nil {
		return f.archiveErr
	}
	delete(f.data, id)
	return nil
}
func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, id)
	return nil
}
func (f *fakeStore) Ping(_ context.Context) error                                     { return nil }
func (f *fakeStore) Close(_ context.Context) error                                    { return nil }
func (f *fakeStore) UpdateAutoApprove(_ context.Context, _ string, _ bool) error      { return nil }
func (f *fakeStore) UpdatePermissionMode(_ context.Context, _ string, _ string) error { return nil }

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

func TestPostEventSessionEndMarksDone(t *testing.T) {
	fs := newFakeStore()
	fs.data["A-1"] = &store.Session{ID: "A-1", Status: store.StatusWorking}
	ts := testServer(t, fs)
	defer ts.Close()

	body, _ := json.Marshal(EventRequest{Session: "A-1", Type: "SessionEnd"})
	resp, err := http.Post(ts.URL+"/events", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, store.StatusDone, fs.data["A-1"].Status, "SessionEnd is terminal")
}

func TestStatusForHook(t *testing.T) {
	require.Equal(t, store.StatusWorking, statusForHook("SessionStart"))
	require.Equal(t, store.StatusWaitingForInput, statusForHook("Notification"))
	require.Equal(t, store.StatusIdle, statusForHook("Stop"))
	require.Equal(t, store.StatusDone, statusForHook("SessionEnd"))
	require.Equal(t, store.Status(""), statusForHook("SubagentStop"), "non-status events log only")
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
