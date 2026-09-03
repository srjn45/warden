package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/branchtrack"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

// autopilotOwnTag mirrors the tag the autopilot Controller stamps on the brain and
// its workers (autopilot.md §1). The land handler reads it (with runTagPrefix,
// declared in ownership_guard.go) to decide ownership (precondition 2) and the
// owning run (for its merge params).
const autopilotOwnTag = "autopilot"

// LandAutopilot implements POST /api/v1/autopilot/land: the brain's only merge
// path (autopilot.md §6). It resolves the worker branch, runs the gated,
// idempotent land orchestration, and — on a non-idempotent success — writes the
// landing to the run ledger authoritatively (never trusted to the brain) and
// audit-logs it. Precondition failures return 409 with a typed kind and change
// nothing.
func (s *Server) LandAutopilot(ctx context.Context, req oapi.LandAutopilotRequestObject) (oapi.LandAutopilotResponseObject, error) {
	if s.autopilot == nil {
		return oapi.LandAutopilot403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	ref := ""
	if req.Body != nil {
		ref = strings.TrimSpace(req.Body.AgentOrBranch)
	}
	if ref == "" {
		return oapi.LandAutopilot400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "agent_or_branch is required"}}, nil
	}

	tgt := s.resolveLandTarget(ctx, ref)

	// The kill switch / run params live in the Controller. A run that is not owned
	// short-circuits to not_owned (RunActive left true so Land returns not_owned,
	// not run_disabled); an owned target whose run has vanished is treated the same.
	var params autopilot.LandParams
	active := true
	if tgt.owned {
		lp, found := s.autopilot.LandParams(tgt.runID)
		if !found {
			tgt.owned = false
		} else {
			params = lp
			active = lp.Active
		}
	}

	var ledger *autopilot.Ledger
	if s.cstore != nil && tgt.runID != "" {
		ledger = autopilot.NewLedger(ctxLedgerStore{cs: s.cstore}, tgt.runID)
	}

	host := s.landHost(firstNonEmptyStr(tgt.worktree, params.Repo))
	res, err := autopilot.Land(ctx, autopilot.LandRequest{
		RunActive:         active,
		Owned:             tgt.owned,
		Branch:            tgt.branch,
		Worktree:          tgt.worktree,
		IntegrationBranch: params.IntegrationBranch,
		DefaultBranch:     params.DefaultBranch,
		Gate:              params.Gate,
		Strategy:          params.Strategy,
		DeleteBranch:      params.DeleteBranch,
	}, host, ledger)
	if err != nil {
		var le *autopilot.LandError
		if errors.As(err, &le) {
			return oapi.LandAutopilot409JSONResponse{
				Error:  "autopilot land precondition failed",
				Kind:   oapi.AutopilotLandErrorKind(le.Kind),
				Detail: le.Detail,
			}, nil
		}
		return nil, err
	}

	// Authoritative landing record (autopilot.md §4, §6): the daemon — not the
	// brain — writes it, inside this handler, so `land`'s idempotency ledger can
	// never be forged or forgotten. Skip on an idempotent re-issue (already
	// recorded). A ledger write failure does not un-merge the PR, so it is logged,
	// not surfaced as a land failure.
	if !res.AlreadyLanded && ledger != nil {
		// The landing's sha is the PR HEAD SHA — the idempotency key `land` reads on
		// a re-issue (autopilot.md §6), not the merge commit.
		if lerr := ledger.AppendLanding(autopilot.Landing{
			Branch:   res.Branch,
			SHA:      res.HeadSHA,
			PR:       res.PR,
			LandedAt: time.Now().UTC().Format(time.RFC3339),
		}); lerr != nil {
			s.recordAuditCtx(ctx, audit.ActionAutopilotLand, tgt.runID, map[string]string{
				"branch": res.Branch, "sha": res.SHA, "ledger_error": lerr.Error(),
			})
		}
	}
	if !res.AlreadyLanded {
		s.recordAuditCtx(ctx, audit.ActionAutopilotLand, tgt.runID, map[string]string{
			"branch": res.Branch, "sha": res.SHA, "pr": strconv.Itoa(res.PR),
		})
		if s.autopilot != nil && res.PR > 0 && tgt.owned && tgt.runID != "" && tgt.taskID != "" {
			if _, err := s.autopilot.UpdateTaskStatus(tgt.runID, tgt.taskID, autopilot.TaskStatusDone, res.PR); err != nil {
				s.recordAuditCtx(ctx, audit.ActionAutopilotLand, tgt.runID, map[string]string{
					"branch": res.Branch, "task_id": tgt.taskID, "task_status_error": err.Error(),
				})
			}
		}
	}

	return oapi.LandAutopilot200JSONResponse{
		Sha:           res.SHA,
		Pr:            res.PR,
		Branch:        res.Branch,
		AlreadyLanded: res.AlreadyLanded,
	}, nil
}

