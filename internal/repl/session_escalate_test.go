package repl

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeEscalator (defined in tier_test.go) returns a pre-drafted plan, standing
// in for the `claude -p` planning call so the Escalate route can be tested
// without shelling out.

// TestSessionEscalatePlanRunsThroughGate verifies the Escalate route: an
// under-tier model hands a Claude-drafted plan to runPlan, whose mutations still
// pass the confirm gate and reach the daemon. The local chat model is never
// called on this path.
func TestSessionEscalatePlanRunsThroughGate(t *testing.T) {
	// T0 model, escalation on, escalator drafts a spawn (a mutation).
	router := NewRouter(T0, true, heuristicClassifier{}, fakeEscalator{
		calls: []ToolCall{{Name: "spawn_agent", Args: map[string]any{"prompt": "do x"}}},
	})
	chat := &scriptChatter{} // must NOT be consulted on the escalate path
	fd := &fakeDaemon{}
	gate := alwaysApprove()
	s := NewSession(chat, fd, NewRegistry(), gate, router)

	// A multi-clause request the heuristic scores above T0 so it escalates.
	out := s.Handle(context.Background(), "spawn one, and then a pipeline, and review")
	require.Contains(t, out, "spawn_agent: spawned new-agent", "runPlan surfaces the real dispatch result")
	require.Equal(t, 1, fd.spawnCalls, "the drafted mutation ran after gate approval")
	require.Equal(t, 0, chat.calls, "the local chat model is bypassed when escalating")
	require.Equal(t, 1, gate.confirmCalls)
}

// TestSessionEscalateEmptyPlan covers runPlan's empty-plan guard.
func TestSessionEscalateEmptyPlan(t *testing.T) {
	router := NewRouter(T0, true, heuristicClassifier{}, fakeEscalator{calls: nil})
	s := NewSession(&scriptChatter{}, &fakeDaemon{}, NewRegistry(), alwaysApprove(), router)
	out := s.Handle(context.Background(), "do this, and that, and a pipeline")
	require.Equal(t, "nothing to do", out)
}

// TestSessionDegradeRoute covers the Degrade branch: under-tier with escalation
// off returns the operator guidance instead of planning.
func TestSessionDegradeRoute(t *testing.T) {
	router := NewRouter(T0, false, heuristicClassifier{}, nil)
	fd := &fakeDaemon{sessions: []*store.Session{active("a1")}}
	s := NewSession(&scriptChatter{}, fd, NewRegistry(), alwaysReject(), router)
	out := s.Handle(context.Background(), "spawn one, and a pipeline, and then merge")
	require.Contains(t, out, "more capable model")
	require.Equal(t, 0, fd.spawnCalls)
}
