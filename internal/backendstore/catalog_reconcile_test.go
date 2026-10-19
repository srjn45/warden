package backendstore

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func catalogRefresh(ai string, complete Completeness, ids ...string) CatalogRefresh {
	models := make([]ObservedModel, 0, len(ids))
	for _, id := range ids {
		models = append(models, ObservedModel{ModelID: id, DisplayName: id})
	}
	return CatalogRefresh{Scope: CatalogScope{AiCli: ai, AuthMode: "chatgpt", AccountRef: "acct"}, Observation: CatalogObservation{Source: SourceLiveCommand, Completeness: complete, Models: models}}
}

func seededIDs(ai, omit string) []string {
	var ids []string
	for _, m := range DefaultModels() {
		if m.BackendID == ai && m.ModelID != omit {
			ids = append(ids, m.ModelID)
		}
	}
	return ids
}

func TestCatalogPreviewDoesNotMutateRowsAndApplyRequiresExactProposal(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	before, err := s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	ids := append(seededIDs("codex", "gpt-5.5"), "gpt-6-sol")
	p, err := s.Preview(catalogRefresh("codex", CompletenessComplete, ids...), now)
	require.NoError(t, err)
	still, err := s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, before, still, "preview must not alter the effective catalog")
	require.NotEmpty(t, p.Ops)
	_, err = s.ApplyCatalogProposal(p.ID, now.Add(time.Minute))
	require.NoError(t, err)
	retired, err := s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, ModelLifecycleRetired, retired.EffectiveLifecycle())
	require.False(t, retired.AutoAssign, "old binaries must not route retired rows")
}

func TestCatalogIncompleteNeverRetiresAndPositiveObservationMayBeAdded(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	p, err := s.Preview(catalogRefresh("codex", CompletenessPartial, "gpt-new"), now)
	require.NoError(t, err)
	for _, op := range p.Ops {
		require.NotEqual(t, "retire", op.Op)
	}
	_, err = s.ApplyCatalogProposal(p.ID, now.Add(time.Minute))
	require.NoError(t, err)
	added, err := s.GetModel("codex", "gpt-new")
	require.NoError(t, err)
	require.Equal(t, ModelOriginDiscovered, added.Origin)
	old, err := s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, ModelLifecycleActive, old.EffectiveLifecycle())
}

func TestCatalogProposalStaleAndRollbackDetectsManualEdit(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	p, err := s.Preview(catalogRefresh("codex", CompletenessComplete, "gpt-5.5"), now)
	require.NoError(t, err)
	require.NoError(t, s.SetModelTier("codex", "gpt-5.5", Tier2))
	_, err = s.ApplyCatalogProposal(p.ID, now.Add(time.Minute))
	require.ErrorIs(t, err, ErrProposalStale)
	p, err = s.Preview(catalogRefresh("codex", CompletenessComplete, "gpt-5.5"), now.Add(2*time.Minute))
	require.NoError(t, err)
	rev, err := s.ApplyCatalogProposal(p.ID, now.Add(3*time.Minute))
	require.NoError(t, err)
	require.NoError(t, s.SetModelEnabled("codex", "gpt-5.5", false))
	_, err = s.RollbackCatalogRevision(rev.Rev, now.Add(4*time.Minute))
	require.True(t, errors.Is(err, ErrRevisionConflict))
}

func TestCatalogMassRetirementGuardAndReappearance(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	p, err := s.Preview(catalogRefresh("codex", CompletenessComplete, "gpt-5.5"), now)
	require.NoError(t, err)
	found := false
	for _, b := range p.Blocked {
		if b.Reason == "mass_retirement_guard" {
			found = true
		}
	}
	require.True(t, found)
	// A previously retired record must reappear, retaining its original intent.
	m, err := s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	m.Lifecycle = ModelLifecycleRetired
	m.AutoAssign = false
	m.RetiredPrior = &RetiredPrior{AutoAssign: true}
	require.NoError(t, s.UpsertModel(m))
	p, err = s.Preview(catalogRefresh("codex", CompletenessComplete, "gpt-5.5"), now.Add(time.Minute))
	require.NoError(t, err)
	rev, err := s.ApplyCatalogProposal(p.ID, now.Add(2*time.Minute))
	require.NoError(t, err)
	require.NotZero(t, rev.Rev)
	m, err = s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, ModelLifecycleActive, m.EffectiveLifecycle())
	require.True(t, m.AutoAssign)
}
