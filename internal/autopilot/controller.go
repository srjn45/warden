package autopilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ControllerConfig is the S1 slice of the `autopilot` config block the Controller
// needs to run the switch and the preflight. The daemon translates
// config.AutopilotConfig into this so the autopilot package stays free of a
// dependency on the config package.
type ControllerConfig struct {
	// Plans are the configured plan-file paths (config autopilot.plans[].file).
	// Relative paths resolve against BaseDir.
	Plans []string
	// IntegrationBranch is the merge target (config autopilot.merge.target_branch).
	// The only branch autopilot merges into; must not be a protected name.
	IntegrationBranch string
	// Gate is the configured gate mode (config autopilot.merge.gate): auto|ci|local.
	// S1 reports it verbatim in status; S4 resolves `auto` at preflight.
	Gate string
	// BaseDir anchors relative plan paths (the daemon's working directory).
	BaseDir string
}

// Controller is the autopilot master switch and per-plan run registry
// (autopilot.md §2.1). In the S1 inert core it runs the full enable-time
// preflight and drives the disabled→starting→active state machine WITHOUT
// spawning a brain — that lands in S3. It is safe for concurrent use.
type Controller struct {
	env               Env
	plans             []string
	integrationBranch string
	gate              string
	baseDir           string

	mu      sync.Mutex
	enabled bool
	runs    map[string]*run // keyed by run_id
}

// run is one registered plan execution. In S1 it holds only the identity + state
// (no brain, ledger, or worker map — those arrive in S3).
type run struct {
	runID    string
	planFile string
	repo     string
	state    RunState
}

// NewController builds a Controller from cfg backed by env (pass NewExecEnv() in
// production). A nil env defaults to the real git+gh-backed environment.
func NewController(cfg ControllerConfig, env Env) *Controller {
	if env == nil {
		env = NewExecEnv()
	}
	branch := strings.TrimSpace(cfg.IntegrationBranch)
	if branch == "" {
		branch = "autopilot/integration"
	}
	gate := strings.TrimSpace(cfg.Gate)
	if gate == "" {
		gate = "auto"
	}
	return &Controller{
		env:               env,
		plans:             cfg.Plans,
		integrationBranch: branch,
		gate:              gate,
		baseDir:           cfg.BaseDir,
		runs:              map[string]*run{},
	}
}

// RunID is the stable identifier for a run: a short hash of the repo root and the
// absolute plan-file path (autopilot.md §1). Stable across daemon restarts so a
// re-enable re-adopts the same run rather than forking a duplicate.
func RunID(repo, planPath string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + planPath))
	return "ap-" + hex.EncodeToString(sum[:])[:12]
}

// PreflightError is the typed 409 result of a failed Enable: the full list of
// actionable failures (autopilot.md §5, §5.1), not just the first.
type PreflightError struct {
	Failures []string
}

func (e *PreflightError) Error() string {
	if len(e.Failures) == 0 {
		return "autopilot preflight failed"
	}
	return "autopilot preflight failed: " + strings.Join(e.Failures, "; ")
}

// Enable turns the switch on. It runs the enable-time preflight (§5.1) across
// every configured plan and, only if ALL checks pass, registers each plan as an
// active run. On any failure it changes no state and returns a *PreflightError
// carrying every failure. Enable is atomic and idempotent: it rebuilds the run
// set from config each call, so re-enabling the same config is a no-op that
// yields the same run ids.
func (c *Controller) Enable(ctx context.Context) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.plans) == 0 {
		return c.statusLocked(), &PreflightError{Failures: []string{
			"no plans configured — add at least one autopilot.plans[].file (run `warden autopilot init`)",
		}}
	}

	var fails []string
	resolvedByID := map[string]resolved{}
	// repoOwner maps a repo root to the run_id that already claimed it in THIS
	// batch — the mechanical "at most one active run per repository" guard (§1).
	repoOwner := map[string]string{}

	for _, file := range c.plans {
		r, planFails := c.preflightPlan(ctx, file)
		fails = append(fails, planFails...)
		if r.repo == "" {
			continue // repo unresolved — its failures are already recorded
		}
		if owner, taken := repoOwner[r.repo]; taken && owner != r.runID {
			fails = append(fails, fmt.Sprintf(
				"repo %s already has an active run in this plan set — at most one active run per repository (conflicting plan: %s)",
				r.repo, file))
			continue
		}
		repoOwner[r.repo] = r.runID
		resolvedByID[r.runID] = r
	}

	if len(fails) > 0 {
		sort.Strings(fails)
		return c.statusLocked(), &PreflightError{Failures: dedupe(fails)}
	}

	// All clear — register every run active. Inert: with no brain to wait on,
	// "brain healthy" is immediate, so starting collapses straight to active.
	runs := map[string]*run{}
	for id, r := range resolvedByID {
		runs[id] = &run{runID: id, planFile: r.file, repo: r.repo, state: StateActive}
	}
	c.runs = runs
	c.enabled = true
	return c.statusLocked(), nil
}

// Disable is the kill switch (§2.1): it flips the master flag off and drops the
// run registry. In S1 there is nothing to tear down; from S3 the brain is
// terminated gracefully here while in-flight workers keep running.
func (c *Controller) Disable(_ context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = false
	c.runs = map[string]*run{}
	return c.statusLocked()
}

// Status returns the current AutopilotStatus (§5).
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

// statusLocked builds the status snapshot; the caller must hold c.mu.
func (c *Controller) statusLocked() Status {
	st := Status{Enabled: c.enabled, Runs: []RunStatus{}}
	for _, r := range c.runs {
		st.Runs = append(st.Runs, RunStatus{
			RunID:    r.runID,
			PlanFile: r.planFile,
			Repo:     r.repo,
			State:    r.state,
			Gate:     c.gate, // S4 replaces this with the resolved mode
			Brain:    nil,    // no brain in S1
			Tasks:    TaskCounts{},
			Backoff:  nil,
		})
	}
	sort.Slice(st.Runs, func(i, j int) bool { return st.Runs[i].RunID < st.Runs[j].RunID })
	return st
}

// dedupe returns xs with adjacent duplicates removed (xs must be sorted).
func dedupe(xs []string) []string {
	out := xs[:0]
	var last string
	for i, x := range xs {
		if i == 0 || x != last {
			out = append(out, x)
		}
		last = x
	}
	return out
}
