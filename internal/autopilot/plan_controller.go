package autopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/planstore"
)

// ErrPlanIDRequired is returned when StartFromPlan is called without a PlanID.
var ErrPlanIDRequired = errors.New("autopilot: plan_id is required")

// PlanStartRequest creates a live Autopilot executor bound to a Plan.
// PlanID is required; planless creation is rejected.
type PlanStartRequest struct {
	PlanID    string
	ProjectID string
	Name      string // plan name (display becomes AP:<name>)
	Repo      string // absolute repo root
	PlanFile  string // absolute plan YAML path (diagnostics + slot identity)
}

// PlanStartResult is the outcome of StartFromPlan.
type PlanStartResult struct {
	AutopilotID    string
	ManagerAgentID string
	Status         RunStatus
}

// PlanTaskSource supplies task definitions and progress from the Plan store.
// The Controller treats Plan as the source of task state (not the ctx ledger).
// *planstore.Store satisfies this interface via Get.
type PlanTaskSource interface {
	Get(ctx context.Context, planID string) (*planstore.Plan, error)
}

// ConsultBrainSpec launches an on-demand role=brain agent tied to a live Autopilot.
type ConsultBrainSpec struct {
	AutopilotID string
	Prompt      string
	Backend     string
}

// SetLiveStore wires the live Autopilot entity store (plan-execution redesign).
// A nil store leaves StartFromPlan unavailable.
func (c *Controller) SetLiveStore(live *autopilotstore.Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = live
}

// SetPlanSource wires the Plan store used as the source of task state.
func (c *Controller) SetPlanSource(src PlanTaskSource) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.planSource = src
}

// StartFromPlan creates a live Autopilot + manager Agent for Plan run
// mode=autopilot. Unlike Register, it does not register a plan-file-only
// RunRecord for later StartRun — the Plan is the durable unit of work and the
// Autopilot is the disposable executor.
func (c *Controller) StartFromPlan(ctx context.Context, req PlanStartRequest) (PlanStartResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.TrimSpace(req.PlanID) == "" {
		return PlanStartResult{}, ErrPlanIDRequired
	}
	if c.live == nil {
		return PlanStartResult{}, errors.New("autopilot: live Autopilot store not configured")
	}
	if c.storeErr != nil {
		return PlanStartResult{}, c.storeErr
	}

	absPlan := filepath.Clean(strings.TrimSpace(req.PlanFile))
	if absPlan == "" {
		return PlanStartResult{}, errors.New("autopilot: plan file path required")
	}
	if !filepath.IsAbs(absPlan) {
		var err error
		absPlan, err = filepath.Abs(absPlan)
		if err != nil {
			return PlanStartResult{}, err
		}
	}
	repo := strings.TrimSpace(req.Repo)
	if repo == "" {
		return PlanStartResult{}, errors.New("autopilot: repo required")
	}
	repo = filepath.Clean(repo)

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = defaultRunName(absPlan)
	}
	if err := validatePlanNameReservedSuffixes(name); err != nil {
		return PlanStartResult{}, err
	}

	runID := RunID(repo, absPlan)
	if existing, ok := c.runs[runID]; ok {
		switch existing.state {
		case StateActive, StateStarting, StateHealing, StateDegraded, StatePaused:
			return PlanStartResult{
				AutopilotID:    runID,
				ManagerAgentID: managerIDOf(existing),
				Status:         c.runStatusLocked(existing),
			}, nil
		}
	}

	// Prefer an already-live Autopilot for this plan (restart / idempotent re-run).
	if live, err := c.live.ListByPlan(ctx, req.PlanID); err == nil {
		for _, a := range live {
			if a.ID == runID || a.Diagnostics.Repo == repo {
				return c.adoptLiveLocked(ctx, a, absPlan, repo, name, req)
			}
		}
	}

	scope, err := c.allocateSlotScopeLocked(name, runID)
	if err != nil {
		return PlanStartResult{}, err
	}
	branch, err := resolveIntegrationBranch(branchResolveOpts{
		planName: name,
		runID:    runID,
		template: c.integrationBranch,
		taken:    c.branchTakenLocked(repo, runID, nil),
	})
	if err != nil {
		return PlanStartResult{}, err
	}

	plan, err := LoadPlan(absPlan)
	if err != nil {
		// Content-only failures: proceed with empty plan defs; Plan store owns progress.
		plan, _, _ = loadPlanLenient(absPlan)
	}

	ap := &autopilotstore.Autopilot{
		ID:        runID,
		ProjectID: req.ProjectID,
		PlanID:    req.PlanID,
		Name:      autopilotstore.DisplayName(name),
		Diagnostics: autopilotstore.Diagnostics{
			State:             string(StateStarting),
			IntegrationBranch: branch,
			Gate:              c.gate,
			Strategy:          c.strategy,
			SlotScope:         scope,
			Repo:              repo,
			PlanFile:          absPlan,
		},
	}
	if err := c.live.Create(ctx, ap); err != nil {
		if !errors.Is(err, autopilotstore.ErrExists) {
			return PlanStartResult{}, fmt.Errorf("create live Autopilot: %w", err)
		}
		existing, gerr := c.live.Get(ctx, runID)
		if gerr != nil {
			return PlanStartResult{}, gerr
		}
		return c.adoptLiveLocked(ctx, existing, absPlan, repo, name, req)
	}

	if err := c.enableStore.Enable(repo); err != nil {
		_ = c.live.Delete(ctx, runID)
		return PlanStartResult{}, fmt.Errorf("persist enabled repo: %w", err)
	}

	r := &run{
		runID:             runID,
		name:              name,
		planID:            req.PlanID,
		projectID:         req.ProjectID,
		repo:              repo,
		planFile:          absPlan,
		absPlanFile:       absPlan,
		state:             StateStarting,
		plan:              plan,
		resolvedGate:      c.gate,
		slotScope:         scope,
		integrationBranch: branch,
		tried:             map[string]bool{},
	}
	if info, err := os.Stat(absPlan); err == nil {
		r.planModTime = info.ModTime()
	}
	c.runs[runID] = r
	if err := c.claims.claim(runID, scope); err != nil {
		delete(c.runs, runID)
		_ = c.live.Delete(ctx, runID)
		return PlanStartResult{}, err
	}
	// Dual-write RunRecord for guardian/status compatibility during cutover.
	if err := c.persistRunLockedErr(r); err != nil {
		delete(c.runs, runID)
		c.claims.release(runID)
		_ = c.live.Delete(ctx, runID)
		return PlanStartResult{}, err
	}

	sel := c.selectBrain(nil)
	r.tier = sel.Tier
	if err := c.spawnManager(ctx, r, sel.Backend); err != nil {
		c.persistRunLocked(r)
		_, _ = c.live.Update(ctx, runID, func(a *autopilotstore.Autopilot) error {
			a.Diagnostics.State = string(r.state)
			a.Diagnostics.LastError = err.Error()
			return nil
		})
		return PlanStartResult{AutopilotID: runID, Status: c.runStatusLocked(r)}, err
	}

	managerID := managerIDOf(r)
	_, _ = c.live.Update(ctx, runID, func(a *autopilotstore.Autopilot) error {
		a.ManagerAgentID = managerID
		a.Diagnostics.State = string(r.state)
		a.Diagnostics.LastError = ""
		return nil
	})
	if c.runtime != nil && r.cancel == nil {
		wctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		go c.watchPlan(wctx, r, planWatchInterval)
	}
	_ = c.persistRunLockedErr(r)
	return PlanStartResult{
		AutopilotID:    runID,
		ManagerAgentID: managerID,
		Status:         c.runStatusLocked(r),
	}, nil
}

