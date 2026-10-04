package fastbrain

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/agentbackend"
)

type counted struct {
	n   atomic.Int32
	out string
	err error
	fn  func(ctx context.Context, p string) (string, error)
}

func (c *counted) Run(ctx context.Context, p string) (string, error) {
	c.n.Add(1)
	if c.fn != nil {
		return c.fn(ctx, p)
	}
	return c.out, c.err
}

// Backend-shaped tool-permission approvals.
var toolApprovals = map[string]*agentbackend.Approval{
	"claude": {Action: "Bash(go test ./...)", Question: "Do you want to proceed?",
		Options: []string{"Yes", "Yes, and always allow", "No"}, AffirmativeIdx: 1},
	"codex": {Action: "Bash(ls -la)", Question: "Allow command?",
		Options: []string{"Yes, proceed", "No, and tell Codex what to do differently"}, AffirmativeIdx: 1},
	"cursor": {Action: "Read(main.go)", Question: "Run this tool?",
		Options: []string{"Run", "Skip"}, AffirmativeIdx: 1},
	"antigravity": {Action: "Edit(README.md)", Question: "Approve edit?",
		Options: []string{"Allow", "Deny"}, AffirmativeIdx: 1},
}

var strategic = &agentbackend.Approval{
	Question: "Which database should we use?",
	Options:  []string{"Postgres", "SQLite", "MySQL"},
}

func arb(t *testing.T, e Engine, a *agentbackend.Approval) ArbiterDecision {
	t.Helper()
	d, err := ArbitrateApproval(context.Background(), e, ArbiterInput{Approval: a, Goal: "ship feature", AgentID: "a1"})
	require.NoError(t, err)
	return d
}

func TestArbiterToolPermission(t *testing.T) {
	for name, a := range toolApprovals {
		require.Equal(t, CategoryToolPermission, ClassifyPrompt(a), name)
		t.Run(name+"/approve", func(t *testing.T) {
			f := &counted{out: `{"approve": true, "confidence": 0.95, "reason": "safe"}`}
			d := arb(t, NewEngine(f, nil), a)
			require.Equal(t, DecisionApprove, d.Action)
			require.Equal(t, TierFast, d.Tier)
			require.Equal(t, "safe", d.Rationale)
			require.EqualValues(t, 1, f.n.Load())
		})
		t.Run(name+"/reject", func(t *testing.T) {
			f := &counted{out: "```json\n{\"approve\": false, \"confidence\": 0.9, \"rationale\": \"out of scope\"}\n```"}
			d := arb(t, NewEngine(f, nil), a)
			require.Equal(t, DecisionReject, d.Action)
			require.Equal(t, "out of scope", d.Rationale)
		})
	}
}

func TestArbiterToolPermissionEscalates(t *testing.T) {
	a := toolApprovals["claude"]
	for _, out := range []string{
		`{"approve": true, "confidence": 0.79}`,
		`{"approve": false, "confidence": 0.5}`,
		`{"approve": true}`,
		`{"confidence": 0.99}`,
		`not json`,
	} {
		d := arb(t, NewEngine(&counted{out: out}, nil), a)
		require.Equal(t, DecisionEscalate, d.Action, out)
	}
}

func TestArbiterStrategicSelection(t *testing.T) {
	th := &counted{out: `{"selected_option": 2, "confidence": 0.85, "reason": "embedded"}`}
	fast := &counted{out: `{}`}
	d := arb(t, NewEngine(fast, th), strategic)
	require.Equal(t, DecisionSelectOption, d.Action)
	require.Equal(t, 2, d.SelectedOption)
	require.Equal(t, TierThinking, d.Tier)
	require.EqualValues(t, 0, fast.n.Load())
	require.EqualValues(t, 1, th.n.Load())
}

