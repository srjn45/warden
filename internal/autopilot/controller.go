package autopilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/fastbrain"
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
	// IntegrationBranch is the merge-target *template* from config
	// autopilot.merge.target_branch. New runs derive a per-plan branch unless
	// this is a custom global or a {{plan}} template; existing records keep
	// their stored IntegrationBranch (grandfather).
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
	// LiveStore is the live Autopilot entity store (plan-execution redesign).
	// When nil and DataDir is set, NewController opens <data>/autopilots-db via
	// autopilotstore. When provided explicitly (tests), it is used as-is.
	LiveStore *autopilotstore.Store
	// PlanSource supplies Plan task progress (Plan is the source of task state).
	PlanSource PlanTaskSource
	// Resolver is the unified router resolver for selecting backends.
	Resolver Resolver
	// FastBrain serves guardian stall triage (nil ⇒ heuristics only).
	FastBrain fastbrain.Engine
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
	// MaxIdenticalFailures is how many consecutive identical non-transient spawn
	// errors park the run as needs-attention (default 5).
	MaxIdenticalFailures int
	// WatchdogDisabled switches the progress watchdog off (default: on).
	WatchdogDisabled bool
	// WatchdogWindow is how long a run may go without progress, with no agent
	// working, before the watchdog escalates (default 2h).
	WatchdogWindow time.Duration
	// UseFastBrain enables Fast-Brain stall triage before an escalation (config
	// autopilot.guardian.use_fast_brain). The zero value is off, which keeps the
	// plain ladder; the daemon passes the config default (on).
	UseFastBrain bool
	// MaxWaits / MaxWaitTotal bound consecutive triage "wait" decisions per stall
	// episode (defaults 3 / 30m).
	MaxWaits     int
	MaxWaitTotal time.Duration
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
	fixPolicy         FixPolicy

	// Guardian triage seams (guardian_triage.go). fastBrain is nil ⇒ heuristics
	// only; triageFn overrides the diagnosis call (tests); stallResolver is the
	// call_resolver seam the resolver work fills (nil ⇒ that action falls open).
	fastBrain     fastbrain.Engine
	triageFn      func(ctx context.Context, in fastbrain.StallInput) fastbrain.StallDiagnosis
	stallResolver StallResolver

	// now is the clock the guardian + tierstate read (injectable for tests via
	// setClock). tierstate tracks per-backend rate-limit windows for selection.
	now       func() time.Time
	tierstate *tierState

	store      *RunStore
	storeErr   error // configured persistence unavailable: lifecycle writes must fail closed
	live       *autopilotstore.Store
	planSource PlanTaskSource

	mu      sync.Mutex
	runtime Runtime         // nil ⇒ inert (S1): no brain spawns
	runs    map[string]*run // keyed by run_id (across all enabled repos)
	claims  *claimRegistry  // slot scope + reserved manager/guardian id claims

	landLocks sync.Map // per-run / per-PR landing mutexes (landing.go)
}

