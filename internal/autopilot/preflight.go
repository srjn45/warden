package autopilot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// protectedBranchNames are integration-branch names autopilot refuses to own:
// the merge target must never be main/master (or HEAD). The repo's own default
// branch is added dynamically in preflightPlan.
var protectedBranchNames = map[string]bool{
	"main":   true,
	"master": true,
	"head":   true,
	"HEAD":   true,
}

// resolved carries everything preflight learned about one configured plan so the
// Controller can register the run without re-deriving it.
type resolved struct {
	file          string // the configured file path (as written in config)
	absFile       string // resolved absolute path
	repo          string // git toplevel containing the plan file
	runID         string // stable hash of repo + absFile
	plan          Plan
	resolvedGate  string // gate mode resolved from `auto` (§6.1): ci | local
	defaultBranch string // repo default branch — the land guard's protected name
	skipComplete  bool   // the plan carries the completion marker (§2.1): skip, don't register
}

// preflightPlan runs the enable-time checks that concern a single plan in
// isolation (autopilot.md §5.1) and returns the resolved run info plus a list of
// human-actionable failure strings — ALL of them, so the owner fixes everything
// in one pass rather than one round-trip per problem. The cross-plan
// "no second active run per repo" check lives in Enable, which sees the whole
// batch. Failures never panic on a half-resolved plan: once the repo can't be
// found we return early because the remaining checks are repo-scoped.
func (c *Controller) preflightPlan(ctx context.Context, file string) (resolved, []string) {
	var fails []string
	r := resolved{file: file}

	abs := file
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(c.baseDir, file)
	}
	abs = filepath.Clean(abs)
	r.absFile = abs

	// Plan file must exist and strict-decode (§3). This is the most common
	// failure and the whole point of front-loading friction to enable time.
	plan, err := LoadPlan(abs)
	if err != nil {
		fails = append(fails, err.Error())
	} else {
		r.plan = plan
	}

	// Resolve the repo from the plan file's directory. Without it, the remaining
	// checks (branch, run_id) have no anchor, so we stop here.
	repo, err := c.env.GitToplevel(ctx, filepath.Dir(abs))
	if err != nil {
		fails = append(fails, fmt.Sprintf("plan file %s is not inside a git repository: %v", file, err))
		return r, fails
	}
	r.repo = repo
	r.runID = RunID(repo, abs)

	// A completed plan (§2.1) is done: preflight neither fails it nor registers it
	// as a run. Signal the skip now — with repo/run_id resolved so Enable can log a
	// useful line — and return before the enable-time checks (gh, branch, backend,
	// gate), which only concern a plan that will actually run again.
	if r.plan.IsComplete() {
		r.skipComplete = true
		return r, fails
	}

	// gh must be authenticated — a dead login would stall PR gating at 3am.
	if err := c.env.GHAuthOK(ctx); err != nil {
		fails = append(fails, err.Error())
	}

	// Protected-name check + integration branch auto-create. The integration
	// branch is the ONLY branch autopilot merges into; it must not be a protected
	// name, and it must exist (created off the default branch when absent).
	branch := c.integrationBranch
	def, defErr := c.env.DefaultBranch(ctx, repo)
	r.defaultBranch = def
	if isProtectedBranch(branch, def) {
		fails = append(fails, fmt.Sprintf("integration branch %q is a protected name — pick a dedicated branch (e.g. autopilot/integration)", branch))
	} else {
		exists, err := c.env.BranchExists(ctx, repo, branch)
		switch {
		case err != nil:
			fails = append(fails, fmt.Sprintf("cannot check integration branch %q: %v", branch, err))
		case exists:
			// good — nothing to create
		case defErr != nil:
			fails = append(fails, fmt.Sprintf("integration branch %q is missing and the default branch could not be resolved to create it: %v", branch, defErr))
		default:
			if err := c.env.CreateBranch(ctx, repo, branch, def); err != nil {
				fails = append(fails, fmt.Sprintf("could not auto-create integration branch %q off %s: %v", branch, def, err))
			}
		}
	}

	// Backend-trust check (§5.1): every backend the ladder could ever select must
	// be one warden can launch, so a typo'd or uninstalled backend surfaces at
	// enable rather than mid-rotation at 3am. S3 checks the mechanical half
	// (known/launchable); deeper per-repo trust/auth detection lands with the tier
	// failover work (S5). An empty ladder is not a failure here — brain selection
	// falls back to the daemon default.
	for _, backend := range c.backends.all() {
		if err := c.env.BackendKnown(backend); err != nil {
			fails = append(fails, err.Error())
		}
	}

	// Gate-mode resolution (§6.1): resolve `auto` to the concrete mode that will
	// gate landings and record it, so the daemon reports it in AutopilotStatus and
	// the land handler gates on it. `ci`/`local` pass through untouched; only
	// `auto` inspects whether the repo has workflows covering integration PRs, and
	// a scan error fails open to the safe `local` gate (frictionless-safeguards):
	// a no-CI repo degrades to local checks instead of wedging, and CI remains the
	// stronger gate the adopter can graduate to.
	covers := false
	if isAutoGate(c.gate) {
		if ok, err := c.env.WorkflowsCoverPRs(ctx, repo, branch); err == nil {
			covers = ok
		}
	}
	r.resolvedGate = resolveGateMode(c.gate, covers)

	return r, fails
}

// isAutoGate reports whether the configured gate is `auto` (or unset, which
// defaults to auto) — the only mode that inspects the repo's workflows.
func isAutoGate(gate string) bool {
	g := strings.ToLower(strings.TrimSpace(gate))
	return g == "" || g == "auto"
}

// isProtectedBranch reports whether branch is a name autopilot must not merge
// into: a built-in protected name or the repo's own default branch.
func isProtectedBranch(branch, defaultBranch string) bool {
	b := strings.TrimSpace(branch)
	if b == "" {
		return true // an empty target is never valid
	}
	if protectedBranchNames[strings.ToLower(b)] {
		return true
	}
	return defaultBranch != "" && b == defaultBranch
}
