package fastbrain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHeuristicStall(t *testing.T) {
	cases := []struct {
		name string
		in   StallInput
		want StallAction
	}{
		{"rate limit wins", StallInput{RateLimited: true, PendingApproval: true}, ActionResumeRateLimit},
		{"approval", StallInput{PendingApproval: true, Pane: "x"}, ActionResolvePrompt},
		{"empty pane no activity", StallInput{Pane: " \n"}, ActionRedeliverPrompt},
		{"empty pane but active", StallInput{ActivitySinceSpawn: true}, ActionMechanical},
		{"busy pane", StallInput{Pane: "working"}, ActionMechanical},
	}
	for _, c := range cases {
		require.Equal(t, c.want, HeuristicStall(c.in).Action, c.name)
	}
}

func TestParseStallDiagnosis(t *testing.T) {
	long := strings.Repeat("é", 700)
	cases := []struct {
		name, js string
		worker   bool
		want     StallAction
		err      bool
	}{
		{"wait", `{"action":"wait","confidence":0.9}`, false, ActionWait, false},
		{"nudge", `{"action":"nudge","text":"go on","confidence":0.7}`, false, ActionNudge, false},
		{"nudge no text", `{"action":"nudge"}`, false, "", true},
		{"unknown", `{"action":"reboot"}`, false, "", true},
		{"mechanical rejected", `{"action":"mechanical"}`, false, "", true},
		{"worker restart", `{"action":"restart","confidence":0.9}`, true, "", true},
		{"worker rotate", `{"action":"rotate"}`, true, "", true},
		{"worker resolver", `{"action":"call_resolver"}`, true, "", true},
		{"manager restart", `{"action":"restart","confidence":0.9}`, false, ActionRestart, false},
		{"conf high", `{"action":"wait","confidence":1.5}`, false, "", true},
		{"conf neg", `{"action":"wait","confidence":-1}`, false, "", true},
		{"cap", `{"action":"nudge","text":"` + long + `"}`, false, ActionNudge, false},
		{"bad type", `{"action":3}`, false, "", true},
	}
	for _, c := range cases {
		d, err := ParseStallDiagnosis(json.RawMessage(c.js), c.worker)
		if c.err {
			require.Error(t, err, c.name)
			continue
		}
		require.NoError(t, err, c.name)
		require.Equal(t, c.want, d.Action, c.name)
		require.LessOrEqual(t, len([]rune(d.Text)), MaxNudgeChars)
	}
	d, _ := ParseStallDiagnosis(json.RawMessage(`{"action":"nudge","text":"key sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUV"}`), false)
	require.NotContains(t, d.Text, "sk-ant")
	d, _ = ParseStallDiagnosis(json.RawMessage(`{"action":"wait","text":"ignored"}`), false)
	require.Empty(t, d.Text)
}

func tierRunner(fast, thinking string, prompts *[]string) *engine {
	rec := func(out string) Runner {
		return RunnerFunc(func(_ context.Context, p string) (string, error) {
			if prompts != nil {
				*prompts = append(*prompts, p)
			}
			return out, nil
		})
	}
	return NewEngine(rec(fast), rec(thinking)).(*engine)
}

func TestDiagnoseStallTiers(t *testing.T) {
	ctx := context.Background()
	in := StallInput{Pane: "busy", ActivitySinceSpawn: true}

	var ps []string
	e := tierRunner(`{"action":"wait","confidence":0.9}`, `{"action":"restart","confidence":0.9}`, &ps)
	d := DiagnoseStall(ctx, e, in)
	require.Equal(t, ActionWait, d.Action)
	require.Equal(t, TierFast, d.Tier)
	require.Len(t, ps, 1)

	ps = nil
	e = tierRunner(`{"action":"wait","confidence":0.2}`, `{"action":"restart","confidence":0.9}`, &ps)
	d = DiagnoseStall(ctx, e, in)
	require.Equal(t, ActionRestart, d.Action)
	require.Equal(t, TierThinking, d.Tier)
	require.Len(t, ps, 2)

	// Heuristic short-circuits: no model call.
	ps = nil
	d = DiagnoseStall(ctx, e, StallInput{RateLimited: true})
	require.Equal(t, ActionResumeRateLimit, d.Action)
	require.Empty(t, ps)

	// Worker prompt omits manager-only actions; manager prompt has them.
	ps = nil
	DiagnoseStall(ctx, e, StallInput{Worker: true, Pane: "x", ActivitySinceSpawn: true})
	require.NotContains(t, ps[0], "call_resolver")
	ps = nil
	DiagnoseStall(ctx, e, in)
	require.Contains(t, ps[0], "call_resolver")
}

