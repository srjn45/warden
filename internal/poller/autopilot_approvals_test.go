package poller

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeAutopilot is a poller.AutopilotApprovals stub: it reports a fixed ownership
// answer and records every Forward call so a test can assert what the brain (and
// the human mirror) would have received.
type fakeAutopilot struct {
	brainID  string
	own      bool
	forwards []fakeForward
}

type fakeForward struct {
	brainID  string
	workerID string
	reason   string
}

func (f *fakeAutopilot) BrainFor(_ *store.Session) (string, bool) { return f.brainID, f.own }

func (f *fakeAutopilot) Forward(_ context.Context, brainID string, worker *store.Session, reason string) {
	f.forwards = append(f.forwards, fakeForward{brainID: brainID, workerID: worker.ID, reason: reason})
}

// denyAllPolicy participates in auto-approve but denies everything, so every
// recognized prompt is a "policy can't answer" case.
func denyAllPolicy() approval.Policy {
	return approval.Policy{
		Enabled: true,
		Rules:   approval.Rules{Deny: []approval.Rule{{}}},
	}
}

// TestForwardUnanswerablePromptToBrain proves an autopilot-owned worker's
// unanswerable prompt is forwarded to its brain (once, de-duped) instead of being
// left for a human, and is never auto-approved.
func TestForwardUnanswerablePromptToBrain(t *testing.T) {
	const prompt = "Bash(terraform apply)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	d := &stubDeps{}
	p := New(d, 30*time.Second)
	p.AutoApprovePolicy = denyAllPolicy()
	fa := &fakeAutopilot{brainID: "brain-1", own: true}
	p.Autopilot = fa
	s := &store.Session{ID: "worker-1", TmuxSession: "tmux-1"}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		p.tryAutoApprove(ctx, s, prompt)
	}

	require.Equal(t, 0, d.sendCount(), "an unanswerable prompt must never be auto-approved")
	require.Len(t, fa.forwards, 1, "the identical prompt must forward to the brain exactly once (de-duped)")
	require.Equal(t, "brain-1", fa.forwards[0].brainID)
	require.Equal(t, "worker-1", fa.forwards[0].workerID)
	require.Empty(t, d.recordedEvents("worker-1"), "forwarding must not raise a human anomaly event")
}

// TestForwardResetsOnNewPrompt proves the forward de-dupe is per-prompt: a
// different prompt forwards again rather than being suppressed.
func TestForwardResetsOnNewPrompt(t *testing.T) {
	const promptA = "Bash(terraform apply)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	const promptB = "Bash(kubectl delete pod x)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	d := &stubDeps{}
	p := New(d, 30*time.Second)
	p.AutoApprovePolicy = denyAllPolicy()
	fa := &fakeAutopilot{brainID: "brain-1", own: true}
	p.Autopilot = fa
	s := &store.Session{ID: "worker-1", TmuxSession: "tmux-1"}

	ctx := context.Background()
	p.tryAutoApprove(ctx, s, promptA)
	p.tryAutoApprove(ctx, s, promptA) // dup — suppressed
	p.tryAutoApprove(ctx, s, promptB) // new prompt — forwards again

	require.Len(t, fa.forwards, 2, "a distinct prompt must forward to the brain again")
}

// TestBreakerTripRoutesToBrain proves a tripped breaker on an autopilot-owned
// worker escalates to the brain and raises NO human anomaly entry (§8).
func TestBreakerTripRoutesToBrain(t *testing.T) {
	const prompt = "Bash(aws sts get-caller-identity)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	d := &stubDeps{}
	p := New(d, 30*time.Second)
	pol := allowAllPolicy()
	pol.MaxRepeats = 3
	p.AutoApprovePolicy = pol
	fa := &fakeAutopilot{brainID: "brain-1", own: true}
	p.Autopilot = fa
	s := &store.Session{ID: "worker-1", TmuxSession: "tmux-1"}

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		p.tryAutoApprove(ctx, s, prompt)
	}

	require.Equal(t, 3, d.sendCount(), "the breaker still caps auto-approvals at MaxRepeats")
	require.Empty(t, d.recordedEvents("worker-1"), "an autopilot worker's breaker trip must not raise a human escalation entry")
	require.Len(t, fa.forwards, 1, "the tripped breaker must escalate to the brain exactly once")
	require.Equal(t, "brain-1", fa.forwards[0].brainID)
}

// TestBreakerTripEscalatesHumanWhenNotOwned proves the human escalation path is
// unchanged for a non-autopilot worker: the breaker trip still records the
// approval-loop anomaly and nothing is forwarded.
func TestBreakerTripEscalatesHumanWhenNotOwned(t *testing.T) {
	const prompt = "Bash(aws sts get-caller-identity)\nDo you want to proceed?\n ❯ 1. Yes\n   2. No"
	d := &stubDeps{}
	p := New(d, 30*time.Second)
	pol := allowAllPolicy()
	pol.MaxRepeats = 3
	p.AutoApprovePolicy = pol
	fa := &fakeAutopilot{own: false} // not an autopilot-owned worker
	p.Autopilot = fa
	s := &store.Session{ID: "agent-1", TmuxSession: "tmux-1"}

	ctx := context.Background()
	for i := 0; i < 10; i++ {
		p.tryAutoApprove(ctx, s, prompt)
	}

	require.Empty(t, fa.forwards, "a non-autopilot worker must never forward to a brain")
	evs := d.recordedEvents("agent-1")
	require.Len(t, evs, 1, "the human escalation path must still raise the approval-loop anomaly")
	require.Equal(t, "anomaly", evs[0].Type)
	require.Contains(t, evs[0].Detail, "answer it manually")
}
