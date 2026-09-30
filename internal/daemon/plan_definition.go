package daemon

import (
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/planstore"
)

// planDefinitionForExecution returns the immutable definition an executor
// should run. Prefer ActiveExecution.Snapshot when present; otherwise the live
// Plan fields (used at the moment run_plan stamps the snapshot).
func planDefinitionForExecution(p *planstore.Plan) *planstore.ExecutionSnapshot {
	if p == nil {
		return nil
	}
	if p.ActiveExecution != nil && p.ActiveExecution.Snapshot != nil {
		return p.ActiveExecution.Snapshot
	}
	return planstore.SnapshotFromPlan(p)
}

// formatCanonicalPlanBody renders a human-readable plan brief from a snapshot
// for orchestrator/manual agent prompts. Never reads repository YAML.
func formatCanonicalPlanBody(snap *planstore.ExecutionSnapshot) string {
	if snap == nil {
		return "(empty plan definition)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", snap.Name)
	fmt.Fprintf(&b, "goal: %s\n", snap.Goal)
	if len(snap.Constraints) > 0 {
		b.WriteString("constraints:\n")
		for _, c := range snap.Constraints {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	if len(snap.DoneWhen) > 0 {
		b.WriteString("done_when:\n")
		for _, d := range snap.DoneWhen {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
	}
	if len(snap.Tasks) > 0 {
		b.WriteString("tasks:\n")
		for _, t := range snap.Tasks {
			fmt.Fprintf(&b, "  - id: %s\n    prompt: %s\n", t.ID, t.Prompt)
			if len(t.After) > 0 {
				fmt.Fprintf(&b, "    after: [%s]\n", strings.Join(t.After, ", "))
			}
		}
	}
	return b.String()
}
