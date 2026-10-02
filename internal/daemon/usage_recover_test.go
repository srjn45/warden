package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestUsageRecoverDryRunFetchesFreshAndDoesNotStartRecovery(t *testing.T) {
	fp := "sha256:shared-account"
	s, _, st, life, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	boundAgent(t, st, "agent-other", "claude", "claude-model", "sha256:other", "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err := s.runUsageRecover(context.Background(), oapi.UsageRecoverRequest{DryRun: true, AiCli: "codex"})
	require.NoError(t, err)
	require.True(t, result.DryRun)
	require.NotEmpty(t, result.Snapshots)
	require.ElementsMatch(t, []string{"agent-1"}, impactIDs(result.Impact))
	require.Len(t, result.Outcomes, 1)
	require.Equal(t, oapi.WouldStart, result.Outcomes[0].Outcome)
	require.Equal(t, "agent-1", result.Outcomes[0].AgentId)

	// Dry-run must not claim fences or start recovery.
	agent, err := st.Get(context.Background(), "agent-1")
	require.NoError(t, err)
	require.Nil(t, agent.BackendRecovery)
	require.Empty(t, life.swaps)
	other, err := st.Get(context.Background(), "agent-other")
	require.NoError(t, err)
	require.Nil(t, other.BackendRecovery)

	// A subsequent applying recover must still be able to claim the same fence.
	apply, err := s.runUsageRecover(context.Background(), oapi.UsageRecoverRequest{AiCli: "codex"})
	require.NoError(t, err)
	require.False(t, apply.DryRun)
	require.ElementsMatch(t, []string{"agent-1"}, impactIDs(apply.Impact))
	require.Eventually(t, func() bool {
		a, err := st.Get(context.Background(), "agent-1")
		return err == nil && a.BackendRecovery != nil
	}, time.Second, 10*time.Millisecond)
}

func TestUsageRecoverApplyStartsCoordinatorAndSkipsUnrelated(t *testing.T) {
	fp := "sha256:shared-account"
	s, _, st, _, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	boundAgent(t, st, "agent-2", "codex", "codex-model", fp, "weekly")
	require.NoError(t, st.Insert(context.Background(), &agentstore.Agent{
		ID: "unrelated", AiCli: "claude", Model: "claude-model", Role: "general", Status: store.StatusWorking,
		ProjectID: "/other/project",
		QuotaBinding: &capacity.QuotaBinding{
			Domain: capacity.CapacityDomain{
				Provider: "claude", AiCli: "claude", AccountFingerprint: "sha256:other", Route: "claude-model", BucketKeys: []string{"weekly"},
			},
			MandatoryBuckets: []string{"weekly"},
		},
	}))
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err := s.runUsageRecover(context.Background(), oapi.UsageRecoverRequest{
		AiCli: "codex", Project: "/does/not/match",
	})
	require.NoError(t, err)
	require.Empty(t, result.Impact.AffectedAgents)
	require.Empty(t, result.Outcomes)

	// Stamp project on the bound agents and re-run.
	for _, id := range []string{"agent-1", "agent-2"} {
		require.NoError(t, st.Update(context.Background(), id, func(a *agentstore.Agent) error {
			a.ProjectID = "/tmp/warden-project"
			a.Repo = "/tmp/warden-project"
			return nil
		}))
	}
	// Fresh snapshot revision so the prior empty reconcile did not fence anything useful;
	// record another exhausted observation.
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	result, err = s.runUsageRecover(context.Background(), oapi.UsageRecoverRequest{
		AiCli: "codex", Project: "/tmp/warden-project",
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"agent-1", "agent-2"}, impactIDs(result.Impact))
	require.Len(t, result.Outcomes, 2)
	for _, oc := range result.Outcomes {
		require.Contains(t, []oapi.UsageRecoverAgentOutcomeOutcome{oapi.Started, oapi.WaitingForCapacity}, oc.Outcome)
	}
	unrelated, err := st.Get(context.Background(), "unrelated")
	require.NoError(t, err)
	require.Nil(t, unrelated.BackendRecovery, "unrelated agents must not be mutated")
}

func TestUsageRecoverStaleSnapshotIsNotForcedLimit(t *testing.T) {
	fp := "sha256:shared-account"
	s, _, st, life, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	now := time.Now().UTC()
	_, err := snapStore.Record(backendusage.UsageSnapshot{
		Domain:        backendusage.CapacityDomain{Provider: "codex", ProfileFingerprint: fp},
		ObservedAt:    now.Add(-2 * time.Hour),
		RecordedAt:    now.Add(-2 * time.Hour),
		SourceStatus:  backendusage.StatusOK,
		Authoritative: true,
		Freshness:     backendusage.FreshnessFresh,
		Buckets:       []backendusage.CapacityBucket{{Key: "weekly", State: backendusage.BucketExhausted}},
	})
	require.NoError(t, err)

	// Force stale_after very short so the pre-recorded snap is stale, and make the
	// live provider refresh return available capacity (not exhausted).
	s.SetUsageReconciliation(true, time.Minute, time.Second)

	result, err := s.runUsageRecover(context.Background(), oapi.UsageRecoverRequest{})
	require.NoError(t, err)
	require.Empty(t, result.Impact.AffectedAgents, "stale/cached exhaustion must not force recovery")
	require.Empty(t, result.Outcomes)
	require.Empty(t, life.swaps)
	agent, err := st.Get(context.Background(), "agent-1")
	require.NoError(t, err)
	require.Nil(t, agent.BackendRecovery)
}

func TestUsageRecoverHTTPRouteParity(t *testing.T) {
	fp := "sha256:shared-account"
	s, _, st, _, snapStore := bulkRecoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})
	boundAgent(t, st, "agent-1", "codex", "codex-model", fp, "weekly")
	recordExhaustedBucket(t, snapStore, "codex", fp, "weekly", nil)

	ts := httptest.NewServer(s.router())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]any{"dry_run": true, "ai_cli": "codex"})
	resp, err := http.Post(ts.URL+"/api/v1/usage/recover", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got oapi.UsageRecoverResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.True(t, got.DryRun)
	require.ElementsMatch(t, []string{"agent-1"}, impactIDs(got.Impact))
	require.Len(t, got.Outcomes, 1)
	require.Equal(t, oapi.WouldStart, got.Outcomes[0].Outcome)
}

func TestUsageRecoverUnavailableWithoutUsageService(t *testing.T) {
	s := NewServer(newFakeStore(), nil, nil, 0, false, nil, nil, nil)
	ts := httptest.NewServer(s.router())
	t.Cleanup(ts.Close)
	resp, err := http.Post(ts.URL+"/api/v1/usage/recover", "application/json", bytes.NewReader([]byte(`{}`)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
