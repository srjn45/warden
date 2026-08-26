package autopilot

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeSources is a scriptable DigestSources for digest composition tests.
type fakeSources struct {
	agents   []AgentInfo
	audit    []AuditEntry
	agentErr error
	auditErr error
}

func (f fakeSources) RunAgents(_ context.Context, _ string) ([]AgentInfo, error) {
	return f.agents, f.agentErr
}

func (f fakeSources) RecentAudit(_ context.Context, _ string, _ int) ([]AuditEntry, error) {
	return f.audit, f.auditErr
}

func TestComposeDigestFromFixtureState(t *testing.T) {
	store := newFakeStore()
	l := NewLedger(store, "ap-run")
	require.NoError(t, l.WriteTasks([]LedgerTask{
		{ID: "api", State: "in_progress", WorkerID: "A-1", Branch: "autopilot/api", PR: 7},
		{ID: "ui", State: "pending"},
	}, "brain"))
	require.NoError(t, l.AppendLanding(Landing{Branch: "autopilot/db", SHA: "deadbeefcafeb00b1234", PR: 5, LandedAt: "2026-07-09T10:00:00Z"}))

	in := DigestInput{
		RunID:    "ap-run",
		Repo:     "/home/dev/project",
		PlanFile: "/home/dev/project/autopilot.plan.yaml",
		Plan: Plan{
			Version:     1,
			Goal:        "Ship the notifications feature",
			Constraints: []string{"all changes behind a feature flag"},
			Tasks:       []PlanTask{{ID: "api", Prompt: "Implement the API"}, {ID: "ui", Prompt: "Build the UI", After: []string{"api"}}},
			DoneWhen:    []string{"wd check passes on integration"},
		},
		Ledger: l,
		Sources: fakeSources{
			agents: []AgentInfo{{ID: "A-1", Name: "api-worker", Role: "worker", State: "running", Branch: "autopilot/api"}},
			audit:  []AuditEntry{{Time: "2026-07-09T10:05:00Z", Action: "spawn", Target: "A-1", Detail: "role=worker"}},
		},
	}

	out, err := ComposeDigest(context.Background(), in)
	require.NoError(t, err)

	// Every section is reconstructed from durable facts, not a prior brain's context.
	require.Contains(t, out, "ap-run")
	require.Contains(t, out, "/home/dev/project")
	require.Contains(t, out, "Ship the notifications feature")
	require.Contains(t, out, "all changes behind a feature flag")
	require.Contains(t, out, "wd check passes on integration")
	// plan tasks + dependency edge
	require.Contains(t, out, "ui (after: api)")
	// live ledger task state
	require.Contains(t, out, "api [in_progress]")
	require.Contains(t, out, "pr=#7")
	// landing (short sha, do-not-re-land)
	require.Contains(t, out, "autopilot/db")
	require.Contains(t, out, "deadbeefcafe") // 12-char short sha
	require.NotContains(t, out, "deadbeefcafeb00b1234")
	// live agents + recent audit
	require.Contains(t, out, "api-worker (A-1)")
	require.Contains(t, out, "spawn A-1")
	// restart-safety reminder
	require.Contains(t, out, "idempotent")
}

func TestComposeDigestDegradesOnSourceErrors(t *testing.T) {
	// A nil ledger and erroring sources must never fail the brief — a missing
	// digest section degrades observability, never blocks a brain (re)spawn.
	in := DigestInput{
		RunID:    "ap-run",
		Repo:     "/repo",
		PlanFile: "/repo/plan.yaml",
		Plan:     Plan{Version: 1, Goal: "do the thing"},
		Ledger:   nil,
		Sources:  nil,
	}
	out, err := ComposeDigest(context.Background(), in)
	require.NoError(t, err)
	require.Contains(t, out, "do the thing")
	require.Contains(t, out, "no ledger available")
	require.Contains(t, out, "no agent source available")
}