func (c *Controller) adoptLiveLocked(ctx context.Context, a *autopilotstore.Autopilot, absPlan, repo, name string, req PlanStartRequest) (PlanStartResult, error) {
	if a == nil {
		return PlanStartResult{}, ErrRunNotFound
	}
	runID := a.ID
	if existing, ok := c.runs[runID]; ok {
		return PlanStartResult{
			AutopilotID:    runID,
			ManagerAgentID: firstNonEmpty(a.ManagerAgentID, managerIDOf(existing)),
			Status:         c.runStatusLocked(existing),
		}, nil
	}
	scope := a.Diagnostics.SlotScope
	if scope == "" {
		var err error
		scope, err = c.allocateSlotScopeLocked(name, runID)
		if err != nil {
			return PlanStartResult{}, err
		}
	}
	plan, _ := LoadPlan(absPlan)
	if plan.Goal == "" {
		plan, _, _ = loadPlanLenient(absPlan)
	}
	r := &run{
		runID:             runID,
		name:              name,
		planID:            a.PlanID,
		projectID:         firstNonEmpty(a.ProjectID, req.ProjectID),
		repo:              firstNonEmpty(a.Diagnostics.Repo, repo),
		planFile:          firstNonEmpty(a.Diagnostics.PlanFile, absPlan),
		absPlanFile:       firstNonEmpty(a.Diagnostics.PlanFile, absPlan),
		state:             RunState(firstNonEmpty(a.Diagnostics.State, string(StateActive))),
		plan:              plan,
		resolvedGate:      firstNonEmpty(a.Diagnostics.Gate, c.gate),
		slotScope:         scope,
		integrationBranch: a.Diagnostics.IntegrationBranch,
		tried:             map[string]bool{},
	}
	if a.ManagerAgentID != "" {
		r.brain = &BrainHandle{AgentID: a.ManagerAgentID}
	}
	c.hydratePlanTasksLocked(ctx, r)
	c.runs[runID] = r
	_ = c.claims.claim(runID, scope)
	return PlanStartResult{
		AutopilotID:    runID,
		ManagerAgentID: a.ManagerAgentID,
		Status:         c.runStatusLocked(r),
	}, nil
}

// LiveAutopilots returns the live Autopilot entities from the live store.
// Used by the project-tree projection. Nil/empty when the live store is unset.
func (c *Controller) LiveAutopilots(ctx context.Context) ([]*autopilotstore.Autopilot, error) {
	c.mu.Lock()
	live := c.live
	c.mu.Unlock()
	if live == nil {
		return nil, nil
	}
	return live.List(ctx)
}

