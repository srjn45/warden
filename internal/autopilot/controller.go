package autopilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/router"
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
	// S4 resolves `auto` at preflight and reports the resolved mode in status.
	Gate string
	// Strategy is the merge strategy for `land` (config autopilot.merge.strategy):
	// squash|merge|rebase. Empty defaults to squash.
	Strategy string
	// DeleteBranch deletes the worker branch after a land (config
	// autopilot.merge.delete_branch).
	DeleteBranch bool
	// BaseDir anchors relative plan paths (the daemon's working directory). It is
	// also the repo an empty Enable/Disable argument defaults to (backward compat).
	BaseDir string
	// DataDir is the daemon data directory the per-repo EnableStore persists under
	// (<data_dir>/autopilot/enabled/). Empty ⇒ an in-memory store (nothing
	// persisted), so a Controller built without a data dir (unit tests) still works.
	DataDir string
	// RunStore overrides the durable run registry (primarily for tests). When nil
	// and DataDir is set, NewController opens <data>/autopilot/runs-db.
	RunStore *RunStore
	// Resolver is the unified router resolver for selecting backends.
	Resolver Resolver
	// Guardian configures the heartbeat guardian's heal ladder + backoff (config
	// autopilot.guardian). Zero-valued fields fall back to sane defaults.
	Guardian GuardianParams
}

// Resolver is the interface for selecting backends and models via the unified router.
type Resolver interface {
	Resolve(ctx context.Context, opts router.ResolveOptions) (*router.Resolution, error)
}

// GuardianParams is the resolved guardian configuration the Controller drives
// (autopilot.md §2.3). Durations are pre-parsed by the daemon from the config
// block's Go-duration strings; NewController defaults any zero value.
type GuardianParams struct {
	Interval         time.Duration // tick cadence
	HeartbeatTimeout time.Duration // brain-idle threshold that declares a wedge
	BackoffMin       time.Duration // first capped-exponential backoff step
	BackoffMax       time.Duration // backoff ceiling (never parks past it)
	RotateAtContext  string        // context level that triggers a planned rotation
	NotifyEach       bool          // notify the owner on every heal step, not just stalls
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
	strategy          string
	deleteBranch      bool
	baseDir           string
	resolver          Resolver
	guardian          GuardianParams

	// now is the clock the guardian + tierstate read (injectable for tests via
	// setClock). tierstate tracks per-backend rate-limit windows for selection.
	now       func() time.Time
	tierstate *tierState

	// enableStore is the persisted per-repo on/off set (autopilot's switch is
	// per-repo, not a single global flag). Read for status/kill-switch checks and
	// written by Enable/Disable. Concurrency-safe on its own; mutated under c.mu so
	// it stays consistent with c.runs.
	enableStore EnableStore
	store       *RunStore

	mu      sync.Mutex
	runtime Runtime         // nil ⇒ inert (S1): no brain spawns
	runs    map[string]*run // keyed by run_id (across all enabled repos)
}

