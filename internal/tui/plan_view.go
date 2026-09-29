package tui

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
)

// planDetailText renders a plan's detail view for display.
// projectRoot is the absolute path to the project's root directory, used to
// resolve p.FilePath (which is relative to that root) for YAML task reading.
func planDetailText(p *planstore.Plan, width int, projectRoot string, expanded bool) string {
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

			var icon string
			statusStyle := stMuted
			switch status {
			case "done", "completed":
				icon = "✓"
				statusStyle = stBusy // green
			case "in_progress":
				icon = "▶"
				statusStyle = stRunning // cyan
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

			if expanded {
				if t.LandedPR > 0 {
					b.WriteString(fmt.Sprintf("     %s PR #%d\n", stMuted.Render("landed:"), t.LandedPR))
				}

				if len(t.After) > 0 {
					b.WriteString(fmt.Sprintf("     %s %s\n", stMuted.Render("after:"), strings.Join(t.After, ", ")))
				}

				if t.Prompt != "" {
					wrapWidth := width - 5
					if wrapWidth < 20 {
						wrapWidth = 20
					}
					for _, line := range wrapText(t.Prompt, wrapWidth) {
						b.WriteString("     " + stMuted.Render(line) + "\n")
					}
				}
			}
		}
		b.WriteString("\n" + stMuted.Render("  [t] toggle task details  [↑↓] scroll") + "\n")
	} else if len(p.TaskProgress) > 0 {
		// YAML unavailable — fall back to DB task progress map
		keys := make([]string, 0, len(p.TaskProgress))
		for k := range p.TaskProgress {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			status := p.TaskProgress[k]
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
		b.WriteString("\n" + stMuted.Render("  [t] toggle task details  [↑↓] scroll") + "\n")
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
			if p.Path != "" {
				return p.Path
			}
			return p.ID // for local projects, ID is the absolute path
		}
	}
	return ""
}

// wrapText word-wraps text at the given width, skipping blank lines.
func wrapText(text string, width int) []string {
	if width <= 0 {
		width = 80
	}
	var result []string
	for _, para := range strings.Split(text, "\n") {
		para = strings.TrimRight(para, " \t")
		if para == "" {
			continue
		}
		words := strings.Fields(para)
		if len(words) == 0 {
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) <= width {
				line += " " + w
			} else {
				result = append(result, line)
				line = w
			}
		}
		result = append(result, line)
	}
	return result
}
