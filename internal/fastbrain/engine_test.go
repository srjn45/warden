package fastbrain

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func fixed(out string) Runner {
	return RunnerFunc(func(context.Context, string) (string, error) { return out, nil })
}

func blocking() Runner {
	return RunnerFunc(func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
}

func req(tier Tier) Request {
	return Request{Kind: KindArbitrateApproval, Tier: tier, Prompt: "decide"}
}

func TestDecideRouting(t *testing.T) {
	e := NewEngine(fixed(`{"from":"fast"}`), fixed(`{"from":"thinking"}`))
	r, err := e.Decide(context.Background(), req(TierFast))
	require.NoError(t, err)
	require.True(t, r.OK())
	require.JSONEq(t, `{"from":"fast"}`, string(r.Output.Parsed))
	r, err = e.Decide(context.Background(), req(TierThinking))
	require.NoError(t, err)
	require.JSONEq(t, `{"from":"thinking"}`, string(r.Output.Parsed))
	require.Equal(t, TierThinking, r.Tier)
}

func TestDecideConfidenceRationale(t *testing.T) {
	e := NewEngine(fixed(`{"confidence":0.9,"rationale":"safe"}`), nil)
	r, err := e.Decide(context.Background(), req(TierFast))
	require.NoError(t, err)
	require.InDelta(t, 0.9, r.Confidence, 1e-9)
	require.Equal(t, "safe", r.Rationale)
}

func TestDecideFastTimeoutFailOpen(t *testing.T) {
	e := NewEngine(blocking(), nil)
	start := time.Now()
	r, err := e.Decide(context.Background(), req(TierFast))
	require.NoError(t, err)
	require.Equal(t, StatusTimeout, r.Status)
	require.False(t, r.OK())
	require.GreaterOrEqual(t, time.Since(start), FastTimeout-50*time.Millisecond)
	require.Less(t, time.Since(start), FastTimeout+500*time.Millisecond)
}

func TestDecideThinkingTimeoutFailOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("waits the full 10s thinking budget")
	}
	t.Parallel()
	e := NewEngine(nil, blocking())
	start := time.Now()
	r, err := e.Decide(context.Background(), req(TierThinking))
	require.NoError(t, err)
	require.Equal(t, StatusTimeout, r.Status)
	require.Less(t, time.Since(start), ThinkingTimeout+time.Second)
}

func TestDecideTimeoutOverrideOnlyLowers(t *testing.T) {
	e := NewEngine(blocking(), nil)
	rq := req(TierFast)
	rq.Timeout = 50 * time.Millisecond
	start := time.Now()
	r, err := e.Decide(context.Background(), rq)
	require.NoError(t, err)
	require.Equal(t, StatusTimeout, r.Status)
	require.Less(t, time.Since(start), time.Second)

	rq.Timeout = time.Hour // must clamp to the tier max
	start = time.Now()
	r, _ = e.Decide(context.Background(), rq)
	require.Equal(t, StatusTimeout, r.Status)
	require.Less(t, time.Since(start), FastTimeout+500*time.Millisecond)
}

func TestDecideParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	e := NewEngine(blocking(), nil)
	r, err := e.Decide(ctx, req(TierFast))
	require.NoError(t, err)
	require.Equal(t, StatusCanceled, r.Status)
}

func TestDecideSanitizesOutput(t *testing.T) {
	tests := []struct{ name, in string }{
		{"fenced", "```json\n{\"a\":1}\n```"},
		{"preamble", "Sure! Here is my answer:\n{\"a\":1}\nHope that helps."},
		{"fenced with preamble", "Okay.\n```\n{\"a\":1}\n```"},
		{"brace in prose first", "use {curly} braces: {\"a\":1}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewEngine(fixed(tc.in), nil).Decide(context.Background(), req(TierFast))
			require.NoError(t, err)
			require.True(t, r.OK())
			require.JSONEq(t, `{"a":1}`, string(r.Output.Parsed))
			require.Equal(t, tc.in, r.Output.Raw)
		})
	}
}

func TestDecideInvalidJSONFailOpen(t *testing.T) {
	for _, in := range []string{"", "   ", "no json here", "{broken", "```json\n```"} {
		r, err := NewEngine(fixed(in), nil).Decide(context.Background(), req(TierFast))
		require.NoError(t, err)
		require.Equal(t, StatusInvalidJSON, r.Status, in)
		require.Nil(t, r.Output.Parsed)
	}
}

func TestDecideNilRunnerFailOpen(t *testing.T) {
	r, err := NewEngine(nil, nil).Decide(context.Background(), req(TierFast))
	require.NoError(t, err)
	require.Equal(t, StatusNoRunner, r.Status)
	r, err = NewEngine(fixed("{}"), nil).Decide(context.Background(), req(TierThinking))
	require.NoError(t, err)
	require.Equal(t, StatusNoRunner, r.Status)
}

func TestDecideRunnerError(t *testing.T) {
	e := NewEngine(RunnerFunc(func(context.Context, string) (string, error) {
		return "", errors.New("boom")
	}), nil)
	r, err := e.Decide(context.Background(), req(TierFast))
	require.NoError(t, err)
	require.Equal(t, StatusRunnerError, r.Status)
	require.Contains(t, r.Error, "boom")
}

func TestDecideInvalidRequest(t *testing.T) {
	e := NewEngine(fixed("{}"), fixed("{}"))
	for _, rq := range []Request{
		{Tier: TierFast, Prompt: "p"},
		{Kind: KindResolveAgentName, Tier: TierFast},
		{Kind: KindResolveAgentName, Tier: "bogus", Prompt: "p"},
	} {
		_, err := e.Decide(context.Background(), rq)
		require.ErrorIs(t, err, ErrInvalidRequest)
	}
}

func TestSanitizeJSON(t *testing.T) {
	_, err := SanitizeJSON("")
	require.ErrorIs(t, err, ErrNoJSON)
	_, err = SanitizeJSON("[1,2]")
	require.ErrorIs(t, err, ErrNoJSON)
}