// run is one registered plan execution: identity + state plus, once the brain
// lifecycle is live (S3), the brain handle, the last-good plan for owner steering
// (autopilot.md §3), and the plan-watch cancel.
type run struct {
	runID         string
	name          string
	planFile      string // configured path (as written in config)
	absPlanFile   string // resolved absolute path (plan-file watch anchor)
	repo          string
	state         RunState
	plan          Plan
	planModTime   time.Time
	resolvedGate  string // gate mode resolved at preflight (§6.1): ci | local
	defaultBranch string // repo default branch — the land guard's protected name

	brain  *BrainHandle       // nil until the brain spawns; nil again after teardown
	cancel context.CancelFunc // stops the plan-watch goroutine (nil in inert mode)

	// Guardian-owned state (autopilot.md §2.3, §7). All mutated only under c.mu, by
	// the guardian tick or the (re)spawn helpers.
	tier                string          // selected cost tier (free|subscription|pay_per_use)
	brainSpawnedAt      time.Time       // last (re)spawn instant — the cold-start heartbeat floor
	lastHeartbeat       time.Time       // most recent brain heartbeat seen by the guardian
	contextLevel        string          // brain context-window level seen by the guardian
	healStage           healStage       // current position on the heal ladder
	healNextAt          time.Time       // earliest instant the next heal step may fire
	backoffStage        int             // capped-exponential backoff exponent (stage 4)
	backoffNextRetry    time.Time       // when the current backoff wait elapses
	backoffLastErr      string          // human-facing reason for the current backoff
	plannedRotateNextAt time.Time       // cooldown floor so planned rotation can't thrash
	tried               map[string]bool // backends tried this heal cycle (rotate-down exclusion)

	// Overwatch-owned state (autopilot.md §2.4). Mutated only under c.mu by the
	// overwatch tick, which nudges a live-but-quiet manager to tend workers that
	// have fallen idle or are waiting on input.
	overwatchLastNudgeAt time.Time // last overwatch nudge instant (periodic + event-debounce clock)
	workersInFlight      int       // busy (spawning/working) non-manager agents, refreshed each overwatch tick
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
	strategy := strings.TrimSpace(cfg.Strategy)
	if strategy == "" {
		strategy = "squash"
	}
	now := time.Now
	c := &Controller{
		env:               env,
		plans:             cfg.Plans,
		integrationBranch: branch,
		gate:              gate,
		strategy:          strategy,
		deleteBranch:      cfg.DeleteBranch,
		baseDir:           cfg.BaseDir,
		resolver:          cfg.Resolver,
		guardian:          withGuardianDefaults(cfg.Guardian),
		now:               now,
		tierstate:         newTierState(now),
		enableStore:       newEnableStore(cfg.DataDir),
		store:             cfg.RunStore,
		runs:              map[string]*run{},
	}
	if c.store == nil && strings.TrimSpace(cfg.DataDir) != "" {
		if st, err := NewRunStore(cfg.DataDir); err != nil {
			slog.Error("autopilot: persistent run store unavailable", "err", err)
		} else {
			c.store = st
		}
	}
	c.restoreStoredRuns()
	return c
}

// withGuardianDefaults fills any zero-valued guardian field with a generous
// default (frictionless-safeguards philosophy — the guardian fires only at
// genuine wedges, never paces normal work). Mirrors the config block defaults so
// a Controller built without a config-sourced GuardianParams still behaves.
func withGuardianDefaults(g GuardianParams) GuardianParams {
	if g.Interval <= 0 {
		g.Interval = 60 * time.Second
	}
	if g.HeartbeatTimeout <= 0 {
		g.HeartbeatTimeout = 10 * time.Minute
	}
	if g.BackoffMin <= 0 {
		g.BackoffMin = 30 * time.Second
	}
	if g.BackoffMax <= 0 {
		g.BackoffMax = 6 * time.Hour
	}
	if g.BackoffMax < g.BackoffMin {
		g.BackoffMax = g.BackoffMin
	}
	if strings.TrimSpace(g.RotateAtContext) == "" {
		g.RotateAtContext = "critical"
	}
	return g
}

// setClock swaps the guardian/tierstate clock. Test-only seam (in-package): the
// daemon never calls it, so the guardian always reads the wall clock in
// production.
func (c *Controller) setClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	c.now = now
	c.tierstate.now = now
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
	if root, err := filepath.Abs(repo); err == nil {
		repo = filepath.Clean(root)
	}
	if path, err := filepath.Abs(planPath); err == nil {
		planPath = filepath.Clean(path)
	}
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

