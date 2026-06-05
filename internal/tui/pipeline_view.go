package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/pipeline"
)

// pipelineHasLiveJobs reports whether any job is still running or awaiting input.
// A pipeline can only be deleted once none of its jobs is live (mirrors the
// daemon's DELETE /pipelines/{pid} guard).
func pipelineHasLiveJobs(p *pipeline.Pipeline) bool {
	for i := range p.Jobs {
		if p.Jobs[i].Status == pipeline.JobRunning || p.Jobs[i].Status == pipeline.JobNeedsAttention {
			return true
		}
	}
	return false
}

// renderPipeline draws a pipeline's DAG in the detail pane when its header row is
// selected (mirrors renderApprovalsQueue). Read-only summary; actions come from
// keys handled by the model.
func renderPipeline(p *pipeline.Pipeline, width, height int) string {
	var b strings.Builder
	b.WriteString(stMuted.Render("pipeline "+p.ID+" — "+string(p.Status)) + "\n\n")
	for i := range p.Jobs {
		j := &p.Jobs[i]
		line := fmt.Sprintf("%s %-12s %-13s", jobGlyph(j.Status), trunc(j.ID, 12), string(j.Status))
		if len(j.DependsOn) > 0 {
			line += stMuted.Render("deps: " + strings.Join(j.DependsOn, ","))
		}
		b.WriteString(line + "\n")
		if j.Output != "" {
			b.WriteString("    " + stMuted.Render(trunc(j.Output, max(0, width-4))) + "\n")
		}
	}
	b.WriteString("\n" + stMuted.Render("x cancel pipeline · D delete (when stopped) · on a job: r retry · a attach"))
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}

// jobIsTerminal reports whether a job's agent is gone (done/skipped/failed), so the
// detail pane should render the job's stored details instead of attaching to tmux.
func jobIsTerminal(s pipeline.JobStatus) bool {
	return s == pipeline.JobDone || s == pipeline.JobSkipped || s == pipeline.JobFailed
}

// renderPipelineJob draws one job's full details in the detail pane — used for
// terminal-status jobs whose agent has been reaped (no live tmux to attach).
// width is accepted for symmetry with renderPipeline and reserved for future line-wrapping; it is not yet used.
func renderPipelineJob(j *pipeline.Job, width, height int) string {
	var b strings.Builder
	b.WriteString(stMuted.Render(jobGlyph(j.Status)+" job "+j.ID+" — "+string(j.Status)) + "\n")
	if len(j.DependsOn) > 0 {
		b.WriteString(stMuted.Render("deps: "+strings.Join(j.DependsOn, ",")) + "\n")
	}
	if j.Branch != "" {
		b.WriteString(stMuted.Render("branch: "+j.Branch) + "\n")
	}
	b.WriteString("\n" + stMuted.Render("Prompt") + "\n" + j.Prompt + "\n")
	if j.Handoff != "" {
		b.WriteString("\n" + stMuted.Render("Handoff") + "\n" + j.Handoff + "\n")
	}
	if j.Output != "" {
		b.WriteString("\n" + stMuted.Render("Output") + "\n" + j.Output + "\n")
	}
	if j.Digest != nil && j.Digest.Summary != "" {
		b.WriteString("\n" + stMuted.Render("Digest") + "\n" + j.Digest.Summary + "\n")
	}
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}
