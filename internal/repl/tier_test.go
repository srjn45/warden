package repl

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouter_AlwaysPlansLocal(t *testing.T) {
	r := NewRouter()
	for _, line := range []string{"what's running", "stand up two agents and a review pipeline", ""} {
		require.Equal(t, PlanLocal, r.Route(context.Background(), line).Mode, line)
	}
}

func TestRouter_NilIsSafe(t *testing.T) {
	var r *Router
	require.Equal(t, PlanLocal, r.Route(context.Background(), "x").Mode)
}