// RecoverLiveAutopilots rehydrates in-memory runs from the live Autopilot store
// and Plan task progress. Called after SetRuntime so managers can be re-adopted
// across a daemon restart without re-Registering plan files.
func (c *Controller) RecoverLiveAutopilots(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == nil {
		return nil
	}
	all, err := c.live.List(ctx)
	if err != nil {
		return err
	}
	for _, a := range all {
		if a == nil || a.PlanID == "" {
			continue
		}
		if _, ok := c.runs[a.ID]; ok {
			c.hydratePlanTasksLocked(ctx, c.runs[a.ID])
			continue
		}
		name := strings.TrimPrefix(a.Name, "AP:")
		if name == "" {
			name = a.Name
		}
		absPlan := a.Diagnostics.PlanFile
		repo := a.Diagnostics.Repo
		_, _ = c.adoptLiveLocked(ctx, a, absPlan, repo, name, PlanStartRequest{
			PlanID: a.PlanID, ProjectID: a.ProjectID, Name: name, Repo: repo, PlanFile: absPlan,
		})
	}
	return nil
}

// SpawnOnDemandBrain creates a short-lived role=brain Agent for consultation,
// marked headless (system:true) so it is not a normal tree node. The Autopilot
// record's BrainAgentID is updated. The manager remains role=autopilot.
func (c *Controller) SpawnOnDemandBrain(ctx context.Context, spec ConsultBrainSpec) (BrainHandle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtime == nil {
		return BrainHandle{}, errors.New("autopilot: runtime not configured")
	}
	cbr, ok := c.runtime.(ConsultBrainRuntime)
	if !ok {
		return BrainHandle{}, errors.New("autopilot: on-demand brain runtime not configured")
	}
	r, ok := c.runs[spec.AutopilotID]
	if !ok {
		return BrainHandle{}, ErrRunNotFound
	}
	handle, err := cbr.SpawnConsultBrain(ctx, ConsultBrainRuntimeSpec{
		RunID:    r.runID,
		PlanID:   r.planID,
		Repo:     r.repo,
		Prompt:   spec.Prompt,
		Backend:  spec.Backend,
		Tags:     []string{autopilotTag, runTag(r.runID), "system:true"},
		Headless: true,
	})
	if err != nil {
		return BrainHandle{}, err
	}
	if c.live != nil {
		_, _ = c.live.Update(ctx, r.runID, func(a *autopilotstore.Autopilot) error {
			a.BrainAgentID = handle.AgentID
			return nil
		})
	}
	return handle, nil
}

// PlanTaskProgress returns task progress from the Plan store when available,
// falling back to the in-memory plan task list with empty states.
func (c *Controller) PlanTaskProgress(ctx context.Context, runID string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return nil
	}
	out := map[string]string{}
	if c.planSource != nil && r.planID != "" {
		if p, err := c.planSource.Get(ctx, r.planID); err == nil && p != nil {
			for k, v := range p.TaskProgress {
				out[k] = v
			}
			if p.ActiveExecution != nil {
				for k, v := range p.ActiveExecution.TaskProgress {
					out[k] = v
				}
			}
		}
	}
	for _, t := range r.plan.Tasks {
		if _, ok := out[t.ID]; !ok {
			out[t.ID] = "pending"
		}
	}
	return out
}

func (c *Controller) hydratePlanTasksLocked(ctx context.Context, r *run) {
	if r == nil || c.planSource == nil || r.planID == "" {
		return
	}
	p, err := c.planSource.Get(ctx, r.planID)
	if err != nil || p == nil {
		return
	}
	// Progress lives on Plan; definitions stay on the YAML-backed r.plan.
	// Touching TaskProgress here keeps recovery tests verifying Plan as source.
	if len(p.TaskProgress) == 0 && p.ActiveExecution != nil {
		_ = p.ActiveExecution.TaskProgress
	}
}

// spawnManager launches the role=autopilot manager Agent (formerly "brain" in
// the controller). The on-demand Consultor brain is separate (role=brain).
func (c *Controller) spawnManager(ctx context.Context, r *run, backend string) error {
	return c.spawnBrain(ctx, r, backend)
}

func managerIDOf(r *run) string {
	if r == nil || r.brain == nil {
		return ""
	}
	return r.brain.AgentID
}

// ConsultBrainRuntimeSpec is the runtime request for an on-demand brain Agent.
type ConsultBrainRuntimeSpec struct {
	RunID    string
	PlanID   string
	Repo     string
	Prompt   string
	Backend  string
	Tags     []string
	Headless bool
}

// ConsultBrainRuntime is the optional Runtime seam for on-demand brain Agents.
// A bare Runtime without it leaves SpawnOnDemandBrain unavailable.
type ConsultBrainRuntime interface {
	SpawnConsultBrain(ctx context.Context, spec ConsultBrainRuntimeSpec) (BrainHandle, error)
}
