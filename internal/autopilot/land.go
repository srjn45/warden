package autopilot

import (
	"context"
	"fmt"
	"strings"
)

// LandErrorKind enumerates the typed `land` failures the brain reasons over
// (autopilot.md §6). Each maps 1:1 to the wire "kind" the daemon returns so the
// brain can branch on the failure without parsing prose.
type LandErrorKind string

const (
	// ErrGatePending: the gate has not concluded yet (CI still running, or the
	// local check could not be evaluated) — retry later.
	ErrGatePending LandErrorKind = "gate_pending"
	// ErrGateRed: the gate concluded red; Detail carries the failing check/CI
	// summary. Fix via the worker, then re-land.
	ErrGateRed LandErrorKind = "gate_red"
	// ErrCIMissing: gate mode `ci` was chosen but the repo has no CI on the PR
	// head — only reachable under an explicit `ci` gate (autopilot.md §6.1).
	ErrCIMissing LandErrorKind = "ci_missing"
	// ErrNotMergeable: the PR has conflicts against the integration branch.
	ErrNotMergeable LandErrorKind = "not_mergeable"
	// ErrNotOwned: the target is not an autopilot-owned worker branch/agent.
	ErrNotOwned LandErrorKind = "not_owned"
	// ErrRunDisabled: the caller's run is not active (kill switch honored).
	ErrRunDisabled LandErrorKind = "run_disabled"
	// ErrWrongBase: no PR based on the integration branch exists, or the branch/
	// target is the repo default (main) — never a valid land source or target.
	ErrWrongBase LandErrorKind = "wrong_base"
)

// LandError is a typed precondition failure (autopilot.md §6). It signals that
// NO side effect occurred: nothing was merged, no branch deleted, no landing
// recorded. Detail is optional context (the failing check summary for gate_red).
type LandError struct {
	Kind   LandErrorKind
	Detail string
}

func (e *LandError) Error() string {
	if e.Detail != "" {
		return "autopilot land: " + string(e.Kind) + ": " + e.Detail
	}
	return "autopilot land: " + string(e.Kind)
}

// LandResult is a successful land outcome. AlreadyLanded is true when the merge
// had already happened (idempotent re-issue after a brain restart, §6): no new
// merge was performed. SHA is the landed commit reported to the caller (the merge
// commit when known); HeadSHA is the PR head the gate ran against — the daemon
// records it as the landing's `sha` because idempotency keys on the head SHA (§6).
type LandResult struct {
	SHA           string `json:"sha"`
	HeadSHA       string `json:"-"`
	PR            int    `json:"pr,omitempty"`
	Branch        string `json:"branch"`
	AlreadyLanded bool   `json:"already_landed"`
}

// GateState is the concluded state of a merge gate for one PR head SHA. It is
// the host's answer, mapped by the land orchestration onto the typed errors.
type GateState string

const (
	GateGreen   GateState = "green"   // gate passed → merge may proceed
	GateRed     GateState = "red"     // gate failed → gate_red
	GatePending GateState = "pending" // gate not concluded → gate_pending
	GateMissing GateState = "missing" // no CI at all → ci_missing (mode `ci`)
)

// PRInfo is the pull request the host resolved for a worker branch (autopilot.md
// §6 preconditions 3/5, idempotency). A found=false lookup means no PR exists.
type PRInfo struct {
	Number      int
	BaseRef     string // base branch — must be the integration branch
	HeadSHA     string // head commit the gate runs against and landings key on
	Merged      bool   // already merged → already_landed (idempotency)
	MergeCommit string // the merge commit SHA when Merged
	Mergeable   bool   // no conflicts against the base (precondition 5)
}

// LandHost is the daemon-provided host surface the land orchestration drives:
// PR lookup, the two gate implementations (CI via branchtrack, local via the
// `check` rail), and the merge itself. It is injected so land.go is fully
// table-testable with a fake, keeping the autopilot package free of gh/git.
type LandHost interface {
	// FindPR returns the PR whose head is branch (found=false when none exists).
	FindPR(ctx context.Context, branch string) (PRInfo, bool, error)
	// GateCI reports the CI conclusion for headSHA (via branchtrack).
	GateCI(ctx context.Context, worktree, branch, headSHA string) (GateState, string, error)
	// GateLocal runs the project checks against the PR head worktree and gates on
	// the result (never GateMissing — local always concludes green or red).
	GateLocal(ctx context.Context, worktree string) (GateState, string, error)
	// Merge merges the PR with strategy (squash|merge|rebase), optionally deleting
	// the worker branch, and returns the resulting merge commit SHA.
	Merge(ctx context.Context, pr int, strategy string, deleteBranch bool) (sha string, err error)
}

// LandRequest is everything the daemon resolves before running the land
// orchestration: the kill-switch/ownership facts plus the resolved merge target,
// gate, and strategy for the owning run.
type LandRequest struct {
	RunActive         bool   // precondition 1: caller's run is active (kill switch)
	Owned             bool   // precondition 2: branch is autopilot-owned
	Branch            string // resolved worker branch (from agent_or_branch)
	Worktree          string // PR head worktree (local gate + CI query dir)
	IntegrationBranch string // the only branch autopilot merges into
	DefaultBranch     string // repo default branch — never a valid source/target
	Gate              string // resolved gate mode: ci | local
	Strategy          string // merge strategy (squash|merge|rebase)
	DeleteBranch      bool   // delete the worker branch after merge
}

