package planexport

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecordStore_upsertFindList(t *testing.T) {
	dir := t.TempDir()
	st, err := NewStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	exported := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	rec := &Record{
		PlanID:      "plan-deadbeef",
		Repository:  "github.com/srjn45/warden",
		TargetRef:   "autopilot/scrivadb-canonical-plans-repo-sync",
		OutputPath:  "plans/pending/example-plan.yaml",
		Revision:    3,
		ContentHash: "sha256:abc",
		PRURL:       "https://github.com/srjn45/warden/pull/999",
		CommitSHA:   "abcdef0123456789",
		Outcome:     OutcomeSuccess,
		ExportedAt:  exported,
	}
	require.NoError(t, st.Upsert(ctx, rec))
	require.Equal(t, RecordID(rec.PlanID, rec.Repository, rec.TargetRef, rec.OutputPath), rec.ID)

	got, err := st.Find(ctx, rec.PlanID, rec.Repository, rec.TargetRef, rec.OutputPath)
	require.NoError(t, err)
	require.Equal(t, rec.ID, got.ID)
	require.Equal(t, OutcomeSuccess, got.Outcome)
	require.Equal(t, "https://github.com/srjn45/warden/pull/999", got.PRURL)
	require.Equal(t, "abcdef0123456789", got.CommitSHA)
	require.True(t, got.SameExport(3, "sha256:abc"))
	require.False(t, got.SameExport(4, "sha256:abc"))

	// Second repo path for same plan.
	other := &Record{
		PlanID:       rec.PlanID,
		Repository:   "github.com/example/fork",
		TargetRef:    "main",
		OutputPath:   "plans/pending/example-plan.yaml",
		Revision:     3,
		ContentHash:  "sha256:abc",
		Outcome:      OutcomeFailed,
		ErrorMessage: "github auth missing",
		ExportedAt:   exported,
	}
	require.NoError(t, st.Upsert(ctx, other))

	list, err := st.ListByPlan(ctx, rec.PlanID)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Upsert replaces prior outcome for same key.
	rec.Outcome = OutcomeSkipped
	rec.PRURL = "https://github.com/srjn45/warden/pull/999"
	require.NoError(t, st.Upsert(ctx, rec))
	got, err = st.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, got.Outcome)

	_, err = st.Find(ctx, "plan-missing", rec.Repository, rec.TargetRef, rec.OutputPath)
	require.ErrorIs(t, err, ErrRecordNotFound)

	// Re-open from disk.
	require.NoError(t, st.Close())
	st2, err := NewStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st2.Close() })
	got, err = st2.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, OutcomeSkipped, got.Outcome)

	// Ensure DB landed under the expected subdir.
	_, err = os.Stat(filepath.Join(dir, "plan-exports-db"))
	require.NoError(t, err)
}

func TestRecordID_stable(t *testing.T) {
	a := RecordID("p", "r", "ref", "path")
	b := RecordID("p", "r", "ref", "path")
	c := RecordID("p", "r", "ref", "other")
	require.Equal(t, a, b)
	require.NotEqual(t, a, c)
	require.True(t, len(a) > len("pex-"))
}
