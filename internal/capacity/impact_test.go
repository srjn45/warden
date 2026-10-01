package capacity

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func binding(provider, fingerprint, route string, buckets ...string) *QuotaBinding {
	keys := append([]string(nil), buckets...)
	return &QuotaBinding{
		Domain: CapacityDomain{
			Provider:           provider,
			AiCli:              provider,
			AccountFingerprint: fingerprint,
			Route:              route,
			BucketKeys:         keys,
		},
		MandatoryBuckets: keys,
	}
}

func live(id, status string, b *QuotaBinding) AgentView {
	return AgentView{ID: id, Status: status, Binding: b}
}

func exhausted(rev uint64, provider, fingerprint, route, bucket, source string) BucketObservation {
	return BucketObservation{
		Revision:           rev,
		Provider:           provider,
		AccountFingerprint: fingerprint,
		Route:              route,
		BucketKey:          bucket,
		State:              ImpactBucketExhausted,
		Freshness:          ImpactFresh,
		Authoritative:      true,
		Source:             source,
	}
}

func TestReconcileImpactTwoAccountsSameAICLI(t *testing.T) {
	dir := t.TempDir()
	fences, err := NewDurableFenceStore(dir)
	require.NoError(t, err)

	acctA := "sha256:account-a"
	acctB := "sha256:account-b"
	agents := []AgentView{
		live("agent-a1", "working", binding("claude", acctA, "sonnet", "weekly")),
		live("agent-a2", "idle", binding("claude", acctA, "opus", "weekly")),
		live("agent-b1", "working", binding("claude", acctB, "sonnet", "weekly")),
	}
	obs := []BucketObservation{exhausted(1, "claude", acctA, "", "weekly", SourceUsage)}

	got, err := ReconcileImpact(agents, obs, fences)
	require.NoError(t, err)
	require.Len(t, got.ExhaustedBuckets, 1)
	require.Equal(t, []string{"agent-a1", "agent-a2"}, affectedIDs(got))
	require.NotContains(t, affectedIDs(got), "agent-b1")
}

func TestReconcileImpactSharedVersusModelSpecificBuckets(t *testing.T) {
	fences, err := NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)
	fp := "sha256:shared"
	agents := []AgentView{
		live("agent-sonnet", "working", binding("claude", fp, "sonnet", "weekly", "sonnet-pool")),
		live("agent-opus", "working", binding("claude", fp, "opus", "weekly", "opus-pool")),
	}

	shared, err := ReconcileImpact(agents, []BucketObservation{exhausted(2, "claude", fp, "", "weekly", SourceUsage)}, fences)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-opus", "agent-sonnet"}, affectedIDs(shared))

	model, err := ReconcileImpact(agents, []BucketObservation{exhausted(3, "claude", fp, "sonnet", "sonnet-pool", SourceUsage)}, fences)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-sonnet"}, affectedIDs(model))
	require.NotContains(t, affectedIDs(model), "agent-opus")
}

func TestReconcileImpactMultipleMandatoryBuckets(t *testing.T) {
	fences, err := NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)
	fp := "sha256:multi"
	agents := []AgentView{
		live("agent-both", "working", binding("antigravity", fp, "gemini", "weekly", "gemini")),
		live("agent-weekly-only", "working", binding("antigravity", fp, "gpt", "weekly")),
	}
	obs := []BucketObservation{
		exhausted(4, "antigravity", fp, "", "weekly", SourceUsage),
		exhausted(4, "antigravity", fp, "gemini", "gemini", SourceUsage),
	}
	got, err := ReconcileImpact(agents, obs, fences)
	require.NoError(t, err)
	require.Len(t, got.ExhaustedBuckets, 2)
	require.ElementsMatch(t, []string{"agent-both", "agent-weekly-only"}, uniqueIDs(affectedIDs(got)))
	var bothBuckets []string
	for _, a := range got.AffectedAgents {
		if a.AgentID == "agent-both" {
			bothBuckets = append(bothBuckets, a.BucketKey)
		}
	}
	require.ElementsMatch(t, []string{"weekly", "gemini"}, bothBuckets)
}

