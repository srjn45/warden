package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/pipeline"
)

func TestRenderPipeline(t *testing.T) {
	p := &pipeline.Pipeline{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{
		{ID: "analyze", Status: pipeline.JobDone, Output: "found X"},
		{ID: "impl", Status: pipeline.JobRunning, DependsOn: []string{"analyze"}},
	}}
	out := renderPipeline(p, 60, 20)
	for _, want := range []string{"demo", "running", "analyze", "impl", "found X", "deps: analyze"} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderPipeline missing %q in:\n%s", want, out)
		}
	}
}
