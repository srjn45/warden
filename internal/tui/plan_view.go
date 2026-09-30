package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

// promptIndent is the leading whitespace before each expanded task-prompt line.
const promptIndent = "     "

// planDetailText renders a plan's detail view for display.
// Sections: lifecycle, active execution, task evidence, historical summaries.
// projectRoot is retained for call-site compatibility but unused — tasks come
// from the canonical ScrivaDB Plan (ActiveExecution snapshot when present).
func planDetailText(p *planstore.Plan, width int, projectRoot string, expanded bool) string {
	if p == nil {
		return ""
	}
	_ = projectRoot
	var b strings.Builder

	// ── Header / lifecycle ────────────────────────────────────────────────────
	b.WriteString(stHeader.Render(p.Name) + "\n\n")

	writeField := func(label, val string) {
		b.WriteString(fmt.Sprintf("%-16s %s\n", stMuted.Render(label+":"), val))
	}

	b.WriteString(stPaneTitle.Render("Lifecycle") + "\n")
	writeField("Status", string(p.Status))
	writeField("Revision", fmt.Sprintf("%d", p.Revision))
	mode := string(p.ExecutionMode)
	if mode == "" {
		mode = "manual"
	}
	writeField("Executed Using", mode)
	if exec := planstore.ExecutorID(p); exec != "" {
		writeField("Executor", exec)
	}
	ts := planstore.ComputeTaskSummary(p)
	if ts.Total > 0 {
		writeField("Tasks", fmt.Sprintf("%s done (%d in progress, %d pending)", ts.String(), ts.InProgress, ts.Pending))
	}
	writeField("Export", string(planstore.ComputeExportStatus(p)))
	writeField("Created At", p.CreatedAt.Format(time.RFC3339))
	writeField("Updated At", p.UpdatedAt.Format(time.RFC3339))
	if p.StartedAt != nil {
		writeField("Started At", p.StartedAt.Format(time.RFC3339))
	}
	if p.CompletedAt != nil {
		writeField("Completed At", p.CompletedAt.Format(time.RFC3339))
	}
	if p.AutopilotRunID != "" {
		writeField("Autopilot Run", p.AutopilotRunID)
	}
	if p.PipelineID != "" {
		writeField("Pipeline", p.PipelineID)
	}
	if p.OrchestratorID != "" {
		writeField("Orchestrator", p.OrchestratorID)
	}
	writeField("ID", p.ID)
	if p.RepoExport != nil && p.RepoExport.FilePath != "" {
		writeField("Last Export", p.RepoExport.FilePath)
	} else if p.FilePath != "" {
		writeField("Last Export", p.FilePath)
	}

	// ── Active execution ──────────────────────────────────────────────────────
	b.WriteString("\n" + stPaneTitle.Render("Active Execution") + "\n")
	if p.ActiveExecution != nil {
		ae := p.ActiveExecution
		writeField("Execution ID", ae.ID)
		writeField("Mode", string(ae.ExecutionMode))
		if ae.ExecutorID != "" {
			writeField("Executor", ae.ExecutorID)
		}
		writeField("Started", ae.StartedAt.Format(time.RFC3339))
		if ae.TerminalStatus != "" {
			writeField("State", string(ae.TerminalStatus))
		}
		if ae.CompletedAt != nil {
			writeField("Completed", ae.CompletedAt.Format(time.RFC3339))
		}
	} else {
		b.WriteString("  " + stMuted.Render("(no active execution)") + "\n")
	}

	// ── Linked PRs / branches ─────────────────────────────────────────────────
	if len(p.Branches) > 0 || len(p.BranchSummaries) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("Branches / PRs") + "\n")
		if len(p.Branches) > 0 {
			writeField("Branches", strings.Join(p.Branches, ", "))
		}
		for _, bs := range p.BranchSummaries {
			line := bs.Name
			if bs.PR != nil {
				if bs.PR.Number > 0 {
					line += fmt.Sprintf(" → #%d (%s)", bs.PR.Number, bs.PR.State)
				} else if bs.PR.URL != "" {
					line += " → " + bs.PR.URL
				}
			}
			b.WriteString("  " + line + "\n")
		}
	}

	// ── Task evidence ─────────────────────────────────────────────────────────
	b.WriteString("\n" + stPaneTitle.Render("Task Evidence") + "\n")

	tasks := planTasksFromPlan(p)
	if len(tasks) > 0 {
		for i, t := range tasks {
			status := t.Status
			if status == "" {
				if s, ok := p.TaskProgress[t.ID]; ok {
					status = s
				}
			}
			if status == "" {
				status = "pending"
			}

			var icon string
			statusStyle := stMuted
			switch status {
			case "done", "completed":
				icon = "✓"
				statusStyle = stBusy
			case "in_progress":
				icon = "▶"
				statusStyle = stRunning
			case "skipped":
				icon = "—"
				statusStyle = stIdle
			default:
				icon = "·"
			}

			b.WriteString(fmt.Sprintf("\n  %s %d. %s\n",
				statusStyle.Render(icon),
				i+1,
				stHeader.Render(t.ID),
			))

			if outcome, ok := p.TaskOutcomes[t.ID]; ok {
				if outcome.AssignedAgent != "" {
					b.WriteString(fmt.Sprintf("     %s %s\n", stMuted.Render("agent:"), outcome.AssignedAgent))
				}
				if outcome.Branch != "" {
					b.WriteString(fmt.Sprintf("     %s %s\n", stMuted.Render("branch:"), outcome.Branch))
				}
				if len(outcome.PullRequests) > 0 {
					for _, pr := range outcome.PullRequests {
						label := pr.URL
						if pr.Number > 0 {
							label = fmt.Sprintf("#%d %s", pr.Number, pr.State)
						}
						b.WriteString(fmt.Sprintf("     %s %s\n", stMuted.Render("pr:"), label))
					}
				}
				if len(outcome.VerifiedChecks) > 0 {
					b.WriteString(fmt.Sprintf("     %s %s\n", stMuted.Render("checks:"), strings.Join(outcome.VerifiedChecks, ", ")))
				}
			}

			if expanded {
				if t.LandedPR > 0 {
					b.WriteString(fmt.Sprintf("     %s PR #%d\n", stMuted.Render("landed:"), t.LandedPR))
				}
				if len(t.After) > 0 {
					b.WriteString(fmt.Sprintf("     %s %s\n", stMuted.Render("after:"), strings.Join(t.After, ", ")))
				}
				if t.Prompt != "" {
					wrapW := width - lipgloss.Width(promptIndent)
					for _, line := range promptPreview(t.Prompt, 3, wrapW) {
						b.WriteString(promptIndent + stMuted.Render(line) + "\n")
					}
				}
			}
		}
		b.WriteString("\n" + stMuted.Render("  [↑↓] scroll · [t] toggle task details") + "\n")
	} else if len(p.TaskProgress) > 0 || len(p.TaskOutcomes) > 0 {
		keys := make([]string, 0, len(p.TaskProgress)+len(p.TaskOutcomes))
		seen := map[string]bool{}
		for k := range p.TaskProgress {
			keys = append(keys, k)
			seen[k] = true
		}
		for k := range p.TaskOutcomes {
			if !seen[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for i, k := range keys {
			status := p.TaskProgress[k]
			if status == "" {
				if o, ok := p.TaskOutcomes[k]; ok {
					status = o.Status
				}
			}
			var icon string
			statusStyle := stMuted
			switch status {
			case "done", "completed":
				icon = "✓"
				statusStyle = stBusy
			case "in_progress":
				icon = "▶"
				statusStyle = stRunning
			default:
				icon = "·"
			}
			b.WriteString(fmt.Sprintf("\n  %s %d. %s\n",
				statusStyle.Render(icon),
				i+1,
				stHeader.Render(k),
			))
		}
	} else {
		b.WriteString("  " + stMuted.Render("(no tasks defined)") + "\n")
	}

	// ── Historical summaries ──────────────────────────────────────────────────
	b.WriteString("\n" + stPaneTitle.Render("Historical Summaries") + "\n")
	if p.ExecutionSummary != nil {
		s := p.ExecutionSummary
		writeField("Plan", s.PlanName)
		writeField("Mode", string(s.ExecutionMode))
		if s.ExecutorID != "" {
			writeField("Executor", s.ExecutorID)
		}
		writeField("Tasks", fmt.Sprintf("%d/%d done", s.TasksDone, s.TasksTotal))
		if !s.StartedAt.IsZero() {
			writeField("Started", s.StartedAt.Format(time.RFC3339))
		}
		if s.CompletedAt != nil {
			writeField("Completed", s.CompletedAt.Format(time.RFC3339))
		}
		if s.OutcomeNote != "" {
			writeField("Note", s.OutcomeNote)
		}
	}
	if len(p.ExecutionHistory) > 0 {
		b.WriteString("\n" + stMuted.Render("Past executions:") + "\n")
		for i, pe := range p.ExecutionHistory {
			line := fmt.Sprintf("  %d. %s  %s", i+1, pe.ID, pe.ExecutionMode)
			if pe.TerminalStatus != "" {
				line += "  [" + string(pe.TerminalStatus) + "]"
			}
			if pe.ExecutorID != "" {
				line += "  " + pe.ExecutorID
			}
			b.WriteString(line + "\n")
		}
	}
	if p.ExecutionSummary == nil && len(p.ExecutionHistory) == 0 {
		b.WriteString("  " + stMuted.Render("(no historical summaries yet)") + "\n")
	}

	return b.String()
}

// planTasksFromPlan returns canonical Plan tasks from ScrivaDB. Prefer the
// active execution snapshot when present; never consult repository YAML.
func planTasksFromPlan(p *planstore.Plan) []planstore.PlanTaskDef {
	if p == nil {
		return nil
	}
	src := p.Tasks
	if p.ActiveExecution != nil && p.ActiveExecution.Snapshot != nil && len(p.ActiveExecution.Snapshot.Tasks) > 0 {
		src = p.ActiveExecution.Snapshot.Tasks
	}
	if len(src) == 0 {
		return nil
	}
	out := make([]planstore.PlanTaskDef, 0, len(src))
	for _, t := range src {
		out = append(out, planstore.PlanTaskDef{
			ID:     t.ID,
			Prompt: t.Prompt,
			After:  append([]string(nil), t.After...),
		})
	}
	return out
}

// projectRootForID returns the absolute path of the project with the given ID,
// or "" if not found.
func projectRootForID(projects []projectstore.Project, projectID string) string {
	for _, p := range projects {
		if p.ID == projectID {
			if p.Path != "" {
				return p.Path
			}
			return p.ID // for local projects, ID is the absolute path
		}
	}
	return ""
}

// promptPreview returns up to maxLines non-empty display lines from a
// (possibly multi-line) YAML scalar, trimming blank lines and word-wrapping
// each source line at width cells. width <= 0 disables wrapping.
func promptPreview(prompt string, maxLines, width int) []string {
	var out []string
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		for _, wrapped := range wrapPromptLine(line, width) {
			out = append(out, wrapped)
			if len(out) >= maxLines {
				return out
			}
		}
	}
	return out
}

// wrapPromptLine word-wraps s to at most width cells. width <= 0 returns s as-is.
func wrapPromptLine(s string, width int) []string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return []string{s}
	}
	return strings.Split(lipgloss.NewStyle().Width(width).Render(s), "\n")
}
