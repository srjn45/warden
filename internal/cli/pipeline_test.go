package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/stretchr/testify/require"
)

// runPipelineValidate writes spec to a temp file and runs `pipeline validate -f`,
// returning combined output and the execute error (non-nil => exit code 1).
func runPipelineValidate(t *testing.T, spec string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pipeline.yaml")
	require.NoError(t, os.WriteFile(path, []byte(spec), 0o644))
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"pipeline", "validate", "-f", path})
	err := root.Execute()
	return buf.String(), err
}

func TestPipelineValidateAcceptsGoodSpec(t *testing.T) {
	out, err := runPipelineValidate(t, `name: demo
repo: /tmp/repo
jobs:
  - id: analyze
    prompt: look around
  - id: impl
    prompt: build it
    depends_on: [analyze]
`)
	require.NoError(t, err)
	require.Contains(t, out, "is valid")
	require.Contains(t, out, "2 jobs")
}

func TestPipelineValidateRejectsCycle(t *testing.T) {
	_, err := runPipelineValidate(t, `name: demo
repo: /tmp/repo
jobs:
  - id: a
    prompt: x
    depends_on: [b]
  - id: b
    prompt: y
    depends_on: [a]
`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle")
}

func TestPipelineValidateRejectsUnknownDependency(t *testing.T) {
	_, err := runPipelineValidate(t, `name: demo
repo: /tmp/repo
jobs:
  - id: a
    prompt: x
    depends_on: [missing]
`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown job")
}

func TestPipelineValidateRequiresFile(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"pipeline", "validate"})
	require.Error(t, root.Execute())
}

func TestPipelineListTemplates(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"pipeline", "list-templates"})
	require.NoError(t, root.Execute())
	out := buf.String()
	for _, name := range []string{
		"analyze-implement-review", "parallel-tasks", "test-fix-verify", "research-synthesis",
	} {
		require.Contains(t, out, name)
	}
	require.Contains(t, out, "placeholders:")
}

func TestPipelineCreateRejectsBothOrNeitherSource(t *testing.T) {
	for _, args := range [][]string{
		{"pipeline", "create"},
		{"pipeline", "create", "-f", "x.yaml", "--template", "parallel-tasks"},
	} {
		root := newRootCmd()
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetErr(&buf)
		root.SetArgs(args)
		require.Error(t, root.Execute(), "args %v should be rejected", args)
	}
}

func TestPipelineCreateTemplateMissingPlaceholder(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	// analyze-implement-review needs TASK; only NAME/REPO are defaulted.
	root.SetArgs([]string{"pipeline", "create", "--template", "analyze-implement-review",
		"--repo", "/r", "--config", t.TempDir() + "/none.yaml"})
	err := root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "TASK")
}

func TestPipelineCreateTemplateRendersAndSends(t *testing.T) {
	var gotSpec string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		gotSpec = in["spec"]
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "myrun", "jobs": []map[string]any{{"id": "analyze"}, {"id": "implement"}, {"id": "review"}},
		})
	}))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"pipeline", "create", "--template", "analyze-implement-review",
		"--name", "myrun", "--repo", "/work/proj", "--set", "TASK=add a flag",
		"--addr", addr, "--config", t.TempDir() + "/none.yaml"})
	require.NoError(t, root.Execute())

	require.Contains(t, buf.String(), "created pipeline myrun")
	// The rendered spec the daemon received must have all placeholders filled.
	require.Contains(t, gotSpec, "name: myrun")
	require.Contains(t, gotSpec, "repo: /work/proj")
	require.Contains(t, gotSpec, "add a flag")
	require.NotContains(t, gotSpec, "{{")
}

