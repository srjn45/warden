package repl

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNewRouterFromConfig_AlwaysPlansLocal(t *testing.T) {
	// Legacy tier/escalate/classifier settings no longer influence routing.
	cfgs := []config.Config{
		{},
		{LocalLLM: config.LocalLLMConfig{Tier: "T0", Escalate: false, Classifier: "model"}},
		{LocalLLM: config.LocalLLMConfig{Tier: "T2", Escalate: true, Model: "tiny"}},
	}
	for _, cfg := range cfgs {
		r := NewRouterFromConfig(cfg, fakeCompleter{out: "T2"})
		require.NotNil(t, r)
		got := r.Route(context.Background(), "build a pipeline and then review and merge")
		require.Equal(t, PlanLocal, got.Mode)
	}
}
