package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/config"
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

// projectGitServer wires a project store and a workspace path so the Phase 2 git
// flows (remote clone / new scaffold) have somewhere to write. Returns the test
// server, the store, and the workspace dir.
func projectGitServer(t *testing.T) (*httptest.Server, *projectstore.Store, string) {
	t.Helper()
	ps, err := projectstore.NewStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { ps.Close() })
	workspace := t.TempDir()
	srv := &Server{store: newFakeStore(), life: &fakeLife{}, projects: ps}
	srv.SetBaselineConfig(config.Config{WorkspacePath: workspace})
	ts := httptest.NewServer(srv.router())
	t.Cleanup(ts.Close)
	return ts, ps, workspace
}

func TestOpenLocalProject(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	dir := t.TempDir()

	resp := postJSON(t, srv.URL+"/api/v1/projects/local", map[string]any{"path": dir, "name": "MyApp"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var p projectstore.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	// The id/path are the resolved absolute path (EvalSymlinks normalizes /tmp on
	// macOS, so compare against the same normalization rather than the raw dir).
	want, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, want, p.ID)
	require.Equal(t, want, p.Path)
	require.Equal(t, "MyApp", p.Name)
	require.Equal(t, projectstore.StatusOpen, p.Status)
}

func TestOpenLocalProjectDefaultsName(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	dir := filepath.Join(t.TempDir(), "widgets")
	require.NoError(t, os.MkdirAll(dir, 0o755))

	resp := postJSON(t, srv.URL+"/api/v1/projects/local", map[string]any{"path": dir})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var p projectstore.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	require.Equal(t, "widgets", p.Name) // defaults to the directory base name
}

func TestOpenLocalProjectMissingPath(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp := postJSON(t, srv.URL+"/api/v1/projects/local", map[string]any{"name": "no path"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOpenLocalProjectNonexistent(t *testing.T) {
	srv, _ := projectServer(t, newFakeStore())
	resp := postJSON(t, srv.URL+"/api/v1/projects/local", map[string]any{
		"path": filepath.Join(t.TempDir(), "does-not-exist"),
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOpenRemoteProject(t *testing.T) {
	source := newLocalRepo(t, "widgets")
	srv, ps, workspace := projectGitServer(t)

	resp := postJSON(t, srv.URL+"/api/v1/projects/remote", map[string]any{"url": source})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var p projectstore.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	dest := filepath.Join(workspace, "widgets")
	require.Equal(t, dest, p.ID)
	require.Equal(t, dest, p.Path)
	require.Equal(t, "widgets", p.Name) // defaults to the repo name from the URL
	require.Equal(t, projectstore.StatusOpen, p.Status)
	require.FileExists(t, filepath.Join(dest, "README.md")) // the clone landed

	// It is persisted in the store.
	got, err := ps.Get(dest)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, got.Status)
}

func TestOpenRemoteProjectRejectsExisting(t *testing.T) {
	source := newLocalRepo(t, "widgets")
	srv, _, workspace := projectGitServer(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "widgets"), 0o755))

	resp := postJSON(t, srv.URL+"/api/v1/projects/remote", map[string]any{"url": source})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestOpenRemoteProjectMissingURL(t *testing.T) {
	srv, _, _ := projectGitServer(t)
	resp := postJSON(t, srv.URL+"/api/v1/projects/remote", map[string]any{})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCreateProject(t *testing.T) {
	srv, ps, workspace := projectGitServer(t)

	resp := postJSON(t, srv.URL+"/api/v1/projects/new", map[string]any{"name": "greenfield"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var p projectstore.Project
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	dest := filepath.Join(workspace, "greenfield")
	require.Equal(t, dest, p.ID)
	require.Equal(t, "greenfield", p.Name)
	require.Equal(t, projectstore.StatusOpen, p.Status)

	// README template is written with the project name.
	readme, err := os.ReadFile(filepath.Join(dest, "README.md"))
	require.NoError(t, err)
	require.Contains(t, string(readme), "# greenfield")

	// git init + initial commit happened.
	require.DirExists(t, filepath.Join(dest, ".git"))
	logCmd := exec.Command("git", "-C", dest, "log", "--oneline")
	out, err := logCmd.CombinedOutput()
	require.NoError(t, err, "git log: %s", out)
	require.Contains(t, string(out), "chore: project initiated using warden")

	// Persisted.
	got, err := ps.Get(dest)
	require.NoError(t, err)
	require.Equal(t, projectstore.StatusOpen, got.Status)
}

func TestCreateProjectRejectsExisting(t *testing.T) {
	srv, _, workspace := projectGitServer(t)
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "greenfield"), 0o755))

	resp := postJSON(t, srv.URL+"/api/v1/projects/new", map[string]any{"name": "greenfield"})
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCreateProjectRejectsBadName(t *testing.T) {
	srv, _, _ := projectGitServer(t)
	for _, name := range []string{"", "../escape", "a/b"} {
		resp := postJSON(t, srv.URL+"/api/v1/projects/new", map[string]any{"name": name})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "name=%q", name)
		resp.Body.Close()
	}
}
