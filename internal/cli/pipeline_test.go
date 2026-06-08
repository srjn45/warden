package cli

import (
	"strings"
	"testing"

	"github.com/srajanpathak/warden/internal/pipeline"
)

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
