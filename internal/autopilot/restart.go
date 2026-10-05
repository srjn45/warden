package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/srjn45/warden/internal/autopilotstore"
)

// ErrRestartUnsupported is returned when the daemon runtime cannot tear a run's
// agents down (a bare Runtime with no RestartRuntime seam).
var ErrRestartUnsupported = errors.New("autopilot: runtime does not support restart")

// RestartTeardown reports what the runtime removed for a restart.
type RestartTeardown struct {
	AgentsRemoved   int
	BranchesKept    []string // task branches with commits beyond the integration branch
	BranchesDeleted []string // task branches with no commits beyond the integration branch
}

// RestartRuntime is the optional daemon seam RestartRun needs: terminate every
// agent tagged run:<runID> (manager and workers), remove their worktrees, and
// apply the branch policy (keep a branch with commits beyond integrationBranch,
// delete an empty one, never close a PR). It must be idempotent — an agent that
// is already gone is success. The runtime may also implement RestartBranchProber.
type RestartRuntime interface {
	TeardownRunAgents(ctx context.Context, runID, repo, integrationBranch string) (RestartTeardown, error)
}

// RestartProberFactory is an optional RestartRuntime extension: it returns a
// branch prober bound to repo (the prober interface itself carries no repo).
type RestartProberFactory interface {
	RestartProber(repo string) RestartBranchProber
}

// RestartRequest is the operator's restart request.
type RestartRequest struct {
	Force   bool   // also restart an active / starting / paused run
	Backend string // optional manager backend override
}

// RestartResult reports a completed restart.
type RestartResult struct {
	Status          RunStatus
	ReasonKind      string
	RestartCount    int
	AgentsRemoved   int
	BranchesKept    []string
	BranchesDeleted []string
	Backend         string
}

// RestartRun is the operator, work-preserving restart (spec §3.1). It holds
// c.mu for the whole critical section so the guardian cannot respawn a manager
// mid-restart, captures the restart context BEFORE any teardown, removes the old
// agent set, resets unfinished ledger tasks to pending, clears the heal/parking
// state and spawns a fresh manager into the same slot. The run identity, plan row,
// landed tasks and landings are untouched. Re-issuing after a partial failure is
// safe: teardown is idempotent and the spawn is retried.
func (c *Controller) RestartRun(ctx context.Context, id string, req RestartRequest) (RestartResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[id]
	if !ok {
		return RestartResult{}, ErrRunNotFound
	}
	kind, reason, err := c.restartPreconditionsLocked(r, req.Force)
	if err != nil {
		return RestartResult{}, err
	}
	rr, ok := c.runtime.(RestartRuntime)
	if !ok {
		return RestartResult{}, ErrRestartUnsupported
	}
	ledger := c.runtime.NewLedger(r.runID)
	if ledger == nil {
		return RestartResult{}, errors.New("autopilot: restart needs the shared-context ledger")
	}

	// Plan-bound runs take task state from the plan store (never repo YAML).
	if err := c.hydratePlanFromSource(ctx, r); err != nil {
		return RestartResult{}, tagFailure(KindDefinitionError, fmt.Errorf("restart: %w", err))
	}

	// 1. Capture + persist the restart context BEFORE anything is torn down.
	in := RestartAssembleInput{
		PlanTasks:         c.restartPlanTasksLocked(ctx, r),
		IntegrationBranch: r.integrationBranch,
		Reason:            reason,
		ReasonKind:        kind,
		CountRestart:      true,
	}
	switch p := c.runtime.(type) {
	case RestartProberFactory:
		in.Prober = p.RestartProber(r.repo)
	case RestartBranchProber:
		in.Prober = p
	}
	rc, err := ledger.CaptureRestartContext(ctx, in)
	if err != nil {
		return RestartResult{}, fmt.Errorf("restart: capture context: %w", err)
	}

	// 2. Stop the watcher and remove every run-tagged agent (manager + workers).
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.brain = nil
	td, err := rr.TeardownRunAgents(ctx, r.runID, r.repo, r.integrationBranch)
	if err != nil {
		// Leave the run stopped, not guardian-supervised, so a re-issue is the way forward.
		r.state = StateStopped
		c.persistRunLocked(r)
		return RestartResult{}, fmt.Errorf("restart: teardown: %w", err)
	}

	// 3. Unfinished ledger tasks back to pending; landed rows + landings untouched.
	if err := resetUnfinishedLedgerTasks(ledger); err != nil {
		r.state = StateStopped
		c.persistRunLocked(r)
		return RestartResult{}, fmt.Errorf("restart: reset ledger: %w", err)
	}

	// 4. Clear heal ladder, backoff, tried set and parking.
	c.recover(r)
	c.clearParked(r)
	r.workersInFlight = 0

	// 5. Select a backend and spawn a fresh manager into the same slot.
	backend, tier := strings.TrimSpace(req.Backend), r.tier
	if backend == "" {
		sel := c.selectBrain(nil)
		if !sel.OK {
			r.state = StateDegraded
			c.persistRunLocked(r)
			return RestartResult{}, tagFailure(KindNoBackendSelectable, errors.New("restart: no backend selectable for the new manager"))
		}
		backend, tier = sel.Backend, sel.Tier
	}
	r.tier = tier
	r.state = StateStarting
	if err := c.spawnManager(ctx, r, backend); err != nil {
		c.persistRunLocked(r)
		c.syncLiveRunLocked(ctx, r, err)
		return RestartResult{}, fmt.Errorf("restart: spawn manager: %w", err)
	}
	r.state = StateActive
	if err := c.persistRunLockedErr(r); err != nil {
		return RestartResult{}, err
	}
	c.syncLiveRunLocked(ctx, r, nil)
	slog.Info("autopilot: run restarted", "run", r.runID, "reason", kind, "agents_removed", td.AgentsRemoved)
	return RestartResult{
		Status:          c.runStatusLocked(r),
		ReasonKind:      kind,
		RestartCount:    rc.RestartCount,
		AgentsRemoved:   td.AgentsRemoved,
		BranchesKept:    td.BranchesKept,
		BranchesDeleted: td.BranchesDeleted,
		Backend:         backend,
	}, nil
}

