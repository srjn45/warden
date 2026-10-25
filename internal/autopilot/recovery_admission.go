package autopilot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/planstore"
)

const recoveryDeadline = 30 * time.Second

// RecoveryStats includes attempts whose dependencies ignored cancellation: they
// retain capacity until external work and ownership-safe cleanup have returned.
type RecoveryStats struct {
	Attempts             uint64 `json:"attempts"`
	InFlight             int    `json:"in_flight"`
	Timeouts             uint64 `json:"timeouts"`
	Refusals             uint64 `json:"refusals"`
	CapacityRefusals     uint64 `json:"capacity_refusals"`
	Backoffs             uint64 `json:"backoffs"`
	VerificationFailures uint64 `json:"verification_failures"`
	Adopted              uint64 `json:"adopted"`
	CleanupFailures      uint64 `json:"cleanup_failures"`
	JournalRepairs       uint64 `json:"journal_repairs"`
	JournalRepairNeeded  uint64 `json:"journal_repair_needed"`
	PersistenceFailures  uint64 `json:"persistence_failures"`
	Capacity             int    `json:"capacity"`
}

func (c *Controller) invalidateRecoveryLocked(r *run) {
	r.recoveryGeneration++
	if r.recoveryCancel != nil {
		r.recoveryCancel()
	}
}

// admitRecoveryLocked is shared by boot, guardian and the plan watcher. It
// reserves a run under c.mu, but all potentially stalled work happens after the
// lock is released by launchRecovery.
func (c *Controller) admitRecoveryLocked(r *run) bool {
	if r == nil || c.runtime == nil || r.recoveryInFlight || c.recoveryOwners[r.runID] != nil {
		c.recoveryStats.Refusals++
		return false
	}
	switch r.state {
	case StatePaused, StateStopped, StateComplete, StateDisabled, StateRegistered:
		return false
	}
	if c.recoveryDeferred {
		r.recoveryInFlight = true
		r.recoveryRotate = false
		r.recoveryBackend, r.recoveryReason = "", ""
		return true
	}
	// Reserve bounded global capacity before starting a goroutine.  A full
	// semaphore is an admission refusal, not an unbounded waiter queue.
	select {
	case c.recoverySem <- struct{}{}:
	default:
		c.recoveryStats.CapacityRefusals++
		return false
	}
	c.recoveryStats.Attempts++
	c.recoveryStats.InFlight++
	r.recoveryAdmittedGeneration = r.recoveryGeneration
	timeout := c.recoveryTimeout
	if timeout <= 0 {
		timeout = recoveryDeadline
	}
	r.recoveryCtx, r.recoveryCancel = context.WithTimeout(context.Background(), timeout)
	r.recoveryAttempt++
	attempt, attemptCtx := r.recoveryAttempt, r.recoveryCtx
	context.AfterFunc(attemptCtx, func() {
		if attemptCtx.Err() == context.DeadlineExceeded {
			c.mu.Lock()
			c.recoveryStats.Timeouts++
			if r.recoveryAttempt == attempt {
				r.recoveryDiagnostic = "timed_out_waiting_for_dependency"
			}
			c.mu.Unlock()
		}
	})
	r.recoveryInFlight = true
	if c.recoveryOwners == nil {
		c.recoveryOwners = make(map[string]*run)
	}
	c.recoveryOwners[r.runID] = r
	r.recoveryRotate = false
	r.recoveryBackend, r.recoveryReason = "", ""
	return true
}

func (c *Controller) admitRotationLocked(r *run, backend, reason string) bool {
	if r == nil || r.brain == nil || r.brain.AgentID == "" || !c.admitRecoveryLocked(r) {
		return false
	}
	r.recoveryRotate = true
	r.recoveryBackend, r.recoveryReason = backend, reason
	return true
}

