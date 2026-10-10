package fastbrain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKindDeadlineBoundsOperationalAndBestEffort(t *testing.T) {
	require.Equal(t, CallerDeadlineRouteTier, KindDeadline(KindRouteTier))
	for _, k := range []DecisionKind{KindCommitMessage, KindSummarizeCheck, KindPRSummary, KindClassifyCIFailure, KindDiagnoseFailure} {
		require.Equal(t, CallerDeadlineOperational, KindDeadline(k), k)
	}
	for _, k := range []DecisionKind{KindClassifyTask, KindResolveAgentName, KindSummarizeActivity, KindCurateExtract} {
		require.Equal(t, CallerDeadlineBestEffort, KindDeadline(k), k)
	}
	// Safety/interactive kinds keep the tier ceiling: never enlarged, never starved.
	for _, k := range []DecisionKind{KindArbitrateApproval, KindRecognizePrompt, KindDiagnoseStall, KindReplTurn} {
		require.Zero(t, KindDeadline(k), k)
	}
	for _, k := range []DecisionKind{KindCommitMessage, KindClassifyTask} {
		require.LessOrEqual(t, KindDeadline(k), FastTimeout)
	}
	require.Less(t, time.Duration(0), CallerDeadlineRouteTier)
}

func TestNewRequestCarriesAgentIdentity(t *testing.T) {
	r := NewRequest(KindCommitMessage, TierFast, "p", "agent-1")
	require.Equal(t, "agent-1", r.Metadata["agent_id"])
	require.Equal(t, CallerDeadlineOperational, r.Timeout)
	require.Nil(t, NewRequest(KindClassifyTask, TierFast, "p", "").Metadata)
}