// Enable switches autopilot on for ONE repository (autopilot's on/off bit is
// per-repo; the plan/brain/merge template stays global in config). It resolves
// repo to a git root (empty ⇒ the controller BaseDir, for backward compat), runs
// the enable-time preflight (§5.1) over only the plans that resolve to that repo,
// and — only if ALL of that repo's checks pass — persists the repo as enabled and
// registers/reconciles ONLY its runs. Runs belonging to OTHER enabled repos are
// left untouched. On any failure it changes no state and returns a *PreflightError
// carrying every failure. Enable is atomic and idempotent per repo: re-enabling an
// already-enabled repo with the same config is a no-op yielding the same run ids.
func (c *Controller) Enable(ctx context.Context, repo string) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	target := c.resolveRepo(ctx, repo)

	if len(c.plans) == 0 {
		return c.statusLocked(), &PreflightError{Failures: []string{
			"no plans configured — add at least one autopilot.plans[].file (run `warden autopilot init`)",
		}}
	}

	var fails []string
	resolvedByID := map[string]resolved{}
	matched := 0 // plans that resolve to the target repo

	for _, file := range c.plans {
		r, planFails := c.preflightPlan(ctx, file)
		switch {
		case r.repo == "":
			// Repo unresolved (not a git repo, etc.): the plan is fundamentally
			// broken and can't be attributed to a repo, so surface its failures
			// regardless of which repo is being enabled — it's a config error.
			fails = append(fails, planFails...)
		case r.repo != target:
			continue // belongs to another repo — untouched by this per-repo enable
		case r.skipComplete:
			// A finished run's plan carries the in-place completion marker (§2.1): it
			// resolves to THIS repo (so enabling is legitimate — count it as matched)
			// but is neither a preflight failure nor a fresh run. Surface the skip so
			// the owner sees the run completed rather than silently vanishing, then
			// leave it out of the active run set (it never claims its repo, so a new
			// plan may take over the same repo).
			matched++
			fails = append(fails, planFails...)
			slog.Info("autopilot: plan already complete — skipping (run finished)",
				"plan", file, "run", r.runID, "completed_at", r.plan.CompletedAt)
			continue
		default:
			matched++
			fails = append(fails, planFails...)
			resolvedByID[r.runID] = r
		}
	}

	if len(fails) > 0 {
		sort.Strings(fails)
		return c.statusLocked(), &PreflightError{Failures: dedupe(fails)}
	}
	if matched == 0 {
		// No plan targets this repo and nothing else was wrong — a clean, actionable
		// per-repo signal rather than a silent no-op.
		return c.statusLocked(), &PreflightError{Failures: []string{fmt.Sprintf(
			"no autopilot plan resolves to %s — add an autopilot.plans[].file inside it (run `warden autopilot init`), or pass --repo",
			target)}}
	}

	// Preflight passed for this repo: persist the switch. Done only now so Enable
	// stays atomic (no state change on failure).
	if err := c.enableStore.Enable(target); err != nil {
		return c.statusLocked(), fmt.Errorf("persist autopilot enable for %s: %w", target, err)
	}

	// Reconcile ONLY this repo's runs against what preflight resolved. Runs for
	// other repos are carried over verbatim. A harmless re-enable (same config)
	// must not kill and respawn a healthy brain, so surviving runs are carried
	// untouched; only genuinely new runs spawn a brain, and runs of THIS repo no
	// longer configured are torn down. In the inert core (no runtime) "brain
	// healthy" is immediate, so a run collapses straight to active with no brain.
	newRuns := map[string]*run{}
	for id, r := range c.runs {
		if r.repo != target || r.state == StateComplete || r.state == StateStopped {
			newRuns[id] = r // another enabled repo — leave it exactly as it is
		}
	}
	for id, res := range resolvedByID {
		if existing, ok := c.runs[id]; ok {
			existing.planFile = res.file
			existing.absPlanFile = res.absFile
			existing.resolvedGate = res.resolvedGate
			existing.defaultBranch = res.defaultBranch
			existing.plan = res.plan
			if existing.brain == nil && (existing.state == StateActive || existing.state == StateStarting || existing.state == StateHealing || existing.state == StateDegraded) {
				existing.state = StateStarting
				c.persistRunLocked(existing)
				sel := c.selectBrain(nil)
				existing.tier = sel.Tier
				if err := c.spawnBrain(ctx, existing, sel.Backend); err != nil {
					slog.Warn("autopilot: boot run reconciliation failed", "run", id, "err", err)
				}
				c.persistRunLocked(existing)
			}
			newRuns[id] = existing
			continue
		}
		r := &run{
			runID:         id,
			name:          defaultRunName(res.absFile),
			planFile:      res.file,
			absPlanFile:   res.absFile,
			repo:          res.repo,
			plan:          res.plan,
			state:         StateStarting,
			resolvedGate:  res.resolvedGate,
			defaultBranch: res.defaultBranch,
			tried:         map[string]bool{},
		}
		if info, err := os.Stat(res.absFile); err == nil {
			r.planModTime = info.ModTime()
		}
		sel := c.selectBrain(nil)
		r.tier = sel.Tier
		if err := c.spawnBrain(ctx, r, sel.Backend); err != nil {
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
		c.persistRunLocked(r)
	}
	for id, old := range c.runs {
		if old.repo != target {
			continue // another repo — already carried over above
		}
		if _, keep := newRuns[id]; keep {
			continue
		}
		c.stopRunLocked(ctx, old)
	}
	c.runs = newRuns
	// Frictionless day-one (§10): when the owner has configured no auto-approve
	// rules, installing autopilot installs a generous default so workers don't
	// stall on recognized non-destructive prompts. Idempotent and owner-respecting
	// (a no-op once any rules exist); the seam owns the "has no rules" check.
	if c.runtime != nil {
		c.runtime.InstallDefaultAutoApprovePolicy()
	}
	return c.statusLocked(), nil
}

