package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// RunPipelineDetailPane renders a pipeline overview to stdout and blocks, so the
// cockpit's agent pane can show the DAG summary when Enter lands on a pipeline
// container. tmux replaces this process via respawn-pane on the next selection.
func RunPipelineDetailPane(a api, pid string) error {
	if text, err := loadPipelineDetail(a, pid); err != nil {
		fmt.Println(stMuted.Render("could not load pipeline overview: " + err.Error()))
	} else {
		fmt.Println(text)
	}
	select {} // hold the pane open until tmux respawns it
}

func loadPipelineDetail(a api, pid string) (string, error) {
	p, err := a.PipelineGet(context.Background(), pid)
	if err != nil {
		return "", err
	}
	return renderPipeline(p, 100, 0), nil
}

// RunAutopilotDetailPane renders an autopilot-run overview to stdout and blocks,
// so Enter on an Autopilot container opens the overview in the agent pane.
func RunAutopilotDetailPane(a api, runID string) error {
	if text, err := loadAutopilotDetail(a, runID); err != nil {
		fmt.Println(stMuted.Render("could not load autopilot overview: " + err.Error()))
	} else {
		fmt.Println(text)
	}
	select {} // hold the pane open until tmux respawns it
}

func loadAutopilotDetail(a api, runID string) (string, error) {
	st, err := a.GetAutopilot(context.Background())
	if err != nil {
		return "", err
	}
	for i := range st.Runs {
		if st.Runs[i].RunID == runID {
			return renderAutopilotRun(&st.Runs[i], 100, 0), nil
		}
	}
	return "", fmt.Errorf("autopilot run %q not found", runID)
}

// renderAutopilotRun draws a compact Autopilot overview for the agent pane.
func renderAutopilotRun(r *client.AutopilotRunStatus, width, height int) string {
	if r == nil {
		return padTo(stMuted.Render("no autopilot run"), height)
	}
	_ = width
	var b strings.Builder
	name := r.Name
	if name == "" {
		name = r.RunID
	}
	b.WriteString(stPaneTitle.Render("autopilot "+name) + "  " + stStatus.Render(r.State) + "\n")
	b.WriteString(stMuted.Render(fmt.Sprintf("run %s · %d/%d tasks · %d workers",
		r.RunID, r.Tasks.Landed, len(r.PlanTasks), r.WorkersInFlight)) + "\n")
	if r.IntegrationBranch != "" {
		b.WriteString(stMuted.Render("branch: "+r.IntegrationBranch) + "\n")
	}
	if r.Repo != "" {
		b.WriteString(stMuted.Render("repo: "+r.Repo) + "\n")
	}
	if r.Gate != "" {
		b.WriteString(stMuted.Render("gate: "+r.Gate) + "\n")
	}
	if r.Watchdog != "" {
		b.WriteString(stMuted.Render("watchdog: "+r.Watchdog) + "\n")
	}
	for _, l := range r.SurfaceLines() {
		b.WriteString(stMuted.Render(l) + "\n")
	}
	b.WriteString("\n" + stMuted.Render("r pause/resume · x stop · ←/→ fold · enter on manager/worker opens agent pane"))
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}

// RunJobDetailPane renders one terminal job's stored detail to stdout and then
// blocks, so the cockpit's agent pane can show a finished job (whose agent tmux
// is gone) instead of a blank attach. tmux replaces this process via respawn-pane
// when the user selects another item; scrolling is handled by tmux copy-mode.
func RunJobDetailPane(a api, pid, jobID string) error {
	defer setupTUILogging()()
	if text, err := loadJobDetail(a, pid, jobID); err != nil {
		fmt.Println(stMuted.Render("could not load job detail: " + err.Error()))
	} else {
		fmt.Println(text)
	}
	select {} // hold the pane open until tmux respawns it
}

// loadJobDetail fetches the pipeline and renders the named job's stored detail.
func loadJobDetail(a api, pid, jobID string) (string, error) {
	p, err := a.PipelineGet(context.Background(), pid)
	if err != nil {
		return "", err
	}
	return jobDetailText(p, jobID, 100)
}

