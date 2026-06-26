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
	r := NewRouterFromConfig(config.Config{LocalLLMTier: "T2", LocalLLMModel: "tiny"}, nil)
	require.NotNil(t, r)
	got := r.Route(context.Background(), "build a pipeline and then review and merge")
	require.Equal(t, PlanLocal, got.Mode, "a T2 model is within tier for this request")
}

func TestNewRouterFromConfig_DegradesWhenUnderTierAndEscalateOff(t *testing.T) {
	// A T0 model with escalation off must degrade (not escalate) on a request the
	// heuristic scores above its tier.
	r := NewRouterFromConfig(config.Config{LocalLLMTier: "T0", LocalLLMEscalate: false}, nil)
	got := r.Route(context.Background(), "spawn one and then another, and a pipeline too")
	require.Equal(t, Degrade, got.Mode)
	require.Contains(t, got.OperatorMessage, "more capable model")
}

func TestNewRouterFromConfig_ModelClassifierWired(t *testing.T) {
	// local_llm_classifier: model routes on the model's verdict. A trivial-looking
	// line the heuristic scores T0 is judged T2 by the model ⇒ a T0 model with
	// escalation off degrades, which only happens if the model verdict was used.
	cfg := config.Config{LocalLLMTier: "T0", LocalLLMEscalate: false, LocalLLMClassifier: "model"}
	r := NewRouterFromConfig(cfg, fakeCompleter{out: "T2"})
	require.Equal(t, Degrade, r.Route(context.Background(), "fix the bug").Mode)
}

func TestNewRouterFromConfig_DefaultsToHeuristicIgnoringComp(t *testing.T) {
	// With local_llm_classifier unset, a present Completer is NOT consulted: the
	// heuristic scores "fix the bug" T0, so a T0 model plans locally even though
	// the (unused) model would have said T2.
	cfg := config.Config{LocalLLMTier: "T0", LocalLLMEscalate: false}
	r := NewRouterFromConfig(cfg, fakeCompleter{out: "T2"})
	require.Equal(t, PlanLocal, r.Route(context.Background(), "fix the bug").Mode)
}

func TestNewRouterFromConfig_FallsBackToModelTier(t *testing.T) {
	// An unparseable tier ("auto"/empty) falls back to the model-name heuristic;
	// the router is still usable and routes a trivial request locally.
	r := NewRouterFromConfig(config.Config{LocalLLMModel: "llama3"}, nil)
	require.NotNil(t, r)
	got := r.Route(context.Background(), "list agents")
	require.Equal(t, PlanLocal, got.Mode)
}