func (c *Controller) launchRecovery(runID string) {
	if c.recoveryDeferred {
		return
	}
	go func() {
		c.mu.Lock()
		r := c.recoveryOwners[runID]
		if r == nil {
			c.mu.Unlock()
			return
		}
		ctx, cancel, attempt := r.recoveryCtx, r.recoveryCancel, r.recoveryAttempt
		c.mu.Unlock()
		defer func() { cancel(); c.releaseRecovery(runID, attempt) }()
		c.recoverAdmitted(ctx, runID)
	}()
}

func (c *Controller) releaseRecovery(id string, attempt uint64) {
	c.mu.Lock()
	if r := c.recoveryOwners[id]; r != nil && r.recoveryAttempt == attempt {
		if r.recoveryCancel != nil {
			r.recoveryCancel()
		}
		r.recoveryInFlight, r.recoveryCancel = false, nil
		delete(c.recoveryOwners, id)
	} else {
		c.mu.Unlock()
		return
	}
	c.recoveryStats.InFlight--
	<-c.recoverySem
	c.mu.Unlock()
}

type recoverySnapshot struct {
	runID, planID, projectID, repo, planFile, slotScope, integrationBranch string
	generation, attempt                                                    uint64
	backend, tier                                                          string
	rotate                                                                 bool
	rotateAgentID, rotateReason                                            string
	runtime                                                                Runtime
	planSource                                                             PlanTaskSource
	env                                                                    Env
	defaultBranch                                                          string
}

// validateRecoveryIdentity makes recovery refuse to turn stale persisted
// metadata into a second manager.  A plan export is deliberately not an
// authority for plan-bound runs: only the canonical row and its active
// execution link may repair a run after boot.
func validateRecoveryIdentity(s recoverySnapshot, p *planstore.Plan) error {
	if p == nil {
		return planstore.ErrNotFound
	}
	if p.ID != s.planID {
		return fmt.Errorf("canonical plan identity mismatch: wanted %q got %q", s.planID, p.ID)
	}
	if s.projectID != "" && p.ProjectID != s.projectID {
		return fmt.Errorf("canonical project identity mismatch: wanted %q got %q", s.projectID, p.ProjectID)
	}
	if want := PlanBoundRunID(s.repo, s.planID); s.runID != want {
		return fmt.Errorf("canonical run identity mismatch: wanted %q got %q", want, s.runID)
	}
	if p.ProjectID == "" || !samePath(p.ProjectID, s.repo) {
		return fmt.Errorf("canonical repository ownership mismatch: project %q repository %q", p.ProjectID, s.repo)
	}
	if p.AutopilotRunID != s.runID && (p.ActiveExecution == nil || p.ActiveExecution.ExecutorID != s.runID) {
		return fmt.Errorf("canonical active execution ownership mismatch for run %q", s.runID)
	}
	if ex := p.ActiveExecution; ex != nil && (ex.PlanID != p.ID || ex.ExecutorID != s.runID || ex.ExecutionMode != planstore.PlanModeAutopilot || ex.TerminalStatus != "") {
		return fmt.Errorf("canonical active execution ownership mismatch for run %q", s.runID)
	}
	if p.AutopilotRunID != "" && p.AutopilotRunID != s.runID {
		return fmt.Errorf("canonical execution ownership mismatch: plan is owned by %q", p.AutopilotRunID)
	}
	return nil
}

