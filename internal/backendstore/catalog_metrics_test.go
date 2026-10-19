package backendstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCatalogMetricsAreBoundedAndDoNotExposeScopeOrModel(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	_, err := s.Preview(CatalogRefresh{Scope: CatalogScope{AiCli: "untrusted-cli", AccountRef: "account-secret"}, Observation: CatalogObservation{Source: SourceCLICache, SourceFetchedAt: now.Add(-25 * time.Hour), Completeness: CompletenessPartial, Err: &CatalogError{Code: "deadline"}, Models: []ObservedModel{{ModelID: "model-secret"}}}}, now)
	require.NoError(t, err)
	m := s.CatalogMetrics()
	require.Len(t, m.Refreshes, 1)
	require.Equal(t, "other", m.Refreshes[0].AiCli)
	require.Equal(t, "partial", m.Refreshes[0].Completeness)
	require.Equal(t, "stale", m.Refreshes[0].Freshness)
	require.Equal(t, "deadline", m.Refreshes[0].Error)
	require.EqualValues(t, 1, m.PartialResults)
	require.EqualValues(t, 1, m.Timeouts)
	require.EqualValues(t, 1, m.Discovered)
	require.EqualValues(t, 1, m.RefreshDuration.Count)

	// A stale proposal is a bounded conflict counter, not a new label series.
	p, err := s.Preview(catalogRefresh("codex", CompletenessComplete, "gpt-5.5"), now)
	require.NoError(t, err)
	require.NoError(t, s.SetModelTier("codex", "gpt-5.5", Tier2))
	_, err = s.ApplyCatalogProposal(p.ID, now.Add(time.Minute))
	require.ErrorIs(t, err, ErrProposalStale)
	require.EqualValues(t, 1, s.CatalogMetrics().ReconciliationConflict)
}