// landHost builds the LandHost the land orchestration drives — the gh/git +
// check-rail host in production, or a test-injected fake via landHostFn.
func (s *Server) landHost(dir string) autopilot.LandHost {
	if s.landHostFn != nil {
		return s.landHostFn(dir)
	}
	return daemonLandHost{s: s, dir: dir}
}

// landTarget is the daemon's resolution of an agent_or_branch reference: the
// worker branch, its worktree, the owning run, and whether it is autopilot-owned.
type landTarget struct {
	branch   string
	worktree string
	runID    string
	taskID   string
	owned    bool
}

// resolveLandTarget maps an agent_or_branch reference to a landTarget. It first
// tries an agent lookup (id or name); on a miss it treats the reference as a
// branch and finds the autopilot-owned session on that branch. Ownership follows
// the tags the Controller stamps (autopilot.md §1): the target must carry the
// `autopilot` tag and a `run:<id>` tag.
func (s *Server) resolveLandTarget(ctx context.Context, ref string) landTarget {
	if sess, err := s.store.GetByNameOrID(ctx, ref); err == nil {
		return sessionLandTarget(sess, sess.Branch)
	}
	// Branch reference: find the autopilot-owned session whose branch matches.
	sessions, err := s.store.List(ctx)
	if err == nil {
		for _, sess := range sessions {
			if sess.Branch == ref && isAutopilotOwned(sess.Tags) {
				return sessionLandTarget(sess, ref)
			}
		}
	}
	// Unresolvable to an owned worker — carry the branch through so Land returns
	// not_owned rather than the handler guessing.
	return landTarget{branch: ref}
}

// sessionLandTarget builds a landTarget from a resolved session.
func sessionLandTarget(sess *store.Session, branch string) landTarget {
	owned, runID := ownershipFromTags(sess.Tags)
	taskID := strings.TrimSpace(sess.AutopilotTaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(sess.Task)
	}
	return landTarget{branch: branch, worktree: sess.Worktree, runID: runID, taskID: taskID, owned: owned}
}

// isAutopilotOwned reports whether tags carry the autopilot ownership tag.
func isAutopilotOwned(tags []string) bool {
	owned, _ := ownershipFromTags(tags)
	return owned
}

// ownershipFromTags extracts autopilot ownership from a session's tags: owned is
// true only when both the `autopilot` tag and a `run:<id>` tag are present, and
// runID is that run.
func ownershipFromTags(tags []string) (owned bool, runID string) {
	hasAutopilot := false
	for _, t := range tags {
		switch {
		case t == autopilotOwnTag:
			hasAutopilot = true
		case strings.HasPrefix(t, runTagPrefix):
			runID = strings.TrimPrefix(t, runTagPrefix)
		}
	}
	return hasAutopilot && runID != "", runID
}

// daemonLandHost implements autopilot.LandHost with gh/git and the check rail.
// dir is where gh/git run (the worker's worktree, so gh infers the remote; the
// repo root as a fallback).
type daemonLandHost struct {
	s   *Server
	dir string
}

// ghPRView is the subset of `gh pr view --json` the land host consumes.
type ghPRView struct {
	Number      int    `json:"number"`
	BaseRefName string `json:"baseRefName"`
	HeadRefOid  string `json:"headRefOid"`
	Mergeable   string `json:"mergeable"` // MERGEABLE | CONFLICTING | UNKNOWN
	State       string `json:"state"`     // OPEN | MERGED | CLOSED
	MergeCommit struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
}