func (c *Controller) recoverAdmitted(ctx context.Context, runID string) {
	c.mu.Lock()
	r := c.runs[runID]
	if r == nil || c.recoveryOwners[runID] != r || !r.recoveryInFlight {
		c.mu.Unlock()
		return
	}
	if r.backoffKind != KindDefinitionError && !r.backoffNextRetry.IsZero() && c.now().Before(r.backoffNextRetry) {
		c.mu.Unlock()
		return
	}
	backend, tier := r.recoveryBackend, r.tier
	s := recoverySnapshot{r.runID, r.planID, r.projectID, r.repo, r.absPlanFile, r.slotScope, r.integrationBranch,
		r.recoveryAdmittedGeneration, r.recoveryAttempt, backend, tier, r.recoveryRotate, managerIDOf(r), r.recoveryReason, c.runtime, c.planSource, c.env, r.defaultBranch}
	localRun := cloneGuardianRun(r)
	local := &Controller{env: c.env, runtime: c.runtime, resolver: c.resolver, tierstate: c.tierstate, now: c.now,
		baseDir: c.baseDir, gate: c.gate, integrationBranch: c.integrationBranch, runs: map[string]*run{r.runID: localRun}}
	c.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if s.backend == "" {
		sel := local.selectBrainContext(ctx, localRun.tried)
		if !sel.OK {
			c.finishRecoveryFailure(runID, s.generation, tagFailure(KindNoBackendSelectable, fmt.Errorf("all backends rate-limited — no backend selectable")), false)
			return
		}
		s.backend, s.tier = sel.Backend, sel.Tier
	}

	if local.awaitingMergeLocked(localRun) {
		c.mu.Lock()
		if current := c.runs[runID]; current != nil && c.recoveryOwners[runID] == current && current.recoveryGeneration == s.generation {
			current.surface, current.surfaceLoaded, current.completion = localRun.surface, localRun.surfaceLoaded, localRun.completion
		}
		c.mu.Unlock()
		return
	}
	var plan Plan
	var warnings []string
	var err error
	if s.planID != "" && s.planSource == nil {
		err = fmt.Errorf("plan_definition_repair_needed: canonical source unavailable for %s", s.planID)
	} else if s.planID != "" {
		p, getErr := s.planSource.Get(ctx, s.planID)
		if getErr != nil || p == nil {
			if getErr == nil {
				getErr = planstore.ErrNotFound
			}
			err = fmt.Errorf("plan_definition_repair_needed: verified plan %s: %w", s.planID, getErr)
		} else if identityErr := validateRecoveryIdentity(s, p); identityErr != nil {
			err = fmt.Errorf("plan_definition_repair_needed: %w", identityErr)
		} else {
			s.projectID = p.ProjectID
			def := planstore.SnapshotFromPlan(p)
			if p.ActiveExecution != nil && p.ActiveExecution.Snapshot != nil {
				def = p.ActiveExecution.Snapshot
			}
			plan = planFromDefinition(def)
		}
	} else {
		preflightErr := local.preflightRegisteredRunLocked(ctx, localRun)
		if pe, ok := preflightErr.(*PreflightError); preflightErr != nil && (!ok || pe.hasStructural()) {
			err = preflightErr
		}
		if err == nil {
			plan, warnings, err = loadPlanLenient(s.planFile)
			if s.integrationBranch == "" {
				s.integrationBranch = localRun.integrationBranch
			}
			if s.defaultBranch == "" {
				s.defaultBranch = localRun.defaultBranch
			}
		}
		if err != nil {
			err = fmt.Errorf("plan_definition_repair_needed: %w", err)
		}
	}
	if err != nil {
		c.finishRecoveryFailure(runID, s.generation, err, true)
		return
	}
	// Default-branch lookup is a git operation. It was historically performed
	// by boot/guardian preflight beneath c.mu; keep the safeguard, but do it as
	// part of the admitted external phase.
	defaultBranch := s.defaultBranch
	if defaultBranch == "" && s.repo != "" && s.env != nil {
		if branch, branchErr := s.env.DefaultBranch(ctx, s.repo); branchErr == nil {
			defaultBranch = branch
		}
	}

	if isProtectedBranch(s.integrationBranch, defaultBranch) {
		c.finishRecoveryFailure(runID, s.generation, fmt.Errorf("integration branch %q is protected", s.integrationBranch), true)
		return
	}
	ledger := s.runtime.NewLedger(s.runID)
	var restartCtx *RestartContext
	if ledger != nil {
		if _, journalErr := ledger.Journal(); journalErr != nil {
			// Repair only through the supported backup/quarantine/rebuild API;
			// never edit the store directly.
			if _, repairErr := ledger.RepairJournal(); repairErr != nil {
				c.finishRecoveryFailure(runID, s.generation, fmt.Errorf("journal_repair_needed: %w (decode: %v)", repairErr, journalErr), false)
				return
			}
			c.mu.Lock()
			c.recoveryStats.JournalRepairs++
			c.mu.Unlock()
		}
		restartCtx, err = ledger.LoadRestartContext()
		if err != nil {
			c.finishRecoveryFailure(runID, s.generation, fmt.Errorf("load restart context: %w", err), true)
			return
		}
	}
	prompt, err := ComposeDigest(ctx, DigestInput{RestartContext: restartCtx, RunID: s.runID, Repo: s.repo, PlanFile: s.planFile, Plan: plan, Ledger: ledger, Sources: s.runtime.DigestSources(), IntegrationBranch: s.integrationBranch})
	if err != nil {
		c.finishRecoveryFailure(runID, s.generation, err, true)
		return
	}
	// Fence as close as possible to the irreversible runtime spawn.  The apply
	// fence below remains necessary because pause/stop can race immediately
	// after this check.
	c.mu.Lock()
	r = c.runs[runID]
	valid := r != nil && c.recoveryOwners[runID] == r && r.recoveryGeneration == s.generation && r.recoveryAttempt == s.attempt && r.recoveryInFlight &&
		((!s.rotate && r.brain == nil) || (s.rotate && managerIDOf(r) == s.rotateAgentID)) && ctx.Err() == nil
	c.mu.Unlock()
	if !valid {
		c.finishRecoveryFailure(runID, s.generation, context.Canceled, false)
		return
	}
	var handle BrainHandle
	if s.rotate {
		handle, err = s.runtime.RotateBrain(ctx, RotateBrainSpec{AgentID: s.rotateAgentID, Backend: s.backend, Prompt: prompt, Reason: s.rotateReason})
	} else {
		handle, err = s.runtime.SpawnBrain(ctx, BrainSpec{RunID: s.runID, PlanID: s.planID, ProjectID: s.projectID, Repo: s.repo, PlanFile: s.planFile, SlotScope: s.slotScope, Backend: s.backend, Prompt: prompt, Tags: []string{autopilotTag, runTag(s.runID)}, Headless: true})
	}
	if err != nil {
		if handle.NewLaunch {
			c.cleanupRecoveryHandle(s.runtime, runID, handle)
		}
		c.finishRecoveryFailure(runID, s.generation, err, false)
		return
	}

	c.mu.Lock()
	r = c.runs[runID]
	if r == nil || c.recoveryOwners[runID] != r || ctx.Err() != nil || r.recoveryGeneration != s.generation || r.recoveryAttempt != s.attempt || !r.recoveryInFlight ||
		(!s.rotate && r.brain != nil) || (s.rotate && managerIDOf(r) != s.rotateAgentID) {
		cleanup := handle.NewLaunch && (!s.rotate || r == nil || c.recoveryOwners[runID] != r || r.state == StateStopped || r.state == StateDisabled || r.state == StateComplete)
		// A rotation replaces the process in the same slot. Pause and a recovery
		// deadline preserve that slot; retain its new identity without reviving the
		// run. Stop/removal instead owns cleanup of this exact new launch.
		if s.rotate && !cleanup && r != nil && managerIDOf(r) == s.rotateAgentID {
			r.brain = &handle
			r.recoveryDiagnostic = "replacement_preserved"
		}
		c.mu.Unlock()
		if cleanup {
			c.cleanupRecoveryHandle(s.runtime, runID, handle)
		}
		return
	}
	// Reservation is released by launchRecovery after persistence/cleanup.
	r.recoveryDiagnostic = "adopted"
	if !handle.NewLaunch {
		c.recoveryStats.Adopted++
	}
	if handle.NewLaunch {
		r.recoveryDiagnostic = "launched"
	}
	r.recoveryRotate, r.recoveryBackend, r.recoveryReason = false, "", ""
	r.plan, r.preflightWarnings, r.brain = plan, warnings, &handle
	if r.planID == "" && r.cancel == nil {
		watchCtx, watchCancel := context.WithCancel(context.Background())
		r.cancel = watchCancel
		go c.watchPlan(watchCtx, r, planWatchInterval)
	}
	r.projectID = s.projectID
	if r.integrationBranch == "" {
		r.integrationBranch = s.integrationBranch
	}
	if localRun.resolvedGate != "" {
		r.resolvedGate = localRun.resolvedGate
	}
	r.surface, r.surfaceLoaded = localRun.surface, localRun.surfaceLoaded
	if defaultBranch != "" {
		r.defaultBranch = defaultBranch
	}
	r.tier, r.brainSpawnedAt, r.state = s.tier, c.now(), StateActive
	if r.needsAttention != "" {
		c.clearParked(r)
	}
	if !s.rotate {
		r.healStage = stageRotated
		r.healNextAt = c.now().Add(c.guardian.HeartbeatTimeout)

	}
	// Persistence (store and ledger I/O) must not run beneath c.mu.  The state
	// has already been fenced by generation and ownership above; write a value
	// snapshot after releasing the controller lock.
	rec := c.recordLocked(r)
	store := c.store
	branch := r.integrationBranch
	lostID, lostCause := r.recoveryLostID, r.recoveryLostCause
	r.recoveryLostID, r.recoveryLostCause = "", ""
	auditRun := cloneGuardianRun(r)
	c.mu.Unlock()
	if gr, ok := s.runtime.(GuardianRuntime); ok && lostID != "" {
		auditRespawn(ctx, gr, auditRun, lostID, lostCause)
	}
	c.persistAdmittedRecord(s.runtime, store, rec, branch)
}

