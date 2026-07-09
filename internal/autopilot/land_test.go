package autopilot

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeLandHost is a scriptable LandHost for the land orchestration table tests:
// each field pins one host answer, and merges are recorded so the delete-branch
// and strategy plumbing can be asserted.
type fakeLandHost struct {
	pr        PRInfo
	prFound   bool
	prErr     error
	ciState   GateState
	ciSummary string
	ciErr     error
	localOK   GateState
	localSum  string
	localErr  error
	mergeSHA  string
	mergeErr  error

	merges []mergeCall // record of Merge calls
}

type mergeCall struct {
	pr           int
	strategy     string
	deleteBranch bool
}

func (h *fakeLandHost) FindPR(context.Context, string) (PRInfo, bool, error) {
	return h.pr, h.prFound, h.prErr
}
func (h *fakeLandHost) GateCI(context.Context, string, string, string) (GateState, string, error) {
	return h.ciState, h.ciSummary, h.ciErr
}
func (h *fakeLandHost) GateLocal(context.Context, string) (GateState, string, error) {
	return h.localOK, h.localSum, h.localErr
}
func (h *fakeLandHost) Merge(_ context.Context, pr int, strategy string, deleteBranch bool) (string, error) {
	h.merges = append(h.merges, mergeCall{pr: pr, strategy: strategy, deleteBranch: deleteBranch})
	return h.mergeSHA, h.mergeErr
}

// baseReq is a request that passes every precondition when paired with a green
// host; tests mutate one field to exercise each failure path.
func baseReq() LandRequest {
	return LandRequest{
		RunActive:         true,
		Owned:             true,
		Branch:            "autopilot/api",
		Worktree:          "/wt",
		IntegrationBranch: "autopilot/integration",
		DefaultBranch:     "main",
		Gate:              "ci",
		Strategy:          "squash",
		DeleteBranch:      true,
	}
}

// greenHost is a host where the PR exists, is mergeable, and CI is green.
func greenHost() *fakeLandHost {
	return &fakeLandHost{
		pr:       PRInfo{Number: 7, BaseRef: "autopilot/integration", HeadSHA: "sha-head", Mergeable: true},
		prFound:  true,
		ciState:  GateGreen,
		mergeSHA: "sha-merge",
	}
}

func TestLandTypedErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*LandRequest)
		host    func() *fakeLandHost
		ledger  *Ledger
		want    LandErrorKind
		wantDet string // substring expected in Detail (optional)
	}{
		{
			name:   "run_disabled: kill switch honored",
			mutate: func(r *LandRequest) { r.RunActive = false },
			host:   greenHost,
			want:   ErrRunDisabled,
		},
		{
			name:   "not_owned: foreign branch",
			mutate: func(r *LandRequest) { r.Owned = false },
			host:   greenHost,
			want:   ErrNotOwned,
		},
		{
			name:   "wrong_base: landing the default branch itself",
			mutate: func(r *LandRequest) { r.Branch = "main" },
			host:   greenHost,
			want:   ErrWrongBase,
		},
		{
			name:   "wrong_base: integration target is the default branch",
			mutate: func(r *LandRequest) { r.IntegrationBranch = "main" },
			host:   greenHost,
			want:   ErrWrongBase,
		},
		{
			name:   "wrong_base: no PR for the branch",
			mutate: func(*LandRequest) {},
			host:   func() *fakeLandHost { h := greenHost(); h.prFound = false; return h },
			want:   ErrWrongBase,
		},
		{
			name:   "wrong_base: PR based on main, not integration",
			mutate: func(*LandRequest) {},
			host:   func() *fakeLandHost { h := greenHost(); h.pr.BaseRef = "main"; return h },
			want:   ErrWrongBase,
		},
		{
			name:   "gate_red: CI failing carries the summary",
			mutate: func(*LandRequest) {},
			host: func() *fakeLandHost {
				h := greenHost()
				h.ciState, h.ciSummary = GateRed, "CI failed: build"
				return h
			},
			want:    ErrGateRed,
			wantDet: "build",
		},
		{
			name:   "gate_pending: CI not concluded",
			mutate: func(*LandRequest) {},
			host:   func() *fakeLandHost { h := greenHost(); h.ciState = GatePending; return h },
			want:   ErrGatePending,
		},
		{
			name:   "ci_missing: no CI under explicit ci gate",
			mutate: func(r *LandRequest) { r.Gate = "ci" },
			host:   func() *fakeLandHost { h := greenHost(); h.ciState = GateMissing; return h },
			want:   ErrCIMissing,
		},
		{
			name:   "gate_red: local checks fail",
			mutate: func(r *LandRequest) { r.Gate = "local" },
			host: func() *fakeLandHost {
				h := greenHost()
				h.localOK, h.localSum = GateRed, "vet: failed"
				return h
			},
			want:    ErrGateRed,
			wantDet: "vet",
		},
		{
			name:   "not_mergeable: conflicts against integration",
			mutate: func(*LandRequest) {},
			host:   func() *fakeLandHost { h := greenHost(); h.pr.Mergeable = false; return h },
			want:   ErrNotMergeable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := baseReq()
			tc.mutate(&req)
			res, err := Land(context.Background(), req, tc.host(), tc.ledger)
			require.Error(t, err)
			var le *LandError
			require.ErrorAs(t, err, &le, "want a typed LandError")
			require.Equal(t, tc.want, le.Kind)
			if tc.wantDet != "" {
				require.Contains(t, le.Detail, tc.wantDet)
			}
			require.Zero(t, res.SHA, "no side effects / no result on a precondition failure")
		})
	}
}

