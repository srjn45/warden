package autopilot

import (
	"context"
	"fmt"
	"strings"
)

// AgentInfo is the digest's view of one live autopilot-owned agent, projected
// from the daemon's session store (a filtered `list_agents`).
type AgentInfo struct {
	ID     string
	Name   string
	Role   string
	State  string
	Branch string
	Tags   []string
}

// AuditEntry is one recent audit line for the run, newest-first.
type AuditEntry struct {
	Time   string
	Action string
	Target string
	Detail string
}

// DigestSources supplies the live inputs the recovery digest needs beyond the
// plan file and the ledger: the run's current agents and its recent audit trail
// (autopilot.md §4). It is an interface so the Controller stays decoupled from
// the daemon store/audit and tests drive digest composition with fixtures.
type DigestSources interface {
	// RunAgents returns the agents currently tagged for runID (the live
	// list_agents filtered to `run:<run_id>`).
	RunAgents(ctx context.Context, runID string) ([]AgentInfo, error)
	// RecentAudit returns the run's most recent audit entries, newest-first, up to
	// limit. Best-effort: an empty slice is fine (it only degrades observability).
	RecentAudit(ctx context.Context, runID string, limit int) ([]AuditEntry, error)
}

// digestAuditLimit caps how many recent audit lines the recovery digest carries.
// Enough to reconstruct what just happened without flooding the opening brief.
const digestAuditLimit = 20

// DigestInput is everything ComposeDigest reads to build a brain's opening brief.
type DigestInput struct {
	RunID             string
	Repo              string
	PlanFile          string // absolute path shown to the brain so it can re-read on change
	Plan              Plan   // the last-good decoded plan
	Ledger            *Ledger
	Sources           DigestSources
	IntegrationBranch string // resolved per-plan merge target workers must base PRs on
}

// ComposeDigest builds the brain's recovery digest (autopilot.md §4): the plan,
// the task ledger, the landings, the live agents, and the recent audit — so a
// freshly (re)spawned brain reconstructs run state from durable facts instead of
// a prior brain's context. Ledger/Sources reads are best-effort: a read error
// degrades that section to a note rather than failing the whole brief, because a
// missing digest section must never block a brain (re)spawn.
func ComposeDigest(ctx context.Context, in DigestInput) (string, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "# Autopilot run digest — %s\n\n", in.RunID)
	fmt.Fprintf(&b, "Repo: %s\n", in.Repo)
	fmt.Fprintf(&b, "Plan file: %s (re-read it when it changes)\n", in.PlanFile)
	if branch := strings.TrimSpace(in.IntegrationBranch); branch != "" {
		fmt.Fprintf(&b, "Integration branch: %s\n", branch)
		fmt.Fprintf(&b, "Workers MUST open PRs based on this branch (never main). Include this in every worker spawn prompt:\n")
		fmt.Fprintf(&b, "  %s\n", WorkerSpawnBranchPrompt(branch))
	}
	b.WriteString("\n")

	b.WriteString("## Goal\n")
	fmt.Fprintf(&b, "%s\n\n", strings.TrimSpace(in.Plan.Goal))

	if len(in.Plan.Constraints) > 0 {
		b.WriteString("## Constraints\n")
		for _, c := range in.Plan.Constraints {
			fmt.Fprintf(&b, "- %s\n", c)
		}
		b.WriteString("\n")
	}

	if len(in.Plan.DoneWhen) > 0 {
		b.WriteString("## Done when\n")
		for _, d := range in.Plan.DoneWhen {
			fmt.Fprintf(&b, "- %s\n", d)
		}
		b.WriteString("\n")
	}

	writePlanTasks(&b, in.Plan.Tasks)
	writeLedgerTasks(&b, in.Ledger)
	writeLandings(&b, in.Ledger)
	writeAgents(ctx, &b, in)
	writeAudit(ctx, &b, in)

	b.WriteString("\nYou may have been restarted: verify before re-issuing anything")
	b.WriteString(" (`land` is idempotent; `list_agents` shows what already exists).\n")
	return b.String(), nil
}

