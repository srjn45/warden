package autopilot

import (
	"context"
	"log/slog"
	"sort"
	"strings"
)

// BootReconcileRun describes one run's migration targets during daemon boot (WP12).
type BootReconcileRun struct {
	RunID            string
	SlotScope        string
	Repo             string
	LegacyBrainID    string
	LegacyGuardianID string
	ManagerSlotID    string
	GuardianSlotID   string
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

func isLegacyGuardianID(id, slotGuardianID string) bool {
	id = strings.TrimSpace(id)
	if id == "" || id == slotGuardianID {
		return false
	}
	return strings.HasPrefix(id, "guardian-")
}

// reconcileRunsAtBootLocked normalizes in-memory run state and asks the runtime
// to migrate legacy sessions before guardian reconciliation and brain spawn.
// Caller holds c.mu.
func (c *Controller) reconcileRunsAtBootLocked(ctx context.Context) {
	if c.runtime == nil {
		return
	}
	for _, r := range c.runs {
		c.grandfatherIntegrationBranchLocked(r)
		c.normalizeSlotIDsLocked(r)
	}
	if mr, ok := c.runtime.(MigrationRuntime); ok {
		specs := c.bootReconcileSpecsLocked()
		if err := mr.ReconcileSessions(ctx, specs); err != nil {
			slog.Warn("autopilot: session boot reconciliation failed", "err", err)
		}
	}
	for _, r := range c.runs {
		if r.slotScope == "" {
			continue
		}
		wantGuardian := GuardianSlotID(r.slotScope)
		if r.guardianID != "" && r.guardianID != wantGuardian {
			r.guardianID = ""
		}
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
	if rec.SlotScope == "" || isLegacyBrainID(rec.BrainID) || isLegacyGuardianID(rec.GuardianID, "") {
		r.integrationBranch = DefaultIntegrationBranch
	}
}

func (c *Controller) normalizeSlotIDsLocked(r *run) {
	if r.slotScope == "" {
		return
	}
	wantManager := ManagerSlotID(r.slotScope)
	wantGuardian := GuardianSlotID(r.slotScope)
	if r.brain != nil && r.brain.AgentID != "" && r.brain.AgentID != wantManager {
		r.brain = nil
	}
	if r.guardianID != "" && r.guardianID != wantGuardian {
		r.guardianID = ""
	}
}

func (c *Controller) bootReconcileSpecsLocked() []BootReconcileRun {
	out := make([]BootReconcileRun, 0, len(c.runs))
	for _, r := range c.runs {
		if r.slotScope == "" {
			continue
		}
		spec := BootReconcileRun{
			RunID:          r.runID,
			SlotScope:      r.slotScope,
			Repo:           r.repo,
			ManagerSlotID:  ManagerSlotID(r.slotScope),
			GuardianSlotID: GuardianSlotID(r.slotScope),
		}
		if c.store != nil {
			if rec, err := c.store.Get(r.runID); err == nil {
				spec.LegacyBrainID = strings.TrimSpace(rec.BrainID)
				spec.LegacyGuardianID = strings.TrimSpace(rec.GuardianID)
			}
		}
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}
