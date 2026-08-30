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

func TestOrchAgentName(t *testing.T) {
	cases := map[string]string{
		"warden":            "orch-warden",
		"My Cool Project":   "orch-my-cool-project",
		"  spaces  ":        "orch-spaces",
		"weird!!name??here": "orch-weird-name-here",
		"":                  "orch-project", // empty name falls back to a slug
		// A very long name is capped at 32 chars with no trailing hyphen.
		"this-is-an-extremely-long-project-name-indeed": "orch-this-is-an-extremely-long-p",
	}
	for in, want := range cases {
		got := orchAgentName(in)
		require.Equal(t, want, got, "orchAgentName(%q)", in)
		require.LessOrEqual(t, len(got), 32)
		require.NoError(t, store.ValidateName(got), "must be a valid store name: %q", got)
	}
}

// orchTestServer builds a daemon Server wired with a fake store + fake lifecycle so
// the guarantee-alive hook can be exercised directly.
func orchTestServer() (*Server, *fakeStore, *fakeLife) {
	fs := newFakeStore()
	fl := &fakeLife{}
	return &Server{store: fs, life: fl}, fs, fl
}

func TestGuaranteeOrchestratorSpawnsWhenMissing(t *testing.T) {
	srv, fs, fl := orchTestServer()
	dir := t.TempDir()
	p := projectstore.Project{ID: dir, Name: "Demo", Path: dir}

	srv.guaranteeOrchestrator(context.Background(), p)

	require.NotNil(t, fl.spawned, "an orchestrator must be spawned when none exists")
	require.Equal(t, "orch-demo", fl.spawned.Name)
	require.Equal(t, orchestratorRole, fl.spawned.Role)
	require.Equal(t, dir, fl.spawnedCwd, "orch launches in the project root")
	require.Equal(t, dir, fl.spawned.ProjectID, "orch is back-ref'd to its project")
	// It was persisted.
	got, err := fs.Get(context.Background(), fl.spawned.ID)
	require.NoError(t, err)
	require.Equal(t, "orch-demo", got.Name)
}

func TestGuaranteeOrchestratorNoOpWhenPresent(t *testing.T) {
	srv, fs, fl := orchTestServer()
	dir := t.TempDir()
	p := projectstore.Project{ID: dir, Name: "Demo", Path: dir}
	// A live orchestrator already exists for this project.
	fs.Insert(context.Background(), &store.Session{
		ID: "existing-orch", Name: "orch-demo", Status: store.StatusWorking, ProjectID: dir,
	})

	srv.guaranteeOrchestrator(context.Background(), p)

	require.Nil(t, fl.spawned, "no second orchestrator when one is already alive")
	require.Empty(t, fl.restored, "a live orch is not revived")
}

func TestGuaranteeOrchestratorRevivesDeadOrch(t *testing.T) {
	srv, fs, fl := orchTestServer()
	dir := t.TempDir()
	p := projectstore.Project{ID: dir, Name: "Demo", Path: dir}
	// A recorded-but-dead orchestrator for this project: revive it, don't spawn.
	fs.Insert(context.Background(), &store.Session{
		ID: "dead-orch", Name: "orch-demo", Status: store.StatusDone, ProjectID: dir, Hibernated: true,
	})

	srv.guaranteeOrchestrator(context.Background(), p)

	require.Nil(t, fl.spawned, "a dead orch is revived, not re-spawned")
	require.Equal(t, "dead-orch", fl.restored, "the existing orch is restored from its transcript")
	got, err := fs.Get(context.Background(), "dead-orch")
	require.NoError(t, err)
	require.Equal(t, store.StatusSpawning, got.Status)
	require.False(t, got.Hibernated, "revive clears the hibernated flag")
}

func TestGuaranteeOrchestratorSkipsWhenNoPath(t *testing.T) {
	srv, _, fl := orchTestServer()
	// A record-only project (e.g. a remote not yet cloned) has no launch dir.
	srv.guaranteeOrchestrator(context.Background(), projectstore.Project{ID: "https://x/y", Name: "y"})
	require.Nil(t, fl.spawned, "no orchestrator without a project path to launch in")
}

// TestOpenProjectAutoSpawnsOrchestrator drives the full HTTP open path and asserts
// the daemon guaranteed an orchestrator agent for the freshly opened project.
func TestOpenProjectAutoSpawnsOrchestrator(t *testing.T) {
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

	require.NotNil(t, fl.spawned, "opening a project must guarantee an orchestrator")
	require.Equal(t, "orch-widget", fl.spawned.Name)
	require.Equal(t, orchestratorRole, fl.spawned.Role)
}

// TestSendMessageWakesIdleOrchestrator locks the Phase 2 auto-wakeup contract: a
// message delivered to an idle orch-<project> agent wakes it via an injected notice
// (the send-message path wakes only parked — idle/waiting — recipients).
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
