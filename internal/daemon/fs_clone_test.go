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

	"github.com/srjn45/warden/internal/config"
	"github.com/stretchr/testify/require"
)

// newLocalRepo creates a git repo with one commit at dir/repo-name and returns
// its path, so tests can `git clone` it without any network access.
func newLocalRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	run("init")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+name+"\n"), 0o644))
	run("add", ".")
	run("commit", "-m", "initial commit")
	return dir
}

func TestCloneRepoClonesIntoWorkspace(t *testing.T) {
	source := newLocalRepo(t, "widgets")
	workspace := t.TempDir()

	srv := &Server{}
	srv.SetBaselineConfig(config.Config{WorkspacePath: workspace})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"url": source})
	resp, err := http.Post(ts.URL+"/api/v1/fs/clone", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out struct {
		Dir string `json:"dir"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	require.Equal(t, filepath.Join(workspace, "widgets"), out.Dir)
	require.FileExists(t, filepath.Join(out.Dir, "README.md"))
}

func TestCloneRepoRejectsEmptyURL(t *testing.T) {
	srv := &Server{}
	srv.SetBaselineConfig(config.Config{WorkspacePath: t.TempDir()})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"url": ""})
	resp, err := http.Post(ts.URL+"/api/v1/fs/clone", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCloneRepoRejectsExistingDestination(t *testing.T) {
	source := newLocalRepo(t, "widgets")
	workspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(workspace, "widgets"), 0o755))

	srv := &Server{}
	srv.SetBaselineConfig(config.Config{WorkspacePath: workspace})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"url": source})
	resp, err := http.Post(ts.URL+"/api/v1/fs/clone", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCloneRepoSurfacesGitFailure(t *testing.T) {
	workspace := t.TempDir()
	srv := &Server{}
	srv.SetBaselineConfig(config.Config{WorkspacePath: workspace})
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"url": filepath.Join(t.TempDir(), "does-not-exist")})
	resp, err := http.Post(ts.URL+"/api/v1/fs/clone", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestRepoNameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/widgets":     "widgets",
		"https://github.com/acme/widgets.git": "widgets",
		"git@github.com:acme/widgets.git":     "widgets",
		"https://gitlab.com/acme/widgets/":    "widgets",
	}
	for url, want := range cases {
		require.Equal(t, want, repoNameFromURL(url), "url=%s", url)
	}
}
