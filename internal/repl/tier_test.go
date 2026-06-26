package repl

import (
	"context"
	"errors"
	"testing"

	"github.com/srjn45/warden/internal/llm"
	"github.com/stretchr/testify/require"
)

type fakeClassifier struct {
	tier Tier
	err  error
}

func (f fakeClassifier) NeededTier(context.Context, string) (Tier, error) { return f.tier, f.err }

type fakeEscalator struct {
	calls []ToolCall
	err   error
}

func (f fakeEscalator) Plan(context.Context, string, []llm.ToolSchema) ([]ToolCall, error) {
	return f.calls, f.err
}

func TestModelTier_Table(t *testing.T) {
	require.Equal(t, T1, modelTier("qwen2.5-coder:3b"))
	require.Equal(t, T2, modelTier("qwen2.5-coder:7b"))
	require.Equal(t, T2, modelTier("qwen2.5-coder:14b"))
	require.Equal(t, T0, modelTier("qwen2.5:1.5b"))
	require.Equal(t, T0, modelTier("some-unknown-model"))
}

func TestParseTier_Override(t *testing.T) {
	tr, ok := ParseTier("t2")
	require.True(t, ok)
	require.Equal(t, T2, tr)
	_, ok = ParseTier("auto")
	require.False(t, ok)
}

func TestRouter_WithinTierPlansLocal(t *testing.T) {
	r := NewRouter(modelTier("qwen2.5-coder:7b"), true, fakeClassifier{tier: T1}, fakeEscalator{})
	require.Equal(t, PlanLocal, r.Route(context.Background(), "spawn one agent").Mode)
}

func TestRouter_BelowTierEscalatesWhenEnabled(t *testing.T) {
	r := NewRouter(modelTier("qwen2.5-coder:3b"), true, fakeClassifier{tier: T2},
		fakeEscalator{calls: []ToolCall{{Name: "spawn_agent"}}})
	got := r.Route(context.Background(), "stand up two agents and a review pipeline")
	require.Equal(t, Escalate, got.Mode)
	require.Len(t, got.Calls, 1)
}

func TestRouter_BelowTierDegradesWhenEscalateOff(t *testing.T) {
	r := NewRouter(modelTier("qwen2.5-coder:3b"), false, fakeClassifier{tier: T2}, nil)
	d := r.Route(context.Background(), "compose a fleet")
	require.Equal(t, Degrade, d.Mode)
	require.NotEmpty(t, d.OperatorMessage)
}

func TestRouter_EscalatorFailureDegrades(t *testing.T) {
	r := NewRouter(modelTier("qwen2.5-coder:3b"), true, fakeClassifier{tier: T2},
		fakeEscalator{err: errors.New("claude unavailable")})
	require.Equal(t, Degrade, r.Route(context.Background(), "x").Mode)
}

func TestRouter_ClassifierErrorTriesLocal(t *testing.T) {
	r := NewRouter(T0, true, fakeClassifier{err: errors.New("nope")}, fakeEscalator{})
	require.Equal(t, PlanLocal, r.Route(context.Background(), "x").Mode)
}

func TestHeuristicClassifier_Buckets(t *testing.T) {
	c := heuristicClassifier{}
	t0, _ := c.NeededTier(context.Background(), "what's running")
	require.Equal(t, T0, t0)
	t1, _ := c.NeededTier(context.Background(), "spawn a dev agent and watch it")
	require.Equal(t, T1, t1)
	t2, _ := c.NeededTier(context.Background(), "stand up two agents and a review pipeline")
	require.Equal(t, T2, t2)
}

func TestParsePlanJSON_ToleratesFenceAndProse(t *testing.T) {
	calls, err := parsePlanJSON("here is the plan:\n```json\n[{\"name\":\"spawn_agent\",\"args\":{\"type\":\"development\"}}]\n```")
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "spawn_agent", calls[0].Name)
	require.Equal(t, "development", calls[0].Args["type"])
}

func TestParsePlanJSON_Garbage(t *testing.T) {
	_, err := parsePlanJSON("no json here")
	require.Error(t, err)
}
