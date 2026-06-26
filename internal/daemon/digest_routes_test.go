package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

type fakeNarrator struct {
	out string
	err error
}

func (n fakeNarrator) Summarize(_ context.Context, _ digest.Facts) (string, error) {
	return n.out, n.err
}

// digestEnv wires a Server with a real lifecycle adapter pointed at a temp
// transcript root + a temp git repo as the session workdir.
func digestEnv(t *testing.T, transcript string, narrator digest.Narrator) (*httptest.Server, string) {
	t.Helper()
	projects := t.TempDir()
	work := t.TempDir()

	// init a real git repo with one committed + one unstaged change.
	// Strip GIT_DIR and related variables from the subprocess environment: when
	// this test runs inside a git pre-commit hook, GIT_DIR is exported and points
	// at the parent repo's .git dir. Child git processes inheriting it would treat
	// the temp dir as a separate work-tree and write core.worktree into the parent
	// repo's .git/config, corrupting it.
	safeEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR":
			// drop — would point back at the parent repo
		default:
			safeEnv = append(safeEnv, e)
		}
	}
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = safeEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "-b", "feature/digest")
	runGit("config", "user.email", "t@t")
	runGit("config", "user.name", "t")
	runGit("config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n"), 0o644)
	runGit("add", "a.go")
	runGit("commit", "-m", "init")
	os.WriteFile(filepath.Join(work, "a.go"), []byte("package a\n// edit\n"), 0o644)

	// Place the transcript where TranscriptPath will find it: <projects>/<glob>/<id>.jsonl.
	encDir := filepath.Join(projects, "encoded")
	os.MkdirAll(encDir, 0o755)
	claudeID := "11111111-1111-1111-1111-111111111111"
	if transcript != "" {
		os.WriteFile(filepath.Join(encDir, claudeID+".jsonl"), []byte(transcript), 0o644)
	}

	lc := lifecycle.New(lifecycle.HintingExecRunner{Inner: lifecycle.ExecRunner{}}, &lifecycle.FakeConfig{})
	lc.ProjectsDir = projects
	fs := newFakeStore()
	fs.data["agent-d1"] = &store.Session{
		ID: "agent-d1", Workdir: work, ClaudeSessionID: claudeID, Status: store.Status("working"),
	}
	srv := &Server{store: fs, life: NewLifecycleAdapter(lc, fs), narrator: narrator}
	return httptest.NewServer(srv.router()), claudeID
}

const digestTranscript = `{"type":"user","message":{"role":"user","content":"Edit a.go"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"a.go","old_string":"x","new_string":"y"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Edited a.go."}]}}
`

func getDigest(t *testing.T, ts *httptest.Server, id string) (*digest.Digest, int) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/v1/sessions/" + id + "/digest")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode
	}
	var d digest.Digest
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return &d, resp.StatusCode
}

func TestDigestHappyPath(t *testing.T) {
	ts, _ := digestEnv(t, digestTranscript, fakeNarrator{out: "Edited a.go to add a comment."})
	defer ts.Close()
	d, code := getDigest(t, ts, "agent-d1")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if d.Summary != "Edited a.go to add a comment." {
		t.Errorf("Summary = %q (want narrator output)", d.Summary)
	}
	if d.Branch != "feature/digest" {
		t.Errorf("Branch = %q, want feature/digest", d.Branch)
	}
	if d.Turns != 2 || d.Task != "Edit a.go" || d.Status != "working" {
		t.Errorf("facts wrong: %+v", d)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "a.go" || !d.Files[0].Edited || d.Files[0].Added == 0 {
		t.Errorf("Files = %+v, want a.go edited with +lines", d.Files)
	}
}

func TestDigestNarratorFailureDegrades(t *testing.T) {
	ts, _ := digestEnv(t, digestTranscript, fakeNarrator{err: errors.New("claude down")})
	defer ts.Close()
	d, _ := getDigest(t, ts, "agent-d1")
	if d.Summary != "Edited a.go." {
		t.Errorf("Summary = %q, want fallback to LastMessage", d.Summary)
	}
}

func TestDigestMissingTranscript(t *testing.T) {
	ts, _ := digestEnv(t, "", fakeNarrator{out: "unused"})
	defer ts.Close()
	d, code := getDigest(t, ts, "agent-d1")
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if d.Summary != "no transcript available" || d.Status != "working" {
		t.Errorf("missing-transcript digest = %+v", d)
	}
}

func TestDigestUnknownSession(t *testing.T) {
	ts, _ := digestEnv(t, digestTranscript, fakeNarrator{out: "x"})
	defer ts.Close()
	_, code := getDigest(t, ts, "nope")
	if code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", code)
	}
}

// TestHandleDigestServesPipelineSnapshot verifies that a reaped pipeline job's
// session returns the stored snapshot instead of attempting a live rebuild.
func TestHandleDigestServesPipelineSnapshot(t *testing.T) {
	ps, err := pipeline.NewStore(t.TempDir())
	require.NoError(t, err)
	_ = ps.Create(&pipeline.Pipeline{
		ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusDone,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone,
			Digest: &digest.Digest{Summary: "frozen snapshot", Turns: 7}}},
	})
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "p-a", TmuxSession: "p-a", PipelineID: "p", JobID: "a", Status: store.StatusDone,
	})
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl, exec: NewExecutor(ps, fs, fl, nil, func() {})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/v1/sessions/p-a/digest")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got digest.Digest
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "frozen snapshot", got.Summary)
	require.Equal(t, 7, got.Turns)
}
