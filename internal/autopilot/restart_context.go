package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Restart reason kinds (docs/specs/2026-10-05-plan-restart-and-watchdog.md §4.1).
const (
	RestartReasonOperatorStop    = "operator_stop"
	RestartReasonNeedsAttention  = "needs_attention"
	RestartReasonDegradedBackoff = "degraded_backoff"
	RestartReasonOperatorForce   = "operator_force"
	RestartReasonWatchdog        = "watchdog"
	RestartReasonUnknown         = "unknown"
	restartContextHeading        = "## Restart context"
	defaultRestartJournalLimit   = 20
	restartContextKeySuffix      = "restart_context"
	restartContextWriter         = ledgerWriter
)

// RestartContext is the durable hand-off a restarted run's new agents receive.
type RestartContext struct {
	RestartCount int       `json:"restart_count"`
	RestartedAt  time.Time `json:"restarted_at"`
	Reason       string    `json:"reason"`
	ReasonKind   string    `json:"reason_kind,omitempty"`

	FinishedTasks []RestartFinishedTask   `json:"finished_tasks"`
	Unfinished    []RestartUnfinishedTask `json:"unfinished_tasks"`
	Journal       []JournalEntry          `json:"journal"` // newest first, bounded
}

// RestartFinishedTask is a task that already merged; it must not be redone.
type RestartFinishedTask struct {
	ID       string `json:"id"`
	PR       int    `json:"pr,omitempty"`
	MergeSHA string `json:"merge_sha,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

// RestartUnfinishedTask is a task the new run must continue.
type RestartUnfinishedTask struct {
	ID                 string `json:"id"`
	PreviousBranch     string `json:"previous_branch,omitempty"`
	HasUnmergedCommits bool   `json:"has_unmerged_commits"`
	OpenPR             int    `json:"open_pr,omitempty"`
	PRBase             string `json:"pr_base,omitempty"`
}

// RestartContextKey is the shared-context key for an autopilot run.
func RestartContextKey(runID string) string {
	return "autopilot." + runID + "." + restartContextKeySuffix
}

// PlanRestartContextKey is the mode-agnostic key for pipeline-only plans (OQ-2).
func PlanRestartContextKey(planID string) string {
	return "plan." + planID + "." + restartContextKeySuffix
}

// BranchFacts is what a prober learns about one task branch.
type BranchFacts struct {
	HasUnmergedCommits bool   // commits not reachable from the integration branch
	OpenPR             int    // open PR number for the branch, 0 when none
	PRBase             string // that PR's base branch
}

// RestartBranchProber inspects git/GitHub state for a kept worker branch. It is
// an interface so assembly is unit-testable and the daemon owns the git calls.
type RestartBranchProber interface {
	BranchFacts(ctx context.Context, branch, integrationBranch string) (BranchFacts, error)
}

// RestartAssembleInput is everything AssembleRestartContext reads.
type RestartAssembleInput struct {
	Ledger            *Ledger
	PlanTasks         []PlanTask // optional: plan-level task states (done w/ landed PR, unfinished)
	IntegrationBranch string
	Prober            RestartBranchProber // nil → unmerged/PR facts unknown (false/0)
	Reason            string
	ReasonKind        string
	JournalLimit      int       // 0 → defaultRestartJournalLimit
	PreviousCount     int       // restart count already recorded
	CountRestart      bool      // true for operator/API restart (OQ-6); false leaves count unchanged
	Now               time.Time // zero → time.Now()
}

// AssembleRestartContext builds the context from ledger state. Probe errors
// degrade that task's facts rather than failing the restart.
func AssembleRestartContext(ctx context.Context, in RestartAssembleInput) (*RestartContext, error) {
	if in.Ledger == nil {
		return nil, errors.New("autopilot: restart context needs a ledger")
	}
	tasks, err := in.Ledger.Tasks()
	if err != nil {
		return nil, fmt.Errorf("restart context: tasks: %w", err)
	}
	landings, err := in.Ledger.Landings()
	if err != nil {
		return nil, fmt.Errorf("restart context: landings: %w", err)
	}
	journal, err := in.Ledger.Journal()
	if err != nil {
		return nil, fmt.Errorf("restart context: journal: %w", err)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	limit := in.JournalLimit
	if limit <= 0 {
		limit = defaultRestartJournalLimit
	}
	kind := in.ReasonKind
	if kind == "" {
		kind = RestartReasonUnknown
	}
	rc := &RestartContext{
		RestartCount:  in.PreviousCount,
		RestartedAt:   now.UTC(),
		Reason:        in.Reason,
		ReasonKind:    kind,
		FinishedTasks: []RestartFinishedTask{},
		Unfinished:    []RestartUnfinishedTask{},
		Journal:       boundJournalNewestFirst(journal, limit),
	}
	if in.CountRestart {
		rc.RestartCount++
	}

	seen := map[string]bool{}
	for _, t := range tasks {
		seen[t.ID] = true
		if t.State == LedgerLanded {
			ft := RestartFinishedTask{ID: t.ID, PR: t.PR, Branch: t.Branch}
			if ld, ok := matchLanding(landings, t); ok {
				ft.MergeSHA = ld.SHA
				if ft.PR == 0 {
					ft.PR = ld.PR
				}
			}
			rc.FinishedTasks = append(rc.FinishedTasks, ft)
			continue
		}
		ut := RestartUnfinishedTask{ID: t.ID, PreviousBranch: t.Branch, OpenPR: t.PR}
		if t.Branch != "" && in.Prober != nil {
			if f, perr := in.Prober.BranchFacts(ctx, t.Branch, in.IntegrationBranch); perr == nil {
				ut.HasUnmergedCommits = f.HasUnmergedCommits
				if f.OpenPR != 0 {
					ut.OpenPR = f.OpenPR
				}
				ut.PRBase = f.PRBase
			}
		}
		rc.Unfinished = append(rc.Unfinished, ut)
	}
	for _, pt := range in.PlanTasks {
		if seen[pt.ID] {
			continue
		}
		if pt.Status == TaskStatusDone {
			rc.FinishedTasks = append(rc.FinishedTasks, RestartFinishedTask{ID: pt.ID, PR: pt.LandedPR})
		} else {
			rc.Unfinished = append(rc.Unfinished, RestartUnfinishedTask{ID: pt.ID})
		}
	}
	return rc, nil
}

func matchLanding(landings []Landing, t LedgerTask) (Landing, bool) {
	for _, ld := range landings {
		if (t.PR != 0 && ld.PR == t.PR) || (t.Branch != "" && ld.Branch == t.Branch) {
			return ld, true
		}
	}
	return Landing{}, false
}

// boundJournalNewestFirst returns up to limit entries ordered newest first. The
// ledger journal is written newest-first, but a writer may append; sort by At
// (RFC3339 strings order lexically) only when the input is not already ordered.
func boundJournalNewestFirst(j []JournalEntry, limit int) []JournalEntry {
	out := make([]JournalEntry, len(j))
	copy(out, j)
	for i := 1; i < len(out); i++ { // stable insertion sort, newest At first
		for k := i; k > 0 && out[k].At > out[k-1].At; k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// PersistRestartContext writes rc at key so it survives a daemon restart.
func PersistRestartContext(store CtxStore, key string, rc *RestartContext) error {
	raw, err := json.Marshal(rc)
	if err != nil {
		return err
	}
	return store.Set(key, string(raw), restartContextWriter)
}

// LoadRestartContext reads the context at key; (nil, nil) when absent.
func LoadRestartContext(store CtxStore, key string) (*RestartContext, error) {
	raw, err := store.Get(key)
	if errors.Is(err, ErrLedgerMissing) || (err == nil && strings.TrimSpace(raw) == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rc RestartContext
	if err := json.Unmarshal([]byte(raw), &rc); err != nil {
		return nil, fmt.Errorf("autopilot: restart context %s: decode: %w", key, err)
	}
	return &rc, nil
}

// CaptureRestartContext assembles and persists the context at key. Callers
// (RestartRun) invoke it BEFORE any teardown. PreviousCount is read from the
// stored context so the counter survives repeated restarts.
func CaptureRestartContext(ctx context.Context, store CtxStore, key string, in RestartAssembleInput) (*RestartContext, error) {
	if prev, err := LoadRestartContext(store, key); err == nil && prev != nil {
		in.PreviousCount = prev.RestartCount
	}
	rc, err := AssembleRestartContext(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := PersistRestartContext(store, key, rc); err != nil {
		return nil, fmt.Errorf("restart context: persist: %w", err)
	}
	return rc, nil
}

// RenderRestartContext renders the delimited prompt section, or "" for nil.
func RenderRestartContext(rc *RestartContext) string {
	if rc == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(restartContextHeading + "\n")
	fmt.Fprintf(&b, "This run was restarted (restart #%d at %s). Reason: %s", rc.RestartCount, rc.RestartedAt.Format(time.RFC3339), orUnknown(rc.ReasonKind))
	if strings.TrimSpace(rc.Reason) != "" {
		fmt.Fprintf(&b, " — %s", strings.TrimSpace(rc.Reason))
	}
	b.WriteString("\nAll previous agents were replaced; the work below was preserved.\n\n")

	b.WriteString("### Finished tasks (do not redo)\n")
	if len(rc.FinishedTasks) == 0 {
		b.WriteString("(none)\n")
	}
	for _, f := range rc.FinishedTasks {
		line := "- " + f.ID
		if f.PR != 0 {
			line += fmt.Sprintf(" pr=#%d", f.PR)
		}
		if f.MergeSHA != "" {
			line += " merge=" + shortSHA(f.MergeSHA)
		}
		if f.Branch != "" {
			line += " branch=" + f.Branch
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n### Unfinished tasks\n")
	if len(rc.Unfinished) == 0 {
		b.WriteString("(none)\n")
	}
	for _, u := range rc.Unfinished {
		line := "- " + u.ID
		if u.PreviousBranch != "" {
			line += fmt.Sprintf(" previous_branch=%s unmerged_commits=%t", u.PreviousBranch, u.HasUnmergedCommits)
		} else {
			line += " (no previous branch)"
		}
		if u.OpenPR != 0 {
			line += fmt.Sprintf(" open_pr=#%d", u.OpenPR)
			if u.PRBase != "" {
				line += " pr_base=" + u.PRBase
			}
		}
		b.WriteString(line + "\n")
	}

	if len(rc.Journal) > 0 {
		b.WriteString("\n### Previous decision journal (newest first)\n")
		for _, j := range rc.Journal {
			fmt.Fprintf(&b, "- %s %s\n", j.At, j.Note)
		}
	}

	b.WriteString("\n### Instructions\n")
	b.WriteString("1. Finished tasks must not be redone.\n")
	b.WriteString("2. For an unfinished task with a kept branch: create the worktree from that branch and continue from its existing commits.\n")
	b.WriteString("3. If an open PR exists for that branch: reuse it (push to it); never open a duplicate PR.\n")
	b.WriteString("4. If the kept work is unusable: report that, start again from the integration branch, and close the old PR with a comment.\n")
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return RestartReasonUnknown
	}
	return s
}

// RestartContextSection loads and renders the run's restart context for
// injection into a worker spawn prompt; "" when absent or unreadable.
func (c *Controller) RestartContextSection(runID string) string {
	if c.runtime == nil {
		return ""
	}
	rc, err := c.runtime.NewLedger(runID).LoadRestartContext() // nil-safe
	if err != nil || rc == nil {
		return ""
	}
	return RenderRestartContext(rc)
}

// AppendRestartContext appends a rendered restart section to a worker prompt.
// Empty section is a no-op; an already-present section is not duplicated.
func AppendRestartContext(prompt, section string) string {
	if strings.TrimSpace(section) == "" || strings.Contains(prompt, restartContextHeading) {
		return prompt
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return section
	}
	return prompt + "\n\n" + section
}