func TestDiagnoseStallSanitizesPrompt(t *testing.T) {
	var ps []string
	e := tierRunner(`{"action":"wait","confidence":0.9}`, `{}`, &ps)
	DiagnoseStall(context.Background(), e, StallInput{Pane: panicTrace, Header: "Bearer abcdef1234567890token", Facts: "/home/alice/x", ActivitySinceSpawn: true})
	for _, bad := range secrets {
		require.NotContains(t, ps[0], bad)
	}
}

func TestDiagnoseStallFailOpen(t *testing.T) {
	in := StallInput{Pane: "busy", ActivitySinceSpawn: true}
	errRunner := RunnerFunc(func(context.Context, string) (string, error) { return "", errors.New("x") })
	cases := []struct {
		name   string
		e      Engine
		reason string
	}{
		{"nil engine", nil, FailOpenNoRunner},
		{"no runner", NewEngine(nil, nil), FailOpenNoRunner},
		{"runner error", NewEngine(errRunner, errRunner), FailOpenRunnerError},
		{"invalid json", NewEngine(fixed("nope"), nil), FailOpenInvalidJSON},
		{"unknown action", NewEngine(fixed(`{"action":"reboot","confidence":1}`), nil), FailOpenInvalidAction},
		{"nudge empty", NewEngine(fixed(`{"action":"nudge","confidence":1}`), nil), FailOpenInvalidOutput},
		{"bad confidence", NewEngine(fixed(`{"action":"wait","confidence":7}`), nil), FailOpenInvalidOutput},
		{"timeout", NewEngine(RunnerFunc(func(ctx context.Context, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() }), nil), FailOpenTimeout},
		{"thinking fails after low fast", NewEngine(fixed(`{"action":"wait","confidence":0.1}`), nil), FailOpenNoRunner},
	}
	for _, c := range cases {
		d := DiagnoseStall(context.Background(), c.e, in)
		require.Equal(t, ActionMechanical, d.Action, c.name)
		require.Equal(t, c.reason, d.FailOpen, c.name)
		require.Equal(t, "failopen", d.Source, c.name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := DiagnoseStall(ctx, NewEngine(fixed(`{"action":"wait"}`), nil), in)
	require.Equal(t, ActionMechanical, d.Action)
	require.NotEmpty(t, d.FailOpen)
}

func TestClassifyCIFailure(t *testing.T) {
	ctx := context.Background()
	errRunner := RunnerFunc(func(context.Context, string) (string, error) { return "", errors.New("x") })
	cases := []struct {
		name   string
		e      Engine
		want   CIClass
		reason string
	}{
		{"flaky", NewEngine(fixed(`{"class":"flaky_or_infra","confidence":0.9}`), nil), CIFlakyOrInfra, ""},
		{"real", NewEngine(fixed(`{"class":"real","confidence":0.95,"rationale":"compile"}`), nil), CIReal, ""},
		{"unknown class", NewEngine(fixed(`{"class":"maybe"}`), nil), CIReal, FailOpenInvalidOutput},
		{"bad conf", NewEngine(fixed(`{"class":"flaky_or_infra","confidence":2}`), nil), CIReal, FailOpenInvalidOutput},
		{"invalid json", NewEngine(fixed("x"), nil), CIReal, FailOpenInvalidJSON},
		{"runner error", NewEngine(errRunner, nil), CIReal, FailOpenRunnerError},
		{"no runner", NewEngine(nil, nil), CIReal, FailOpenNoRunner},
		{"nil engine", nil, CIReal, FailOpenNoRunner},
	}
	for _, c := range cases {
		r := ClassifyCIFailure(ctx, c.e, CIInput{Check: "build", Log: "boom"})
		require.Equal(t, c.want, r.Class, c.name)
		require.Equal(t, c.reason, r.FailOpen, c.name)
	}
	var ps []string
	e := tierRunner(`{"class":"real"}`, `{}`, &ps)
	ClassifyCIFailure(ctx, e, CIInput{Log: panicTrace})
	for _, bad := range secrets {
		require.NotContains(t, ps[0], bad)
	}
}
