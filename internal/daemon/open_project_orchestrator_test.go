package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// TestOpenProjectDoesNotAutoSpawnOrchestrator locks the Phase 1 contract (spec
// 2026-09-25-project-entity-hierarchy.md D10): opening a project no longer
// auto-spawns an orch-<project> agent. This is the inverse of the pre-hierarchy
// baseline (git-blame: the removed TestOpenProjectAutoSpawnsOrchestrator) — a
// freshly opened project must be empty until an operator or parent agent spawns
// into it.
func TestOpenProjectDoesNotAutoSpawnOrchestrator(t *testing.T) {
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })
	fs := newFakeStore()
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl, projects: ps}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	dir := t.TempDir()
	body := strings.NewReader(`{"id":"` + dir + `","name":"Widget","path":"` + dir + `"}`)
	resp, err := http.Post(ts.URL+"/api/v1/projects/open", "application/json", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// D10: no orchestrator is minted, and the project opens with empty membership.
	require.Nil(t, fl.spawned, "opening a project must NOT auto-spawn an orchestrator")
	require.Empty(t, fl.restored, "opening a fresh project revives nothing")
	got, err := ps.Get(dir)
	require.NoError(t, err)
	require.Empty(t, got.Agents, "a freshly opened project has an empty agents list")
	require.Empty(t, got.Terminals, "a freshly opened project has an empty terminals list")
}

// TestSendMessageWakesIdleOrchestrator locks the auto-wakeup contract (independent
// of the removed open-path auto-spawn): a message delivered to an idle orchestrator
// wakes it via an injected notice (the send-message path wakes only parked —
// idle/waiting — recipients).
func TestSendMessageWakesIdleOrchestrator(t *testing.T) {
	mb, err := mailbox.New(t.TempDir())
	require.NoError(t, err)
	fs := newFakeStore()
	srv := &Server{store: fs, life: &fakeLife{}, mbox: mb, hub: newHub(), done: make(chan struct{})}
	fs.Insert(context.Background(), &store.Session{
		ID: "orch-1", Name: "orch-demo", TmuxSession: "orch-1", Status: store.StatusIdle,
	})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/v1/sessions/orch-1/messages", "application/json",
		strings.NewReader(`{"from":"worker-7","body":"phase done, please review"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	fl := srv.life.(*fakeLife)
	require.Contains(t, fl.lastInput, "New message from worker-7",
		"an idle orchestrator must be woken when a message arrives")
}