func TestReconcileImpactSkipsIneligibleAndUnbound(t *testing.T) {
	fences, err := NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)
	fp := "sha256:skip"
	agents := []AgentView{
		live("agent-live", "working", binding("claude", fp, "sonnet", "weekly")),
		{ID: "agent-done", Status: "done", Binding: binding("claude", fp, "sonnet", "weekly")},
		{ID: "agent-archived", Status: "done", Archived: true, Binding: binding("claude", fp, "sonnet", "weekly")},
		{ID: "agent-terminal", Status: "working", Terminal: true, Binding: binding("claude", fp, "sonnet", "weekly")},
		{ID: "agent-unbound", Status: "working"},
		{ID: "agent-recovering", Status: "rate_limited", Binding: binding("claude", fp, "sonnet", "weekly"), Recovering: true, RecoveryGeneration: 3},
		{ID: "agent-superseded", Status: "working", Binding: binding("claude", fp, "sonnet", "weekly"), Superseded: true, RecoveryGeneration: 2},
	}
	got, err := ReconcileImpact(agents, []BucketObservation{exhausted(5, "claude", fp, "", "weekly", SourceUsage)}, fences)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-live"}, affectedIDs(got))
	reasons := skipReasons(got)
	require.Equal(t, SkipDone, reasons["agent-done"])
	require.Equal(t, SkipArchived, reasons["agent-archived"])
	require.Equal(t, SkipTerminal, reasons["agent-terminal"])
	require.Equal(t, SkipUnboundLegacy, reasons["agent-unbound"])
	require.Equal(t, SkipRecovering, reasons["agent-recovering"])
	require.Equal(t, SkipSuperseded, reasons["agent-superseded"])
}

func TestReconcileImpactRepeatedObservationsIdempotent(t *testing.T) {
	fences, err := NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)
	fp := "sha256:idem"
	agents := []AgentView{live("agent-1", "working", binding("claude", fp, "sonnet", "weekly"))}
	obs := []BucketObservation{exhausted(6, "claude", fp, "", "weekly", SourceUsage)}

	first, err := ReconcileImpact(agents, obs, fences)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-1"}, affectedIDs(first))

	second, err := ReconcileImpact(agents, obs, fences)
	require.NoError(t, err)
	require.Empty(t, second.AffectedAgents)
	require.Equal(t, SkipAlreadyReconciled, skipReasons(second)["agent-1"])
	require.Len(t, second.ExhaustedBuckets, 1, "exhausted buckets remain visible on repeat")
}

func TestReconcileImpactSimultaneousAPIAndPaneTriggers(t *testing.T) {
	fences, err := NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)
	fp := "sha256:dual"
	agents := []AgentView{
		live("agent-1", "working", binding("claude", fp, "sonnet", "weekly")),
		live("agent-2", "idle", binding("claude", fp, "opus", "weekly")),
	}

	var (
		mu      sync.Mutex
		results []ImpactResult
		wg      sync.WaitGroup
	)
	run := func(rev uint64, source string) {
		defer wg.Done()
		got, err := ReconcileImpact(agents, []BucketObservation{exhausted(rev, "claude", fp, "", "weekly", source)}, fences)
		require.NoError(t, err)
		mu.Lock()
		results = append(results, got)
		mu.Unlock()
	}
	wg.Add(2)
	go run(10, SourceUsage)
	go run(11, SourcePane)
	wg.Wait()

	var affected []string
	for _, r := range results {
		affected = append(affected, affectedIDs(r)...)
	}
	require.ElementsMatch(t, []string{"agent-1", "agent-2"}, uniqueIDs(affected),
		"simultaneous API/pane evidence must select each agent once across both triggers")
}

