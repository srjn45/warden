package autopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrRunConflict = errors.New("autopilot run lifecycle conflict")

type RegisterRequest struct {
	Name     string `json:"name"`
	Repo     string `json:"repo"`
	PlanFile string `json:"plan_file"`
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
		r := &run{runID: rec.RunID, name: rec.Name, repo: rec.Repo, planFile: rec.PlanFile,
			absPlanFile: rec.PlanFile, state: rec.State, resolvedGate: rec.Gate,
			guardianID: rec.GuardianID, tried: map[string]bool{}}
		// Agent ids are process/session observations, not proof of a live manager.
		// Boot reconciliation re-spawns only runs whose durable intent is live.
		if plan, err := LoadPlan(rec.PlanFile); err == nil {
			r.plan = plan
		}
		c.runs[rec.RunID] = r
	}
}

func (c *Controller) recordLocked(r *run) RunRecord {
	now := c.now().UTC()
	rec := RunRecord{RunID: r.runID, Name: r.name, Repo: r.repo, PlanFile: r.absPlanFile,
		State: r.state, IntegrationBranch: c.integrationBranch, Gate: c.runGate(r),
		Strategy: c.strategy, DeleteBranch: c.deleteBranch, UpdatedAt: now}
	if r.brain != nil {
		rec.BrainID = r.brain.AgentID
	}
	rec.GuardianID = r.guardianID
	return rec
}

func (c *Controller) persistRunLocked(r *run) {
	_ = c.persistRunLockedErr(r)
}

func (c *Controller) persistRunLockedErr(r *run) error {
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

// Register validates and durably registers a named plan without starting it.
func (c *Controller) Register(ctx context.Context, req RegisterRequest) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	abs, err := filepath.Abs(strings.TrimSpace(req.PlanFile))
	if err != nil {
		return RunStatus{}, err
	}
	abs = filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	plan, err := LoadPlan(abs)
	if err != nil {
		return RunStatus{}, err
	}
	planRepo, err := c.env.GitToplevel(ctx, filepath.Dir(abs))
	if err != nil {
		return RunStatus{}, err
	}
	repo := planRepo
	if req.Repo != "" {
		repo = c.resolveRepo(ctx, req.Repo)
		if repo != planRepo {
			return RunStatus{}, fmt.Errorf("plan file belongs to %s, not %s", planRepo, repo)
		}
	}
	if real, err := filepath.EvalSymlinks(repo); err == nil {
		repo = real
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = defaultRunName(abs)
	}
	for _, existing := range c.runs {
		if existing.repo == repo && existing.name == name && existing.runID != RunID(repo, abs) {
			return RunStatus{}, fmt.Errorf("%w: run name %q already exists in %s", ErrRunConflict, name, repo)
		}
	}
	id := RunID(repo, abs)
	if existing, ok := c.runs[id]; ok {
		return c.runStatusLocked(existing), nil
	}
	r := &run{runID: id, name: name, repo: repo, planFile: abs, absPlanFile: abs,
		state: StateRegistered, plan: plan, resolvedGate: c.gate, tried: map[string]bool{}}
	if info, err := os.Stat(abs); err == nil {
		r.planModTime = info.ModTime()
	}
	c.runs[id] = r
	if err := c.persistRunLockedErr(r); err != nil {
		delete(c.runs, id)
		return RunStatus{}, err
	}
	return c.runStatusLocked(r), nil
}

func (c *Controller) StartRun(ctx context.Context, id string) (RunStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	r.state = StateStarting
	if err := c.enableStore.Enable(r.repo); err != nil {
		return RunStatus{}, fmt.Errorf("persist enabled repo: %w", err)
	}
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
	if plan, err := LoadPlan(r.absPlanFile); err != nil {
		return RunStatus{}, err
	} else {
		r.plan = plan
	}
	r.state = StateActive
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

func (c *Controller) runStatusLocked(r *run) RunStatus {
	return RunStatus{RunID: r.runID, Name: r.name, PlanFile: r.planFile, Repo: r.repo,
		State: r.state, Gate: c.runGate(r), Tasks: TaskCounts{}}
}

func (c *Controller) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.store == nil {
		return nil
	}
	err := c.store.Close()
	c.store = nil
	return err
}