// resolveRepo canonicalizes an Enable/Disable repo argument to the git toplevel
// used as a run's repo key, so a per-repo toggle matches the repo preflight
// resolves from a plan file. An empty arg defaults to the controller BaseDir (the
// daemon's cwd) for backward compatibility. A path that can't be resolved to a git
// root is returned cleaned, so a non-repo arg still yields a stable key (Enable
// then finds no matching plan and reports it).
func (c *Controller) resolveRepo(ctx context.Context, repo string) string {
	target := strings.TrimSpace(repo)
	if target == "" {
		target = c.baseDir
	}
	if root, err := c.env.GitToplevel(ctx, target); err == nil && strings.TrimSpace(root) != "" {
		if real, err := filepath.EvalSymlinks(root); err == nil {
			return real
		}
		return root
	}
	return filepath.Clean(target)
}

// PersistedEnabled returns every repo the EnableStore has recorded as switched on.
// The daemon calls Enable(ctx, repo) for each on boot so previously-enabled repos
// come back up across a restart.
func (c *Controller) PersistedEnabled() []string {
	return c.enableStore.List()
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

// Disable is the per-repo kill switch (§2.1): it clears repo's persisted switch
// (empty ⇒ the controller BaseDir), stops that repo's plan watchers, and
// terminates its brains gracefully. Runs belonging to OTHER enabled repos are left
// untouched. In-flight workers are deliberately left running — disable stops the
// orchestrator, not its work.
func (c *Controller) Disable(ctx context.Context, repo string) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	target := c.resolveRepo(ctx, repo)
	if err := c.enableStore.Disable(target); err != nil {
		slog.Warn("autopilot: persist disable failed", "repo", target, "err", err)
	}
	remaining := map[string]*run{}
	for id, r := range c.runs {
		if r.repo == target {
			c.stopRunLocked(ctx, r)
			r.state = StateStopped
			c.persistRunLocked(r)
			remaining[id] = r
			continue
		}
		remaining[id] = r
	}
	c.runs = remaining
	return c.statusLocked()
}

