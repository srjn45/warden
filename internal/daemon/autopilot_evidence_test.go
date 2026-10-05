package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// redeliverLife wraps fakeLife with the optional prompt-redeliver extension.
type redeliverLife struct {
	*fakeLife
	seeded *agentstore.Agent
	ok     bool
}

func (r *redeliverLife) RedeliverPrompt(a *agentstore.Agent) bool { r.seeded = a; return r.ok }

func evidenceRT(t *testing.T, life Lifecycle, a *agentstore.Agent) autopilotRuntime {
	st := newFakeStore()
	require.NoError(t, st.Insert(context.Background(), a))
	return autopilotRuntime{s: &Server{store: st, life: life, hub: newHub()}}
}

func TestAgentEvidence(t *testing.T) {
	life := &fakeLife{output: "pane tail"}
	rt := evidenceRT(t, life, &agentstore.Agent{ID: "w1", TmuxSession: "w1",
		Status: store.StatusRateLimited, ContextState: "warning"})
	ev, err := rt.AgentEvidence(context.Background(), "w1")
	require.NoError(t, err)
	require.Equal(t, autopilot.AgentEvidence{AgentID: "w1", Status: "rate_limited", PaneTail: "pane tail",
		RateLimited: true, ContextLevel: "warning"}, ev)

	_, err = rt.AgentEvidence(context.Background(), "nope")
	require.ErrorIs(t, err, autopilot.ErrAgentNotFound)
}

func TestRedeliverPromptDelegatesToSeeder(t *testing.T) {
	life := &redeliverLife{fakeLife: &fakeLife{}, ok: true}
	rt := evidenceRT(t, life, &agentstore.Agent{ID: "w1", TmuxSession: "w1", Prompt: "do it"})
	require.NoError(t, rt.RedeliverPrompt(context.Background(), "w1"))
	require.Equal(t, "w1", life.seeded.ID)
	require.Empty(t, life.lastInput, "seeder handled it; no direct typing")
}

func TestRedeliverPromptFallsBackToInput(t *testing.T) {
	life := &redeliverLife{fakeLife: &fakeLife{}, ok: false}
	rt := evidenceRT(t, life, &agentstore.Agent{ID: "w1", TmuxSession: "w1", Prompt: "do it"})
	require.NoError(t, rt.RedeliverPrompt(context.Background(), "w1"))
	require.Equal(t, "do it", life.lastInput)

	rt2 := evidenceRT(t, &fakeLife{}, &agentstore.Agent{ID: "w2", TmuxSession: "w2"})
	require.Error(t, rt2.RedeliverPrompt(context.Background(), "w2"), "no recorded prompt")
}

func TestResolvePromptAndResumeUnavailable(t *testing.T) {
	rt := evidenceRT(t, &fakeLife{}, &agentstore.Agent{ID: "w1", TmuxSession: "w1"})
	require.Error(t, rt.ResolvePrompt(context.Background(), "w1"))
	require.Error(t, rt.ResumeRateLimit(context.Background(), "w1"))
	require.ErrorIs(t, rt.ResolvePrompt(context.Background(), "x"), autopilot.ErrAgentNotFound)
}
