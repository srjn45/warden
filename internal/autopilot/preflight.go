package autopilot

import (
	"context"
	"fmt"
	"log/slog"
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
	file              string // the configured file path (as written in config)
	absFile           string // resolved absolute path
	repo              string // git toplevel containing the plan file
	runID             string // stable hash of repo + absFile
	plan              Plan
	resolvedGate      string // gate mode resolved from `auto` (§6.1): ci | local
	defaultBranch     string // repo default branch — the land guard's protected name
	integrationBranch string // per-run merge target, resolved once
	skipComplete      bool   // the plan carries the completion marker (§2.1): skip, don't register
	gateWarning       string // set when gate auto downgrades to local (uncovered branch)
}

// preflightPlan runs the enable-time checks that concern a single plan in
// isolation (autopilot.md §5.1) and returns the resolved run info plus a list of
// human-actionable failure strings — ALL of them, so the owner fixes everything
// in one pass rather than one round-trip per problem. Cross-plan identity
// checks (name uniqueness, slot-scope claims) live in Register/Enable, which
// see the full run set. Failures never panic on a half-resolved plan: once the
// repo can't be found we return early because the remaining checks are repo-scoped.
func (c *Controller) preflightPlan(ctx context.Context, file string, pending map[string]string) (resolved, []string) {
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
	r.repo = filepath.Clean(repo)
	r.runID = RunID(r.repo, abs)

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

	name := defaultRunName(abs)
	stored := ""
	if existing, ok := c.runs[r.runID]; ok {
		if existing.name != "" {
			name = existing.name
		}
		stored = existing.integrationBranch
	}
	branch, err := resolveIntegrationBranch(branchResolveOpts{
		planName: name,
		runID:    r.runID,
		stored:   stored,
		template: c.integrationBranch,
		taken:    c.branchTakenLocked(r.repo, r.runID, pending),
	})
	if err != nil {
		fails = append(fails, err.Error())
	} else {
		r.integrationBranch = branch
		c.warnSameBranchLocked(r.repo, branch, r.runID, pending)
	}

	// Protected-name check + integration branch auto-create. The integration
	// branch is the ONLY branch autopilot merges into; it must not be a protected
	// name, and it must exist (created off the default branch when absent).
	def, defErr := c.env.DefaultBranch(ctx, repo)
	r.defaultBranch = def
	if isProtectedBranch(branch, def) {
		fails = append(fails, fmt.Sprintf("integration branch %q is a protected name — pick a dedicated branch (e.g. autopilot/<plan-name>)", branch))
	} else if branch != "" {
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
	r.gateWarning = gateDowngradeWarning(c.gate, r.resolvedGate, branch, covers)
	if r.gateWarning != "" {
		slog.Warn("autopilot: "+r.gateWarning, "run", r.runID, "branch", branch)
	}

	return r, fails
}

// isAutoGate reports whether the configured gate is `auto` (or unset, which
// defaults to auto) — the only mode that inspects the repo's workflows.
func isAutoGate(gate string) bool {
	g := strings.ToLower(strings.TrimSpace(gate))
	return g == "" || g == "auto"
}

// gateDowngradeWarning is the operator-visible note when `auto` falls back to
// `local` because no workflow covers the resolved integration branch.
func gateDowngradeWarning(configured, resolved, branch string, covers bool) string {
	if !isAutoGate(configured) || resolved != "local" || covers {
		return ""
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return ""
	}
	return fmt.Sprintf("gate auto downgraded to local: no CI workflow covers %q; add %q to on.pull_request.branches", branch, "autopilot/**")
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
