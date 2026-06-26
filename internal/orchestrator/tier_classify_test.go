package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTierWord(t *testing.T) {
	cases := []struct {
		raw  string
		want Tier
		ok   bool
	}{
		{"T0", T0, true},
		{"t1", T1, true},
		{"  T2  ", T2, true},
		{"T2.", T2, true},
		{"T1 — a couple of related actions", T1, true},
		// a model that echoes the rubric (lists T0/T1/T2) then answers: last wins.
		{"T0 = ... T1 = ... T2 = ...\nAnswer: T1", T1, true},
		{"I think this is hard", T0, false},
		{"", T0, false},
	}
	for _, c := range cases {
		got, ok := parseTierWord(c.raw)
		require.Equal(t, c.ok, ok, "ok for %q", c.raw)
		if c.ok {
			require.Equal(t, c.want, got, "tier for %q", c.raw)
		}
	}
}

func TestModelClassifier_UsesModelVerdict(t *testing.T) {
	// The heuristic would score this T0 (no conjunctions), but the model judges it
	// hard — the model classifier returns the model's verdict.
	m := modelClassifier{comp: fakeCompleter{out: "T2"}, fallback: heuristicClassifier{}}
	got, err := m.NeededTier(context.Background(), "untangle the auth race across all three worktrees")
	require.NoError(t, err)
	require.Equal(t, T2, got)
}

func TestModelClassifier_FallsBackOnError(t *testing.T) {
	// Model down ⇒ defer to the heuristic, which scores this T2 (two conjunctions
	// + "pipeline"). Classification never blocks on the model.
	m := modelClassifier{comp: fakeCompleter{err: errors.New("refused")}, fallback: heuristicClassifier{}}
	got, err := m.NeededTier(context.Background(), "spawn one and another, and a pipeline")
	require.NoError(t, err)
	require.Equal(t, T2, got)
}

func TestModelClassifier_FallsBackOnUnparseableReply(t *testing.T) {
	m := modelClassifier{comp: fakeCompleter{out: "hard to say"}, fallback: heuristicClassifier{}}
	got, err := m.NeededTier(context.Background(), "list agents")
	require.NoError(t, err)
	require.Equal(t, T0, got, "no marker in the reply ⇒ heuristic's verdict")
}

func TestModelClassifier_NilCompFallsBack(t *testing.T) {
	m := modelClassifier{comp: nil, fallback: heuristicClassifier{}}
	got, err := m.NeededTier(context.Background(), "spawn an agent and review")
	require.NoError(t, err)
	require.Equal(t, T1, got)
}
