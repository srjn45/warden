package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	require.Contains(t, out, "3 jobs")
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
	out := renderPipelineDetail(p)
	for _, want := range []string{
		"demo [done] repo=/r",
		"analyze", "found X; no code",
		"impl", "(depends: [analyze])",
		"branch: demo-impl", "output: done on demo-impl",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderPipelineDetail missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderPipelineDetailOmitsEmptyBranchAndOutput(t *testing.T) {
	p := &pipeline.Pipeline{ID: "p", Status: pipeline.StatusRunning, Repo: "/r",
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}}}
	out := renderPipelineDetail(p)
	if strings.Contains(out, "branch:") || strings.Contains(out, "output:") {
		t.Fatalf("a job with no branch/output should print neither:\n%s", out)
	}
}
