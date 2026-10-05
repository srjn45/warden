package poller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/fastbrain"
	"github.com/stretchr/testify/require"
)

// fakeAutopilot is a poller.AutopilotApprovals stub with a scripted stage-3
// brain. It records every consult, typed text, hand-off, audit and forward.
type fakeAutopilot struct {
	mu       sync.Mutex
	brainID  string
	own      bool
	ans      PromptAnswer
	errs     []error // consumed one per consult; empty ⇒ success with ans
	block    chan struct{}
	forwards []string
	consults []PromptQuestion
	typed    []string
	handoffs []string
	audits   []string
}

func (f *fakeAutopilot) OwnsAgent(*agentstore.Agent) bool { return f.own }
func (f *fakeAutopilot) BrainFor(*agentstore.Agent) (string, bool) {
	return f.brainID, f.brainID != ""
}
func (f *fakeAutopilot) Forward(_ context.Context, _ string, _ *agentstore.Agent, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forwards = append(f.forwards, reason)
}
func (f *fakeAutopilot) ConsultPrompt(ctx context.Context, _ *agentstore.Agent, q PromptQuestion) (PromptAnswer, error) {
	f.mu.Lock()
	f.consults = append(f.consults, q)
	var err error
	if len(f.errs) > 0 {
		err, f.errs = f.errs[0], f.errs[1:]
	}
	blk := f.block
	f.mu.Unlock()
	if blk != nil {
		select {
		case <-blk:
		case <-ctx.Done():
			return PromptAnswer{}, ctx.Err()
		}
	}
	return f.ans, err
}
func (f *fakeAutopilot) TypeText(_ context.Context, _ *agentstore.Agent, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = append(f.typed, text)
	return nil
}
func (f *fakeAutopilot) HandOff(_ context.Context, _ *agentstore.Agent, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handoffs = append(f.handoffs, reason)
}
func (f *fakeAutopilot) Audit(_ context.Context, action string, _ *agentstore.Agent, d map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audits = append(f.audits, action+":"+d["stage"])
}

const (
	yesNoPane  = "Bash(terraform apply)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	multiPane  = "Which approach should I take?\n ❯ 1. Refactor\n   2. Rewrite\n   3. Leave as is"
	destrPane  = "Bash(rm -rf /data)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	agentTmux  = "tmux-1"
	managerTmx = "tmux-mgr"
)

// denyAllPolicy participates in auto-approve but denies everything, so every
// recognized prompt is a "policy can't answer" case.
func denyAllPolicy() approval.Policy {
	return approval.Policy{Enabled: true, Rules: approval.Rules{Deny: []approval.Rule{{}}}}
}

func apPoller(fa *fakeAutopilot) (*Poller, *stubDeps) {
	d := &stubDeps{}
	p := New(d, 30*time.Second)
	p.AutoApprovePolicy = denyAllPolicy()
	p.Autopilot = fa
	return p, d
}

// Yes/no and multi-option prompts on a worker AND on the manager are answered by
// the brain stage with no manager mailbox involved.
func TestPromptChainAnswersWorkerAndManager(t *testing.T) {
	cases := []struct {
		name    string
		agent   *agentstore.Agent
		brainID string // "" ⇒ the agent is the manager / manager absent
		pane    string
		ans     PromptAnswer
		wantKey string
	}{
		{"worker yes/no, manager absent", &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, "", yesNoPane,
			PromptAnswer{Kind: PromptApprove}, "1"},
		{"worker multi-option", &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, "mgr", multiPane,
			PromptAnswer{Kind: PromptSelectOption, Option: 2}, "2"},
		{"manager yes/no", &agentstore.Agent{ID: "mgr", TmuxSession: managerTmx}, "", yesNoPane,
			PromptAnswer{Kind: PromptReject}, "2"},
		{"manager multi-option", &agentstore.Agent{ID: "mgr", TmuxSession: managerTmx}, "", multiPane,
			PromptAnswer{Kind: PromptSelectOption, Option: 3}, "3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeAutopilot{own: true, brainID: tc.brainID, ans: tc.ans}
			p, d := apPoller(fa)
			p.tryAutoApprove(context.Background(), tc.agent, tc.pane)
			p.WaitPromptChains()
			require.Equal(t, tc.wantKey, d.lastSentKey(tc.agent.TmuxSession))
			require.Len(t, fa.consults, 1)
			require.Contains(t, fa.audits, "autopilot_prompt_resolved:brain")
			require.Empty(t, d.recordedEvents(tc.agent.ID), "nothing escalates to a human")
			require.Empty(t, fa.handoffs)
		})
	}
}

