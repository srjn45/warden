package daemon

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
)

const usageRecoverOutcomeSettle = 2 * time.Second

// runUsageRecover is the operator-triggered one-shot reconciliation entry point
// (plan-69eb481d Phase 8). It always fetches fresh supported usage snapshots,
// calculates bucket impact (with optional ai_cli / project filters), and —
// unless dry-run — invokes the same startBulkRecovery → OnHardLimit path as the
// background usage reconciliation loop.
func (s *Server) runUsageRecover(ctx context.Context, req oapi.UsageRecoverRequest) (oapi.UsageRecoverResponse, error) {
	empty := oapi.UsageRecoverResponse{
		DryRun:    req.DryRun,
		Snapshots: []backendusage.UsageSnapshot{},
		Impact: capacity.ImpactResult{
			ExhaustedBuckets: []capacity.ExhaustedBucket{},
			AffectedAgents:   []capacity.AffectedAgent{},
			SkippedAgents:    []capacity.SkippedAgent{},
			StaleOrUnknown:   []capacity.StaleOrUnknownInput{},
		},
		Outcomes: []oapi.UsageRecoverAgentOutcome{},
	}
	if s == nil || s.usage == nil {
		return empty, errStatus(http.StatusServiceUnavailable, "backend usage unavailable")
	}
	if !req.DryRun && s.recovery == nil {
		return empty, errStatus(http.StatusServiceUnavailable, "backend recovery unavailable")
	}
	if req.MaxParallelSwaps < 0 {
		return empty, errStatus(http.StatusBadRequest, "max_parallel_swaps must be >= 1 when set")
	}

	// Always refresh — never treat a cached/stale observation as forced-limit
	// evidence for an operator recover.
	providerUsage, err := s.usage.Snapshot(ctx, true)
	if err != nil {
		return empty, errStatus(http.StatusServiceUnavailable, "backend usage unavailable")
	}
	s.syncUsageSnapshot(providerUsage)

	staleAfter := s.usageReconciliationStaleAfter
	if staleAfter <= 0 {
		staleAfter = 15 * time.Minute
	}
	snaps, err := s.usage.LatestSnapshots(time.Now().UTC(), staleAfter)
	if err != nil {
		return empty, errStatus(http.StatusServiceUnavailable, "backend usage unavailable")
	}
	aiCli := strings.TrimSpace(req.AiCli)
	project := normalizeProjectFilter(req.Project)
	snaps = filterSnapshots(snaps, aiCli)

	agents, err := s.store.List(ctx)
	if err != nil {
		return empty, err
	}
	views := make([]capacity.AgentView, 0, len(agents))
	agentsByID := make(map[string]*agentstore.Agent, len(agents))
	for _, a := range agents {
		if a == nil {
			continue
		}
		if !agentMatchesRecoverFilter(a, aiCli, project) {
			continue
		}
		views = append(views, agentViewForImpact(a))
		agentsByID[a.ID] = a
	}

	fences := s.impactFences
	if req.DryRun {
		if durable, ok := s.impactFences.(*capacity.DurableFenceStore); ok {
			fences = capacity.ProbeFenceStore{Inner: durable}
		} else {
			// Unknown fence implementation: dry-run must not mutate, so skip fencing.
			fences = nil
		}
	}
	impact, err := capacity.ReconcileImpact(views, observationsFromSnapshots(snaps, capacity.SourceManual), fences)
	if err != nil {
		return empty, err
	}
	s.impactMu.Lock()
	s.lastBucketImpact = cloneImpact(impact)
	s.impactMu.Unlock()

	effectiveParallel := 0
	if s.recovery != nil {
		effectiveParallel = s.recovery.MaxParallelAdvance()
	}
	if req.MaxParallelSwaps > 0 {
		effectiveParallel = req.MaxParallelSwaps
	}

	outcomes := s.previewRecoverOutcomes(ctx, impact.AffectedAgents, agentsByID, req.DryRun)

	if !req.DryRun {
		run := func() {
			s.startBulkRecovery(ctx, impact.AffectedAgents)
			outcomes = s.settleRecoverOutcomes(ctx, outcomes, usageRecoverOutcomeSettle)
		}
		if req.MaxParallelSwaps > 0 && s.recovery != nil {
			s.recovery.runWithMaxParallel(req.MaxParallelSwaps, run)
		} else {
			run()
		}
	}

	if outcomes == nil {
		outcomes = []oapi.UsageRecoverAgentOutcome{}
	}
	if snaps == nil {
		snaps = []backendusage.UsageSnapshot{}
	}
	return oapi.UsageRecoverResponse{
		DryRun:           req.DryRun,
		ProviderUsage:    providerUsage,
		Snapshots:        snaps,
		Impact:           impact,
		Outcomes:         outcomes,
		MaxParallelSwaps: effectiveParallel,
	}, nil
}

