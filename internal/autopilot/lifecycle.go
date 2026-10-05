package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrRunConflict = errors.New("autopilot run lifecycle conflict")

type RegisterRequest struct {
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	PlanFile  string `json:"plan_file"`
	PlanID    string `json:"plan_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

func defaultRunName(planFile string) string {
	base := filepath.Base(planFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func (c *Controller) restoreStoredRuns() {
	if c.store == nil {
		return
	}
	records, err := c.store.List()
	if err != nil {
		return
	}
	for _, rec := range records {
		legacyID := rec.RunID
		if strings.TrimSpace(rec.PlanID) != "" {
			// Plan-bound executors use PlanID-stable identity; never re-key from
			// (possibly empty) export PlanFile paths.
			rec.RunID = PlanBoundRunID(rec.Repo, rec.PlanID)
		} else {
			rec.RunID = RunID(rec.Repo, rec.PlanFile)
		}
		// Transparently re-key records written before canonical path identity was
		// enforced. Creating first makes an interrupted migration retry-safe.
		if rec.RunID != legacyID {
			if err := c.store.Create(rec); err == nil || errors.Is(err, ErrRunExists) {
				_ = c.store.Delete(legacyID)
			}
		}
		r := &run{runID: rec.RunID, name: rec.Name, planID: rec.PlanID, projectID: rec.ProjectID, repo: rec.Repo, planFile: rec.PlanFile,
			absPlanFile: rec.PlanFile, state: rec.State, resolvedGate: rec.Gate,
			slotScope: rec.SlotScope, integrationBranch: rec.IntegrationBranch,
			tried: map[string]bool{}}
		// Restore slot ids only when they match the persisted scope — legacy
		// agent-<hex> ids are ignored so boot reconciliation adopts into slots.
		if rec.SlotScope != "" {
			if rec.BrainID == ManagerSlotID(rec.SlotScope) {
				r.brain = &BrainHandle{AgentID: rec.BrainID}
			}
		}
		// Boot reconciliation re-spawns only runs whose durable intent is live.
		// Plan-bound runs hydrate definition from PlanTaskSource (RecoverLiveAutopilots);
		// legacy file-registered runs still load from PlanFile.
		if rec.PlanID == "" {
			if plan, err := LoadPlan(rec.PlanFile); err == nil {
				r.plan = plan
			} else if plan, warnings, lerr := loadPlanLenient(rec.PlanFile); lerr == nil {
				r.plan = plan
				r.preflightWarnings = warnings
			}
		}
		c.hydratePlanTasksLocked(context.Background(), r)
		c.runs[rec.RunID] = r
	}
	c.rebuildClaimsLocked()
}

func (c *Controller) rebuildClaimsLocked() {
	if c.claims == nil {
		c.claims = newClaimRegistry()
	}
	c.claims = newClaimRegistry()
	runs := make([]*run, 0, len(c.runs))
	for _, r := range c.runs {
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].runID < runs[j].runID })
	for _, r := range runs {
		scope := r.slotScope
		if scope == "" {
			derived, err := SlotScope(r.name, r.runID, func(s string) bool { return c.claims.scopeTaken(s, r.runID) })
			if err != nil {
				slog.Warn("autopilot: skip run with invalid slot scope on rebuild", "run", r.runID, "err", err)
				continue
			}
			scope = derived
			r.slotScope = scope
		}
		if err := c.claims.claim(r.runID, scope); err != nil {
			slog.Warn("autopilot: slot scope claim conflict on rebuild", "run", r.runID, "scope", scope, "err", err)
		}
	}
}

func (c *Controller) recordLocked(r *run) RunRecord {
	now := c.now().UTC()
	rec := RunRecord{RunID: r.runID, Name: r.name, Repo: r.repo, PlanFile: r.absPlanFile,
		PlanID: r.planID, ProjectID: r.projectID,
		State: r.state, IntegrationBranch: r.integrationBranch, Gate: c.runGate(r),
		Strategy: c.strategy, DeleteBranch: c.deleteBranch, SlotScope: r.slotScope, UpdatedAt: now}
	if r.brain != nil && r.slotScope != "" {
		rec.BrainID = ManagerSlotID(r.slotScope)
	}
	return rec
}

func (c *Controller) persistRunLocked(r *run) {
	_ = c.persistRunLockedErr(r)
}

func (c *Controller) persistRunLockedErr(r *run) error {
	c.persistIntegrationBranch(r)
	if c.store == nil {
		return nil
	}
	rec := c.recordLocked(r)
	old, err := c.store.Get(r.runID)
	if errors.Is(err, ErrRunNotFound) {
		rec.CreatedAt = rec.UpdatedAt
		return c.store.Create(rec)
	}
	if err != nil {
		return err
	}
	rec.CreatedAt = old.CreatedAt
	_, err = c.store.Update(r.runID, func(dst *RunRecord) error { *dst = rec; return nil })
	return err
}

func (c *Controller) persistIntegrationBranch(r *run) {
	if c.runtime == nil || r == nil || strings.TrimSpace(r.integrationBranch) == "" {
		return
	}
	l := c.runtime.NewLedger(r.runID)
	if l == nil {
		return
	}
	if err := l.WriteIntegrationBranch(r.integrationBranch, ledgerWriter); err != nil {
		slog.Warn("autopilot: persist integration branch to ledger failed", "run", r.runID, "err", err)
	}
}

// Register validates and durably registers a named plan without starting it.
func (c *Controller) Register(ctx context.Context, req RegisterRequest) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return RunStatus{}, c.storeErr
	}
	abs, err := filepath.Abs(strings.TrimSpace(req.PlanFile))
	if err != nil {
		return RunStatus{}, err
	}
	abs = filepath.Clean(abs)
	plan, err := LoadPlan(abs)
	if err != nil {
		return RunStatus{}, err
	}
	planRepo, err := c.env.GitToplevel(ctx, filepath.Dir(abs))
	if err != nil {
		return RunStatus{}, err
	}
	planRepo = filepath.Clean(planRepo)
	repo := planRepo
	if req.Repo != "" {
		repo = c.resolveRepo(ctx, req.Repo)
		if !samePath(repo, planRepo) {
			return RunStatus{}, fmt.Errorf("plan file belongs to %s, not %s", planRepo, repo)
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = defaultRunName(abs)
	}
	if err := validatePlanNameReservedSuffixes(name); err != nil {
		return RunStatus{}, err
	}
	id := RunID(repo, abs)
	if err := c.validateRunNameLocked(repo, name, id); err != nil {
		return RunStatus{}, err
	}
	scope, err := c.allocateSlotScopeLocked(name, id)
	if err != nil {
		return RunStatus{}, err
	}
	if existing, ok := c.runs[id]; ok {
		return c.runStatusLocked(existing), nil
	}
	branch, err := resolveIntegrationBranch(branchResolveOpts{
		planName: name,
		runID:    id,
		template: c.integrationBranch,
		taken:    c.branchTakenLocked(repo, id, nil),
	})
	if err != nil {
		return RunStatus{}, err
	}
	c.warnSameBranchLocked(repo, branch, id, nil)
	r := &run{runID: id, name: name, planID: req.PlanID, projectID: req.ProjectID, repo: repo, planFile: abs, absPlanFile: abs,
		state: StateRegistered, plan: plan, resolvedGate: c.gate, slotScope: scope,
		integrationBranch: branch, tried: map[string]bool{}}
	if info, err := os.Stat(abs); err == nil {
		r.planModTime = info.ModTime()
	}
	c.runs[id] = r
	if err := c.claims.claim(id, scope); err != nil {
		delete(c.runs, id)
		return RunStatus{}, err
	}
	if err := c.persistRunLockedErr(r); err != nil {
		delete(c.runs, id)
		c.claims.release(id)
		return RunStatus{}, err
	}
	return c.runStatusLocked(r), nil
}

func (c *Controller) validateRunNameLocked(repo, name, exceptRunID string) error {
	for _, existing := range c.runs {
		if existing.repo == repo && existing.name == name && existing.runID != exceptRunID {
			return fmt.Errorf("%w: run name %q already exists in %s", ErrRunConflict, name, repo)
		}
	}
	return nil
}

func (c *Controller) allocateSlotScopeLocked(name, runID string) (string, error) {
	scope, err := SlotScope(name, runID, func(s string) bool { return c.claims.scopeTaken(s, runID) })
	if err != nil {
		return "", err
	}
	if err := c.claims.validateClaim(runID, scope); err != nil {
		return "", err
	}
	return scope, nil
}

// RetargetIntegrationBranchRequest selects a new merge target for a run.
// Either IntegrationBranch is set explicitly or Derive requests resolution from
// the run's current display name and the global template — never both.
type RetargetIntegrationBranchRequest struct {
	IntegrationBranch string `json:"integration_branch,omitempty"`
	Derive            bool   `json:"derive,omitempty"`
}

func (c *Controller) retargetableRunLocked(id string) (*run, error) {
	r, ok := c.runs[id]
	if !ok {
		return nil, ErrRunNotFound
	}
	switch r.state {
	case StateActive, StateStarting, StateHealing, StateDegraded:
		return nil, fmt.Errorf("%w: cannot retarget run in state %s", ErrRunConflict, r.state)
	}
	return r, nil
}

// RenameRun changes a run's display name and slot scope without altering its
// path-derived run_id. Integration branch retarget is explicit (WP11).
func (c *Controller) RenameRun(_ context.Context, id, newName string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return RunStatus{}, c.storeErr
	}
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return RunStatus{}, fmt.Errorf("%w: run name is required", ErrRunConflict)
	}
	r, err := c.retargetableRunLocked(id)
	if err != nil {
		return RunStatus{}, err
	}
	if r.name == newName {
		return c.runStatusLocked(r), nil
	}
	if err := validatePlanNameReservedSuffixes(newName); err != nil {
		return RunStatus{}, err
	}
	if err := c.validateRunNameLocked(r.repo, newName, id); err != nil {
		return RunStatus{}, err
	}
	scope, err := c.allocateSlotScopeLocked(newName, id)
	if err != nil {
		return RunStatus{}, err
	}
	oldName := r.name
	oldScope := r.slotScope
	r.name = newName
	r.slotScope = scope
	if err := c.claims.claim(id, scope); err != nil {
		r.name = oldName
		r.slotScope = oldScope
		return RunStatus{}, err
	}
	if err := c.persistRunLockedErr(r); err != nil {
		r.name = oldName
		r.slotScope = oldScope
		_ = c.claims.claim(id, oldScope)
		return RunStatus{}, err
	}
	return c.runStatusLocked(r), nil
}

// RetargetIntegrationBranch updates the stored merge target for a run. Open PRs
// on the previous branch are not migrated automatically; land continues to use
// the stored value and rejects PRs based on the old branch with ErrWrongBase.
func (c *Controller) RetargetIntegrationBranch(_ context.Context, id string, req RetargetIntegrationBranchRequest) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return RunStatus{}, c.storeErr
	}
	r, err := c.retargetableRunLocked(id)
	if err != nil {
		return RunStatus{}, err
	}
	explicit := strings.TrimSpace(req.IntegrationBranch)
	switch {
	case explicit != "" && req.Derive:
		return RunStatus{}, fmt.Errorf("%w: set integration_branch or derive, not both", ErrRunConflict)
	case explicit == "" && !req.Derive:
		return RunStatus{}, fmt.Errorf("%w: integration branch or derive is required", ErrRunConflict)
	}
	var branch string
	if req.Derive {
		branch, err = resolveIntegrationBranch(branchResolveOpts{
			planName: r.name,
			runID:    r.runID,
			template: c.integrationBranch,
			taken:    c.branchTakenLocked(r.repo, r.runID, nil),
		})
	} else {
		branch = explicit
		if err = validateIntegrationBranch(branch); err != nil {
			return RunStatus{}, err
		}
		if c.branchTakenLocked(r.repo, r.runID, nil)(branch) {
			return RunStatus{}, fmt.Errorf("%w: integration branch %q is already claimed", ErrRunConflict, branch)
		}
	}
	if err != nil {
		return RunStatus{}, err
	}
	if branch == r.integrationBranch {
		return c.runStatusLocked(r), nil
	}
	oldBranch := r.integrationBranch
	r.integrationBranch = branch
	c.warnSameBranchLocked(r.repo, branch, r.runID, nil)
	if err := c.persistRunLockedErr(r); err != nil {
		r.integrationBranch = oldBranch
		return RunStatus{}, err
	}
	return c.runStatusLocked(r), nil
}

// preflightRegisteredRunLocked applies the same safety checks as legacy Enable
// to one durable V2 record before a start or resume. Returns a *PreflightError
// carrying typed classification (structural vs content) so the boot-recovery
// path in SetRuntime can distinguish permanent failures from transient ones.
// Caller holds c.mu.
func (c *Controller) preflightRegisteredRunLocked(ctx context.Context, r *run) error {
	if c.planBound(r) {
		return c.preflightPlanBoundRunLocked(ctx, r)
	}
	resolved, failures := c.preflightPlan(ctx, r.absPlanFile, nil)
	if len(failures) == 0 && !resolved.skipComplete {
		for _, msg := range c.validatePersistedDoneClaims(resolved.runID, resolved.plan) {
			failures = append(failures, preflightFailure{msg: msg, kind: preflightKindContent})
		}
	}
	if resolved.skipComplete {
		failures = append(failures, preflightFailure{msg: "plan is already marked complete", kind: preflightKindStructural})
	}
	// Plan-bound runs (canonical ScrivaDB PlanID) use PlanBoundRunID, while legacy
	// Register still uses the file-path RunID even when PlanID is attached.
	// Comparing against preflight's file-derived runID would false-fail resume
	// after the DB cutover. When planID is set, repo path match is enough.
	if resolved.repo != "" && !samePath(resolved.repo, r.repo) {
		failures = append(failures, preflightFailure{msg: "registered plan identity no longer matches its repository", kind: preflightKindStructural})
	} else if resolved.repo != "" && strings.TrimSpace(r.planID) == "" && resolved.runID != r.runID {
		failures = append(failures, preflightFailure{msg: "registered plan identity no longer matches its repository", kind: preflightKindStructural})
	}
	if len(failures) > 0 {
		return newPreflightError(failures)
	}
	r.plan = resolved.plan
	r.planFile = resolved.file
	r.absPlanFile = resolved.absFile
	r.resolvedGate = resolved.resolvedGate
	r.defaultBranch = resolved.defaultBranch
	if r.integrationBranch == "" {
		r.integrationBranch = resolved.integrationBranch
	}
	if info, err := os.Stat(resolved.absFile); err == nil {
		r.planModTime = info.ModTime()
	}
	return nil
}

// preflightPlanBoundRunLocked is the preflight for a run whose definition is
// canonical in the plan store: the YAML export is never read, so a stale or
// unparsable export can neither fail preflight nor degrade the run. Only an
// unloadable plan-store definition is a (structural) failure. Caller holds c.mu.
func (c *Controller) preflightPlanBoundRunLocked(ctx context.Context, r *run) error {
	if err := c.hydratePlanFromSource(ctx, r); err != nil {
		return newPreflightError([]preflightFailure{{msg: err.Error(), kind: preflightKindStructural}})
	}
	if r.defaultBranch == "" && r.repo != "" {
		if def, err := c.env.DefaultBranch(ctx, r.repo); err == nil {
			r.defaultBranch = def
		}
	}
	return nil
}

func (c *Controller) StartRun(ctx context.Context, id string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return RunStatus{}, c.storeErr
	}
	r, ok := c.runs[id]
	if !ok {
		return RunStatus{}, ErrRunNotFound
	}
	switch r.state {
	case StateActive, StateStarting, StateHealing, StateDegraded:
		return c.runStatusLocked(r), nil
	case StateComplete, StatePaused:
		return RunStatus{}, fmt.Errorf("%w: cannot start run in state %s", ErrRunConflict, r.state)
	case StateRegistered, StateStopped, StateDisabled:
	default:
		return RunStatus{}, fmt.Errorf("%w: cannot start run in state %s", ErrRunConflict, r.state)
	}
	if err := c.preflightRegisteredRunLocked(ctx, r); err != nil {
		return c.runStatusLocked(r), err
	}
	r.state = StateStarting
	if err := c.persistRunLockedErr(r); err != nil {
		return RunStatus{}, err
	} // persist intent before spawn
	sel := c.selectBrain(nil)
	r.tier = sel.Tier
	if err := c.spawnBrain(ctx, r, sel.Backend); err != nil {
		c.persistRunLocked(r)
		return c.runStatusLocked(r), err
	}
	if c.runtime != nil && r.cancel == nil {
		wctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		go c.watchPlan(wctx, r, planWatchInterval)
	}
	if err := c.persistRunLockedErr(r); err != nil {
		return RunStatus{}, err
	}
	return c.runStatusLocked(r), nil
}

func (c *Controller) PauseRun(ctx context.Context, id string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return RunStatus{}, ErrRunNotFound
	}
	if r.state == StatePaused {
		return c.runStatusLocked(r), nil
	}
	switch r.state {
	case StateActive, StateHealing, StateDegraded:
	default:
		return RunStatus{}, fmt.Errorf("%w: cannot pause run in state %s", ErrRunConflict, r.state)
	}
	r.state = StatePaused
	c.clearParked(r)
	if err := c.persistRunLockedErr(r); err != nil {
		return RunStatus{}, err
	}
	return c.runStatusLocked(r), nil
}

func (c *Controller) ResumeRun(ctx context.Context, id string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return RunStatus{}, ErrRunNotFound
	}
	if r.state == StateActive {
		return c.runStatusLocked(r), nil
	}
	if r.state != StatePaused {
		return RunStatus{}, fmt.Errorf("%w: cannot resume run in state %s", ErrRunConflict, r.state)
	}
	c.clearParked(r)
	if err := c.preflightRegisteredRunLocked(ctx, r); err != nil {
		return c.runStatusLocked(r), err
	}
	if r.brain == nil {
		r.state = StateStarting
		sel := c.selectBrain(nil)
		r.tier = sel.Tier
		if err := c.spawnBrain(ctx, r, sel.Backend); err != nil {
			c.persistRunLocked(r)
			return c.runStatusLocked(r), err
		}
		if c.runtime != nil && r.cancel == nil {
			wctx, cancel := context.WithCancel(context.Background())
			r.cancel = cancel
			go c.watchPlan(wctx, r, planWatchInterval)
		}
	} else {
		r.state = StateActive
	}
	if err := c.persistRunLockedErr(r); err != nil {
		return RunStatus{}, err
	}
	return c.runStatusLocked(r), nil
}

func (c *Controller) StopRun(ctx context.Context, id string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return RunStatus{}, ErrRunNotFound
	}
	if r.state == StateComplete {
		return RunStatus{}, fmt.Errorf("%w: complete run is terminal", ErrRunConflict)
	}
	if r.state != StateStopped {
		c.stopRunLocked(ctx, r)
		r.state = StateStopped
		if err := c.persistRunLockedErr(r); err != nil {
			return RunStatus{}, err
		}
	}
	return c.runStatusLocked(r), nil
}

// UnregisterRun removes a durable run registration. Live or paused runs must be
// stopped first so deregistration can never strand a brain, guardian, or watcher.
func (c *Controller) UnregisterRun(_ context.Context, id string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.storeErr != nil {
		return RunStatus{}, c.storeErr
	}
	r, ok := c.runs[id]
	if !ok {
		return RunStatus{}, ErrRunNotFound
	}
	switch r.state {
	case StateRegistered, StateStopped, StateComplete, StateDisabled:
	default:
		return RunStatus{}, fmt.Errorf("%w: stop run in state %s before unregistering", ErrRunConflict, r.state)
	}
	removed := c.runStatusLocked(r)
	if c.store != nil {
		if err := c.store.Delete(id); err != nil {
			return RunStatus{}, err
		}
	}
	c.claims.release(id)
	delete(c.runs, id)
	return removed, nil
}

func (c *Controller) runStatusLocked(r *run) RunStatus {
	st := RunStatus{RunID: r.runID, Name: r.name, PlanFile: r.planFile, Repo: r.repo, PlanID: r.planID, ProjectID: r.projectID,
		State: r.state, Gate: c.runGate(r), Tasks: TaskCounts{},
		PlanTasks: append([]PlanTask(nil), r.plan.Tasks...), GuardianID: guardianSlotIDOrEmpty(r.slotScope),
		SlotScope: r.slotScope, IntegrationBranch: r.integrationBranch, GateWarning: r.gateWarning,
		ManagerSlotID: managerSlotIDOrEmpty(r.slotScope), GuardianSlotID: guardianSlotIDOrEmpty(r.slotScope),
		LedgerTasks:       c.ledgerTasksLocked(r.runID),
		PreflightWarnings: append([]string(nil), r.preflightWarnings...)}
	return st
}

func (c *Controller) ledgerTasksLocked(runID string) []LedgerTask {
	if c.runtime == nil {
		return nil
	}
	l := c.runtime.NewLedger(runID)
	if l == nil {
		return nil
	}
	tasks, err := l.Tasks()
	if err != nil || len(tasks) == 0 {
		return nil
	}
	return tasks
}

func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.runs {
		if r.cancel != nil {
			r.cancel()
			r.cancel = nil
		}
	}
	if c.store == nil {
		return nil
	}
	err := c.store.Close()
	c.store = nil
	return err
}
