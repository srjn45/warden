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
