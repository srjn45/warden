package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/capacity"
)

// bulkRecoveryFallbackWindow is the scheduling fallback used when a usage-API
// exhausted-bucket observation carries no provider reset time. It mirrors the
// literal fallback the coordinator itself already uses when no reset is
// parseable (see waitLocked/retry in backend_recovery.go), so usage-driven and
// pane-driven hard limits schedule consistently.
const bulkRecoveryFallbackWindow = 30 * time.Minute

// startBulkRecovery is the coordinated-bulk-recovery entry point (plan-69eb481d
// Phase 6). For every agent the usage-API bucket-impact reconciliation
// (calculateBucketImpact) identified as affected, it invokes the existing
// backend recovery coordinator's confirmed hard-limit entry point — OnHardLimit
// — instead of issuing any direct hot-swap. OnHardLimit alone owns candidate
// selection against a fresh capacity snapshot, cooldown/disabled-model/role-tier
// exclusion, per-agent handoff, recovery phases, stabilization, reset
// scheduling, and manual-override behavior; this wiring adds nothing to that
// contract beyond driving it from usage evidence.
//
// Fan-out across the affected set is deliberately a plain, deterministic loop:
// OnHardLimit itself only claims/advances a recovery generation synchronously
// (a fast store write) before handing the actual candidate-selection/launch
// pass to the coordinator's own bounded dispatch (BackendRecoveryCoordinator.
// WithMaxParallelAdvance / advanceGated). That coordinator-level semaphore is
// what actually bounds concurrent candidate selection — the real protection a
// shared-bucket loss needs so every affected agent cannot stampede onto the
// same limited alternative at once; looping here does not reintroduce
// unbounded fan-out.
func (s *Server) startBulkRecovery(ctx context.Context, affected []capacity.AffectedAgent) {
	if s == nil || s.recovery == nil || s.store == nil || len(affected) == 0 {
		return
	}
	for _, aa := range affected {
		s.startBulkRecoveryForAgent(ctx, aa)
	}
}

// startBulkRecoveryForAgent loads the live agent and hands it to
// BackendRecoveryCoordinator.OnHardLimit. A missing agent (raced delete/archive
// between reconciliation's List() snapshot and this call) is a silent skip —
// the next reconciliation pass re-evaluates eligibility from scratch.
func (s *Server) startBulkRecoveryForAgent(ctx context.Context, aa capacity.AffectedAgent) {
	sess, err := s.store.Get(ctx, aa.AgentID)
	if err != nil || sess == nil {
		return
	}
	now := time.Now().UTC()
	fallbackAt := now.Add(bulkRecoveryFallbackWindow)
	if aa.ResetsAt != nil && aa.ResetsAt.After(now) {
		fallbackAt = *aa.ResetsAt
	}
	owned := s.recovery.OnHardLimit(sess, fallbackAt)
	slog.Info("daemon: usage-driven bulk recovery advanced",
		"agent_id", aa.AgentID,
		"provider", aa.Provider,
		"bucket_key", aa.BucketKey,
		"recovery_generation", aa.RecoveryGeneration,
		"source", aa.Source,
		"owned", owned,
	)
}