func (c *Controller) cleanupRecoveryHandle(rt Runtime, runID string, handle BrainHandle) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), recoveryDeadline)
	defer cancel()
	var err error
	if cleaner, ok := rt.(RecoveryBrainCleaner); ok {
		err = cleaner.CleanupRecoveryBrain(cleanupCtx, runID, handle)
	} else {
		err = fmt.Errorf("runtime cannot clean owned launch %s", handle.LaunchID)
	}
	if err != nil {
		c.mu.Lock()
		c.recoveryStats.CleanupFailures++
		if current := c.runs[runID]; current != nil {
			current.recoveryDiagnostic = "child_cleanup_failed: " + err.Error()
		}
		c.mu.Unlock()
	}
}

func (c *Controller) finishRecoveryFailure(runID string, generation uint64, cause error, definition bool) {
	c.mu.Lock()
	r := c.runs[runID]
	if r == nil || c.recoveryOwners[runID] != r || !r.recoveryInFlight || r.recoveryGeneration != generation {
		c.mu.Unlock()
		return
	}
	snapshot := cloneGuardianRun(r)
	rt := c.runtime
	local := &Controller{runtime: rt, planSource: c.planSource, now: c.now, guardian: c.guardian, tierstate: c.tierstate}
	c.mu.Unlock()
	kind := classifySpawnError(cause)
	if definition {
		kind = KindDefinitionError
		cause = fmt.Errorf("plan_definition_repair_needed: plan not loadable: %w", cause)
	}
	if gr, ok := rt.(GuardianRuntime); ok {
		local.enterBackoff(gr, snapshot, local.now(), kind, false, cause)
	} else {
		local.enterWatcherBackoffLocked(snapshot, local.now(), cause)
		if definition {
			snapshot.backoffKind, snapshot.needsAttention = KindDefinitionError, cause.Error()
			snapshot.parkedPlanKey = local.planKey(context.Background(), snapshot)
		}
	}
	c.mu.Lock()
	r = c.runs[runID]
	if r == nil || c.recoveryOwners[runID] != r || r.recoveryGeneration != generation || r.recoveryAttempt != snapshot.recoveryAttempt {
		c.mu.Unlock()
		return
	}
	c.recoveryStats.Backoffs++
	if definition {
		c.recoveryStats.VerificationFailures++
	}
	r.recoveryDiagnostic = "waiting_for_backoff"
	if strings.Contains(cause.Error(), "journal_repair_needed") {
		c.recoveryStats.JournalRepairNeeded++
		r.recoveryDiagnostic = "journal_repair_needed"
	}
	if definition {
		r.recoveryDiagnostic = "verification_failed"
	}
	r.state, r.needsAttention, r.parkedPlanKey = snapshot.state, snapshot.needsAttention, snapshot.parkedPlanKey
	r.healStage, r.healNextAt = snapshot.healStage, snapshot.healNextAt
	r.backoffStage, r.backoffNextRetry, r.backoffKind, r.backoffLastErr = snapshot.backoffStage, snapshot.backoffNextRetry, snapshot.backoffKind, snapshot.backoffLastErr
	r.failStreak, r.failStreakText = snapshot.failStreak, snapshot.failStreakText
	if definition && !canonicalRecoveryPlanID(r) {
		r.recoveryDefinition = snapshot.parkedPlanKey
	}
	r.recoveryRotate, r.recoveryBackend, r.recoveryReason = false, "", ""
	if definition && !canonicalRecoveryPlanID(r) && r.cancel == nil {
		wctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		go c.watchPlan(wctx, r, planWatchInterval)
	}
	rec, store, branch := c.recordLocked(r), c.store, r.integrationBranch
	c.mu.Unlock()
	c.persistAdmittedRecord(rt, store, rec, branch)
}

