package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/stretchr/testify/require"
)

func TestJobDetailTextRendersStoredJob(t *testing.T) {
	p := &pipeline.Pipeline{ID: "pl", Jobs: []pipeline.Job{
		{ID: "only", Status: pipeline.JobDone, Prompt: "do the thing", Output: "did it"},
	}}
	out, err := jobDetailText(p, "only", 80)
	require.NoError(t, err)
	require.Contains(t, out, "only")
	require.Contains(t, out, "do the thing", "prompt must render")
	require.Contains(t, out, "did it", "output must render")
}

func TestJobDetailTextMissingJob(t *testing.T) {
	p := &pipeline.Pipeline{ID: "pl", Jobs: []pipeline.Job{{ID: "a"}}}
	_, err := jobDetailText(p, "ghost", 80)
	require.Error(t, err)
}

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

func TestRenderPipelineFooterMentionsDelete(t *testing.T) {
	p := &pipeline.Pipeline{ID: "demo", Status: pipeline.StatusCanceled, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone}}}
	out := renderPipeline(p, 60, 20)
	if !strings.Contains(out, "D delete") {
		t.Fatalf("renderPipeline footer should mention D delete, got:\n%s", out)
	}
}

func TestPipelineHasLiveJobs(t *testing.T) {
	live := &pipeline.Pipeline{Jobs: []pipeline.Job{{Status: pipeline.JobDone}, {Status: pipeline.JobRunning}}}
	if !pipelineHasLiveJobs(live) {
		t.Fatal("a running job should count as live")
	}
	attn := &pipeline.Pipeline{Jobs: []pipeline.Job{{Status: pipeline.JobNeedsAttention}}}
	if !pipelineHasLiveJobs(attn) {
		t.Fatal("a needs_attention job should count as live")
	}
	stopped := &pipeline.Pipeline{Jobs: []pipeline.Job{{Status: pipeline.JobDone}, {Status: pipeline.JobSkipped}, {Status: pipeline.JobFailed}}}
	if pipelineHasLiveJobs(stopped) {
		t.Fatal("done/skipped/failed jobs are not live")
	}
}

func TestRenderPipelineJobShowsDetails(t *testing.T) {
	j := &pipeline.Job{
		ID: "impl", Status: pipeline.JobDone, Prompt: "implement the thing",
		Handoff: "branch + summary", Output: "did the thing", Branch: "p-impl",
		Digest: &digest.Digest{Summary: "narrated summary"},
	}
	out := renderPipelineJob(j, 80, 40)
	for _, want := range []string{"impl", "implement the thing", "branch + summary", "did the thing", "p-impl", "narrated summary"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered job missing %q:\n%s", want, out)
		}
	}
}

func TestJobIsTerminal(t *testing.T) {
	for _, s := range []pipeline.JobStatus{pipeline.JobDone, pipeline.JobSkipped, pipeline.JobFailed} {
		if !jobIsTerminal(s) {
			t.Fatalf("%s should be terminal", s)
		}
	}
	for _, s := range []pipeline.JobStatus{pipeline.JobPending, pipeline.JobRunning, pipeline.JobNeedsAttention} {
		if jobIsTerminal(s) {
			t.Fatalf("%s should NOT be terminal", s)
		}
	}
}