// Identical re-observed prompts consult once; the stage-3 brain gets the context.
func TestPromptChainDedupesAndPassesContext(t *testing.T) {
	fa := &fakeAutopilot{own: true, brainID: "mgr", ans: PromptAnswer{Kind: PromptApprove}}
	p, d := apPoller(fa)
	s := &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}
	for i := 0; i < 5; i++ {
		p.tryAutoApprove(context.Background(), s, yesNoPane)
		p.WaitPromptChains()
	}
	require.Len(t, fa.consults, 1)
	require.Equal(t, 1, d.sendCount())
	require.Equal(t, []string{"Yes", "No"}, fa.consults[0].Options)
	require.Contains(t, fa.consults[0].PaneTail, "terraform apply")
	require.NotEmpty(t, fa.forwards, "informational note still sent")
}

// Arbiter low confidence (escalate) falls to the brain; a confident arbiter
// answer never reaches it. The arbiter runs even though use_fast_brain is off.
func TestPromptChainArbiterThenBrain(t *testing.T) {
	t.Run("low confidence falls to brain", func(t *testing.T) {
		fa := &fakeAutopilot{own: true, ans: PromptAnswer{Kind: PromptSelectOption, Option: 1}}
		p, d := apPoller(fa)
		fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionEscalate, Confidence: 0.3}}
		p.FastBrain = fb // policy.UseFastBrain is false
		p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, multiPane)
		p.WaitPromptChains()
		require.Equal(t, 1, fb.calls, "arbiter is unconditional for run agents")
		require.Len(t, fa.consults, 1)
		require.Equal(t, "1", d.lastSentKey(agentTmux))
	})
	t.Run("confident arbiter wins", func(t *testing.T) {
		fa := &fakeAutopilot{own: true}
		p, d := apPoller(fa)
		p.FastBrain = &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionSelectOption, SelectedOption: 2, Confidence: 0.9}}
		p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, multiPane)
		p.WaitPromptChains()
		require.Equal(t, "2", d.lastSentKey(agentTmux))
		require.Empty(t, fa.consults)
		require.Contains(t, fa.audits, "autopilot_prompt_resolved:arbiter")
	})
}

// A destructive prompt skips stages 1-2 and is never auto-selected, even when
// the brain tries to approve it; it is rejected with the safe-alternative text.
func TestPromptChainDestructiveNeverSelected(t *testing.T) {
	fa := &fakeAutopilot{own: true, ans: PromptAnswer{Kind: PromptApprove, Text: "use a scratch dir instead"}}
	p, d := apPoller(fa)
	p.AutoApprovePolicy = allowAllPolicy()
	fb := &fakeFB{dec: fastbrain.ArbiterDecision{Action: fastbrain.DecisionApprove, Confidence: 1}}
	p.FastBrain = fb
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, destrPane)
	p.WaitPromptChains()
	require.Zero(t, fb.calls, "destructive skips the arbiter")
	require.Len(t, fa.consults, 1)
	require.True(t, fa.consults[0].Destructive)
	require.Equal(t, []string{"2"}, d.sentSequence(agentTmux), "only the deny option is sent, never the destructive Yes")
	require.Equal(t, []string{"use a scratch dir instead"}, fa.typed)
}

// Stage 3 failing twice rejects a destructive prompt, audits the escalation and
// hands the agent to the guardian — never the human inbox.
func TestPromptChainTimeoutRetriesThenGuardian(t *testing.T) {
	fa := &fakeAutopilot{own: true, errs: []error{errors.New("timeout"), errors.New("timeout")}}
	p, d := apPoller(fa)
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, destrPane)
	p.WaitPromptChains()
	require.Len(t, fa.consults, 2, "one retry with a fresh brain")
	require.Equal(t, "2", d.lastSentKey(agentTmux), "safe default: reject")
	require.Len(t, fa.handoffs, 1)
	require.Contains(t, fa.audits, "autopilot_prompt_escalated:brain")
	require.Empty(t, d.recordedEvents("w1"))
}

