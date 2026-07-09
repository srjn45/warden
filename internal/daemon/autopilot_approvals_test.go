package daemon

import (
	"context"
	"testing"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestRunIDFromTags(t *testing.T) {
	require.Equal(t, "ap-1", runIDFromTags([]string{"autopilot", "run:ap-1"}))
	require.Equal(t, "", runIDFromTags([]string{"autopilot"}))
	require.Equal(t, "", runIDFromTags(nil))
	require.Equal(t, "", runIDFromTags([]string{"run:"}), "an empty run id is ignored")
}

// TestAutopilotApprovalsForward proves an unanswerable worker prompt lands in the
// brain's mailbox AND mirrors a non-blocking copy to the human inbox (§8).
func TestAutopilotApprovalsForward(t *testing.T) {
	mb, err := mailbox.New(t.TempDir())
	require.NoError(t, err)
	defer mb.Close()

	ap := autopilotApprovals{s: &Server{mbox: mb}}
	worker := &store.Session{ID: "worker-1", Name: "fixer"}
	ap.Forward(context.Background(), "brain-1", worker, "policy could not answer: Bash(x)")

	brainMsgs, err := mb.Messages("brain-1")
	require.NoError(t, err)
	require.Len(t, brainMsgs, 1, "the brain receives the actionable forward")
	require.Contains(t, brainMsgs[0].Body, "fixer")
	require.Contains(t, brainMsgs[0].Body, "answer via approve")

	humanMsgs, err := mb.Messages(humanRecipient)
	require.NoError(t, err)
	require.Len(t, humanMsgs, 1, "the human inbox mirrors the event for visibility")
	require.Contains(t, humanMsgs[0].Body, "no action needed")
}
