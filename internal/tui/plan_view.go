package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/planstore"
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
		fmt.Println(planDetailText(p, 100))
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
func planDetailText(p *planstore.Plan, width int) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(stHeader.Render("Plan: "+p.Name) + "\n\n")

	writeField := func(label, val string) {
		b.WriteString(fmt.Sprintf("%-16s %s\n", stMuted.Render(label+":"), val))
	}

	writeField("ID", p.ID)
	writeField("Project ID", p.ProjectID)
	writeField("File Path", p.FilePath)
	writeField("Status", string(p.Status))

	mode := string(p.ExecutionMode)
	if mode == "" {
		mode = "manual"
	}
	writeField("Execution Mode", mode)

	// Linked IDs
	b.WriteString("\n" + stPaneTitle.Render("Execution Links") + "\n")
	runID := p.AutopilotRunID
	if runID == "" {
		runID = "—"
	}
	writeField("Autopilot Run", runID)

	pipeID := p.PipelineID
	if pipeID == "" {
		pipeID = "—"
	}
	writeField("Pipeline", pipeID)

	orchID := p.OrchestratorID
	if orchID == "" {
		orchID = "—"
	}
	writeField("Orchestrator", orchID)

	// Task Progress
	b.WriteString("\n" + stPaneTitle.Render("Task Progress") + "\n")
	if len(p.TaskProgress) == 0 {
		b.WriteString("  " + stMuted.Render("(no task progress recorded)") + "\n")
	} else {
		keys := make([]string, 0, len(p.TaskProgress))
		for k := range p.TaskProgress {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(fmt.Sprintf("  • %-16s %s\n", k, p.TaskProgress[k]))
		}
	}

	// Timestamps
	b.WriteString("\n" + stPaneTitle.Render("Timestamps") + "\n")
	writeField("Created At", p.CreatedAt.Format(time.RFC3339))
	writeField("Updated At", p.UpdatedAt.Format(time.RFC3339))
	if p.StartedAt != nil {
		writeField("Started At", p.StartedAt.Format(time.RFC3339))
	} else {
		writeField("Started At", "—")
	}
	if p.CompletedAt != nil {
		writeField("Completed At", p.CompletedAt.Format(time.RFC3339))
	} else {
		writeField("Completed At", "—")
	}

	return b.String()
}
