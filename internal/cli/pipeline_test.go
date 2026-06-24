package cli

import (
	"bytes"
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