func (c *Controller) persistAdmittedRecord(rt Runtime, store *RunStore, rec RunRecord, branch string) {
	if err := persistRecoveryRecord(rt, store, rec, branch); err != nil {
		c.mu.Lock()
		c.recoveryStats.PersistenceFailures++
		if r := c.runs[rec.RunID]; r != nil && r.persistRevision == rec.PersistRevision {
			r.recoveryDiagnostic = "persistence_failed: " + err.Error()
		}
		c.mu.Unlock()
	}
}

// persistRecoveryRecord is intentionally fed only immutable snapshots.  It is
// used by admitted work so a stalled ledger/store can never make Status, pause,
// stop, or another run wait behind c.mu.
func persistRecoveryRecord(rt Runtime, store *RunStore, rec RunRecord, branch string) error {
	// Serialize all effects for one run, outside the controller mutex. Revision
	// comparison also rejects an older snapshot that acquired this lock later.
	if store != nil {
		lock, _ := store.persistenceLocks.LoadOrStore(rec.RunID, &sync.Mutex{})
		mu := lock.(*sync.Mutex)
		mu.Lock()
		defer mu.Unlock()
		old, err := store.Get(rec.RunID)
		if err == nil {
			if old.PersistRevision > rec.PersistRevision {
				return nil
			}
			rec.CreatedAt = old.CreatedAt
		} else if !errors.Is(err, ErrRunNotFound) {
			return err
		} else {
			rec.CreatedAt = rec.UpdatedAt
		}
	}
	var ledgerErr error
	if rt != nil && strings.TrimSpace(branch) != "" {
		if ledger := rt.NewLedger(rec.RunID); ledger != nil {
			if err := ledger.WriteIntegrationBranch(branch, ledgerWriter); err != nil {
				ledgerErr = fmt.Errorf("persist integration branch: %w", err)
			}
		}
	}
	if store == nil {
		return ledgerErr
	}
	_, err := store.Update(rec.RunID, func(dst *RunRecord) error {
		if dst.PersistRevision <= rec.PersistRevision {
			*dst = rec
		}
		return nil
	})
	if errors.Is(err, ErrRunNotFound) {
		err = store.Create(rec)
	}
	return errors.Join(ledgerErr, err)
}

func canonicalRecoveryPlanID(r *run) bool { return r != nil && strings.TrimSpace(r.planID) != "" }