// writePlanTasks renders the plan's authored tasks (owner's coarse decomposition).
func writePlanTasks(b *strings.Builder, tasks []PlanTask) {
	if len(tasks) == 0 {
		return
	}
	b.WriteString("## Plan tasks\n")
	for _, t := range tasks {
		state := t.Status
		if state == "" {
			state = TaskStatusPending
		}
		evidence := ""
		if t.LandedPR > 0 {
			evidence = fmt.Sprintf(", landed PR #%d", t.LandedPR)
		}
		if len(t.After) > 0 {
			fmt.Fprintf(b, "- [%s%s] %s (after: %s): %s\n", state, evidence, t.ID, strings.Join(t.After, ", "), t.Prompt)
		} else {
			fmt.Fprintf(b, "- [%s%s] %s: %s\n", state, evidence, t.ID, t.Prompt)
		}
	}
	b.WriteString("\n")
}

// writeLedgerTasks renders the brain-written task ledger (live task state).
func writeLedgerTasks(b *strings.Builder, l *Ledger) {
	b.WriteString("## Task ledger\n")
	if l == nil {
		b.WriteString("(no ledger available)\n\n")
		return
	}
	tasks, err := l.Tasks()
	if err != nil {
		fmt.Fprintf(b, "(ledger read error: %v)\n\n", err)
		return
	}
	if len(tasks) == 0 {
		b.WriteString("(empty — decompose the goal and populate it)\n\n")
		return
	}
	for _, t := range tasks {
		line := fmt.Sprintf("- %s [%s]", t.ID, t.State)
		if t.WorkerID != "" {
			line += " worker=" + t.WorkerID
		}
		if t.Branch != "" {
			line += " branch=" + t.Branch
		}
		if t.PR != 0 {
			line += fmt.Sprintf(" pr=#%d", t.PR)
		}
		if t.Note != "" {
			line += " — " + t.Note
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

// writeLandings renders the authoritative landings ledger (what already merged).
func writeLandings(b *strings.Builder, l *Ledger) {
	if l == nil {
		return
	}
	landings, err := l.Landings()
	if err != nil {
		fmt.Fprintf(b, "## Landings\n(ledger read error: %v)\n\n", err)
		return
	}
	if len(landings) == 0 {
		return
	}
	b.WriteString("## Landings (already merged — do not re-land)\n")
	for _, ld := range landings {
		line := fmt.Sprintf("- %s @ %s", ld.Branch, shortSHA(ld.SHA))
		if ld.PR != 0 {
			line += fmt.Sprintf(" pr=#%d", ld.PR)
		}
		if ld.LandedAt != "" {
			line += " at " + ld.LandedAt
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

// writeAgents renders the live autopilot-owned agents for this run.
func writeAgents(ctx context.Context, b *strings.Builder, in DigestInput) {
	b.WriteString("## Live agents\n")
	if in.Sources == nil {
		b.WriteString("(no agent source available)\n\n")
		return
	}
	agents, err := in.Sources.RunAgents(ctx, in.RunID)
	if err != nil {
		fmt.Fprintf(b, "(list_agents error: %v)\n\n", err)
		return
	}
	if len(agents) == 0 {
		b.WriteString("(none — no workers in flight)\n\n")
		return
	}
	for _, a := range agents {
		label := a.ID
		if a.Name != "" {
			label = a.Name + " (" + a.ID + ")"
		}
		line := fmt.Sprintf("- %s", label)
		if a.Role != "" {
			line += " role=" + a.Role
		}
		if a.State != "" {
			line += " state=" + a.State
		}
		if a.Branch != "" {
			line += " branch=" + a.Branch
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

// writeAudit renders the run's recent audit trail (newest-first).
func writeAudit(ctx context.Context, b *strings.Builder, in DigestInput) {
	if in.Sources == nil {
		return
	}
	entries, err := in.Sources.RecentAudit(ctx, in.RunID, digestAuditLimit)
	if err != nil {
		fmt.Fprintf(b, "## Recent audit\n(audit read error: %v)\n\n", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	b.WriteString("## Recent audit (newest first)\n")
	for _, e := range entries {
		line := fmt.Sprintf("- %s %s", e.Time, e.Action)
		if e.Target != "" {
			line += " " + e.Target
		}
		if e.Detail != "" {
			line += " (" + e.Detail + ")"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
}

// shortSHA trims a git SHA to 12 chars for compact display, leaving shorter
// values (or empty) untouched.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