func TestReconcileImpactDaemonRestartHalfway(t *testing.T) {
	dir := t.TempDir()
	fences, err := NewDurableFenceStore(dir)
	require.NoError(t, err)
	fp := "sha256:restart"
	agents := []AgentView{
		live("agent-1", "working", binding("claude", fp, "sonnet", "weekly")),
		live("agent-2", "working", binding("claude", fp, "opus", "weekly")),
		live("agent-3", "idle", binding("claude", fp, "haiku", "weekly")),
	}
	obs := []BucketObservation{exhausted(20, "claude", fp, "", "weekly", SourceUsage)}

	// Simulate crash after the first agent is claimed: write one fence, then
	// "restart" with a fresh store handle on the same durable path.
	claimed, err := fences.Claim(FenceRecord{
		SnapshotRevision:   20,
		DomainKey:          DomainKey("claude", fp, ""),
		BucketKey:          "weekly",
		AgentID:            "agent-1",
		RecoveryGeneration: 0,
		Source:             SourceUsage,
	})
	require.NoError(t, err)
	require.True(t, claimed)

	restarted, err := NewDurableFenceStore(dir)
	require.NoError(t, err)
	got, err := ReconcileImpact(agents, obs, restarted)
	require.NoError(t, err)
	require.Equal(t, []string{"agent-2", "agent-3"}, affectedIDs(got))
	require.Equal(t, SkipAlreadyReconciled, skipReasons(got)["agent-1"])

	// A second restart must not re-select anyone.
	again, err := NewDurableFenceStore(dir)
	require.NoError(t, err)
	second, err := ReconcileImpact(agents, obs, again)
	require.NoError(t, err)
	require.Empty(t, second.AffectedAgents)
}

func TestReconcileImpactStaleUnknownNotExhaustion(t *testing.T) {
	fences, err := NewDurableFenceStore(t.TempDir())
	require.NoError(t, err)
	fp := "sha256:stale"
	agents := []AgentView{live("agent-1", "working", binding("claude", fp, "sonnet", "weekly"))}
	obs := []BucketObservation{
		{Revision: 1, Provider: "claude", AccountFingerprint: fp, BucketKey: "weekly", State: ImpactBucketExhausted, Freshness: ImpactStale, Authoritative: true, Source: SourceUsage},
		{Revision: 2, Provider: "claude", AccountFingerprint: fp, BucketKey: "weekly", State: ImpactBucketUnknown, Freshness: ImpactFresh, Authoritative: true, Source: SourceUsage},
		{Revision: 3, Provider: "claude", AccountFingerprint: fp, BucketKey: "weekly", State: ImpactBucketExhausted, Freshness: ImpactFresh, Authoritative: false, Source: SourceUsage},
	}
	got, err := ReconcileImpact(agents, obs, fences)
	require.NoError(t, err)
	require.Empty(t, got.ExhaustedBuckets)
	require.Empty(t, got.AffectedAgents)
	require.Len(t, got.StaleOrUnknown, 3)
	require.Equal(t, "stale", got.StaleOrUnknown[0].Reason)
	require.Equal(t, "unknown_bucket", got.StaleOrUnknown[1].Reason)
	require.Equal(t, "not_authoritative", got.StaleOrUnknown[2].Reason)
}

func affectedIDs(r ImpactResult) []string {
	out := make([]string, 0, len(r.AffectedAgents))
	for _, a := range r.AffectedAgents {
		out = append(out, a.AgentID)
	}
	return out
}

func uniqueIDs(ids []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func skipReasons(r ImpactResult) map[string]string {
	out := map[string]string{}
	for _, s := range r.SkippedAgents {
		out[s.AgentID] = s.Reason
	}
	return out
}

func TestDurableFenceStorePersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	a, err := NewDurableFenceStore(dir)
	require.NoError(t, err)
	ok, err := a.Claim(FenceRecord{SnapshotRevision: 1, DomainKey: "d", BucketKey: "b", AgentID: "a1", RecoveryGeneration: 0})
	require.NoError(t, err)
	require.True(t, ok)
	b, err := NewDurableFenceStore(dir)
	require.NoError(t, err)
	ok, err = b.Claim(FenceRecord{SnapshotRevision: 1, DomainKey: "d", BucketKey: "b", AgentID: "a1", RecoveryGeneration: 0})
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = b.Claim(FenceRecord{SnapshotRevision: 2, DomainKey: "d", BucketKey: "b", AgentID: "a1", RecoveryGeneration: 0})
	require.NoError(t, err)
	require.False(t, ok, "generation fence must span revisions for the same incident")
	ok, err = b.Claim(FenceRecord{SnapshotRevision: 3, DomainKey: "d", BucketKey: "b", AgentID: "a1", RecoveryGeneration: 1})
	require.NoError(t, err)
	require.True(t, ok, fmt.Sprintf("new recovery generation must be claimable"))
}