// Reconfigure swaps the GLOBAL plan/brain/merge template live (config hot-reload,
// feature 3) WITHOUT touching the per-repo enable set — the EnableStore is the
// source of truth for WHICH repos are on; config only carries the template. It:
//
//	(a) replaces the template fields (plans, integration branch, gate, strategy,
//	    delete-branch, backend ladder, pay-per-use, guardian heal params), applying
//	    the same defaults NewController does;
//	(b) re-runs the per-repo Enable reconcile over every persisted-enabled repo, so
//	    an added plan spawns, a repo's changed template re-applies, and a preflight
//	    that now fails is logged (the repo stays enabled — a later good edit or
//	    `warden autopilot on` recovers it), exactly like the daemon's boot re-enable;
//	(c) tears down any run whose plan-file entry was REMOVED from config, so deleting
//	    an autopilot.plans[] entry stops its run. Removal is decided by config
//	    presence, not preflight, so a transient preflight failure never kills a run
//	    whose plan is still configured.
//
// The EnableStore and DataDir are NOT reset here: changing data_dir requires a
// restart (a different store) and the daemon logs that. The guardian TICK CADENCE
// (Guardian.Interval) is read once when the guardian loop starts, so a changed
// interval needs a restart; the heal thresholds (heartbeat timeout, backoff,
// rotate level, notify-each) hot-apply on the guardian's next tick since it reads
// them under c.mu.
func (c *Controller) Reconfigure(ctx context.Context, cfg ControllerConfig) {
	c.mu.Lock()
	branch := strings.TrimSpace(cfg.IntegrationBranch)
	if branch == "" {
		branch = "autopilot/integration"
	}
	gate := strings.TrimSpace(cfg.Gate)
	if gate == "" {
		gate = "auto"
	}
	strategy := strings.TrimSpace(cfg.Strategy)
	if strategy == "" {
		strategy = "squash"
	}
	c.plans = cfg.Plans
	c.integrationBranch = branch
	c.gate = gate
	c.strategy = strategy
	c.deleteBranch = cfg.DeleteBranch
	c.resolver = cfg.Resolver
	c.guardian = withGuardianDefaults(cfg.Guardian)
	// BaseDir is the daemon cwd (stable for a daemon's life); guard against an
	// empty override clobbering the anchor for relative plan paths.
	if bd := strings.TrimSpace(cfg.BaseDir); bd != "" {
		c.baseDir = bd
	}
	planSet := make(map[string]struct{}, len(cfg.Plans))
	for _, p := range cfg.Plans {
		planSet[p] = struct{}{}
	}
	enabled := c.enableStore.List()
	c.mu.Unlock()

	// (b) Re-run the per-repo reconcile under the NEW template. Enable takes c.mu
	// itself, so this runs outside the lock. Best-effort per repo (mirrors boot).
	for _, repo := range enabled {
		if _, err := c.Enable(ctx, repo); err != nil {
			slog.Warn("autopilot: reconfigure re-enable skipped", "repo", repo, "err", err)
		}
	}

	// (c) Tear down runs whose plan entry was removed from config. A repo whose
	// plans all vanished fails Enable's matched-count check above (its run is left
	// intact there), so this deterministic, config-presence-based sweep is what
	// actually stops it — without ever killing a run whose plan is still listed.
	c.mu.Lock()
	for id, r := range c.runs {
		if _, still := planSet[r.planFile]; still {
			continue
		}
		slog.Info("autopilot: plan removed from config — stopping run", "run", id, "plan", r.planFile)
		c.stopRunLocked(ctx, r)
		r.state = StateStopped
		c.persistRunLocked(r)
	}
	c.mu.Unlock()
}