func (h daemonLandHost) FindPR(ctx context.Context, branch string) (autopilot.PRInfo, bool, error) {
	out, err := h.runGH(ctx, "pr", "view", branch,
		"--json", "number,baseRefName,headRefOid,mergeable,state,mergeCommit")
	if err != nil {
		// gh exits non-zero when no PR exists for the branch — that is "not found",
		// not an infrastructure failure.
		if strings.Contains(strings.ToLower(err.Error()), "no pull requests found") {
			return autopilot.PRInfo{}, false, nil
		}
		return autopilot.PRInfo{}, false, err
	}
	var v ghPRView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return autopilot.PRInfo{}, false, fmt.Errorf("decode gh pr view: %w", err)
	}
	return autopilot.PRInfo{
		Number:      v.Number,
		BaseRef:     v.BaseRefName,
		HeadSHA:     v.HeadRefOid,
		Merged:      strings.EqualFold(v.State, "MERGED"),
		MergeCommit: v.MergeCommit.Oid,
		// Only a confirmed MERGEABLE state clears precondition 5; CONFLICTING is a
		// conflict and UNKNOWN (GitHub still computing) is not-yet-confirmed — both
		// yield not_mergeable and the brain retries.
		Mergeable: strings.EqualFold(v.Mergeable, "MERGEABLE"),
	}, true, nil
}

func (h daemonLandHost) GateCI(ctx context.Context, worktree, branch, headSHA string) (autopilot.GateState, string, error) {
	dir := firstNonEmptyStr(worktree, h.dir)
	st := branchtrack.StatusForSHA(ctx, dir, branch, headSHA)
	switch st.State {
	case branchtrack.CISuccess:
		return autopilot.GateGreen, "", nil
	case branchtrack.CIFailure:
		return autopilot.GateRed, fmt.Sprintf("CI failed: %s (%s)", st.Workflow, st.URL), nil
	case branchtrack.CIPending:
		return autopilot.GatePending, "CI has not concluded for the PR head", nil
	default: // CINone
		return autopilot.GateMissing, "no CI runs found for the PR head", nil
	}
}

func (h daemonLandHost) GateLocal(ctx context.Context, worktree string) (autopilot.GateState, string, error) {
	dir := firstNonEmptyStr(worktree, h.dir)
	res, err := h.s.life.Check(ctx, dir, "")
	if errors.Is(err, lifecycle.ErrNoCheckConfig) {
		// A no-CI repo resolved to `local` but registered no checks: there is
		// nothing to gate on, so nothing is red. Frictionless degradation — the
		// owner enabled autopilot on an ungated repo (autopilot.md §6.1).
		return autopilot.GateGreen, "no project checks configured", nil
	}
	if err != nil {
		return "", "", err
	}
	if res.Passed {
		return autopilot.GateGreen, "", nil
	}
	return autopilot.GateRed, summarizeFailedChecks(res), nil
}

func (h daemonLandHost) Merge(ctx context.Context, pr int, strategy string, deleteBranch bool) (string, error) {
	args := []string{"pr", "merge", strconv.Itoa(pr), mergeStrategyFlag(strategy)}
	if deleteBranch {
		args = append(args, "--delete-branch")
	}
	if _, err := h.runGH(ctx, args...); err != nil {
		return "", err
	}
	// Read back the merge commit so the landing records the authoritative SHA.
	out, err := h.runGH(ctx, "pr", "view", strconv.Itoa(pr), "--json", "mergeCommit")
	if err != nil {
		return "", nil // merged, but SHA readback failed — Land falls back to head SHA
	}
	var v ghPRView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return "", nil
	}
	return v.MergeCommit.Oid, nil
}

// runGH runs a gh subprocess in the host's dir and returns stdout, folding
// stderr into the error so "no pull requests found" is detectable.
func (h daemonLandHost) runGH(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = h.dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return string(out), nil
}

// mergeStrategyFlag maps a configured merge strategy to the gh pr merge flag,
// defaulting to squash (the configured default).
func mergeStrategyFlag(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "merge":
		return "--merge"
	case "rebase":
		return "--rebase"
	default:
		return "--squash"
	}
}

// summarizeFailedChecks renders a compact one-line-per-failure summary of a
// failed check run for the gate_red detail, so the brain sees which check failed
// without the full log.
func summarizeFailedChecks(res lifecycle.CheckResult) string {
	var parts []string
	for _, c := range res.Checks {
		if c.Passed {
			continue
		}
		line := firstLine(c.Output)
		if line == "" {
			line = "exit " + strconv.Itoa(c.ExitCode)
		}
		parts = append(parts, fmt.Sprintf("%s: %s", c.Name, line))
	}
	if len(parts) == 0 {
		return "project checks failed"
	}
	return strings.Join(parts, "; ")
}

// firstLine returns the last non-empty line of s (test runners print the
// decisive failure last), capped so the detail stays compact.
func firstLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 200 {
				return l[:199] + "…"
			}
			return l
		}
	}
	return ""
}

// firstNonEmptyStr returns the first non-empty argument.
func firstNonEmptyStr(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
