package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/projectstore"
)

// projectServer builds a route server backed by a real ScrivaDB project store in a
// temp dir.
func projectServer(t *testing.T, fs *fakeStore) (*httptest.Server, *projectstore.Store) {
	t.Helper()
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })
	srv := &Server{store: fs, life: &fakeLife{}, projects: ps}
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, ps
}

func TestHandleListProjectsEmpty(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp, err := http.Get(srv.URL + "/api/v1/projects")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out struct {
		Projects []projectstore.Project `json:"projects"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.NotNil(t, out.Projects) // never null — encodes as [] when empty
	require.Empty(t, out.Projects)
}

func TestHandleListProjectsUnconfigured(t *testing.T) {
	// A server without a wired project store returns 503, not a panic.
	srv := lifeServer(t, newFakeStore(), &fakeLife{})
	resp, err := http.Get(srv.URL + "/api/v1/projects")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	require.NoError(t, err)
	return resp
}

func TestHandleOpenProject(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())

	resp := postJSON(t, srv.URL+"/api/v1/projects/open", map[string]any{
		"id": "/repos/alpha", "name": "Alpha", "path": "/repos/alpha",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var p projectstore.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	require.Equal(t, "/repos/alpha", p.ID)
	require.Equal(t, "Alpha", p.Name)
	require.Equal(t, projectstore.StatusOpen, p.Status)

	// It now appears in the list.
	lresp, err := http.Get(srv.URL + "/api/v1/projects")
	require.NoError(t, err)
	defer lresp.Body.Close()
	var out struct {
		Projects []projectstore.Project `json:"projects"`
	}
	require.NoError(t, json.NewDecoder(lresp.Body).Decode(&out))
	require.Len(t, out.Projects, 1)
	require.Equal(t, "Alpha", out.Projects[0].Name)
}

func TestHandleOpenProjectMissingID(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp := postJSON(t, srv.URL+"/api/v1/projects/open", map[string]any{"name": "no id"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHandleCloseProject(t *testing.T) {
	srv, ps := projectServer(t, newFakeStore())
	_, err := ps.OpenProject("beta", "Beta", "/repos/beta")
	require.NoError(t, err)

	resp := postJSON(t, srv.URL+"/api/v1/projects/beta/close", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var p projectstore.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	require.Equal(t, projectstore.StatusClosed, p.Status)
	require.Equal(t, "Beta", p.Name) // hibernation keeps the display name
}

func TestHandleCloseProjectNotFound(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp := postJSON(t, srv.URL+"/api/v1/projects/nope/close", nil)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
