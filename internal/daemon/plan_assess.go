package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/planstore"
)

// assessPlanProgress runs brain-assisted task progress assessment for one plan
// (D9). It reads the plan YAML, builds an evidence string from recent git
// commits and open PRs, and calls the consultor. The returned map is
// task_id → status; a nil map means the brain chose not to update progress
// (e.g. it returned noop or the YAML had no tasks).
func assessPlanProgress(
	ctx context.Context,
	plan *planstore.Plan,
	root string,
	consultor brainconsult.Consultor,
) (map[string]string, error) {
	absPath := filepath.Join(root, plan.FilePath)
	p, err := autopilot.LoadPlan(absPath)
	if err != nil {
		return nil, fmt.Errorf("read plan yaml: %w", err)
	}

	situation := buildAssessSituation(plan.Name, p.Tasks)
	evidence := buildAssessEvidence(ctx, root)

	req := brainconsult.Request{
		Intent:    "plan_progress_assessment",
		Situation: situation,
		Evidence:  evidence,
		Allowed:   []brainconsult.Action{brainconsult.ActionUpdateTaskProgress},
		Repo:      root,
	}
	result, err := consultor.Consult(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("brain assess: %w", err)
	}
	if result.Action != brainconsult.ActionUpdateTaskProgress {
		return nil, nil
	}
	return result.TaskProgress, nil
}

// buildAssessSituation builds the brain situation string: plan name + task list.
func buildAssessSituation(planName string, tasks []autopilot.PlanTask) string {
	var sb strings.Builder
	sb.WriteString("Plan: ")
	sb.WriteString(planName)
	if len(tasks) == 0 {
		sb.WriteString("\nNo tasks defined in plan YAML.")
		return sb.String()
	}
	sb.WriteString(fmt.Sprintf("\nTasks (%d):\n", len(tasks)))
	for _, t := range tasks {
		sb.WriteString(fmt.Sprintf("  - id: %s\n    prompt: %s\n", t.ID, t.Prompt))
	}
	return sb.String()
}

// buildAssessEvidence collects the last 50 commits on origin/main and open PR
// titles from gh. Errors are swallowed — partial evidence is better than none.
func buildAssessEvidence(ctx context.Context, root string) string {
	var sb strings.Builder

	// Git log: last 50 commits on origin/main.
	gitOut, err := exec.CommandContext(ctx, "git", "-C", root,
		"log", "--oneline", "origin/main", "-50").Output()
	if err == nil && len(gitOut) > 0 {
		sb.WriteString("Recent commits (origin/main -50):\n")
		sb.Write(gitOut)
	} else {
		// Fallback: local HEAD log.
		gitOut, err = exec.CommandContext(ctx, "git", "-C", root,
			"log", "--oneline", "-50").Output()
		if err == nil && len(gitOut) > 0 {
			sb.WriteString("Recent commits (HEAD -50):\n")
			sb.Write(gitOut)
		}
	}

	// Open PR titles from gh.
	ghOut, err := exec.CommandContext(ctx, "gh", "pr", "list",
		"--state", "open", "--json", "title,number",
		"--template", "{{range .}}#{{.number}} {{.title}}\n{{end}}").Output()
	if err == nil && len(ghOut) > 0 {
		sb.WriteString("\nOpen PRs:\n")
		sb.Write(ghOut)
	}

	if sb.Len() == 0 {
		return "(no git or PR evidence available)"
	}
	return sb.String()
}
