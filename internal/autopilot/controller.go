package autopilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
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
	// Backends is the cost-tier backend ladder (config autopilot.brain.backends).
	// S3 uses Free (brain selection) and the union (preflight trust check).
	Backends BackendLadder
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
	backends          BackendLadder

	mu      sync.Mutex
	runtime Runtime // nil ⇒ inert (S1): no brain spawns
	enabled bool
	runs    map[string]*run // keyed by run_id
}

// run is one registered plan execution: identity + state plus, once the brain
// lifecycle is live (S3), the brain handle, the last-good plan for owner steering
// (autopilot.md §3), and the plan-watch cancel.
type run struct {
	runID       string
	planFile    string // configured path (as written in config)
	absPlanFile string // resolved absolute path (plan-file watch anchor)
	repo        string
	state       RunState
	plan        Plan
	planModTime time.Time

	brain  *BrainHandle       // nil until the brain spawns; nil again after teardown
	cancel context.CancelFunc // stops the plan-watch goroutine (nil in inert mode)
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
		backends:          cfg.Backends,
		runs:              map[string]*run{},
	}
}

// SetRuntime injects the daemon-provided brain/ledger/digest surface. It must be
// called before Enable to run real brains; without it the Controller stays inert
// (S1 behavior: the switch + preflight work but no brain spawns). The daemon wires
// it inside SetAutopilotController.
func (c *Controller) SetRuntime(rt Runtime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtime = rt
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

	// Reconcile the run set against what is already live. A harmless re-enable
	// (same config) must not kill and respawn a healthy brain, so runs whose
	// run_id survives are carried over untouched; only genuinely new runs spawn a
	// brain, and runs no longer configured are torn down. In the inert core (no
	// runtime) "brain healthy" is immediate, so a run collapses straight to active
	// with no brain (S1 behavior).
	newRuns := map[string]*run{}
	for id, res := range resolvedByID {
		if existing, ok := c.runs[id]; ok {
			existing.planFile = res.file
			existing.absPlanFile = res.absFile
			newRuns[id] = existing
			continue
		}
		r := &run{
			runID:       id,
			planFile:    res.file,
			absPlanFile: res.absFile,
			repo:        res.repo,
			plan:        res.plan,
			state:       StateStarting,
		}
		if info, err := os.Stat(res.absFile); err == nil {
			r.planModTime = info.ModTime()
		}
		if err := c.spawnBrain(ctx, r, selectBrainBackend(c.backends)); err != nil {
			slog.Warn("autopilot: brain spawn failed; run degraded", "run", id, "err", err)
		}
		// Watch the plan file for owner steering only when a brain is actually
		// serving it (runtime wired); the inert core has no brain to notify.
		if c.runtime != nil {
			wctx, cancel := context.WithCancel(context.Background())
			r.cancel = cancel
			go c.watchPlan(wctx, r, planWatchInterval)
		}
		newRuns[id] = r
	}
	for id, old := range c.runs {
		if _, keep := newRuns[id]; keep {
			continue
		}
		c.stopRunLocked(ctx, old)
	}
	c.runs = newRuns
	c.enabled = true
	return c.statusLocked(), nil
}

// stopRunLocked tears one run down: cancel its plan watcher and gracefully
// terminate its brain (in-flight workers are untouched, §2.1). Caller holds c.mu.
func (c *Controller) stopRunLocked(ctx context.Context, r *run) {
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	if err := c.teardownBrain(ctx, r); err != nil {
		slog.Warn("autopilot: brain teardown error", "run", r.runID, "err", err)
	}
}

// Disable is the kill switch (§2.1): it flips the master flag off, stops each
// run's plan watcher, and terminates each brain gracefully. In-flight workers are
// deliberately left running — disable stops the orchestrator, not its work.
func (c *Controller) Disable(ctx context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.runs {
		c.stopRunLocked(ctx, r)
	}
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
		var brain *BrainStatus
		if r.brain != nil {
			// Tier is hardcoded "free" in S3 (matching the hardcoded first-free
			// selection); the full tier ladder + heartbeat/context land in S5.
			brain = &BrainStatus{
				AgentID: r.brain.AgentID,
				Backend: r.brain.Backend,
				Tier:    "free",
			}
		}
		st.Runs = append(st.Runs, RunStatus{
			RunID:    r.runID,
			PlanFile: r.planFile,
			Repo:     r.repo,
			State:    r.state,
			Gate:     c.gate, // S4 replaces this with the resolved mode
			Brain:    brain,
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
