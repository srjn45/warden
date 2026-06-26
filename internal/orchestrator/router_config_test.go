package orchestrator

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNewRouterFromConfig_ExplicitTier(t *testing.T) {
	// An explicit local_llm_tier overrides the model-name heuristic. A T2 model
	// plans even a multi-clause ("pipeline") request locally.
	r := NewRouterFromConfig(config.Config{LocalLLMTier: "T2", LocalLLMModel: "tiny"})
	require.NotNil(t, r)
	got := r.Route(context.Background(), "build a pipeline and then review and merge")
	require.Equal(t, PlanLocal, got.Mode, "a T2 model is within tier for this request")
}

func TestNewRouterFromConfig_DegradesWhenUnderTierAndEscalateOff(t *testing.T) {
	// A T0 model with escalation off must degrade (not escalate) on a request the
	// heuristic scores above its tier.
	r := NewRouterFromConfig(config.Config{LocalLLMTier: "T0", LocalLLMEscalate: false})
	got := r.Route(context.Background(), "spawn one and then another, and a pipeline too")
	require.Equal(t, Degrade, got.Mode)
	require.Contains(t, got.OperatorMessage, "more capable model")
}

func TestNewRouterFromConfig_FallsBackToModelTier(t *testing.T) {
	// An unparseable tier ("auto"/empty) falls back to the model-name heuristic;
	// the router is still usable and routes a trivial request locally.
	r := NewRouterFromConfig(config.Config{LocalLLMModel: "llama3"})
	require.NotNil(t, r)
	got := r.Route(context.Background(), "list agents")
	require.Equal(t, PlanLocal, got.Mode)
}