// run is one registered plan execution: identity + state plus, once the brain
// lifecycle is live (S3), the brain handle, the last-good plan for owner steering
// (autopilot.md §3), and the plan-watch cancel.
type run struct {
	runID             string
	name              string
	planID            string
	projectID         string
	planFile          string // configured path (as written in config)
	absPlanFile       string // resolved absolute path (plan-file watch anchor)
	repo              string
	state             RunState
	plan              Plan
	planModTime       time.Time
	resolvedGate      string // gate mode resolved at preflight (§6.1): ci | local
	defaultBranch     string // repo default branch — the land guard's protected name
	integrationBranch string // per-run merge target, resolved once and persisted
	gateWarning       string // operator-visible auto→local CI coverage warning

	brain             *BrainHandle       // nil until the brain spawns; nil again after teardown
	slotScope         string             // stable scope for <scope>-autopilot / <scope>-guardian slot ids
	cancel            context.CancelFunc // stops the plan-watch goroutine (nil in inert mode)
	preflightWarnings []string           // content warnings from boot-recovery lenient load; cleared on clean preflight

	// Guardian-owned state (autopilot.md §2.3, §7). All mutated only under c.mu, by
	// the guardian tick or the (re)spawn helpers.
	tier             string      // selected cost tier (free|subscription|pay_per_use)
	brainSpawnedAt   time.Time   // last (re)spawn instant — the cold-start heartbeat floor
	lastHeartbeat    time.Time   // most recent brain heartbeat seen by the guardian
	contextLevel     string      // brain context-window level seen by the guardian
	healStage        healStage   // current position on the heal ladder
	healNextAt       time.Time   // earliest instant the next heal step may fire
	backoffStage     int         // capped-exponential backoff exponent (stage 4)
	backoffNextRetry time.Time   // when the current backoff wait elapses
	backoffLastErr   string      // human-facing reason for the current backoff
	backoffKind      FailureKind // classified cause of the current backoff
	failStreak       int         // consecutive identical non-transient spawn failures
	failStreakText   string      // error text the streak is counting
	needsAttention   string      // non-empty ⇒ parked: retries stopped, reason for the operator
	// Progress watchdog (watchdog.go): last observed progress, its fingerprint
	// (both persisted), and whether the heal ladder is being climbed by the
	// watchdog / the run was parked by it.
	lastProgressAt      time.Time
	progressFP          string
	wdActive            bool
	wdParked            bool
	parkedPlanKey       string          // plan revision:hash recorded when parked (plan-bound runs)
	plannedRotateNextAt time.Time       // cooldown floor so planned rotation can't thrash
	tried               map[string]bool // backends tried this heal cycle (rotate-down exclusion)

	// Guardian triage state (guardian_triage.go); mutated only under c.mu.
	triage triageState

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
		branch = DefaultIntegrationBranch
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
		fastBrain:         cfg.FastBrain,
		now:               now,
		tierstate:         newTierState(now),
		store:             cfg.RunStore,
		live:              cfg.LiveStore,
		planSource:        cfg.PlanSource,
		runs:              map[string]*run{},
		claims:            newClaimRegistry(),
	}
	if c.store == nil && strings.TrimSpace(cfg.DataDir) != "" {
		if st, err := NewRunStore(cfg.DataDir); err != nil {
			slog.Error("autopilot: persistent run store unavailable", "err", err)
			c.storeErr = fmt.Errorf("autopilot persistent run store unavailable: %w", err)
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
	if g.WatchdogWindow <= 0 {
		g.WatchdogWindow = DefaultWatchdogWindow
	}
	if g.MaxIdenticalFailures <= 0 {
		g.MaxIdenticalFailures = 5
	}
	if g.MaxWaits <= 0 {
		g.MaxWaits = 3
	}
	if g.MaxWaitTotal <= 0 {
		g.MaxWaitTotal = 30 * time.Minute
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

// SetFastBrain injects the Fast-Brain engine used for guardian stall triage.
func (c *Controller) SetFastBrain(e fastbrain.Engine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fastBrain = e
}

// SetStallResolver fills the call_resolver seam: the resolver agent work
// registers its starter here. Without one, a call_resolver diagnosis falls open
// to the mechanical ladder step.
func (c *Controller) SetStallResolver(sr StallResolver) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stallResolver = sr
}

// SetRuntime injects the daemon-provided brain/ledger/digest surface. It must be
// called before Enable to run real brains; without it the Controller stays inert
// (S1 behavior: the switch + preflight work but no brain spawns). The daemon wires
// it inside SetAutopilotController.
func (c *Controller) SetRuntime(rt Runtime) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runtime = rt
	c.reconcileRunsAtBootLocked(context.Background())
	// Durable run records, not the deprecated config plan list, are the V2
	// restart authority. Recreate every run whose persisted intent is live even
	// when it was registered directly and therefore is absent from c.plans.
	for _, r := range c.runs {
		switch r.state {
		case StateActive, StateStarting, StateHealing, StateDegraded:
		default:
			continue
		}
		if err := c.preflightRegisteredRunLocked(context.Background(), r); err != nil {
			var pfe *PreflightError
			if !errors.As(err, &pfe) || pfe.hasStructural() {
				// Structural failure: plan is unrunnable — stay degraded.
				// Start watchPlan anyway so when the operator fixes the file the
				// degraded-recovery tick can promote the run automatically.
				r.state = StateDegraded
				c.persistRunLocked(r)
				slog.Warn("autopilot: stored run recovery skipped", "run", r.runID, "err", err)
				if r.cancel == nil {
					wctx, cancel := context.WithCancel(context.Background())
					r.cancel = cancel
					go c.watchPlan(wctx, r, planWatchInterval)
				}
				continue
			}
			// Content-only failure: normalize the plan leniently and proceed.
			plan, warnings, lerr := loadPlanLenient(r.absPlanFile)
			if lerr != nil {
				r.state = StateDegraded
				c.persistRunLocked(r)
				slog.Warn("autopilot: stored run recovery skipped (lenient load failed)",
					"run", r.runID, "err", lerr)
				if r.cancel == nil {
					wctx, cancel := context.WithCancel(context.Background())
					r.cancel = cancel
					go c.watchPlan(wctx, r, planWatchInterval)
				}
				continue
			}
			r.plan = plan
			r.preflightWarnings = warnings
			slog.Warn("autopilot: boot recovery proceeding with content warnings",
				"run", r.runID, "warnings", warnings)
		} else {
			r.preflightWarnings = nil // clean preflight — clear any prior warnings
		}
		r.state = StateStarting
		sel := c.selectBrain(nil)
		r.tier = sel.Tier
		if err := c.spawnBrain(context.Background(), r, sel.Backend); err != nil {
			slog.Warn("autopilot: stored run recovery failed", "run", r.runID, "err", err)
		}
		if r.brain != nil && r.cancel == nil {
			wctx, cancel := context.WithCancel(context.Background())
			r.cancel = cancel
			go c.watchPlan(wctx, r, planWatchInterval)
		}
		c.persistRunLocked(r)
	}
}

// RunID is the stable identifier for a run: a short hash of the repo root and the
// absolute plan-file path (autopilot.md §1). Stable across daemon restarts so a
// re-enable re-adopts the same run rather than forking a duplicate.
func RunID(repo, planPath string) string {
	repo = canonicalPath(repo)
	planPath = canonicalPath(planPath)
	sum := sha256.Sum256([]byte(repo + "\x00" + planPath))
	return "ap-" + hex.EncodeToString(sum[:])[:12]
}

// PreflightError is the typed 409 result of a failed Enable: the full list of
// actionable failures (autopilot.md §5, §5.1), not just the first.
// kinds is the internal classification parallel to Failures; the wire format
// (Failures []string) is unchanged. See preflight_kind.go for the helpers.
type PreflightError struct {
	Failures []string
	kinds    []preflightFailureKind
}

func (e *PreflightError) Error() string {
	if len(e.Failures) == 0 {
		return "autopilot preflight failed"
	}
	return "autopilot preflight failed: " + strings.Join(e.Failures, "; ")
}

// Enable switches autopilot on for ONE repository. It is a capability
// configuration switch only: it persists the repo as enabled so Autopilot
// executors may run there, and does NOT register plan files, reconcile
// autopilot.plans[], or start work. Start execution via StartFromPlan /
// POST /plans/{plan_id}/run. Disabling remains the kill switch.
func (c *Controller) Enable(ctx context.Context, repo string) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return c.statusLocked(), c.storeErr
	}

	// Frictionless day-one (§10): when the owner has configured no auto-approve
	// rules, enabling the capability installs a generous default so workers don't
	// stall on recognized non-destructive prompts once a plan run starts.
	if c.runtime != nil {
		c.runtime.InstallDefaultAutoApprovePolicy()
	}
	return c.statusLocked(), nil
}