// A retry that succeeds delivers normally without involving the guardian.
func TestPromptChainRetrySucceeds(t *testing.T) {
	fa := &fakeAutopilot{own: true, errs: []error{errors.New("boom")}, ans: PromptAnswer{Kind: PromptApprove}}
	p, d := apPoller(fa)
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, yesNoPane)
	p.WaitPromptChains()
	require.Len(t, fa.consults, 2)
	require.Equal(t, "1", d.lastSentKey(agentTmux))
	require.Empty(t, fa.handoffs)
}

// A changed prompt cancels the in-flight consult for the old one (one in flight
// per agent); a vanished prompt cancels it too.
func TestPromptChainCancelOnChangeOrDisappear(t *testing.T) {
	fa := &fakeAutopilot{own: true, block: make(chan struct{}), ans: PromptAnswer{Kind: PromptApprove}}
	p, d := apPoller(fa)
	s := &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}
	ctx := context.Background()

	p.tryAutoApprove(ctx, s, yesNoPane)
	p.tryAutoApprove(ctx, s, yesNoPane) // same prompt: still one consult
	require.Eventually(t, func() bool { fa.mu.Lock(); defer fa.mu.Unlock(); return len(fa.consults) == 1 }, time.Second, 5*time.Millisecond)

	p.tryAutoApprove(ctx, s, multiPane) // prompt changed: old consult cancelled
	require.Eventually(t, func() bool { fa.mu.Lock(); defer fa.mu.Unlock(); return len(fa.consults) == 2 }, time.Second, 5*time.Millisecond)

	p.endPromptChain(s.ID) // prompt disappeared
	p.WaitPromptChains()
	require.Zero(t, d.sendCount(), "cancelled consults deliver nothing")
	require.Empty(t, fa.handoffs, "a cancelled consult is not a failure")
}

// Brain type answer is delivered as text.
func TestPromptChainTypeAnswer(t *testing.T) {
	fa := &fakeAutopilot{own: true, ans: PromptAnswer{Kind: PromptType, Text: "use postgres"}}
	p, d := apPoller(fa)
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, multiPane)
	p.WaitPromptChains()
	require.Equal(t, []string{"use postgres"}, fa.typed)
	require.Zero(t, d.sendCount())
}

// A tripped breaker on a run agent hands the loop to the brain, no human anomaly.
func TestBreakerTripRoutesToBrain(t *testing.T) {
	fa := &fakeAutopilot{own: true, ans: PromptAnswer{Kind: PromptReject}}
	p, d := apPoller(fa)
	pol := allowAllPolicy()
	pol.MaxRepeats = 3
	p.AutoApprovePolicy = pol
	s := &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}
	for i := 0; i < 10; i++ {
		p.tryAutoApprove(context.Background(), s, yesNoPane)
	}
	p.WaitPromptChains()
	require.Len(t, fa.consults, 1)
	require.Empty(t, d.recordedEvents("w1"))
}

// Non-autopilot agents are unchanged: human path, no consult, no forward.
func TestNonAutopilotAgentUnchanged(t *testing.T) {
	fa := &fakeAutopilot{own: false}
	p, d := apPoller(fa)
	pol := allowAllPolicy()
	pol.MaxRepeats = 3
	p.AutoApprovePolicy = pol
	s := &agentstore.Agent{ID: "a1", TmuxSession: agentTmux}
	for i := 0; i < 10; i++ {
		p.tryAutoApprove(context.Background(), s, yesNoPane)
	}
	p.WaitPromptChains()
	require.Empty(t, fa.consults)
	require.Empty(t, fa.forwards)
	evs := d.recordedEvents("a1")
	require.Len(t, evs, 1)
	require.Equal(t, "anomaly", evs[0].Type)

	// Auto-approve off + not owned ⇒ still a total no-op.
	p2, d2 := apPoller(fa)
	p2.AutoApprovePolicy = approval.Policy{}
	p2.tryAutoApprove(context.Background(), s, yesNoPane)
	require.Zero(t, d2.sendCount())
}

// Stage 1 approvals on run agents are audited.
func TestPromptChainPolicyAudited(t *testing.T) {
	fa := &fakeAutopilot{own: true}
	p, d := apPoller(fa)
	p.AutoApprovePolicy = allowAllPolicy()
	p.tryAutoApprove(context.Background(), &agentstore.Agent{ID: "w1", TmuxSession: agentTmux}, yesNoPane)
	require.Equal(t, "1", d.lastSentKey(agentTmux))
	require.Contains(t, fa.audits, "autopilot_prompt_resolved:policy")
}
