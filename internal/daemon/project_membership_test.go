package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
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
	sessExplicit := &store.Session{
		ID:        "agent-explicit",
		Repo:      "/projects/alpha",
		ProjectID: "custom-id",
	}
	require.Equal(t, "custom-id", s.resolveProjectID(sessExplicit))

	// 2. Path match resolves open project
	sessPathMatch := &store.Session{
		ID:   "agent-path",
		Repo: "/projects/alpha",
	}
	require.Equal(t, proj.ID, s.resolveProjectID(sessPathMatch))

	// 3. Closed project does not match
	sessClosed := &store.Session{
		ID:   "agent-closed",
		Repo: "/projects/closed",
	}
	require.Equal(t, "", s.resolveProjectID(sessClosed))

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

	// 6. Add terminal membership
	termSess := &store.Session{
		ID:        "term-1",
		Kind:      store.KindTerminal,
		ProjectID: proj.ID,
	}
	s.addProjectMembership(termSess)
	updated, err = ps.Get(proj.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-path"}, updated.Agents)
	require.Equal(t, []string{"term-1"}, updated.Terminals)

	// 7. Remove membership
	s.removeProjectMembership(sessPathMatch)
	s.removeProjectMembership(termSess)
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