// CompleteRun records that run runID has finished (autopilot.md §2.1, the
// `active --all tasks landed--> complete` transition). The brain calls it after
// it has verified the plan's done_when criteria (persona rule §9.6). It, in order:
//
//	(a) writes the in-place completion marker (`status: complete` + `completed_at`)
//	    into the plan file, so preflight SKIPS the plan on every future enable and
//	    the run can never be executed again by mistake;
//	(b) advances the run to StateComplete;
//	(c) tears the brain down gracefully (kill-switch semantics: in-flight workers
//	    keep running) while RETAINING the ledger — the run's durable record lives in
//	    the ctx store and is deliberately untouched.
//
// The marker is written FIRST: if that fails the run stays active and the brain
// can retry, rather than leaving a torn-down run whose plan would re-execute on
// the next enable. Idempotent: a second call on an already-complete run is a
// no-op success, so a brain re-issuing after a restart completes nothing twice.
func (c *Controller) CompleteRun(ctx context.Context, runID string) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return c.statusLocked(), fmt.Errorf("autopilot: unknown run %q", runID)
	}
	if r.state == StateComplete {
		return c.statusLocked(), nil // already complete — idempotent no-op
	}
	if err := markPlanCompleteInPlace(r.absPlanFile, c.now().UTC().Format(time.RFC3339)); err != nil {
		return c.statusLocked(), fmt.Errorf("autopilot: mark run %s complete: %w", runID, err)
	}
	// Reflect the marker in the in-memory plan so any later read of this run agrees
	// with the file (a re-enable would re-skip it regardless).
	r.plan.Status = PlanStatusComplete
	// Graceful teardown: stop the plan watcher and terminate the brain; the ledger
	// (ctx store) is untouched. The run stays registered as StateComplete so status
	// still reports it until the next enable reconciles it away (preflight now skips
	// the marked plan). isLandableState(StateComplete) is false, so a late land
	// attempt on this run now returns run_disabled.
	c.stopRunLocked(ctx, r)
	r.state = StateComplete
	c.persistRunLocked(r)
	return c.statusLocked(), nil
}

// ActiveBrainForRun returns the agent id of the brain serving run runID while the
// switch is on and that run has a live brain; ok=false otherwise (unknown run,
// autopilot disabled, or a degraded run with no brain). The approval router uses
// it to forward a worker's unanswerable prompt to the right brain — and only
// while the run is genuinely active, so a torn-down run never captures a worker's
// escalation (autopilot.md §8).
func (c *Controller) ActiveBrainForRun(runID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok || r.brain == nil || r.brain.AgentID == "" {
		return "", false
	}
	if !c.enableStore.IsEnabled(r.repo) {
		return "", false // the run's repo has been switched off
	}
	return r.brain.AgentID, true
}

// Status returns the current AutopilotStatus (§5).
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

// statusLocked builds the status snapshot; the caller must hold c.mu. Enabled is
// now "any repo enabled" and EnabledRepos names exactly which ones — the switch is
// per-repo, not a single global flag.
func (c *Controller) statusLocked() Status {
	enabledRepos := c.enableStore.List()
	if enabledRepos == nil {
		enabledRepos = []string{}
	}
	st := Status{Enabled: len(enabledRepos) > 0, EnabledRepos: enabledRepos, Runs: []RunStatus{}}
	for _, r := range c.runs {
		var brain *BrainStatus
		if r.brain != nil {
			brain = &BrainStatus{
				AgentID:       r.brain.AgentID,
				Backend:       r.brain.Backend,
				Tier:          tierOrDefault(r.tier),
				LastHeartbeat: rfc3339OrEmpty(r.lastHeartbeat),
				ContextLevel:  r.contextLevel,
			}
		}
		st.Runs = append(st.Runs, RunStatus{
			RunID:           r.runID,
			Name:            r.name,
			PlanFile:        r.planFile,
			Repo:            r.repo,
			State:           r.state,
			Gate:            c.runGate(r), // the mode resolved at preflight (§6.1)
			Brain:           brain,
			WorkersInFlight: r.workersInFlight, // last roster count from the overwatch tick
			Tasks:           TaskCounts{},
			Backoff:         r.backoffStatus(),
		})
	}
	sort.Slice(st.Runs, func(i, j int) bool { return st.Runs[i].RunID < st.Runs[j].RunID })
	return st
}

// runGate returns the gate mode reported for a run: the value resolved at
// preflight (§6.1), falling back to the configured mode for a run registered
// before resolution ran (defensive — preflight always sets it).
func (c *Controller) runGate(r *run) string {
	if r.resolvedGate != "" {
		return r.resolvedGate
	}
	return c.gate
}

