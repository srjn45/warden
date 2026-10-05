package client

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSurfaceBadge(t *testing.T) {
	r := AutopilotRunStatus{
		FinalPR:  &AutopilotFinalPR{Number: 9, Gate: "red"},
		Fix:      []AutopilotFixStatus{{Task: "t1", Gate: "red"}, {Task: "t2", Gate: "clear"}},
		Resolver: &AutopilotResolverStatus{Attempts: 2},
		Watchdog: "escalating",
	}
	require.Equal(t, "final PR #9 red · 1 red · resolver ×2 · watchdog escalating", r.SurfaceBadge())
	require.Empty(t, AutopilotRunStatus{}.SurfaceBadge())
	require.Empty(t, AutopilotRunStatus{Watchdog: "idle"}.SurfaceBadge())
}

func TestSurfaceLines(t *testing.T) {
	r := AutopilotRunStatus{
		FinalPR: &AutopilotFinalPR{Number: 3, Gate: "green", FixAttempts: 1, URL: "https://x/3"},
		LastDiagnosis: &AutopilotDiagnosis{
			Action: "wait", Confidence: 0.9, Source: "model", Outcome: "applied",
			At: "2026-10-05T12:00:00Z", Rationale: "long tool call",
		},
		Fix:      []AutopilotFixStatus{{Task: "t1", Gate: "clear", FixAttempts: 2, PR: 7}},
		Resolver: &AutopilotResolverStatus{Attempts: 1, LastOutcome: "started", LastClass: "red_gate", LastTask: "t1", LastAt: "t"},
	}
	lines := r.SurfaceLines()
	require.Equal(t, "final PR: #3 gate=green fix_attempts=1 https://x/3", lines[0])
	require.True(t, strings.HasPrefix(lines[1], "last diagnosis: wait"))
	require.Contains(t, lines[1], "→ applied")
	require.Equal(t, "  rationale: long tool call", lines[2])
	require.Contains(t, lines[3], "task t1: gate=clear")
	require.Contains(t, lines[4], "resolver: attempts=1 last=started")
}
