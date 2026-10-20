package backendstore

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCatalogScopeFingerprintDoesNotPersistIdentityInputs(t *testing.T) {
	s := CatalogScope{AiCli: "codex", AuthMode: "chatgpt", AccountRef: "account-secret", Profile: "profile-secret"}
	require.Equal(t, "sha256:dd9208115a23db829577df25", s.ScopeFP())
	require.NotContains(t, s.ScopeFP(), "secret")
}

func TestCatalogObservationLongKeyAndRoundTrip(t *testing.T) {
	s := newTestStore(t)
	modelID := strings.Repeat("model/", 30)
	key := ObservationKey("codex", "sha256:scope", modelID)
	require.LessOrEqual(t, len(key), 128)
	require.NoError(t, s.PutCatalogObservation(StoredObservation{AiCli: "codex", ScopeFP: "sha256:scope", ModelID: modelID, Availability: AvailabilityAvailable}))
	got, err := s.GetCatalogObservation("codex", "sha256:scope", modelID)
	require.NoError(t, err)
	require.Equal(t, modelID, got.ModelID)
}

func TestLaunchEvidenceIsModelScopedBoundedAndSuccessSupersedesIt(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.PutCatalogScope(ActiveCatalogScope{AiCli: "codex", ActiveScopeFP: "scope"}))
	require.NoError(t, s.RecordLaunchEvidence("codex", "gone", LaunchEvidence{Kind: LaunchFailureInvalidModel, PreTask: true, ObservedAt: now}, time.Hour))
	got, err := s.GetCatalogObservation("codex", "scope", "gone")
	require.NoError(t, err)
	require.Equal(t, AvailabilityUnavailable, got.Availability)
	require.Equal(t, now.Add(time.Hour), got.LaunchRejectedUntil)
	require.Equal(t, "invalid_model", got.LaunchRejectionReason)

	// A real successful startup is newer, stronger evidence for the exact same
	// account/model, and must re-enable it without affecting any backend row.
	require.NoError(t, s.RecordLaunchEvidence("codex", "gone", LaunchEvidence{Succeeded: true, PreTask: true, ObservedAt: now.Add(time.Minute)}, time.Hour))
	got, err = s.GetCatalogObservation("codex", "scope", "gone")
	require.NoError(t, err)
	require.Equal(t, AvailabilityAvailable, got.Availability)
	require.True(t, got.LaunchRejectedUntil.IsZero())
}

func TestClassifyLaunchFailureIsConservative(t *testing.T) {
	require.Equal(t, LaunchFailureInvalidModel, ClassifyLaunchFailure("error: model gpt-old is not available for this account"))
	require.Equal(t, LaunchFailureQuota, ClassifyLaunchFailure("HTTP 429 rate limit exceeded"))
	require.Equal(t, LaunchFailureReasoning, ClassifyLaunchFailure("reasoning effort is unsupported"))
	require.Equal(t, LaunchFailureOther, ClassifyLaunchFailure("process exited with status 1"))
	// A backend-level unsupported error is not proof about the selected model.
	require.Equal(t, LaunchFailureOther, ClassifyLaunchFailure("unsupported command"))
}

func TestCatalogSnapshotRetentionKeepsRecentAndReferencedEvidence(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	// Record the audit reference before retention sees the oldest snapshot.
	require.NoError(t, s.PutCatalogRevision(CatalogRevision{Rev: 1, Snapshots: []string{revisionKey(1)}}))
	for i := 0; i < 22; i++ {
		id := revisionKey(uint64(i + 1))
		require.NoError(t, s.PutCatalogSnapshot(CatalogSnapshot{ID: id, AiCli: "codex", ScopeFP: "sha256:scope", ObservedAt: now.Add(time.Duration(i-40) * 24 * time.Hour)}))
	}
	// The oldest normally eligible snapshot remains because an audit record
	// references it.
	snaps, err := s.ListCatalogSnapshots("codex", "sha256:scope")
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, snap := range snaps {
		ids[snap.ID] = true
	}
	require.True(t, ids[revisionKey(1)])
	require.LessOrEqual(t, len(snaps), 21)
}

func TestDeleteSeedModelWritesExcludedTombstone(t *testing.T) {
	dir := t.TempDir()
	s, err := openTestStore(dir)
	require.NoError(t, err)
	require.NoError(t, s.DeleteModel("codex", "gpt-5.5"))
	m, err := s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, ModelLifecycleExcluded, m.EffectiveLifecycle())
	require.False(t, m.Enabled)
	require.False(t, m.AutoAssign)
	require.NoError(t, s.Close())

	s, err = openTestStore(dir)
	require.NoError(t, err)
	defer s.Close()
	m, err = s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, ModelLifecycleExcluded, m.EffectiveLifecycle())
}

func TestUnclassifiedTierPersistsButCannotBeTierFiltered(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.SetModelTier("codex", "gpt-5.5", TierUnclassified))
	m, err := s.GetModel("codex", "gpt-5.5")
	require.NoError(t, err)
	require.Equal(t, ModelTier(TierUnclassified), m.Tier)
	for _, tier := range []ModelTier{Tier1, Tier2, Tier3} {
		models, err := s.ListModels(tier)
		require.NoError(t, err)
		for _, model := range models {
			require.NotEqual(t, "gpt-5.5", model.ModelID)
		}
	}
}

func TestCatalogScopeOpaqueFingerprintOverridesDerivedValue(t *testing.T) {
	derived := CatalogScope{AiCli: "codex", AccountRef: "acct", Profile: "p"}.ScopeFP()
	require.True(t, ValidScopeFP(derived))
	require.Equal(t, derived, CatalogScope{AiCli: "codex", Fingerprint: derived}.ScopeFP())
	require.NotEqual(t, derived, CatalogScope{AiCli: "codex"}.ScopeFP())
	for _, bad := range []string{"", "acct", "sha256:", "sha256:ABCDEF0123456789ABCDEF01", derived + "0"} {
		require.False(t, ValidScopeFP(bad), bad)
	}
}