// Land runs the idempotent, gated merge of one worker branch into the
// integration branch (autopilot.md §6). It performs the preconditions in spec
// order, short-circuits idempotently on an already-merged/recorded head, and
// returns either a LandResult or a typed *LandError (no side effects on any
// LandError). A non-LandError return is an infrastructure failure (a host error
// or a ledger read failure) — also with no side effects. The AUTHORITATIVE
// landings-ledger write is the daemon handler's job AFTER a non-idempotent
// success; ledger here is read-only, used for the idempotency check.
func Land(ctx context.Context, req LandRequest, host LandHost, ledger *Ledger) (LandResult, error) {
	// Precondition 1: kill switch. A disabled/torn-down run lands nothing.
	if !req.RunActive {
		return LandResult{}, &LandError{Kind: ErrRunDisabled}
	}
	// Precondition 2: ownership. Foreign/manual branches are untouchable.
	if !req.Owned {
		return LandResult{}, &LandError{Kind: ErrNotOwned}
	}
	// main/default is never a valid land source or target (§6 semantics). The
	// integration branch being protected is a preflight-time error too, but guard
	// it here so a misconfigured run can never merge into main.
	if isProtectedBranch(req.Branch, req.DefaultBranch) || isProtectedBranch(req.IntegrationBranch, req.DefaultBranch) {
		return LandResult{}, &LandError{Kind: ErrWrongBase, Detail: "the default branch (main) is never a valid land source or target"}
	}

	// Precondition 3: a PR exists whose base is the integration branch.
	pr, ok, err := host.FindPR(ctx, req.Branch)
	if err != nil {
		return LandResult{}, fmt.Errorf("land: find PR for %s: %w", req.Branch, err)
	}
	if !ok {
		return LandResult{}, &LandError{Kind: ErrWrongBase, Detail: "no pull request found for branch " + req.Branch}
	}
	// Idempotency: an already-merged PR is a no-op success (§6). Checked before the
	// base assertion so a merged-and-deleted branch still reports already_landed.
	if pr.Merged {
		return LandResult{SHA: firstNonEmpty(pr.MergeCommit, pr.HeadSHA), HeadSHA: pr.HeadSHA, PR: pr.Number, Branch: req.Branch, AlreadyLanded: true}, nil
	}
	if pr.BaseRef != req.IntegrationBranch {
		return LandResult{}, &LandError{Kind: ErrWrongBase, Detail: fmt.Sprintf("PR #%d base is %q, not the integration branch %q", pr.Number, pr.BaseRef, req.IntegrationBranch)}
	}
	// Idempotency: the head SHA already recorded in the landings ledger is a no-op
	// success — a brain re-issuing after a mid-action restart lands nothing twice.
	if ledger != nil {
		landings, err := ledger.Landings()
		if err != nil {
			return LandResult{}, fmt.Errorf("land: read landings: %w", err)
		}
		for _, l := range landings {
			if l.SHA == pr.HeadSHA {
				return LandResult{SHA: l.SHA, HeadSHA: l.SHA, PR: l.PR, Branch: l.Branch, AlreadyLanded: true}, nil
			}
		}
	}

	// Precondition 4: the gate is green for the PR head SHA. Never merge on a red,
	// pending, missing, or un-evaluable gate.
	if err := checkGate(ctx, req, host, pr); err != nil {
		return LandResult{}, err
	}
	// Precondition 5: the PR is mergeable (no conflicts).
	if !pr.Mergeable {
		return LandResult{}, &LandError{Kind: ErrNotMergeable, Detail: fmt.Sprintf("PR #%d has conflicts against %s", pr.Number, req.IntegrationBranch)}
	}

	sha, err := host.Merge(ctx, pr.Number, req.Strategy, req.DeleteBranch)
	if err != nil {
		return LandResult{}, fmt.Errorf("land: merge PR #%d: %w", pr.Number, err)
	}
	return LandResult{SHA: firstNonEmpty(sha, pr.HeadSHA), HeadSHA: pr.HeadSHA, PR: pr.Number, Branch: req.Branch}, nil
}

// checkGate evaluates the resolved gate for the PR head SHA and maps the host's
// GateState onto the typed errors. A host error is surfaced as a plain error (an
// infra failure, not a precondition) — "never merge on an unknown gate" holds
// either way because Land returns before merging.
func checkGate(ctx context.Context, req LandRequest, host LandHost, pr PRInfo) error {
	var (
		state   GateState
		summary string
		err     error
	)
	switch req.Gate {
	case "local":
		state, summary, err = host.GateLocal(ctx, req.Worktree)
	default: // "ci" — the stronger gate; any unexpected value defaults to it
		state, summary, err = host.GateCI(ctx, req.Worktree, req.Branch, pr.HeadSHA)
	}
	if err != nil {
		return fmt.Errorf("land: evaluate %s gate: %w", req.Gate, err)
	}
	switch state {
	case GateGreen:
		return nil
	case GateRed:
		return &LandError{Kind: ErrGateRed, Detail: summary}
	case GatePending:
		return &LandError{Kind: ErrGatePending, Detail: summary}
	case GateMissing:
		return &LandError{Kind: ErrCIMissing, Detail: summary}
	default:
		return &LandError{Kind: ErrGatePending, Detail: "gate returned an unknown state"}
	}
}

// resolveGateMode resolves the configured gate mode to the concrete mode that
// actually runs (autopilot.md §6.1): `auto` becomes `ci` when the repo has
// workflows covering integration PRs, else `local`; `ci`/`local` pass through.
// An empty/unknown value is treated as `auto`, so a no-CI repo never dead-ends.
func resolveGateMode(configured string, workflowsCoverPRs bool) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "ci":
		return "ci"
	case "local":
		return "local"
	default: // auto (and any unexpected value)
		if workflowsCoverPRs {
			return "ci"
		}
		return "local"
	}
}

// firstNonEmpty returns the first non-empty argument (or "" when all are empty).
func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