// restartPreconditionsLocked applies the §1 refusal matrix for an autopilot run
// and derives the restart reason. Caller holds c.mu.
func (c *Controller) restartPreconditionsLocked(r *run, force bool) (kind, reason string, err error) {
	if c.runtime == nil {
		return "", "", ErrRestartUnsupported
	}
	switch r.state {
	case StateComplete:
		return "", "", fmt.Errorf("%w: complete run is terminal", ErrRunConflict)
	case StateActive, StateStarting, StatePaused:
		if !force {
			return "", "", fmt.Errorf("%w: executor is healthy and active; pass --force to restart", ErrRunConflict)
		}
		return RestartReasonOperatorForce, "operator forced a restart of a " + string(r.state) + " executor", nil
	}
	switch {
	case r.needsAttention != "":
		return RestartReasonNeedsAttention, r.needsAttention, nil
	case r.state == StateDegraded:
		why := r.backoffLastErr
		if why == "" {
			why = "executor degraded"
		}
		return RestartReasonDegradedBackoff, why, nil
	case r.state == StateStopped:
		return RestartReasonOperatorStop, "executor was stopped", nil
	}
	return RestartReasonUnknown, "executor was " + string(r.state), nil
}

// restartPlanTasksLocked returns the plan's tasks with plan-store statuses
// overlaid so assembly can tell finished from unfinished work not in the ledger.
func (c *Controller) restartPlanTasksLocked(ctx context.Context, r *run) []PlanTask {
	tasks := append([]PlanTask(nil), r.plan.Tasks...)
	if c.planSource == nil || r.planID == "" {
		return tasks
	}
	p, err := c.planSource.Get(ctx, r.planID)
	if err != nil || p == nil {
		return tasks
	}
	for i := range tasks {
		if st, ok := p.TaskProgress[tasks[i].ID]; ok {
			tasks[i].Status = st
		}
	}
	return tasks
}

func resetUnfinishedLedgerTasks(l *Ledger) error {
	tasks, err := l.Tasks()
	if err != nil {
		return err
	}
	changed := false
	for i := range tasks {
		if tasks[i].State == LedgerLanded || tasks[i].State == LedgerPending && tasks[i].WorkerID == "" {
			continue
		}
		tasks[i].State = LedgerPending
		tasks[i].WorkerID = "" // Branch/PR stay: the restart context + re-issue read them
		changed = true
	}
	if !changed {
		return nil
	}
	return l.WriteTasks(tasks, ledgerWriter)
}

// syncLiveRunLocked mirrors the run's state + manager into the live Autopilot row.
func (c *Controller) syncLiveRunLocked(ctx context.Context, r *run, spawnErr error) {
	if c.live == nil {
		return
	}
	managerID := managerIDOf(r)
	_, _ = c.live.Update(ctx, r.runID, func(a *autopilotstore.Autopilot) error {
		a.ManagerAgentID = managerID
		a.Diagnostics.State = string(r.state)
		a.Diagnostics.LastError = ""
		if spawnErr != nil {
			a.Diagnostics.LastError = spawnErr.Error()
		}
		return nil
	})
}
