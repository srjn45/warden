package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestCalculateBucketImpactRestartAndDualSource(t *testing.T) {
	dir := t.TempDir()
	fs, err := agentstore.New(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = fs.Close() })
	ctx := context.Background()
	fp := "sha256:acct"
	require.NoError(t, fs.Insert(ctx, &agentstore.Agent{
		ID: "agent-1", Name: "test-agent", Status: store.StatusWorking, AiCli: "claude", Model: "sonnet",
		QuotaBinding: &capacity.QuotaBinding{
			Domain: capacity.CapacityDomain{
				Provider: "claude", AiCli: "claude", AccountFingerprint: fp, Route: "sonnet",
				BucketKeys: []string{"weekly"},
			},
			MandatoryBuckets: []string{"weekly"},
		},
		TmuxSession: "agent-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))
	require.NoError(t, fs.Insert(ctx, &agentstore.Agent{
		ID: "agent-2", Name: "n-agent-2", Status: store.StatusIdle, AiCli: "claude", Model: "opus",
		QuotaBinding: &capacity.QuotaBinding{
			Domain: capacity.CapacityDomain{
				Provider: "claude", AiCli: "claude", AccountFingerprint: fp, Route: "opus",
				BucketKeys: []string{"weekly"},
			},
			MandatoryBuckets: []string{"weekly"},
		},
		TmuxSession: "agent-2", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))

	snapStore, err := backendusage.NewSnapshotStore(dir)
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = snapStore.Record(backendusage.UsageSnapshot{
		Domain:        backendusage.CapacityDomain{Provider: "claude", ProfileFingerprint: fp},
		ObservedAt:    now,
		RecordedAt:    now,
		SourceStatus:  backendusage.StatusOK,
		Authoritative: true,
		Freshness:     backendusage.FreshnessFresh,
		Buckets:       []backendusage.CapacityBucket{{Key: "weekly", State: backendusage.BucketExhausted}},
	})
	require.NoError(t, err)

	fences, err := capacity.NewDurableFenceStore(dir)
	require.NoError(t, err)
	usage := backendusage.NewService(nil)
	usage.SetSnapshotStore(snapStore)

	s := NewServer(fs, nil, nil, 0, false, nil, nil, nil)
	s.SetUsageService(usage)
	s.SetUsageReconciliation(true, time.Minute, 15*time.Minute)
	s.SetImpactFences(fences)

	first, err := s.calculateBucketImpact(ctx)
	require.NoError(t, err)
	require.Len(t, first.ExhaustedBuckets, 1)
	require.ElementsMatch(t, []string{"agent-1", "agent-2"}, impactIDs(first))

	// Pane evidence with a new revision must not re-select the same generation.
	paneObs := []capacity.BucketObservation{{
		Revision: 99, Provider: "claude", AccountFingerprint: fp, BucketKey: "weekly",
		State: capacity.ImpactBucketExhausted, Freshness: capacity.ImpactFresh, Authoritative: true, Source: capacity.SourcePane,
	}}
	agents, err := fs.List(ctx)
	require.NoError(t, err)
	views := make([]capacity.AgentView, 0, len(agents))
	for _, a := range agents {
		views = append(views, agentViewForImpact(a))
	}
	pane, err := capacity.ReconcileImpact(views, paneObs, fences)
	require.NoError(t, err)
	require.Empty(t, pane.AffectedAgents)

	// Restart: new server + fence handle on the same durable dir.
	restartedFences, err := capacity.NewDurableFenceStore(dir)
	require.NoError(t, err)
	restarted := NewServer(fs, nil, nil, 0, false, nil, nil, nil)
	restarted.SetUsageService(usage)
	restarted.SetUsageReconciliation(true, time.Minute, 15*time.Minute)
	restarted.SetImpactFences(restartedFences)
	again, err := restarted.calculateBucketImpact(ctx)
	require.NoError(t, err)
	require.Empty(t, again.AffectedAgents)
	require.Equal(t, capacity.SkipAlreadyReconciled, skipReason(again, "agent-1"))
}

func impactIDs(r capacity.ImpactResult) []string {
	out := make([]string, 0, len(r.AffectedAgents))
	for _, a := range r.AffectedAgents {
		out = append(out, a.AgentID)
	}
	return out
}

func skipReason(r capacity.ImpactResult, id string) string {
	for _, s := range r.SkippedAgents {
		if s.AgentID == id {
			return s.Reason
		}
	}
	return ""
}