func (s *Server) previewRecoverOutcomes(ctx context.Context, affected []capacity.AffectedAgent, agentsByID map[string]*agentstore.Agent, dryRun bool) []oapi.UsageRecoverAgentOutcome {
	out := make([]oapi.UsageRecoverAgentOutcome, 0, len(affected))
	for _, aa := range affected {
		oc := oapi.UsageRecoverAgentOutcome{
			AgentId:            aa.AgentID,
			BucketKey:          aa.BucketKey,
			RecoveryGeneration: int64(aa.RecoveryGeneration),
		}
		sess := agentsByID[aa.AgentID]
		if sess == nil && s.store != nil {
			sess, _ = s.store.Get(ctx, aa.AgentID)
		}
		var selected *store.BackendCandidate
		var ranked []store.BackendCandidate
		if s.recovery != nil && sess != nil {
			selected, ranked = s.recovery.PreviewCandidates(ctx, sess)
		}
		oc.Candidates = ranked
		if selected != nil {
			oc.Selected = *selected
		}
		if dryRun {
			if selected == nil {
				oc.Outcome = oapi.WouldWait
				oc.Phase = recoveryWaiting
				oc.Reason = "no eligible replacement with known usable capacity"
			} else {
				oc.Outcome = oapi.WouldStart
				oc.Phase = recoverySwitching
			}
		} else {
			// Pre-settle defaults; settleRecoverOutcomes refines from live state.
			if selected == nil {
				oc.Outcome = oapi.WaitingForCapacity
				oc.Phase = recoveryWaiting
			} else {
				oc.Outcome = oapi.Started
				oc.Phase = recoveryRefreshing
			}
		}
		out = append(out, oc)
	}
	return out
}

func (s *Server) settleRecoverOutcomes(ctx context.Context, outcomes []oapi.UsageRecoverAgentOutcome, wait time.Duration) []oapi.UsageRecoverAgentOutcome {
	if s == nil || s.store == nil || len(outcomes) == 0 {
		return outcomes
	}
	deadline := time.Now().Add(wait)
	for {
		allSettled := true
		for i := range outcomes {
			sess, err := s.store.Get(ctx, outcomes[i].AgentId)
			if err != nil || sess == nil || sess.BackendRecovery == nil {
				allSettled = false
				continue
			}
			br := sess.BackendRecovery
			outcomes[i].Phase = br.Phase
			outcomes[i].RecoveryGeneration = int64(br.Generation)
			if br.Current != nil {
				outcomes[i].Selected = *br.Current
			}
			switch br.Phase {
			case recoveryWaiting:
				outcomes[i].Outcome = oapi.WaitingForCapacity
			case recoverySwitching, recoveryStabilizing, recoveryRefreshing:
				outcomes[i].Outcome = oapi.Started
				if br.Phase == recoveryRefreshing {
					allSettled = false
				}
			default:
				outcomes[i].Outcome = oapi.Started
			}
		}
		if allSettled || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return outcomes
		case <-time.After(25 * time.Millisecond):
		}
	}
	return outcomes
}

func normalizeProjectFilter(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return ""
	}
	if abs, err := filepath.Abs(project); err == nil {
		project = abs
	}
	return filepath.Clean(project)
}

func agentMatchesRecoverFilter(a *agentstore.Agent, aiCli, project string) bool {
	if a == nil {
		return false
	}
	if aiCli != "" && !strings.EqualFold(strings.TrimSpace(a.AiCli), aiCli) {
		return false
	}
	if project == "" {
		return true
	}
	if filepath.Clean(a.ProjectID) == project {
		return true
	}
	if filepath.Clean(a.Repo) == project {
		return true
	}
	return false
}

func filterSnapshots(snaps []backendusage.UsageSnapshot, aiCli string) []backendusage.UsageSnapshot {
	if aiCli == "" || len(snaps) == 0 {
		return snaps
	}
	out := make([]backendusage.UsageSnapshot, 0, len(snaps))
	for _, snap := range snaps {
		if strings.EqualFold(snap.Domain.Provider, aiCli) {
			out = append(out, snap)
		}
	}
	return out
}
