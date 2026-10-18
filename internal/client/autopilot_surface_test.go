package client

import (
	"encoding/json"
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

func TestEffectiveSupervisorOldServerFallback(t *testing.T) {
	sv := AutopilotRunStatus{State: "active"}.EffectiveSupervisor()
	if sv.Kind != "daemon" || sv.Addressable || sv.State != "supervising" {
		t.Fatalf("fallback = %+v", sv)
	}
	if got := (AutopilotRunStatus{State: "paused"}).EffectiveSupervisor().State; got != "suspended" {
		t.Fatalf("paused state = %q", got)
	}
	if got := (AutopilotRunStatus{NextStep: &AutopilotNextStep{Action: "x", Owner: "guardian"}}).NextStepLine(); got != "next: x (daemon)" {
		t.Fatalf("next line = %q", got)
	}
}

func TestEffectiveSupervisorKeepsReservedSessionKind(t *testing.T) {
	var r AutopilotRunStatus
	require.NoError(t, json.Unmarshal([]byte(`{"state":"active","supervisor":{"kind":"session","addressable":true,`+
		`"session_id":"agent-42","state":"supervising","health":"healthy","heal_stage":"none"}}`), &r))
	sv := r.EffectiveSupervisor()
	require.True(t, sv.Addressable)
	require.Equal(t, "agent-42", sv.SessionID)
	require.Equal(t, "supervisor: session (healthy)", r.SupervisorLine())
}

func TestDaemonSupervisorLineNeverCarriesAnID(t *testing.T) {
	for _, body := range []string{
		`{"state":"active"}`, // old server, no supervisor
		`{"state":"degraded","guardian_slot_id":"x-guardian","supervisor":{"kind":"daemon","state":"supervising","health":"healing","heal_stage":"nudged"}}`,
		`{"state":"paused","supervisor":{"kind":"none","state":"suspended","health":"unknown","heal_stage":"none"}}`,
	} {
		var r AutopilotRunStatus
		require.NoError(t, json.Unmarshal([]byte(body), &r))
		require.False(t, r.EffectiveSupervisor().Addressable, body)
		require.NotContains(t, r.SupervisorLine(), "guardian", body)
	}
	r := AutopilotRunStatus{NextStep: &AutopilotNextStep{Action: "supervise", Owner: "guardian"}}
	require.NotContains(t, r.NextStepLine(), "guardian", "legacy owner is shown as daemon")
}

func TestSupervisorAndNextStepLineCharacterizationMatrix(t *testing.T) {
	cases := []struct {
		name     string
		status   AutopilotRunStatus
		wantSup  string
		wantNext string
	}{
		{
			name: "healthy",
			status: AutopilotRunStatus{
				State: "active",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "healthy", HealStage: "none",
				},
				NextStep: &AutopilotNextStep{
					Action: "supervise: landing, overwatch and stall triage", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (healthy)",
			wantNext: "next: supervise: landing, overwatch and stall triage at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "nudged",
			status: AutopilotRunStatus{
				State: "healing",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "healing", HealStage: "nudged",
				},
				NextStep: &AutopilotNextStep{
					Action: "restart the manager if it shows no heartbeat", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (healing, heal stage nudged)",
			wantNext: "next: restart the manager if it shows no heartbeat at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "restarted",
			status: AutopilotRunStatus{
				State: "healing",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "healing", HealStage: "restarted",
				},
				NextStep: &AutopilotNextStep{
					Action: "rotate the manager to the next backend if it shows no heartbeat", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (healing, heal stage restarted)",
			wantNext: "next: rotate the manager to the next backend if it shows no heartbeat at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "rotated",
			status: AutopilotRunStatus{
				State: "healing",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "healing", HealStage: "rotated",
				},
				NextStep: &AutopilotNextStep{
					Action: "back off (or park) if the rotated manager shows no heartbeat", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (healing, heal stage rotated)",
			wantNext: "next: back off (or park) if the rotated manager shows no heartbeat at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "backoff",
			status: AutopilotRunStatus{
				State: "degraded",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "backoff", HealStage: "backoff",
				},
				NextStep: &AutopilotNextStep{
					Action: "retry the backend ladder from the top", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (backoff, heal stage backoff)",
			wantNext: "next: retry the backend ladder from the top at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "limit-resting",
			status: AutopilotRunStatus{
				State: "active",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "healthy", HealStage: "none",
				},
				NextStep: &AutopilotNextStep{
					Action: "resume or switch a rate-limited agent", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (healthy)",
			wantNext: "next: resume or switch a rate-limited agent at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "awaiting-merge",
			status: AutopilotRunStatus{
				State: "active",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "idle", Health: "healthy", HealStage: "none",
				},
				NextStep: &AutopilotNextStep{
					Action: "check final PR #42 for merge", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (healthy)",
			wantNext: "next: check final PR #42 for merge at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "parked",
			status: AutopilotRunStatus{
				State: "degraded",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "parked", HealStage: "none",
				},
				NextStep: &AutopilotNextStep{
					Action: "waiting for you: plan is broken", Owner: "operator",
				},
			},
			wantSup:  "supervisor: daemon (parked)",
			wantNext: "next: waiting for you: plan is broken (operator)",
		},
		{
			name: "paused",
			status: AutopilotRunStatus{
				State: "paused",
				Supervisor: &AutopilotSupervisor{
					Kind: "none", State: "suspended", Health: "unknown", HealStage: "none",
				},
			},
			wantSup:  "supervisor: none",
			wantNext: "",
		},
		{
			name: "stopped",
			status: AutopilotRunStatus{
				State: "stopped",
				Supervisor: &AutopilotSupervisor{
					Kind: "none", State: "suspended", Health: "unknown", HealStage: "none",
				},
			},
			wantSup:  "supervisor: none",
			wantNext: "",
		},
		{
			name: "complete",
			status: AutopilotRunStatus{
				State: "complete",
				Supervisor: &AutopilotSupervisor{
					Kind: "none", State: "suspended", Health: "unknown", HealStage: "none",
				},
			},
			wantSup:  "supervisor: none",
			wantNext: "",
		},
		{
			name: "boot-unknown",
			status: AutopilotRunStatus{
				State: "active",
				Supervisor: &AutopilotSupervisor{
					Kind: "daemon", State: "supervising", Health: "unknown", HealStage: "none",
				},
				NextStep: &AutopilotNextStep{
					Action: "supervise: landing, overwatch and stall triage", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: daemon (unknown)",
			wantNext: "next: supervise: landing, overwatch and stall triage at 2026-10-19T01:00:00Z (daemon)",
		},
		{
			name: "session-kind-forward-compat",
			status: AutopilotRunStatus{
				State: "active",
				Supervisor: &AutopilotSupervisor{
					Kind: "session", Addressable: true, SessionID: "agent-123", State: "supervising", Health: "healthy", HealStage: "none",
				},
				NextStep: &AutopilotNextStep{
					Action: "supervise: landing, overwatch and stall triage", At: "2026-10-19T01:00:00Z", Owner: "daemon",
				},
			},
			wantSup:  "supervisor: session (healthy)",
			wantNext: "next: supervise: landing, overwatch and stall triage at 2026-10-19T01:00:00Z (daemon)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantSup, tc.status.SupervisorLine())
			require.Equal(t, tc.wantNext, tc.status.NextStepLine())
		})
	}
}