// RunAgentDetailPane renders one terminal agent's stored detail to stdout and then
// blocks, so the cockpit's agent pane can show a finished or tombstoned agent
// (whose tmux is gone) instead of a blank attach. tmux replaces this process via
// respawn-pane when the user selects another item; scrolling uses copy-mode.
func RunAgentDetailPane(a api, agentID string) error {
	defer setupTUILogging()()
	if text, err := loadAgentDetail(a, agentID); err != nil {
		fmt.Println(stMuted.Render("could not load agent detail: " + err.Error()))
	} else {
		fmt.Println(text)
	}
	select {} // hold the pane open until tmux respawns it
}

// loadAgentDetail finds the agent by id in the session list and renders its
// stored detail (the same body the `i` overlay shows).
func loadAgentDetail(a api, agentID string) (string, error) {
	sessions, err := a.List(context.Background())
	if err != nil {
		return "", err
	}
	for _, s := range sessions {
		if s.ID == agentID {
			return detailBody(s, -1, 100), nil
		}
	}
	return "", fmt.Errorf("agent %q not found", agentID)
}

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

// renderPipeline draws a pipeline's DAG in the agent pane when its header row is
// selected (mirrors renderApprovalsQueue). Read-only summary; actions come from
// keys handled by the model.
func renderPipeline(p *pipeline.Pipeline, width, height int) string {
	var b strings.Builder
	b.WriteString(stMuted.Render("pipeline "+p.ID+" — "+string(p.Status)) + "\n\n")
	for i := range p.Jobs {
		j := &p.Jobs[i]
		glyph, st := jobBadge(j.Status)
		line := fmt.Sprintf("%s %-12s %s", st.Render(glyph), trunc(j.ID, 12), st.Render(fmt.Sprintf("%-13s", string(j.Status))))
		if len(j.DependsOn) > 0 {
			line += stMuted.Render("deps: " + strings.Join(j.DependsOn, ","))
		}
		b.WriteString(line + "\n")
		if j.Output != "" {
			b.WriteString("    " + stMuted.Render(trunc(j.Output, max(0, width-4))) + "\n")
		}
	}
	b.WriteString("\n" + stMuted.Render("p pause/resume · x cancel pipeline · D delete (when stopped) · on a job: r retry · a attach"))
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}

// jobIsTerminal reports whether a job's agent is gone (done/skipped/failed), so the
// agent pane should render the job's stored details instead of attaching to tmux.
func jobIsTerminal(s pipeline.JobStatus) bool {
	return s == pipeline.JobDone || s == pipeline.JobSkipped || s == pipeline.JobFailed
}

// jobDetailText renders a single job's stored detail for the cockpit's job-detail
// pane (height 0 ⇒ no blank-line padding; the pane scrolls via tmux copy-mode).
// Returns an error if the job is not in the pipeline.
func jobDetailText(p *pipeline.Pipeline, jobID string, width int) (string, error) {
	for i := range p.Jobs {
		if p.Jobs[i].ID == jobID {
			return renderPipelineJob(&p.Jobs[i], width, 0), nil
		}
	}
	return "", fmt.Errorf("job %q not found in pipeline %s", jobID, p.ID)
}

// jobDetailBody renders a pipeline job's detail for the in-pane modeDetails
// overlay (height 0 ⇒ no padding; the viewport scrolls). A job whose agent is
// still live shows the full agent detail — parity with pressing i on any agent;
// a terminal or not-yet-spawned job shows its stored detail (prompt/handoff/
// output/digest) since there is no live session to inspect.
func jobDetailBody(j *pipeline.Job, sess *store.Session, width int) string {
	if sess != nil {
		return detailBody(sess, -1, width)
	}
	return renderPipelineJob(j, width, 0)
}

// renderPipelineJob draws one job's full details in the agent pane — used for
// terminal-status jobs whose agent has been reaped (no live tmux to attach).
// width is accepted for symmetry with renderPipeline and reserved for future line-wrapping; it is not yet used.
func renderPipelineJob(j *pipeline.Job, width, height int) string {
	var b strings.Builder
	glyph, st := jobBadge(j.Status)
	b.WriteString(st.Render(glyph) + " job " + j.ID + " — " + st.Render(string(j.Status)) + "\n")
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
