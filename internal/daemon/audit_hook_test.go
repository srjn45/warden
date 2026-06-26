package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/audit"
	"github.com/stretchr/testify/require"
)

// A successful spawn through the handler should leave one audit record naming
// the new agent, with who (Actor) stamped from the request origin.
func TestHandleSpawnRecordsAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, audit: audit.NewWriter(path)}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-1", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	events, err := audit.Read(path, audit.Filter{})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, audit.ActionSpawn, events[0].Action)
	require.Equal(t, "A-1", events[0].Target)
	require.NotEmpty(t, events[0].Actor, "request origin is recorded as the actor")
	require.Equal(t, "/repo", events[0].Detail["repo"])
}

// recordAudit must be a no-op (no panic, no file) when auditing is unconfigured.
func TestRecordAuditNilWriter(t *testing.T) {
	srv := &Server{store: newFakeStore(), life: &fakeLife{}}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(SpawnRequest{Type: "development", Ticket: "A-2", Repo: "/repo"})
	resp, err := http.Post(ts.URL+"/api/v1/spawn", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "spawn still succeeds with auditing off")
}
