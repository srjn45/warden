package autopilot

import (
	"context"
	"sort"
	"strings"
)

// BootReconcileRun describes one run's migration targets during daemon boot (WP12).
type BootReconcileRun struct {
	RunID         string
	SlotScope     string
	Repo          string
	PlanID        string
	ProjectID     string
	LegacyBrainID string
	ManagerSlotID string
}

// MigrationRuntime is the optional boot-time session reconciler (WP12). The
// daemon implements it to adopt live legacy managers, migrate guardian ids, and
// stamp worker back-refs. Idempotent: safe to call on every SetRuntime.
type MigrationRuntime interface {
	ReconcileSessions(ctx context.Context, runs []BootReconcileRun) error
}

func isLegacyBrainID(id string) bool {
	id = strings.TrimSpace(id)
	return strings.HasPrefix(id, "agent-") && id != ""
}

// reconcileRunsAtBootLocked only normalizes in-memory state. Runtime/store work
// is deliberately scheduled by SetRuntime after releasing c.mu.
func (c *Controller) reconcileRunsAtBootLocked(ctx context.Context) {
	if c.store != nil && c.store.IsSchema3() {
		return
	}
	if c.runtime == nil {
		return
	}
	for _, r := range c.runs {
		c.normalizeSlotIDsLocked(r)
	}
}

func (c *Controller) grandfatherIntegrationBranchLocked(r *run) {
	if strings.TrimSpace(r.integrationBranch) != "" {
		return
	}
	if c.store == nil {
		return
	}
	rec, err := c.store.Get(r.runID)
	if err != nil {
		return
	}
	if stored := strings.TrimSpace(rec.IntegrationBranch); stored != "" {
		r.integrationBranch = stored
		return
	}
	// Pre-WP9 durable records without a stored branch used the shared global target.
	if rec.SlotScope == "" || isLegacyBrainID(rec.BrainID) {
		r.integrationBranch = DefaultIntegrationBranch
	}
}

func (c *Controller) normalizeSlotIDsLocked(r *run) {
	if r.slotScope == "" {
		return
	}
	wantManager := ManagerSlotID(r.slotScope)
	if r.brain != nil && r.brain.AgentID != "" && r.brain.AgentID != wantManager {
		r.brain = nil
	}
}

func (c *Controller) bootReconcileSpecsLocked() []BootReconcileRun {
	out := make([]BootReconcileRun, 0, len(c.runs))
	for _, r := range c.runs {
		if r.slotScope == "" {
			continue
		}
		spec := BootReconcileRun{
			RunID:         r.runID,
			SlotScope:     r.slotScope,
			Repo:          r.repo,
			PlanID:        r.planID,
			ProjectID:     r.projectID,
			ManagerSlotID: ManagerSlotID(r.slotScope),
		}
		if r.brain != nil {
			spec.LegacyBrainID = strings.TrimSpace(r.brain.AgentID)
		}
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}
