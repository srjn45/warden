package daemon

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// newSavingsServer builds a minimal Server with an enabled savings ledger backed
// by a temp dir — enough to exercise the record* helpers in isolation.
func newSavingsServer(t *testing.T) *Server {
	t.Helper()
	st, err := savings.NewStore(t.TempDir())
	require.NoError(t, err)
	return &Server{savingsOn: true, savings: st}
}

func TestRecordCheckSavings(t *testing.T) {
	s := newSavingsServer(t)
	// One failing check: 4000 raw bytes condensed to a 40-byte summary.
	res := lifecycle.CheckResult{Checks: []lifecycle.CheckOutcome{
		{Name: "lint", Passed: false, Output: "lint failed: 1 error", RawBytes: 4000},
	}}
	s.recordCheckSavings(&store.Session{ID: "A-1"}, res)

	sum, err := s.savings.Summary(time.Time{})
	require.NoError(t, err)
	require.Equal(t, 1, sum.Events)
	require.Len(t, sum.Features, 1)
	require.Equal(t, savings.FeatureCheck, sum.Features[0].Feature)
	require.Greater(t, sum.SavedTokens, 0)
	// raw≈1000 tok, kept≈5 tok → the saving is the bulk of it.
	require.Equal(t, savings.EstimateTokensLen(4000)-savings.EstimateTokensLen(len(res.Checks[0].Output)), sum.SavedTokens)
}

func TestRecordCheckSavingsNoRawIsNoop(t *testing.T) {
	s := newSavingsServer(t)
	// Every check passed with no captured output — nothing the agent would read.
	s.recordCheckSavings(nil, lifecycle.CheckResult{Checks: []lifecycle.CheckOutcome{{Name: "t", Passed: true}}})
	sum, err := s.savings.Summary(time.Time{})
	require.NoError(t, err)
	require.Equal(t, 0, sum.Events)
}

func TestRecordGitSavings(t *testing.T) {
	s := newSavingsServer(t)
	// 800 bytes of git status/commit/rev-parse output the agent never read.
	res := lifecycle.CommitResult{Committed: true, SHA: "abc1234", Branch: "feat/x", RawBytes: 800}
	s.recordGitSavings(&store.Session{ID: "A-1"}, res.RawBytes, res)

	sum, err := s.savings.Summary(time.Time{})
	require.NoError(t, err)
	require.Equal(t, 1, sum.Events)
	require.Equal(t, savings.FeatureCommit, sum.Features[0].Feature)
	require.Greater(t, sum.SavedTokens, 0)
}

func TestRecordGitSavingsZeroRawIsNoop(t *testing.T) {
	s := newSavingsServer(t)
	// A clean-tree commit: no git output the agent would have read.
	s.recordGitSavings(nil, 0, lifecycle.CommitResult{Branch: "feat/x"})
	sum, err := s.savings.Summary(time.Time{})
	require.NoError(t, err)
	require.Equal(t, 0, sum.Events)
}

func TestRecordSavingsDisabledIsNoop(t *testing.T) {
	st, err := savings.NewStore(t.TempDir())
	require.NoError(t, err)
	s := &Server{savingsOn: false, savings: st} // gate off
	s.recordGitSavings(nil, 800, lifecycle.CommitResult{RawBytes: 800})
	s.recordCheckSavings(nil, lifecycle.CheckResult{Checks: []lifecycle.CheckOutcome{{RawBytes: 800, Output: "x"}}})
	sum, err := s.savings.Summary(time.Time{})
	require.NoError(t, err)
	require.Equal(t, 0, sum.Events, "a disabled ledger records nothing")
}