func TestRenderPipelineDetailShowsBranchAndOutput(t *testing.T) {
	p := &pipeline.Pipeline{
		ID: "demo", Status: pipeline.StatusDone, Repo: "/r",
		Jobs: []pipeline.Job{
			{ID: "analyze", Status: pipeline.JobDone, Output: "found X; no code"},
			{ID: "impl", Status: pipeline.JobDone, DependsOn: []string{"analyze"},
				Branch: "demo-impl", Output: "done on demo-impl"},
		},
	}
	out := renderPipelineDetail(p, showOpts{})
	for _, want := range []string{
		"name:", "demo", "status:    done", "repo:",
		"analyze", "output: found X; no code",
		"impl", "demo-impl", "output: done on demo-impl",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderPipelineDetail missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderPipelineDetailShowsPlanAndProject(t *testing.T) {
	p := &pipeline.Pipeline{
		ID: "bound", Status: pipeline.StatusPending, Repo: "/r",
		ProjectID: "/proj", PlanID: "plan-aabbccdd",
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobPending}},
	}
	out := renderPipelineDetail(p, showOpts{})
	for _, want := range []string{"project:   /proj", "plan:      plan-aabbccdd", "wd plan show plan-aabbccdd"} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderPipelineDetail missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderPipelineDetailOmitsEmptyBranchAndOutput(t *testing.T) {
	p := &pipeline.Pipeline{ID: "p", Status: pipeline.StatusRunning, Repo: "/r",
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}}}
	out := renderPipelineDetail(p, showOpts{})
	if strings.Contains(out, "output:") {
		t.Fatalf("a job with no output should not print an output line:\n%s", out)
	}
}

func TestRenderPipelineDetailHidesSyntheticJobs(t *testing.T) {
	p := &pipeline.Pipeline{
		ID: "demo", Status: pipeline.StatusRunning, Repo: "/r",
		Jobs: []pipeline.Job{
			{ID: "root-span-out", Type: "span-out", Status: pipeline.JobDone},
			{ID: "a", Status: pipeline.JobDone, DependsOn: []string{"root-span-out"}},
			{ID: "a-span-out", Type: "span-out", Status: pipeline.JobDone, DependsOn: []string{"a"}},
			{ID: "b", Status: pipeline.JobPending, DependsOn: []string{"a-span-out"}},
		},
	}
	out := renderPipelineDetail(p, showOpts{})
	if strings.Contains(out, "span-out") {
		t.Fatalf("default view must hide synthetic jobs and deps:\n%s", out)
	}
	if !strings.Contains(out, "AFTER") || !strings.Contains(out, "  a  ") {
		t.Fatalf("synthetic dep should resolve to the real job:\n%s", out)
	}
	all := renderPipelineDetail(p, showOpts{allJobs: true})
	for _, want := range []string{"root-span-out [warden]", "a-span-out [warden]", "a-span-out"} {
		if !strings.Contains(all, want) {
			t.Fatalf("--all-jobs missing %q:\n%s", want, all)
		}
	}
}

func TestRenderPipelineDetailAlignsLongJobIDs(t *testing.T) {
	long := "a-very-long-job-identifier-here"
	p := &pipeline.Pipeline{ID: "p", Status: pipeline.StatusRunning, Repo: "/r", Jobs: []pipeline.Job{
		{ID: "a", Status: pipeline.JobDone, AgentID: "ag-1", Backend: "claude", Model: "opus", Branch: "p-a"},
		{ID: long, Status: pipeline.JobNeedsAttention, DependsOn: []string{"a", "c"}, Output: "x\ny"},
		{ID: "c", Status: pipeline.JobPending},
	}}
	out := renderPipelineDetail(p, showOpts{})
	lines := strings.Split(out, "\n")
	col := -1
	for _, l := range lines {
		if strings.HasPrefix(l, "JOB") || strings.HasPrefix(l, "a ") || strings.HasPrefix(l, long) || strings.HasPrefix(l, "c ") {
			idx := strings.Index(l, strings.Fields(l)[1])
			if col == -1 {
				col = idx
			} else if idx != col {
				t.Fatalf("status column misaligned (%d vs %d):\n%s", idx, col, out)
			}
		}
	}
	for _, want := range []string{"claude/opus", "ag-1", "a,c", "! needs_attention", "output: x y"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[a c]") {
		t.Fatalf("deps must not print as a Go slice:\n%s", out)
	}
}