func TestArbiterStrategicEscalates(t *testing.T) {
	for _, out := range []string{
		`{"selected_option": 4, "confidence": 0.9}`,
		`{"selected_option": 0, "confidence": 0.9}`,
		`{"selected_option": -1, "confidence": 0.9}`,
		`{"confidence": 0.9}`,
		`{"selected_option": 1, "confidence": 0.5}`,
		`{"selected_option": "two", "confidence": 0.9}`,
	} {
		d := arb(t, NewEngine(nil, &counted{out: out}), strategic)
		require.Equal(t, DecisionEscalate, d.Action, out)
		require.Zero(t, d.SelectedOption)
	}
}

func TestArbiterFailOpen(t *testing.T) {
	a := toolApprovals["claude"]
	t.Run("timeout", func(t *testing.T) {
		f := &counted{fn: func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}}
		d := arb(t, NewEngine(f, nil), a)
		require.Equal(t, DecisionEscalate, d.Action)
		require.Contains(t, d.Rationale, string(StatusTimeout))
	})
	t.Run("runner error", func(t *testing.T) {
		d := arb(t, NewEngine(&counted{err: errors.New("network down")}, nil), a)
		require.Equal(t, DecisionEscalate, d.Action)
		d = arb(t, NewEngine(nil, &counted{err: errors.New("network down")}), strategic)
		require.Equal(t, DecisionEscalate, d.Action)
	})
	t.Run("nil runner", func(t *testing.T) {
		require.Equal(t, DecisionEscalate, arb(t, NewEngine(nil, nil), a).Action)
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f := &counted{fn: func(ctx context.Context, _ string) (string, error) { return "", ctx.Err() }}
		d, err := ArbitrateApproval(ctx, NewEngine(f, nil), ArbiterInput{Approval: a})
		require.NoError(t, err)
		require.Equal(t, DecisionEscalate, d.Action)
	})
}

func TestArbiterDestructiveNeverCallsRunner(t *testing.T) {
	f := &counted{out: `{"approve": true, "confidence": 1}`}
	th := &counted{out: `{"selected_option": 1, "confidence": 1}`}
	e := NewEngine(f, th)
	for _, a := range []*agentbackend.Approval{
		{Action: "Bash(rm -rf /tmp/x)", Question: "Do you want to proceed?", Options: []string{"Yes", "No"}, AffirmativeIdx: 1},
		{Action: "Bash(git push --force)", Question: "Allow?", Options: []string{"Allow", "Deny"}, AffirmativeIdx: 1},
		{Question: "Drop the production table?", Options: []string{"Yes", "No"}},
	} {
		d := arb(t, e, a)
		require.Equal(t, DecisionEscalate, d.Action)
		require.Zero(t, d.Tier)
	}
	require.EqualValues(t, 0, f.n.Load())
	require.EqualValues(t, 0, th.n.Load())
}

func TestArbiterPromptsAndInvalidInput(t *testing.T) {
	var got string
	f := &counted{fn: func(_ context.Context, p string) (string, error) {
		got = p
		return `{"approve":true,"confidence":1}`, nil
	}}
	d, err := ArbitrateApproval(context.Background(), NewEngine(f, nil),
		ArbiterInput{Approval: toolApprovals["claude"], Goal: "fix bug", Constraints: "no network"})
	require.NoError(t, err)
	require.Equal(t, DecisionApprove, d.Action)
	require.True(t, strings.Contains(got, "Bash(go test ./...)") && strings.Contains(got, "fix bug") && strings.Contains(got, "no network"))

	var tp string
	th := &counted{fn: func(_ context.Context, p string) (string, error) { tp = p; return `{}`, nil }}
	arb(t, NewEngine(nil, th), strategic)
	require.Contains(t, tp, "2. SQLite")

	_, err = ArbitrateApproval(context.Background(), NewEngine(nil, nil), ArbiterInput{})
	require.ErrorIs(t, err, ErrInvalidRequest)
	_, err = ArbitrateApproval(context.Background(), nil, ArbiterInput{Approval: strategic})
	require.ErrorIs(t, err, ErrInvalidRequest)
}
