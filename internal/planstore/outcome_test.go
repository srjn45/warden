package planstore

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlanOutcomeNeedsBranchDeleteRetry(t *testing.T) {
	require.False(t, (*PlanOutcome)(nil).NeedsBranchDeleteRetry())
	require.False(t, (&PlanOutcome{BranchFate: BranchFateDeleted}).NeedsBranchDeleteRetry())
	require.False(t, (&PlanOutcome{
		BranchFate: BranchFateDeleteFailed, BranchDeleteAttempts: MaxBranchDeleteAttempts,
		IntegrationBranch: "autopilot/x",
	}).NeedsBranchDeleteRetry())
	require.True(t, (&PlanOutcome{
		BranchFate: BranchFateDeleteFailed, BranchDeleteAttempts: 2,
		IntegrationBranch: "autopilot/x",
	}).NeedsBranchDeleteRetry())
}

func TestIntegrationUnmergedErrorMessage(t *testing.T) {
	err := &IntegrationUnmergedError{
		Branch: "autopilot/demo", DefaultBranch: "main", CommitCount: 4,
		FinalPRNumber: 12, HasOpenPR: true,
	}
	require.Contains(t, err.Error(), "autopilot/demo")
	require.Contains(t, err.Error(), "4 commits")
	require.Contains(t, err.Error(), "final PR #12 is open")
	require.Contains(t, err.Error(), "--abandon-unmerged")
	require.ErrorIs(t, err, ErrIntegrationUnmerged)
}

func TestFirstPRNumber(t *testing.T) {
	require.Equal(t, 42, firstPRNumber(`[{"number":42,"url":"x"}]`))
	require.Equal(t, 0, firstPRNumber(`[]`))
}
