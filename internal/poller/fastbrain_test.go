package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/stretchr/testify/require"
)

// fakeFB is a scripted fastbrain.Engine; it counts ArbitrateApproval calls.
type fakeFB struct {
	fastbrain.Engine
	dec   fastbrain.ArbiterDecision
	err   error
	calls int
}

func (f *fakeFB) ArbitrateApproval(context.Context, fastbrain.ArbiterInput) (fastbrain.ArbiterDecision, error) {
	f.calls++
	return f.dec, f.err
}

const (
	fbToolPane = "Bash(terraform plan)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	fbQPane    = "Which approach should I take?\n ❯ 1. Refactor\n   2. Rewrite\n   3. Leave as is"
)

func fbPoller(fb fastbrain.Engine, pol approval.Policy) (*Poller, *stubDeps) {
	d := &stubDeps{}
	p := New(d, 30*time.Second)
	p.AutoApprovePolicy = pol
	p.FastBrain = fb
	return p, d
}

// noMatchPolicy has an allow rule that never matches the test prompts, so the
// static rules cannot answer.
func noMatchPolicy(useFB bool) approval.Policy {
	return approval.Policy{
		Enabled: true, UseFastBrain: useFB,
		Rules: approval.Rules{Allow: []approval.Rule{{Tool: "Read"}}},
	}
}

func TestFastBrainArbiter(t *testing.T) {
	agent := &agentstore.Agent{ID: "a1", TmuxSession: "t1"}
	cases := []struct {
		name     string
		pane     string
		dec      fastbrain.ArbiterDecision
		err      error
		wantKey  string // "" ⇒ no keys
		wantCall int
	}{
		{"tool approve", fbToolPane, fastbrain.ArbiterDecision{Action: fastbrain.DecisionApprove}, nil, "1", 1},
		{"select option 2", fbQPane, fastbrain.ArbiterDecision{Action: fastbrain.DecisionSelectOption, SelectedOption: 2}, nil, "2", 1},
		{"escalate", fbToolPane, fastbrain.ArbiterDecision{Action: fastbrain.DecisionEscalate}, nil, "", 1},
		{"reject", fbToolPane, fastbrain.ArbiterDecision{Action: fastbrain.DecisionReject}, nil, "", 1},
		{"error", fbToolPane, fastbrain.ArbiterDecision{}, errors.New("boom"), "", 1},
		{"out-of-range option", fbQPane, fastbrain.ArbiterDecision{Action: fastbrain.DecisionSelectOption, SelectedOption: 9}, nil, "", 1},
		{"approve without affirmative", fbQPane, fastbrain.ArbiterDecision{Action: fastbrain.DecisionApprove}, nil, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeFB{dec: tc.dec, err: tc.err}
			p, d := fbPoller(fb, noMatchPolicy(true))
			p.tryAutoApprove(context.Background(), agent, tc.pane)
			require.Equal(t, tc.wantCall, fb.calls)
			if tc.wantKey == "" {
				require.Equal(t, 0, d.sendCount())
			} else {
				require.Equal(t, tc.wantKey, d.lastSentKey("t1"))
			}
			if tc.err == nil {
				require.NotEmpty(t, d.recordedEvents("a1"), "arbiter decision must be audited")
			}
		})
	}
}

func TestFastBrainDisabledPreservesBehavior(t *testing.T) {
	agent := &agentstore.Agent{ID: "a1", TmuxSession: "t1"}
	// UseFastBrain off with an engine present, and on with a nil engine: never consulted.
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionApprove}}
	p, d := fbPoller(fb, noMatchPolicy(false))
	p.tryAutoApprove(context.Background(), agent, fbToolPane)
	require.Zero(t, fb.calls)
	require.Zero(t, d.sendCount())

	p2, d2 := fbPoller(nil, noMatchPolicy(true))
	p2.tryAutoApprove(context.Background(), agent, fbToolPane)
	require.Zero(t, d2.sendCount())
}

func TestFastBrainStaticRulesStayFirst(t *testing.T) {
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionEscalate}}
	pol := allowAllPolicy()
	pol.UseFastBrain = true
	p, d := fbPoller(fb, pol)
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "a1", TmuxSession: "t1"}, fbToolPane)
	require.Zero(t, fb.calls, "static rules that can answer must not consult the arbiter")
	require.Equal(t, "1", d.lastSentKey("t1"))
}

func TestFastBrainStrategicQuestionLegacyNoRules(t *testing.T) {
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionSelectOption, SelectedOption: 2}}
	p, d := fbPoller(fb, approval.Policy{Enabled: true, UseFastBrain: true})
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "a1", TmuxSession: "t1"}, fbQPane)
	require.Equal(t, 1, fb.calls)
	require.Equal(t, "2", d.lastSentKey("t1"))
}

func TestFastBrainNeverSeesDestructive(t *testing.T) {
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionApprove}}
	pol := noMatchPolicy(true)
	p, d := fbPoller(fb, pol)
	const destructive = "Bash(rm -rf build)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "a1", TmuxSession: "t1"}, destructive)
	require.Zero(t, fb.calls)
	require.Zero(t, d.sendCount())
}

func TestFastBrainBreakerRunsBeforeArbiter(t *testing.T) {
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionApprove}}
	pol := noMatchPolicy(true)
	pol.MaxRepeats = 2
	p, d := fbPoller(fb, pol)
	agent := &agentstore.Agent{ID: "a1", TmuxSession: "t1"}
	for i := 0; i < 5; i++ {
		p.tryAutoApprove(context.Background(), agent, fbToolPane)
	}
	require.Equal(t, 2, fb.calls, "breaker must stop the loop before further model calls")
	require.Equal(t, 2, d.sendCount())
}

func TestFastBrainOtherBackendPane(t *testing.T) {
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionSelectOption, SelectedOption: 3}}
	p, d := fbPoller(fb, noMatchPolicy(true))
	p.Backend = func(*agentstore.Agent) agentbackend.Backend {
		return fakeBackend{approval: &agentbackend.Approval{
			Question: "Allow codex to run this command?",
			Options:  []string{"Yes", "Yes, always", "No, tell codex what to do"},
		}}
	}
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "c1", TmuxSession: "tc"}, "codex pane")
	require.Equal(t, "3", d.lastSentKey("tc"))
}

func TestFastBrainEscalatesToAutopilotBrain(t *testing.T) {
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionEscalate}}
	p, d := fbPoller(fb, noMatchPolicy(true))
	fa := &fakeAutopilot{brainID: "brain-1", own: true}
	p.Autopilot = fa
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: "tw"}, fbToolPane)
	require.Zero(t, d.sendCount())
	require.Len(t, fa.forwards, 1)
}