// ReconcileConfiguredPlans is the legacy plan-file registration path: preflight
// every autopilot.plans[] entry for repo, persist the capability switch, and
// register/start matching runs. Production lifecycle starts via StartFromPlan /
// POST /plans/{id}/run; Enable is switch-only. Kept for unit tests and any
// transitional callers that still drive the config plan list.
func (c *Controller) ReconcileConfiguredPlans(ctx context.Context, repo string) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return c.statusLocked(), c.storeErr
	}

	target := c.resolveRepo(ctx, repo)

	if len(c.plans) == 0 {
		return c.statusLocked(), &PreflightError{Failures: []string{
			"no plans configured — add at least one autopilot.plans[].file (run `warden autopilot init`)",
		}}
	}

	var fails []string
	resolvedByID := map[string]resolved{}
	matched := 0 // plans that resolve to the target repo

	pendingBranches := map[string]string{}
	for _, file := range c.plans {
		r, typedFails := c.preflightPlan(ctx, file, pendingBranches)
		if r.repo != "" && r.integrationBranch != "" {
			pendingBranches[r.repo+"\x00"+r.integrationBranch] = r.runID
		}
		if len(typedFails) == 0 && !r.skipComplete {
			for _, msg := range c.validatePersistedDoneClaims(r.runID, r.plan) {
				typedFails = append(typedFails, preflightFailure{msg: msg, kind: preflightKindContent})
			}
		}
		// Convert typed failures to message strings for the user-facing strict path.
		planFails := make([]string, len(typedFails))
		for i, f := range typedFails {
			planFails[i] = f.msg
		}
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
	batchNames := map[string]string{}
	for id, res := range resolvedByID {
		name := defaultRunName(res.absFile)
		if err := validatePlanNameReservedSuffixes(name); err != nil {
			fails = append(fails, fmt.Sprintf("plan %s: %v", res.file, err))
			continue
		}
		key := res.repo + "\x00" + name
		if other, ok := batchNames[key]; ok {
			fails = append(fails, fmt.Sprintf("plan %s: %v", res.file,
				fmt.Errorf("%w: run name %q already exists in %s (plan %s)", ErrRunConflict, name, res.repo, resolvedByID[other].file)))
			continue
		}
		batchNames[key] = id
		if err := c.validateRunNameLocked(res.repo, name, id); err != nil {
			fails = append(fails, fmt.Sprintf("plan %s: %v", res.file, err))
			continue
		}
		if _, err := c.allocateSlotScopeLocked(name, id); err != nil {
			fails = append(fails, fmt.Sprintf("plan %s: %v", res.file, err))
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
			existing.gateWarning = res.gateWarning
			if existing.integrationBranch == "" {
				existing.integrationBranch = res.integrationBranch
				c.persistRunLocked(existing)
			}
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
			runID:             id,
			name:              defaultRunName(res.absFile),
			planFile:          res.file,
			absPlanFile:       res.absFile,
			repo:              res.repo,
			plan:              res.plan,
			state:             StateStarting,
			resolvedGate:      res.resolvedGate,
			defaultBranch:     res.defaultBranch,
			integrationBranch: res.integrationBranch,
			gateWarning:       res.gateWarning,
			tried:             map[string]bool{},
		}
		scope, err := c.allocateSlotScopeLocked(r.name, id)
		if err != nil {
			slog.Warn("autopilot: slot scope allocation failed", "run", id, "err", err)
			continue
		}
		r.slotScope = scope
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
		if err := c.claims.claim(id, r.slotScope); err != nil {
			slog.Warn("autopilot: slot scope claim failed", "run", id, "err", err)
		}
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

// validatePersistedDoneClaims makes restart reconstruction apply the same
// daemon-authoritative evidence rule as live write-back. The nil-runtime path is
// retained for the deliberately inert controller used by early-stage callers.
func (c *Controller) validatePersistedDoneClaims(runID string, plan Plan) []string {
	if c.runtime == nil || c.runtime.NewLedger(runID) == nil {
		return nil
	}
	landings, err := c.runtime.NewLedger(runID).Landings()
	if err != nil {
		return []string{fmt.Sprintf("plan %s: verify landed task evidence: %v", runID, err)}
	}
	landed := make(map[int]bool, len(landings))
	for _, landing := range landings {
		landed[landing.PR] = true
	}
	var failures []string
	for _, task := range plan.Tasks {
		if task.Status == TaskStatusDone && !landed[task.LandedPR] {
			failures = append(failures, fmt.Sprintf("plan %s: task %q claims done with PR #%d, but that PR is not recorded as landed", runID, task.ID, task.LandedPR))
		}
	}
	return failures
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
		return filepath.Clean(root)
	}
	return filepath.Clean(target)
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

// Disable is DEPRECATED (there is no per-repo switch any more). It pauses every
// active run in repo (empty ⇒ the controller BaseDir), same effect as PauseRun on
// each, and returns the ids it paused. Nothing is torn down.
func (c *Controller) Disable(ctx context.Context, repo string) (Status, []string) {
	c.mu.Lock()
	target := c.resolveRepo(ctx, repo)
	var ids []string
	for id, r := range c.runs {
		if r.repo == target && (r.state == StateActive || r.state == StateHealing || r.state == StateDegraded) {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	sort.Strings(ids)
	paused := ids[:0]
	for _, id := range ids {
		if _, err := c.PauseRun(ctx, id); err != nil {
			slog.Warn("autopilot: deprecated disable could not pause run", "run", id, "err", err)
			continue
		}
		paused = append(paused, id)
	}
	return c.Status(), paused
}

// Reconfigure swaps the GLOBAL plan/brain/merge template live (config hot-reload,
// feature 3) WITHOUT touching the per-repo enable set — the EnableStore is the
// source of truth for WHICH repos are on; config only carries the template. It:
//
//	(a) replaces the template fields (plans, integration branch, gate, strategy,
//	    delete-branch, backend ladder, pay-per-use, guardian heal params), applying
//	    the same defaults NewController does;
//	(b) re-asserts the capability switch for every persisted-enabled repo (Enable
//	    no longer registers or starts work — live executors recover via SetRuntime
//	    / StartFromPlan);
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
	oldPlanSet := make(map[string]struct{}, len(c.plans))
	for _, p := range c.plans {
		oldPlanSet[p] = struct{}{}
	}
	branch := strings.TrimSpace(cfg.IntegrationBranch)
	if branch == "" {
		branch = DefaultIntegrationBranch
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
	// Repos that already host config-managed runs: re-resolve them under the NEW
	// template. (There is no per-repo enable set any more.)
	var repos []string
	seen := map[string]bool{}
	for _, r := range c.runs {
		if _, managed := oldPlanSet[r.planFile]; managed && !seen[r.repo] {
			seen[r.repo] = true
			repos = append(repos, r.repo)
		}
	}
	sort.Strings(repos)
	c.mu.Unlock()

	// (b) Reconcile configured plan-file runs under the NEW template so
	// autopilot.plans[] hot-reload keeps working for legacy config-driven runs.
	for _, repo := range repos {
		if _, err := c.ReconcileConfiguredPlans(ctx, repo); err != nil {
			slog.Debug("autopilot: reconfigure reconcile skipped", "repo", repo, "err", err)
		}
	}

	// (c) Tear down runs whose plan entry was removed from config. Removal is
	// decided by config presence so deleting an autopilot.plans[] entry stops
	// its legacy run without affecting Plan-bound StartFromPlan executors.
	c.mu.Lock()
	for id, r := range c.runs {
		if _, legacyManaged := oldPlanSet[r.planFile]; !legacyManaged {
			continue // durable register-created / plan-bound runs are independent of global config
		}
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
	// DB-canonical Plan-bound runs do not write completion into repository YAML;
	// PlanService / plan complete owns lifecycle. Skip file marker when there is
	// no export path or the run is Plan-bound.
	if r.absPlanFile != "" && r.planID == "" {
		if err := markPlanCompleteInPlace(r.absPlanFile, c.now().UTC().Format(time.RFC3339)); err != nil {
			return c.statusLocked(), fmt.Errorf("autopilot: mark run %s complete: %w", runID, err)
		}
	}
	// Reflect the marker in the in-memory plan so any later read of this run agrees.
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

// UpdateTaskStatus is the sole task-ledger write-back path. The controller lock
// serializes concurrent updates, while the file helper provides crash-safe
// replacement. A done claim must cite a PR recorded by the daemon land handler.
func (c *Controller) UpdateTaskStatus(runID, taskID, status string, landedPR int) (PlanTask, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return PlanTask{}, fmt.Errorf("autopilot: unknown run %q", runID)
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status != TaskStatusPending && status != TaskStatusActive && status != TaskStatusDone && status != TaskStatusFailed {
		return PlanTask{}, fmt.Errorf("autopilot: invalid task status %q", status)
	}
	idx := -1
	for i := range r.plan.Tasks {
		if r.plan.Tasks[i].ID == taskID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return PlanTask{}, fmt.Errorf("autopilot: unknown task %q", taskID)
	}
	if status == TaskStatusDone {
		if landedPR <= 0 {
			return PlanTask{}, fmt.Errorf("autopilot: status done requires landed_pr")
		}
		if c.runtime == nil || c.runtime.NewLedger(runID) == nil {
			return PlanTask{}, fmt.Errorf("autopilot: cannot verify landed_pr without ledger")
		}
		landings, err := c.runtime.NewLedger(runID).Landings()
		if err != nil {
			return PlanTask{}, fmt.Errorf("autopilot: verify landed_pr: %w", err)
		}
		verified := false
		for _, landing := range landings {
			if landing.PR == landedPR {
				verified = true
				break
			}
		}
		if !verified {
			return PlanTask{}, fmt.Errorf("autopilot: PR #%d is not recorded as landed for run %s", landedPR, runID)
		}
	} else if landedPR != 0 {
		return PlanTask{}, fmt.Errorf("autopilot: landed_pr is only valid with status done")
	}
	// Plan-bound / DB-canonical runs skip YAML write-back; progress lives on Plan.
	if r.absPlanFile != "" && r.planID == "" {
		if err := writeTaskStatusAtomic(r.absPlanFile, taskID, status, landedPR); err != nil {
			return PlanTask{}, err
		}
		if info, err := os.Stat(r.absPlanFile); err == nil {
			r.planModTime = info.ModTime()
		}
	}
	r.plan.Tasks[idx].Status, r.plan.Tasks[idx].LandedPR = status, landedPR
	return r.plan.Tasks[idx], nil
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
	return r.brain.AgentID, true
}

// CanBrainComplete authorizes the state-changing completion edge to the current
// brain. Once complete, retries are harmless no-ops and remain idempotent for a
// correctly tagged brain whose first response may have been lost.
func (c *Controller) CanBrainComplete(runID, brainID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return false
	}
	if r.state == StateComplete {
		return true
	}
	return r.brain != nil && r.brain.AgentID == brainID
}

// Status returns the current AutopilotStatus (§5).
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

// LookupRun returns one run's status by id, or ErrRunNotFound.
func (c *Controller) LookupRun(runID string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return RunStatus{}, ErrRunNotFound
	}
	return c.runStatusLocked(r), nil
}

// statusLocked builds the status snapshot; the caller must hold c.mu. Enabled is
// now "any repo enabled" and EnabledRepos names exactly which ones — the switch is
// per-repo, not a single global flag.
func (c *Controller) statusLocked() Status {
	// EnabledRepos is deprecated and always empty: there is no per-repo switch.
	st := Status{EnabledRepos: []string{}, Runs: []RunStatus{}}
	for _, r := range c.runs {
		counts := TaskCounts{}
		for _, task := range r.plan.Tasks {
			switch task.Status {
			case TaskStatusActive:
				counts.InProgress++
			case TaskStatusDone:
				counts.Landed++
			case TaskStatusFailed:
				counts.Failed++
			default:
				counts.Pending++
			}
		}
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
			RunID:             r.runID,
			Name:              r.name,
			PlanFile:          r.planFile,
			Repo:              r.repo,
			PlanID:            r.planID,
			ProjectID:         r.projectID,
			State:             r.state,
			Gate:              c.runGate(r), // the mode resolved at preflight (§6.1)
			Brain:             brain,
			WorkersInFlight:   r.workersInFlight, // last roster count from the overwatch tick
			Tasks:             counts,
			Backoff:           r.backoffStatus(),
			NeedsAttention:    r.needsAttention,
			LastProgressAt:    rfc3339OrEmpty(r.lastProgressAt),
			Watchdog:          c.watchdogState(r, c.now()),
			PlanTasks:         append([]PlanTask(nil), r.plan.Tasks...),
			GuardianID:        guardianSlotIDOrEmpty(r.slotScope),
			SlotScope:         r.slotScope,
			IntegrationBranch: r.integrationBranch,
			GateWarning:       r.gateWarning,
			ManagerSlotID:     managerSlotIDOrEmpty(r.slotScope),
			GuardianSlotID:    guardianSlotIDOrEmpty(r.slotScope),
			LedgerTasks:       c.ledgerTasksLocked(r.runID),
			PreflightWarnings: append([]string(nil), r.preflightWarnings...),
		})
	}
	sort.Slice(st.Runs, func(i, j int) bool { return st.Runs[i].RunID < st.Runs[j].RunID })
	for _, rs := range st.Runs {
		if rs.State == StateActive || rs.State == StateStarting || rs.State == StateHealing {
			st.Enabled = true
			break
		}
	}
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
		IntegrationBranch: r.integrationBranch,
		DefaultBranch:     r.defaultBranch,
		Gate:              c.runGate(r),
		Strategy:          c.strategy,
		DeleteBranch:      c.deleteBranch,
		Active:            isLandableState(r.state),
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
