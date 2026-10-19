package backendstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These cases exercise the evidence boundary with deliberately hostile refresh
// shapes. They are store-only: adapters must finish provider I/O before calling
// Preview, so no provider process can be held under Store.mu.
func TestCatalogStalePartialAndEmptyEvidenceNeverRetires(t *testing.T) {
	cases := []struct {
		name string
		obs  CatalogObservation
	}{
		{"stale cache", CatalogObservation{Source: SourceCLICache, SourceFetchedAt: time.Now().Add(-25 * time.Hour), Completeness: CompletenessComplete}},
		{"partial", CatalogObservation{Source: SourceLiveCommand, Completeness: CompletenessPartial}},
		{"empty", CatalogObservation{Source: SourceLiveCommand, Completeness: CompletenessEmpty}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			p, err := s.Preview(CatalogRefresh{Scope: CatalogScope{AiCli: "codex", AccountRef: "a"}, Observation: tc.obs}, time.Now().UTC())
			require.NoError(t, err)
			for _, op := range p.Ops {
				require.NotEqual(t, "retire", op.Op)
			}
			m, err := s.GetModel("codex", "gpt-5.5")
			require.NoError(t, err)
			require.Equal(t, ModelLifecycleActive, m.EffectiveLifecycle())
		})
	}
}

func TestCatalogEvidenceAndLaunchRejectionStayInAccountScope(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	// Account A observes a model and applies its scope. Account B must not be
	// allowed to overwrite that observation or its launch rejection.
	a, err := s.Preview(CatalogRefresh{Scope: CatalogScope{AiCli: "codex", AccountRef: "account-a"}, Observation: CatalogObservation{Source: SourceLiveCommand, Completeness: CompletenessComplete, Models: []ObservedModel{{ModelID: "gpt-scope-only"}}}}, now)
	require.NoError(t, err)
	_, err = s.ApplyCatalogProposal(a.ID, now.Add(time.Second))
	require.NoError(t, err)
	require.NoError(t, s.RecordLaunchEvidence("codex", "gpt-scope-only", LaunchEvidence{Kind: LaunchFailureInvalidModel, PreTask: true, ObservedAt: now}, time.Hour))

	b, err := s.Preview(CatalogRefresh{Scope: CatalogScope{AiCli: "codex", AccountRef: "account-b"}, Observation: CatalogObservation{Source: SourceLiveCommand, Completeness: CompletenessPartial}}, now.Add(2*time.Second))
	require.NoError(t, err)
	_, err = s.ApplyCatalogProposal(b.ID, now.Add(3*time.Second))
	require.NoError(t, err)

	aObs, err := s.GetCatalogObservation("codex", CatalogScope{AiCli: "codex", AccountRef: "account-a"}.ScopeFP(), "gpt-scope-only")
	require.NoError(t, err)
	require.True(t, aObs.LaunchRejectedUntil.After(now))
	bObs, err := s.GetActiveCatalogObservation("codex", "gpt-scope-only")
	require.NoError(t, err)
	require.Equal(t, AvailabilityUnknown, bObs.Availability)
	require.True(t, bObs.LaunchRejectedUntil.IsZero())
}
