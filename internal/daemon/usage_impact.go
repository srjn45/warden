package daemon

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
)

// SetImpactFences wires the durable snapshot-revision/domain/bucket/generation
// fence store used by bucket-impact reconciliation. Nil disables fencing (tests).
func (s *Server) SetImpactFences(fences capacity.FenceStore) {
	s.impactFences = fences
}

// LastBucketImpact returns the most recent structured impact result produced by
// the usage reconciliation loop. It never invokes recovery swaps.
func (s *Server) LastBucketImpact() capacity.ImpactResult {
	s.impactMu.Lock()
	defer s.impactMu.Unlock()
	return cloneImpact(s.lastBucketImpact)
}

// calculateBucketImpact maps the latest durable usage snapshots onto live agents.
// It intentionally stops at the structured impact result; reconcileBucketImpactAfterPoll
// is the sole caller that forwards AffectedAgents into startBulkRecovery.
func (s *Server) calculateBucketImpact(ctx context.Context) (capacity.ImpactResult, error) {
	empty := capacity.ImpactResult{
		ExhaustedBuckets: []capacity.ExhaustedBucket{},
		AffectedAgents:   []capacity.AffectedAgent{},
		SkippedAgents:    []capacity.SkippedAgent{},
		StaleOrUnknown:   []capacity.StaleOrUnknownInput{},
	}
	if s == nil || s.usage == nil || s.store == nil {
		return empty, nil
	}
	staleAfter := s.usageReconciliationStaleAfter
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	snaps, err := s.usage.LatestSnapshots(time.Now().UTC(), staleAfter)
	if err != nil {
		return empty, err
	}
	agents, err := s.store.List(ctx)
	if err != nil {
		return empty, err
	}
	views := make([]capacity.AgentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, agentViewForImpact(a))
	}
	result, err := capacity.ReconcileImpact(views, observationsFromSnapshots(snaps, capacity.SourceUsage), s.impactFences)
	if err != nil {
		return empty, err
	}
	s.impactMu.Lock()
	s.lastBucketImpact = cloneImpact(result)
	s.impactMu.Unlock()
	return result, nil
}

func agentViewForImpact(a *agentstore.Agent) capacity.AgentView {
	if a == nil {
		return capacity.AgentView{}
	}
	v := capacity.AgentView{
		ID:                 a.ID,
		Status:             string(a.Status),
		Binding:            a.QuotaBinding,
		Recovering:         a.BackendRecovery != nil,
		RecoveryGeneration: a.BackendRecoveryGeneration,
	}
	if a.BackendRecovery != nil {
		v.RecoveryGeneration = a.BackendRecovery.Generation
	}
	v.Superseded = agentManuallySuperseded(a)
	return v
}

// agentManuallySuperseded detects an operator stop/switch that cleared an active
// recovery. Only the most recent recovery-lifecycle event counts, so a later
// stabilized or restarted recovery clears the skip.
func agentManuallySuperseded(a *agentstore.Agent) bool {
	if a == nil || a.BackendRecovery != nil {
		return false
	}
	for i := len(a.Events) - 1; i >= 0; i-- {
		switch a.Events[i].Type {
		case "backend_recovery_superseded":
			detail := a.Events[i].Detail
			return strings.Contains(detail, "manual_switch") || strings.Contains(detail, "manual_stop")
		case "backend_recovery_started", "backend_recovery_stabilized":
			return false
		}
	}
	return false
}

func observationsFromSnapshots(snaps []backendusage.UsageSnapshot, source string) []capacity.BucketObservation {
	var out []capacity.BucketObservation
	for _, snap := range snaps {
		for _, b := range snap.Buckets {
			out = append(out, capacity.BucketObservation{
				Revision:           snap.Revision,
				Provider:           snap.Domain.Provider,
				AccountFingerprint: snap.Domain.ProfileFingerprint,
				Route:              snap.Domain.Route,
				BucketKey:          b.Key,
				State:              string(b.State),
				Freshness:          string(snap.Freshness),
				Authoritative:      snap.Authoritative,
				Source:             source,
				ResetsAt:           b.ResetsAt,
			})
		}
		if len(snap.Buckets) == 0 && (!snap.Authoritative || snap.Freshness != backendusage.FreshnessFresh) {
			out = append(out, capacity.BucketObservation{
				Revision:           snap.Revision,
				Provider:           snap.Domain.Provider,
				AccountFingerprint: snap.Domain.ProfileFingerprint,
				Route:              snap.Domain.Route,
				State:              capacity.ImpactBucketUnknown,
				Freshness:          string(snap.Freshness),
				Authoritative:      snap.Authoritative,
				Source:             source,
			})
		}
	}
	return out
}

func cloneImpact(in capacity.ImpactResult) capacity.ImpactResult {
	return capacity.ImpactResult{
		ExhaustedBuckets: append([]capacity.ExhaustedBucket(nil), in.ExhaustedBuckets...),
		AffectedAgents:   append([]capacity.AffectedAgent(nil), in.AffectedAgents...),
		SkippedAgents:    append([]capacity.SkippedAgent(nil), in.SkippedAgents...),
		StaleOrUnknown:   append([]capacity.StaleOrUnknownInput(nil), in.StaleOrUnknown...),
	}
}

// reconcileBucketImpactAfterPoll runs impact calculation after a usage
// observation, then (coordinated-bulk-recovery) advances every AffectedAgent
// through the existing backend recovery coordinator via startBulkRecovery.
// Impact-calculation failures are logged; they must not break the polling loop.
func (s *Server) reconcileBucketImpactAfterPoll(ctx context.Context) {
	if s == nil || s.impactFences == nil {
		return
	}
	result, err := s.calculateBucketImpact(ctx)
	if err != nil {
		slog.Warn("daemon: bucket impact reconciliation failed", "err", err)
		return
	}
	if len(result.ExhaustedBuckets) == 0 && len(result.AffectedAgents) == 0 {
		return
	}
	slog.Info("daemon: bucket impact calculated",
		"exhausted_buckets", len(result.ExhaustedBuckets),
		"affected_agents", len(result.AffectedAgents),
		"skipped_agents", len(result.SkippedAgents),
		"stale_or_unknown", len(result.StaleOrUnknown),
	)
	s.startBulkRecovery(ctx, result.AffectedAgents)
}