func TestRenderPipelineDetailPromptsAndHeader(t *testing.T) {
	p := &pipeline.Pipeline{Name: "nm", ID: "p", Status: pipeline.StatusPending, Repo: "/r", ScheduleName: "nightly",
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobPending, Prompt: "do it\nnow", Handoff: "summarize"}}}
	plain := renderPipelineDetail(p, showOpts{})
	if strings.Contains(plain, "do it") || !strings.Contains(plain, "schedule:") || !strings.Contains(plain, "nightly") {
		t.Fatalf("unexpected default view:\n%s", plain)
	}
	out := renderPipelineDetail(p, showOpts{prompts: true})
	for _, want := range []string{"    prompt:\n      do it\n      now", "    handoff:\n      summarize"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestIsTerminalPipeline(t *testing.T) {
	for s, want := range map[pipeline.Status]bool{
		pipeline.StatusDone: true, pipeline.StatusCanceled: true, pipeline.StatusStalled: true,
		pipeline.StatusRunning: false, pipeline.StatusPending: false, pipeline.StatusPaused: false,
	} {
		if isTerminalPipeline(s) != want {
			t.Errorf("isTerminalPipeline(%s) != %v", s, want)
		}
	}
}

const pipelineShowJSON = `{"id":"p1","name":"p1","repo":"/r","status":"%s","jobs":[{"id":"a","status":"done","prompt":"hello","output":"full output"}]}`

func TestPipelineShowJSONRaw(t *testing.T) {
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/p1": fmt.Sprintf(pipelineShowJSON, "done"),
	}, nil, nil))
	out, err := runCLI(t, addr, "pipeline", "show", "p1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"prompt": "hello"`) || !strings.Contains(out, `"output": "full output"`) {
		t.Fatalf("json should carry full job fields:\n%s", out)
	}
}

func TestPipelineShowWatchStopsOnTerminal(t *testing.T) {
	old := planWatchInterval
	planWatchInterval = 5 * time.Millisecond
	defer func() { planWatchInterval = old }()
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/p1": fmt.Sprintf(pipelineShowJSON, "done"),
	}, nil, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := runCLICtx(t, ctx, addr, "pipeline", "show", "p1", "--watch")
	if err != nil || ctx.Err() != nil {
		t.Fatalf("watch on a done pipeline must return promptly: err=%v ctx=%v", err, ctx.Err())
	}
	if strings.Count(out, "status:") != 1 {
		t.Fatalf("expected a single render:\n%s", out)
	}
}

func TestWatchRefreshStopsWhenDone(t *testing.T) {
	n := 0
	cmd := newPipelineShowCmd()
	cmd.SetContext(context.Background())
	err := watchRefresh(cmd, func() (bool, error) { n++; return n == 3, nil }, true, time.Millisecond)
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

const pipelineCancelLiveJSON = `{"id":"demo","name":"refactor","status":"running","jobs":[{"id":"analyze","status":"running","agent_id":"agent-a1"},{"id":"impl","status":"pending"}]}`
const pipelineCancelIdleJSON = `{"id":"demo","name":"refactor","status":"running","jobs":[{"id":"analyze","status":"done","agent_id":"agent-a1"},{"id":"impl","status":"pending"}]}`
const pipelineDeleteJSON = `{"id":"demo","name":"refactor","status":"canceled","jobs":[{"id":"analyze","status":"skipped"}]}`

func TestPipelineCancelNoRunningJobsFastPath(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo":         pipelineCancelIdleJSON,
		"POST /api/v1/pipelines/demo/cancel": `{"status":"canceled"}`,
	}, seen, nil))
	out, err := runCLI(t, addr, "pipeline", "cancel", "demo")
	require.NoError(t, err)
	require.Contains(t, out, "canceled demo")
	require.NotContains(t, out, "Cancel pipeline")
	require.Equal(t, "POST", seen["/api/v1/pipelines/demo/cancel"])
}

func TestPipelineCancelPromptYes(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo":         pipelineCancelLiveJSON,
		"POST /api/v1/pipelines/demo/cancel": `{"status":"canceled"}`,
	}, seen, nil))
	out, err := runCLIStdin(t, addr, "y\n", "pipeline", "cancel", "demo")
	require.NoError(t, err)
	require.Contains(t, out, "1 job(s) are still running")
	require.Contains(t, out, "job analyze (agent agent-a1)")
	require.Contains(t, out, "cannot be restarted")
	require.Contains(t, out, "canceled demo")
	require.Equal(t, "POST", seen["/api/v1/pipelines/demo/cancel"])
}

