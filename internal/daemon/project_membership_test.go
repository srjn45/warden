package daemon

import (
	"context"
	"encoding/json"
	"github.com/srjn45/warden/internal/agentstore"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/terminalstore"
	"github.com/stretchr/testify/require"
)

func TestProjectMembershipHelpers(t *testing.T) {
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })

	proj, err := ps.OpenProject("/projects/alpha", "alpha", "/projects/alpha")
	require.NoError(t, err)

	closedProj, err := ps.OpenProject("/projects/closed", "closed", "/projects/closed")
	require.NoError(t, err)
	_, err = ps.CloseProject(closedProj.ID)
	require.NoError(t, err)

	s := &Server{projects: ps}

	// 1. Explicit ProjectID wins over path match
	sessExplicit := &agentstore.Agent{
		ID:        "agent-explicit",
		Repo:      "/projects/alpha",
		ProjectID: "custom-id",
	}
	require.Equal(t, "custom-id", s.resolveProjectID(sessExplicit))

	// 2. Path match resolves open project
	sessPathMatch := &agentstore.Agent{
		ID:   "agent-path",
		Repo: "/projects/alpha",
	}
	require.Equal(t, proj.ID, s.resolveProjectID(sessPathMatch))

	// 3. Closed project is reopened (not skipped) and keeps its fields
	sessClosed := &agentstore.Agent{
		ID:   "agent-closed",
		Repo: "/projects/closed",
	}
	require.Equal(t, closedProj.ID, s.resolveProjectID(sessClosed))
	reopened, err := ps.Get(closedProj.ID)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, projectstore.NormalizeStatus(reopened.Status))
	require.Equal(t, "closed", reopened.Name)

	// 3b. Unknown directory is auto-registered open
	sessNew := &agentstore.Agent{ID: "agent-new", Repo: "/projects/brand-new"}
	require.Equal(t, "/projects/brand-new", s.resolveProjectID(sessNew))

	// 4. Stamp fills empty, leaves set alone
	s.stampProjectMembership(sessPathMatch)
	require.Equal(t, proj.ID, sessPathMatch.ProjectID)

	s.stampProjectMembership(sessExplicit)
	require.Equal(t, "custom-id", sessExplicit.ProjectID)

	// 5. Add agent membership
	s.addProjectMembership(sessPathMatch)
	updated, err := ps.Get(proj.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-path"}, updated.Agents)
	require.Empty(t, updated.Terminals)

	// Idempotent add
	s.addProjectMembership(sessPathMatch)
	updated, err = ps.Get(proj.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-path"}, updated.Agents)

	// 7. Remove membership
	s.removeProjectMembership(sessPathMatch)
	updated, err = ps.Get(proj.ID)
	require.NoError(t, err)
	require.Empty(t, updated.Agents)
	require.Empty(t, updated.Terminals)
}

func TestSpawnStampsProjectAndUpdatesMembership(t *testing.T) {
	ctx := context.Background()
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })

	projectDir := t.TempDir()
	proj, err := ps.OpenProject(projectDir, "myproject", projectDir)
	require.NoError(t, err)

	fs := newFakeStore()
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl, projects: ps}

	// 1. Spawn agent with explicit project_id
	bodyExplicit := `{"prompt":"do work","cwd":"/tmp","project_id":"` + proj.ID + `"}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/spawn", strings.NewReader(bodyExplicit))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var respExplicit oapi.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &respExplicit))
	require.Equal(t, proj.ID, respExplicit.ProjectID)

	// Verify project store membership
	p, err := ps.Get(proj.ID)
	require.NoError(t, err)
	require.Contains(t, p.Agents, respExplicit.ID)

	// 2. Spawn agent with path-matching cwd (empty project_id)
	bodyMatch := `{"ticket":"agent-match","prompt":"sub work","cwd":"` + projectDir + `"}`
	reqMatch, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/spawn", strings.NewReader(bodyMatch))
	reqMatch.Host = "127.0.0.1"
	reqMatch.Header.Set("Content-Type", "application/json")
	wMatch := httptest.NewRecorder()
	srv.router().ServeHTTP(wMatch, reqMatch)
	require.Equal(t, http.StatusCreated, wMatch.Code)

	var respMatch oapi.Session
	require.NoError(t, json.Unmarshal(wMatch.Body.Bytes(), &respMatch))
	require.Equal(t, proj.ID, respMatch.ProjectID)

	p, err = ps.Get(proj.ID)
	require.NoError(t, err)
	require.Contains(t, p.Agents, respMatch.ID)

	// 3. Delete session and verify removal from membership
	delReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/sessions/"+respExplicit.ID+"/delete", strings.NewReader(`{}`))
	delReq.Host = "127.0.0.1"
	delReq.Header.Set("Content-Type", "application/json")
	delW := httptest.NewRecorder()
	srv.router().ServeHTTP(delW, delReq)
	require.Equal(t, http.StatusOK, delW.Code)

	p, err = ps.Get(proj.ID)
	require.NoError(t, err)
	require.NotContains(t, p.Agents, respExplicit.ID)
	require.Contains(t, p.Agents, respMatch.ID)
}

func spawnAgentAt(t *testing.T, srv *Server, cwd, ticket string) oapi.Session {
	t.Helper()
	body := `{"ticket":"` + ticket + `","prompt":"work","cwd":"` + cwd + `"}`
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/spawn", strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.router().ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var out oapi.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	return out
}

func newAutoRegServer(t *testing.T) (*Server, *projectstore.Store) {
	t.Helper()
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })
	return &Server{store: newFakeStore(), life: &fakeLife{}, projects: ps}, ps
}

func TestSpawnAutoRegistersUnknownDirectory(t *testing.T) {
	srv, ps := newAutoRegServer(t)
	dir := t.TempDir()

	resp := spawnAgentAt(t, srv, dir, "auto-1")
	require.Equal(t, dir, resp.ProjectID)

	p, err := ps.Get(dir)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, projectstore.NormalizeStatus(p.Status))
	require.Equal(t, dir, p.Path)
	require.Equal(t, filepath.Base(dir), p.Name)
	require.Contains(t, p.Agents, resp.ID)
}

func TestSpawnReopensClosedProjectPreservingFields(t *testing.T) {
	srv, ps := newAutoRegServer(t)
	dir := t.TempDir()
	_, err := ps.OpenProject(dir, "custom-name", dir)
	require.NoError(t, err)
	_, err = ps.AddAgentToProject(dir, "old-agent")
	require.NoError(t, err)
	_, err = ps.AddPlanToProject(dir, "plan-1")
	require.NoError(t, err)
	_, err = ps.CloseProject(dir)
	require.NoError(t, err)

	resp := spawnAgentAt(t, srv, dir, "reopen-1")
	require.Equal(t, dir, resp.ProjectID)

	p, err := ps.Get(dir)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, projectstore.NormalizeStatus(p.Status))
	require.Equal(t, "custom-name", p.Name)
	require.Equal(t, []string{"plan-1"}, p.Plans)
	require.ElementsMatch(t, []string{"old-agent", resp.ID}, p.Agents)
}

func TestSpawnWorktreeBindsToParentRepo(t *testing.T) {
	srv, ps := newAutoRegServer(t)
	root := t.TempDir()
	wt := filepath.Join(root, ".worktrees", "foo")
	require.NoError(t, os.MkdirAll(wt, 0o755))

	resp := spawnAgentAt(t, srv, wt, "wt-1")
	require.Equal(t, root, resp.ProjectID)

	projs, err := ps.List()
	require.NoError(t, err)
	require.Len(t, projs, 1)
	require.Equal(t, root, projs[0].ID)
	require.Contains(t, projs[0].Agents, resp.ID)
}

func TestEnsureOpenProjectIdempotent(t *testing.T) {
	srv, ps := newAutoRegServer(t)
	dir := t.TempDir()

	first, err := srv.ensureOpenProject(dir)
	require.NoError(t, err)
	require.NotNil(t, first)
	_, err = ps.AddAgentToProject(dir, "a1")
	require.NoError(t, err)
	second, err := srv.ensureOpenProject(dir)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	projs, err := ps.List()
	require.NoError(t, err)
	require.Len(t, projs, 1)
	require.Equal(t, []string{"a1"}, projs[0].Agents)

	// nil store / empty dir are no-ops.
	p, err := (&Server{}).ensureOpenProject(dir)
	require.NoError(t, err)
	require.Nil(t, p)
	p, err = srv.ensureOpenProject("")
	require.NoError(t, err)
	require.Nil(t, p)
}

func TestSpawnTerminalAutoRegistersProject(t *testing.T) {
	srv, ps := newAutoRegServer(t)
	ts, err := terminalstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = ts.Close() })
	srv.terminals = ts
	dir := t.TempDir()

	_, err = srv.spawnTerminal(context.Background(), SpawnRequest{Cwd: dir, Ticket: "term-auto"})
	require.NoError(t, err)

	p, err := ps.Get(dir)
	require.NoError(t, err)
	require.Contains(t, p.Terminals, "term-auto")
}