func TestLandSuccessHonorsStrategyAndDeleteBranch(t *testing.T) {
	host := greenHost()
	req := baseReq()
	req.Strategy = "rebase"
	req.DeleteBranch = true

	res, err := Land(context.Background(), req, host, nil)
	require.NoError(t, err)
	require.False(t, res.AlreadyLanded)
	require.Equal(t, "sha-merge", res.SHA)
	require.Equal(t, 7, res.PR)
	require.Equal(t, "autopilot/api", res.Branch)

	require.Len(t, host.merges, 1)
	require.Equal(t, mergeCall{pr: 7, strategy: "rebase", deleteBranch: true}, host.merges[0])
}

func TestLandNoMergeOnPreconditionFailure(t *testing.T) {
	host := greenHost()
	host.pr.Mergeable = false // trip not_mergeable
	req := baseReq()

	_, err := Land(context.Background(), req, host, nil)
	require.Error(t, err)
	require.Empty(t, host.merges, "a precondition failure must not merge")
}

func TestLandIdempotentAlreadyMergedPR(t *testing.T) {
	host := greenHost()
	host.pr.Merged = true
	host.pr.MergeCommit = "sha-old-merge"
	req := baseReq()

	res, err := Land(context.Background(), req, host, nil)
	require.NoError(t, err)
	require.True(t, res.AlreadyLanded)
	require.Equal(t, "sha-old-merge", res.SHA)
	require.Empty(t, host.merges, "an already-merged PR must not merge again")
}

func TestLandIdempotentRecordedHeadSHA(t *testing.T) {
	store := newFakeStore()
	ledger := NewLedger(store, "ap-run")
	// Pre-record a landing for the PR head SHA — a brain re-issuing after a restart.
	require.NoError(t, ledger.AppendLanding(Landing{Branch: "autopilot/api", SHA: "sha-head", PR: 7, LandedAt: "t0"}))

	host := greenHost() // pr.HeadSHA == "sha-head"
	res, err := Land(context.Background(), baseReq(), host, ledger)
	require.NoError(t, err)
	require.True(t, res.AlreadyLanded)
	require.Equal(t, "sha-head", res.SHA)
	require.Equal(t, 7, res.PR)
	require.Empty(t, host.merges, "a recorded head SHA must not merge again")
}

func TestLandInfraErrorIsNotTyped(t *testing.T) {
	host := greenHost()
	host.prErr = errors.New("gh exploded")
	_, err := Land(context.Background(), baseReq(), host, nil)
	require.Error(t, err)
	var le *LandError
	require.False(t, errors.As(err, &le), "an infra failure is a plain error, not a typed LandError")
	require.Empty(t, host.merges)
}

func TestLandLocalGateUsesLocalHost(t *testing.T) {
	host := greenHost()
	host.ciState = GateRed // would fail if CI gate were used
	host.localOK = GateGreen
	req := baseReq()
	req.Gate = "local"

	res, err := Land(context.Background(), req, host, nil)
	require.NoError(t, err)
	require.Equal(t, "sha-merge", res.SHA)
	require.Len(t, host.merges, 1)
}

func TestResolveGateMode(t *testing.T) {
	tests := []struct {
		configured string
		covers     bool
		want       string
	}{
		{"ci", false, "ci"},
		{"ci", true, "ci"},
		{"local", true, "local"},
		{"local", false, "local"},
		{"auto", true, "ci"},     // workflows cover integration PRs → ci
		{"auto", false, "local"}, // no CI → local fallback (never dead-ends)
		{"", false, "local"},     // empty defaults to auto
		{"", true, "ci"},
		{"AUTO", true, "ci"}, // case-insensitive
		{"bogus", false, "local"},
	}
	for _, tc := range tests {
		require.Equalf(t, tc.want, resolveGateMode(tc.configured, tc.covers),
			"resolveGateMode(%q, %v)", tc.configured, tc.covers)
	}
}
