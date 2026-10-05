package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/poller"
	"github.com/stretchr/testify/require"
)

func TestRunIDFromTags(t *testing.T) {
	require.Equal(t, "ap-1", runIDFromTags([]string{"autopilot", "run:ap-1"}))
	require.Equal(t, "", runIDFromTags([]string{"autopilot"}))
	require.Equal(t, "", runIDFromTags(nil))
	require.Equal(t, "", runIDFromTags([]string{"run:"}), "an empty run id is ignored")
}

// TestAutopilotApprovalsForward proves an unanswerable worker prompt is an informational note in the
// manager's mailbox AND mirrors a non-blocking copy to the human inbox (§8).
func TestAutopilotApprovalsForward(t *testing.T) {
	mb, err := mailbox.New(t.TempDir())
	require.NoError(t, err)
	defer mb.Close()

	ap := autopilotApprovals{s: &Server{mbox: mb}}
	worker := &agentstore.Agent{ID: "worker-1", Name: "fixer"}
	ap.Forward(context.Background(), "brain-1", worker, "policy could not answer: Bash(x)")

	brainMsgs, err := mb.Messages("brain-1")
	require.NoError(t, err)
	require.Len(t, brainMsgs, 1, "the brain receives the actionable forward")
	require.Contains(t, brainMsgs[0].Body, "fixer")
	require.Contains(t, brainMsgs[0].Body, "no action needed")

	humanMsgs, err := mb.Messages(humanRecipient)
	require.NoError(t, err)
	require.Len(t, humanMsgs, 1, "the human inbox mirrors the event for visibility")
	require.Contains(t, humanMsgs[0].Body, "no action needed")
}

type fakeAnswerConsultor struct{ got brainconsult.Request }

func (f *fakeAnswerConsultor) Consult(_ context.Context, r brainconsult.Request) (brainconsult.Result, error) {
	f.got = r
	return brainconsult.Result{Answer: brainconsult.AnswerSelectOption, Option: 2, Reason: "r"}, nil
}

// TestConsultPromptBuildsAnswerRequest proves stage 3 asks the brain in answer
// mode with the prompt, options, pane tail and task, and maps the reply.
func TestConsultPromptBuildsAnswerRequest(t *testing.T) {
	fc := &fakeAnswerConsultor{}
	ap := autopilotApprovals{s: &Server{promptConsultor: fc}}
	w := &agentstore.Agent{ID: "w1", Task: "fix bug", Tags: []string{"autopilot", "run:r1"}}
	ans, err := ap.ConsultPrompt(context.Background(), w, poller.PromptQuestion{
		Question: "Which?", Options: []string{"a", "b"}, PaneTail: "tail"})
	require.NoError(t, err)
	require.Equal(t, brainconsult.ModeAnswer, fc.got.Mode)
	require.Equal(t, "r1", fc.got.RunID)
	require.Equal(t, "fix bug", fc.got.Answer.Task)
	require.Equal(t, []string{"a", "b"}, fc.got.Answer.Options)
	require.Equal(t, poller.PromptSelectOption, ans.Kind)
	require.Equal(t, 2, ans.Option)
}