// LandParams is the per-run merge context the daemon `land` handler needs
// (autopilot.md §6): whether the run is active (kill switch) plus the resolved
// merge target/gate/strategy. found=false when runID is not a registered run.
type LandParams struct {
	Repo              string
	IntegrationBranch string
	DefaultBranch     string
	Gate              string // resolved gate mode: ci | local
	Strategy          string
	DeleteBranch      bool
	Active            bool // the run is enabled and in a landable state
}

// LandParams returns the merge parameters for runID and whether it is a known
// run. It is the daemon land handler's single read of the run registry, so the
// handler never reaches into Controller internals. Active honors the kill switch:
// a disabled/complete run is not landable (precondition 1, §6).
func (c *Controller) LandParams(runID string) (LandParams, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return LandParams{}, false
	}
	return LandParams{
		Repo:              r.repo,
		IntegrationBranch: c.integrationBranch,
		DefaultBranch:     r.defaultBranch,
		Gate:              c.runGate(r),
		Strategy:          c.strategy,
		DeleteBranch:      c.deleteBranch,
		Active:            c.enableStore.IsEnabled(r.repo) && isLandableState(r.state),
	}, true
}

// isLandableState reports whether a run's state permits landing: the brain is
// (or is being) kept alive. A complete or disabled run lands nothing.
func isLandableState(s RunState) bool {
	switch s {
	case StateActive, StateHealing, StateDegraded, StateStarting, StatePaused:
		return true
	default:
		return false
	}
}

// selectBrain calls the unified Resolver to pick the backend for this run's next
// brain (autopilot.md §7). The exclude map lets the guardian rotate DOWN — any
// backend the heal cycle already tried is skipped by returning no selection so the
// caller advances to backoff, rather than re-picking a backend that just failed.
// If the resolver returns a backend that is in exclude, we treat it as exhausted
// (no selection). A nil resolver falls back to the daemon default (""): this
// preserves the inert-core / unit-test behaviour where no resolver is injected.
func (c *Controller) selectBrain(exclude map[string]bool) selection {
	if c.resolver == nil {
		if exclude[""] {
			return selection{}
		}
		return selection{Backend: "", Tier: tierFree, OK: true}
	}
	res, err := c.resolver.Resolve(context.Background(), router.ResolveOptions{Role: "autopilot"})
	if err != nil {
		return selection{}
	}
	if exclude[res.BackendID] {
		return selection{}
	}
	return selection{Backend: res.BackendID, Tier: string(res.Tier), OK: true}
}

// SelectWorkerBackend resolves the backend a worker for runID should launch on,
// walking the same cost-tier ladder as the brain (autopilot.md §7). ok=false when
// the run is unknown, autopilot is off, or nothing is currently selectable (the
// whole ladder is limited/gated). The daemon uses it to fill an autopilot worker's
// backend when the brain leaves it to warden.
func (c *Controller) SelectWorkerBackend(runID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return "", false
	}
	if r.state == StatePaused || r.state == StateStopped || r.state == StateComplete {
		return "", false
	}
	if !c.enableStore.IsEnabled(r.repo) {
		return "", false
	}
	if c.resolver == nil {
		return "", false
	}
	res, err := c.resolver.Resolve(context.Background(), router.ResolveOptions{Role: "worker"})
	if err != nil {
		return "", false
	}
	return res.BackendID, true
}

// MarkBackendLimited records that backend is rate-limited until `until` so the
// selection loop skips it (autopilot.md §7). Fed by the daemon from the poller's
// rate-limit detection (the parsed reset time, else the configured retry/spend
// fallback). A backend not in this run set's ladder is still recorded — harmless,
// and it re-qualifies on expiry.
func (c *Controller) MarkBackendLimited(backend string, until time.Time) {
	c.tierstate.markLimited(backend, until)
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
