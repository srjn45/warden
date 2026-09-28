package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

// RunPlanDetailPane renders one plan's stored detail to stdout and blocks,
// so the cockpit's agent pane can show a plan's detail view instead of a blank attach.
// tmux replaces this process via respawn-pane when the user selects another item;
// scrolling uses copy-mode.
func RunPlanDetailPane(a api, projectID, planID string) error {
	p, err := a.PlanGet(context.Background(), projectID, planID)
	if err != nil {
		fmt.Println(stMuted.Render("could not load plan detail: " + err.Error()))
	} else {
		// The subprocess runs in the tmux pane whose cwd is the project root,
		// so passing "" falls back to filepath.Abs which resolves correctly.
		fmt.Println(planDetailText(p, 100, ""))
	}
	select {} // hold the pane open until tmux respawns it
}

// respawnPlanDetailArgs builds the tmux command that replaces the agent pane with
// a render of one plan's stored detail.
func respawnPlanDetailArgs(agentPane, self, projectID, planID string) []string {
	return []string{"respawn-pane", "-k", "-t", agentPane,
		self + " tui --pane=plandetail --pipeline=" + projectID + " --job=" + planID}
}

// openPlanDetailCmd renders a plan's stored detail into the agent pane.
func openPlanDetailCmd(agentPane, projectID, planID string) tea.Cmd {
	return func() tea.Msg {
		self, err := os.Executable()
		if err != nil {
			return attachDoneMsg{err: err}
		}
		return attachDoneMsg{err: exec.Command("tmux", respawnPlanDetailArgs(agentPane, self, projectID, planID)...).Run()}
	}
}

// planDetailText renders a plan's detail view for display.
// projectRoot is the absolute path to the project's root directory, used to
// resolve p.FilePath (which is relative to that root) for YAML task reading.
func planDetailText(p *planstore.Plan, width int, projectRoot string) string {
	if p == nil {
		return ""
	}
	var b strings.Builder

	// ── Header ────────────────────────────────────────────────────────────────
	b.WriteString(stHeader.Render(p.Name) + "\n\n")

	writeField := func(label, val string) {
		b.WriteString(fmt.Sprintf("%-16s %s\n", stMuted.Render(label+":"), val))
	}

	writeField("Status", string(p.Status))
	mode := string(p.ExecutionMode)
	if mode == "" {
		mode = "manual"
	}
	writeField("Executed Using", mode)
	writeField("Created At", p.CreatedAt.Format(time.RFC3339))
	writeField("Updated At", p.UpdatedAt.Format(time.RFC3339))
	if p.StartedAt != nil {
		writeField("Started At", p.StartedAt.Format(time.RFC3339))
	}
	if p.CompletedAt != nil {
		writeField("Completed At", p.CompletedAt.Format(time.RFC3339))
	}

	// Execution links (shown only when set)
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
	writeField("File", p.FilePath)

	// ── Tasks ─────────────────────────────────────────────────────────────────
	b.WriteString("\n" + stPaneTitle.Render("Tasks") + "\n")

	tasks, _ := planTasksFromPlan(p, projectRoot)
	if len(tasks) > 0 {
		for i, t := range tasks {
			// status: prefer YAML field, fall back to DB TaskProgress map
			status := t.Status
			if status == "" {
				if s, ok := p.TaskProgress[t.ID]; ok {
					status = s
				}
			}
			if status == "" {
				status = "pending"
			}

			statusStyle := stMuted
			switch status {
			case "done", "completed":
				statusStyle = stBusy // green
			case "in_progress":
				statusStyle = stRunning // cyan
			case "skipped":
				statusStyle = stIdle
			}

			b.WriteString(fmt.Sprintf("\n  %d. %s  %s\n",
				i+1,
				stHeader.Render(t.ID),
				statusStyle.Render("["+status+"]"),
			))

			if t.LandedPR > 0 {
				b.WriteString(fmt.Sprintf("     %s PR #%d\n", stMuted.Render("landed:"), t.LandedPR))
			}

			if len(t.After) > 0 {
				b.WriteString(fmt.Sprintf("     %s %s\n", stMuted.Render("after:"), strings.Join(t.After, ", ")))
			}

			if t.Prompt != "" {
				for _, line := range promptPreview(t.Prompt, 3) {
					b.WriteString("     " + stMuted.Render(line) + "\n")
				}
			}
		}
	} else if len(p.TaskProgress) > 0 {
		// YAML unavailable — fall back to DB task progress map
		keys := make([]string, 0, len(p.TaskProgress))
		for k := range p.TaskProgress {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			status := p.TaskProgress[k]
			statusStyle := stMuted
			switch status {
			case "done", "completed":
				statusStyle = stBusy
			case "in_progress":
				statusStyle = stRunning
			}
			b.WriteString(fmt.Sprintf("\n  %d. %s  %s\n",
				i+1,
				stHeader.Render(k),
				statusStyle.Render("["+status+"]"),
			))
		}
	} else {
		b.WriteString("  " + stMuted.Render("(no tasks defined)") + "\n")
	}

	return b.String()
}

// planTasksFromPlan loads YAML tasks for a plan.
// projectRoot is the absolute path of the project root; p.FilePath is relative to it.
func planTasksFromPlan(p *planstore.Plan, projectRoot string) ([]planstore.PlanTaskDef, error) {
	if p.FilePath == "" {
		return nil, nil
	}
	var abs string
	if filepath.IsAbs(p.FilePath) {
		abs = p.FilePath
	} else if projectRoot != "" {
		abs = filepath.Join(projectRoot, p.FilePath)
	} else {
		var err error
		abs, err = filepath.Abs(p.FilePath)
		if err != nil {
			return nil, err
		}
	}
	return planstore.ReadPlanTasks(abs)
}

// projectRootForID returns the absolute path of the project with the given ID,
// or "" if not found.
func projectRootForID(projects []projectstore.Project, projectID string) string {
	for _, p := range projects {
		if p.ID == projectID {
			return p.Path
		}
	}
	return ""
}

// promptPreview returns up to maxLines non-empty lines from a (possibly
// multi-line) YAML scalar, trimming leading/trailing blank lines.
func promptPreview(prompt string, maxLines int) []string {
	var out []string
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) >= maxLines {
			break
		}
	}
	return out
}