func TestPipelineCancelPromptNo(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo": pipelineCancelLiveJSON,
	}, seen, nil))
	out, err := runCLIStdin(t, addr, "n\n", "pipeline", "cancel", "demo")
	require.NoError(t, err)
	require.Contains(t, out, "aborted")
	require.Empty(t, seen["/api/v1/pipelines/demo/cancel"])
}

func TestPipelineCancelYesFlag(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo":         pipelineCancelLiveJSON,
		"POST /api/v1/pipelines/demo/cancel": `{"status":"canceled"}`,
	}, seen, nil))
	out, err := runCLI(t, addr, "pipeline", "cancel", "demo", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "canceled demo")
	require.NotContains(t, out, "[y/N]")
	require.Equal(t, "POST", seen["/api/v1/pipelines/demo/cancel"])
}

func TestPipelineCancelNonTTYRequiresYes(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo": pipelineCancelLiveJSON,
	}, seen, nil))
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close(); w.Close() })
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(r)
	root.SetArgs([]string{"pipeline", "cancel", "demo", "--addr", addr, "--config", t.TempDir() + "/none.yaml"})
	err = root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "--yes")
	require.Empty(t, seen["/api/v1/pipelines/demo/cancel"])
}

func TestPipelineDeletePromptYes(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo":    pipelineDeleteJSON,
		"DELETE /api/v1/pipelines/demo": `{"status":"deleted"}`,
	}, seen, nil))
	out, err := runCLIStdin(t, addr, "yes\n", "pipeline", "delete", "demo")
	require.NoError(t, err)
	require.Contains(t, out, "pipeline record and its job history")
	require.Contains(t, out, "Branches and worktrees are kept")
	require.Contains(t, out, "deleted demo")
	require.Equal(t, "DELETE", seen["/api/v1/pipelines/demo"])
}

func TestPipelineDeletePromptNo(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo": pipelineDeleteJSON,
	}, seen, nil))
	out, err := runCLIStdin(t, addr, "n\n", "pipeline", "delete", "demo")
	require.NoError(t, err)
	require.Contains(t, out, "aborted")
	require.NotEqual(t, "DELETE", seen["/api/v1/pipelines/demo"])
}

func TestPipelineDeleteYesFlag(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo":    pipelineDeleteJSON,
		"DELETE /api/v1/pipelines/demo": `{"status":"deleted"}`,
	}, seen, nil))
	out, err := runCLI(t, addr, "pipeline", "delete", "demo", "-y")
	require.NoError(t, err)
	require.Contains(t, out, "deleted demo")
	require.Equal(t, "DELETE", seen["/api/v1/pipelines/demo"])
}

func TestPipelineDeleteNonTTYRequiresYes(t *testing.T) {
	seen := map[string]string{}
	addr := stubDaemon(t, routedDaemon(t, map[string]string{
		"GET /api/v1/pipelines/demo": pipelineDeleteJSON,
	}, seen, nil))
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { r.Close(); w.Close() })
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(r)
	root.SetArgs([]string{"pipeline", "delete", "demo", "--addr", addr, "--config", t.TempDir() + "/none.yaml"})
	err = root.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "--yes")
	require.NotEqual(t, "DELETE", seen["/api/v1/pipelines/demo"])
}

func TestPipelineStartCanceledHint(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"pipeline already started (status canceled)"}`))
	})
	_, err := runCLI(t, addr, "pipeline", "start", "demo")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status canceled")
	require.Contains(t, err.Error(), "create a new pipeline from the same spec or template")
}

func TestConfirmYN(t *testing.T) {
	var sink bytes.Buffer
	require.True(t, confirmYN(strings.NewReader("y\n"), &sink, "go? "))
	require.True(t, confirmYN(strings.NewReader("YES\n"), &sink, "go? "))
	require.False(t, confirmYN(strings.NewReader("\n"), &sink, "go? "))
	require.False(t, confirmYN(strings.NewReader("n\n"), &sink, "go? "))
}
