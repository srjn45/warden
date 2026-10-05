package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// gitRepo makes a temp git repo with one commit and returns its resolved root.
func gitRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "init")
	return root
}

func TestProjectIDForDir(t *testing.T) {
	root := gitRepo(t)
	sub := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(sub, 0o755))

	got, err := projectIDForDir(sub)
	require.NoError(t, err)
	require.Equal(t, root, got, "subdir resolves to repo root")

	wt := filepath.Join(root, ".worktrees", "feat")
	out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "feat", wt).CombinedOutput()
	require.NoError(t, err, string(out))
	got, err = projectIDForDir(wt)
	require.NoError(t, err)
	require.Equal(t, root, got, "linked worktree resolves to parent repo root")

	plain, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	got, err = projectIDForDir(plain)
	require.NoError(t, err)
	require.Equal(t, plain, got, "non-git dir falls back to the dir")
}

func spawnProjectID(t *testing.T, args ...string) any {
	t.Helper()
	t.Setenv("WARDEN_SESSION_ID", "")
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/spawn": `{"id":"code-1","status":"spawning"}`,
	}, nil, body))
	_, err := runCLI(t, addr, args...)
	require.NoError(t, err)
	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(body["/api/v1/spawn"]), &sent))
	return sent["project_id"]
}

func TestStartDefaultsProjectToDirGitRoot(t *testing.T) {
	root := gitRepo(t)
	sub := filepath.Join(root, "pkg")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.Equal(t, root, spawnProjectID(t, "start", "do it", "--role", "general", "--dir", sub))
}

func TestStartDefaultsProjectNonGitDir(t *testing.T) {
	plain, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.Equal(t, plain, spawnProjectID(t, "start", "do it", "--role", "general", "--dir", plain))
}

func TestStartDefaultsProjectLinkedWorktree(t *testing.T) {
	root := gitRepo(t)
	wt := filepath.Join(root, ".worktrees", "w")
	out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "w", wt).CombinedOutput()
	require.NoError(t, err, string(out))
	require.Equal(t, root, spawnProjectID(t, "start", "do it", "--role", "general", "--dir", wt))
}

func TestStartExplicitProjectUnchanged(t *testing.T) {
	root := gitRepo(t)
	require.Equal(t, "/repos/alpha", spawnProjectID(t, "start", "do it", "--role", "general", "--dir", root, "--project", "/repos/alpha"))
}

func TestStartManagedDefaultsProjectFromRepo(t *testing.T) {
	root := gitRepo(t)
	require.Equal(t, root, spawnProjectID(t, "start", "--role", "worker", "--repo", root, "--name", "x"))
}

func pipelineProjectID(t *testing.T, args ...string) (any, bool) {
	t.Helper()
	var in map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "p", "jobs": []map[string]any{}})
	}))
	t.Cleanup(srv.Close)
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append(args, "--addr", strings.TrimPrefix(srv.URL, "http://"), "--config", t.TempDir()+"/none.yaml"))
	require.NoError(t, root.Execute(), buf.String())
	v, ok := in["project_id"]
	return v, ok
}

func TestPipelineCreateDefaultsProjectFromRepo(t *testing.T) {
	root := gitRepo(t)
	got, _ := pipelineProjectID(t, "pipeline", "create", "--template", "analyze-implement-review",
		"--name", "n", "--repo", root, "--set", "TASK=t")
	require.Equal(t, root, got)
}

func TestPipelineCreateDefaultsProjectFromSpecDir(t *testing.T) {
	root := gitRepo(t)
	spec := filepath.Join(root, "spec.yaml")
	require.NoError(t, os.WriteFile(spec, []byte("name: s\nrepo: "+root+"\njobs:\n  - id: a\n    prompt: hi\n"), 0o644))
	got, _ := pipelineProjectID(t, "pipeline", "create", "-f", spec)
	require.Equal(t, root, got)
}

func TestPipelineCreateExplicitAndSpecProjectWin(t *testing.T) {
	root := gitRepo(t)
	got, _ := pipelineProjectID(t, "pipeline", "create", "--template", "analyze-implement-review",
		"--name", "n", "--repo", root, "--set", "TASK=t", "--project", "/p/x")
	require.Equal(t, "/p/x", got)

	spec := filepath.Join(root, "spec.yaml")
	require.NoError(t, os.WriteFile(spec, []byte("name: s\nproject_id: /spec/proj\nrepo: "+root+"\njobs:\n  - id: a\n    prompt: hi\n"), 0o644))
	_, sent := pipelineProjectID(t, "pipeline", "create", "-f", spec)
	require.False(t, sent, "spec-authored project_id must not be overridden by a CLI default")
}

func planCreateProjectID(t *testing.T, dir string) any {
	t.Helper()
	t.Chdir(dir)
	body := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"POST /api/v1/plans": planSingleJSON,
	}, nil, body))
	_, err := runCLI(t, addr, "plan", "create", "--name", "n", "--goal", "g", "--task", "t1:do it")
	require.NoError(t, err)
	var sent map[string]any
	require.NoError(t, json.Unmarshal([]byte(body["/api/v1/plans"]), &sent))
	return sent["project_id"]
}

func TestPlanCreateDefaultsProjectToGitRoot(t *testing.T) {
	root := gitRepo(t)
	sub := filepath.Join(root, "pkg", "x")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.Equal(t, root, planCreateProjectID(t, sub), "subdirectory")

	wt := filepath.Join(root, ".worktrees", "w")
	out, err := exec.Command("git", "-C", root, "worktree", "add", "-q", "-b", "w", wt).CombinedOutput()
	require.NoError(t, err, string(out))
	require.Equal(t, root, planCreateProjectID(t, wt), "linked worktree")
}
