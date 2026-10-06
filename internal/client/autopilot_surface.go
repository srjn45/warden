package client

import (
	"fmt"
	"strings"
)

// AutopilotDiagnosis is the guardian's last triage decision for a run.
type AutopilotDiagnosis struct {
	Action         string  `json:"action"`
	Confidence     float64 `json:"confidence"`
	Rationale      string  `json:"rationale,omitempty"`
	FailOpenReason string  `json:"fail_open_reason,omitempty"`
	Source         string  `json:"source,omitempty"`
	Outcome        string  `json:"outcome,omitempty"`
	At             string  `json:"at"`
}

// AutopilotFixStatus is one task's gate / fix-loop state.
type AutopilotFixStatus struct {
	Task              string `json:"task"`
	PR                int    `json:"pr,omitempty"`
	Gate              string `json:"gate"` // red | conflict | clear
	Kind              string `json:"kind,omitempty"`
	RedStreak         int    `json:"red_streak"`
	FixAttempts       int    `json:"fix_attempts"`
	Reruns            int    `json:"reruns,omitempty"`
	Fixing            bool   `json:"fixing,omitempty"`
	LastDispatchedSHA string `json:"last_dispatched_sha,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

// AutopilotResolverStatus summarises resolver-agent activity for a run.
type AutopilotResolverStatus struct {
	Attempts    int    `json:"attempts"`
	LastClass   string `json:"last_class,omitempty"`
	LastTask    string `json:"last_task,omitempty"`
	LastBranch  string `json:"last_branch,omitempty"`
	LastOutcome string `json:"last_outcome,omitempty"`
	LastAt      string `json:"last_at,omitempty"`
}

// AutopilotFinalPR is the single final PR (integration → default branch).
type AutopilotFinalPR struct {
	Number      int    `json:"number"`
	URL         string `json:"url,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	Gate        string `json:"gate"` // pending | red | green
	FixAttempts int    `json:"fix_attempts"`
	State       string `json:"state,omitempty"` // open | merged | closed
	MergedAt    string `json:"merged_at,omitempty"`
}

// AutopilotAwaitingMerge is the persisted wait-for-merge record.
type AutopilotAwaitingMerge struct {
	Since    string `json:"since"`
	GreenSHA string `json:"green_sha,omitempty"`
	Notified bool   `json:"notified"`
	PR       int    `json:"pr,omitempty"`
}

// SurfaceBadge is a compact one-line summary of the operator status surface for
// a run row (TUI): final PR, open fixes, resolver. Empty when there is nothing.
func (r AutopilotRunStatus) SurfaceBadge() string {
	var parts []string
	if r.State == "awaiting_merge" && r.FinalPR != nil {
		parts = append(parts, fmt.Sprintf("awaiting final PR merge (#%d)", r.FinalPR.Number))
	} else if r.AwaitingMerge != nil && r.AwaitingMerge.PR > 0 {
		parts = append(parts, fmt.Sprintf("awaiting final PR merge (#%d)", r.AwaitingMerge.PR))
	} else if r.FinalPR != nil {
		parts = append(parts, fmt.Sprintf("final PR #%d %s", r.FinalPR.Number, r.FinalPR.Gate))
	}
	if n := r.redTasks(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d red", n))
	}
	if r.Resolver != nil && r.Resolver.Attempts > 0 {
		parts = append(parts, fmt.Sprintf("resolver ×%d", r.Resolver.Attempts))
	}
	if r.Watchdog != "" && r.Watchdog != "idle" && r.Watchdog != "disabled" {
		parts = append(parts, "watchdog "+r.Watchdog)
	}
	return strings.Join(parts, " · ")
}

func (r AutopilotRunStatus) redTasks() int {
	n := 0
	for _, f := range r.Fix {
		if f.Gate != "clear" {
			n++
		}
	}
	return n
}

// SurfaceLines renders the operator status surface as indented plain-text lines
// (shared by `warden autopilot status` and the TUI run detail pane).
func (r AutopilotRunStatus) SurfaceLines() []string {
	var out []string
	if r.State == "awaiting_merge" || r.AwaitingMerge != nil {
		pr := 0
		if r.FinalPR != nil {
			pr = r.FinalPR.Number
		} else if r.AwaitingMerge != nil {
			pr = r.AwaitingMerge.PR
		}
		if pr > 0 {
			out = append(out, fmt.Sprintf("awaiting final PR merge (#%d)", pr))
		} else {
			out = append(out, "awaiting final PR merge")
		}
	}
	if r.FinalPR != nil {
		l := fmt.Sprintf("final PR: #%d gate=%s fix_attempts=%d", r.FinalPR.Number, r.FinalPR.Gate, r.FinalPR.FixAttempts)
		if r.FinalPR.State != "" {
			l += " state=" + r.FinalPR.State
		}
		if r.FinalPR.URL != "" {
			l += " " + r.FinalPR.URL
		}
		out = append(out, l)
	}
	if d := r.LastDiagnosis; d != nil {
		l := fmt.Sprintf("last diagnosis: %s (confidence %.2f, %s) at %s", d.Action, d.Confidence, firstNonEmptyStr(d.Source, "-"), d.At)
		if d.Outcome != "" {
			l += " → " + d.Outcome
		}
		out = append(out, l)
		if d.Rationale != "" {
			out = append(out, "  rationale: "+d.Rationale)
		}
		if d.FailOpenReason != "" {
			out = append(out, "  fail-open: "+d.FailOpenReason)
		}
	}
	for _, f := range r.Fix {
		l := fmt.Sprintf("task %s: gate=%s fix_attempts=%d red_streak=%d", f.Task, f.Gate, f.FixAttempts, f.RedStreak)
		if f.PR != 0 {
			l += fmt.Sprintf(" pr=#%d", f.PR)
		}
		if f.Reruns > 0 {
			l += fmt.Sprintf(" reruns=%d", f.Reruns)
		}
		if f.Fixing {
			l += " (fix in flight)"
		}
		out = append(out, l)
	}
	if rs := r.Resolver; rs != nil {
		l := fmt.Sprintf("resolver: attempts=%d", rs.Attempts)
		if rs.LastOutcome != "" {
			l += fmt.Sprintf(" last=%s (%s", rs.LastOutcome, firstNonEmptyStr(rs.LastClass, "-"))
			if rs.LastTask != "" {
				l += " task " + rs.LastTask
			}
			l += ") at " + rs.LastAt
		}
		out = append(out, l)
	}
	return out
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
