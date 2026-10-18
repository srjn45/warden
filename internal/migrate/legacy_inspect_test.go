package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInspectLegacy_ReadOnlyAfterCutover(t *testing.T) {
	dir, _ := cutOver(t)
	before, err := fingerprintTree(dir)
	require.NoError(t, err)

	rep, err := InspectLegacy(context.Background(), dir)
	require.NoError(t, err)
	require.True(t, rep.ReadOnly)
	require.Equal(t, "none", rep.Authority)
	require.True(t, rep.Unchanged)
	require.True(t, rep.Backup.Verified, rep.Backup.Detail)
	require.NotEmpty(t, rep.Sources, "legacy stores are retained after cutover")
	require.NotEmpty(t, rep.Collections)
	for _, c := range rep.Collections {
		require.Zero(t, c.OnlyLegacy, "collection %s: cutover must have carried every legacy record", c.Name)
	}

	after, err := fingerprintTree(dir)
	require.NoError(t, err)
	require.Equal(t, before, after, "inspection must not touch the data dir")
}

func TestInspectLegacy_ReportsMissingCanonicalRecord(t *testing.T) {
	dir, _ := cutOver(t)
	// Remove the canonical db: every legacy record is now only-legacy.
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "warden-db")))
	rep, err := InspectLegacy(context.Background(), dir)
	require.NoError(t, err)
	var diffs int
	for _, c := range rep.Collections {
		if !c.Match {
			diffs++
			require.Equal(t, c.LegacyRecords, c.OnlyLegacy)
		}
	}
	require.NotZero(t, diffs)
}

func TestInspectLegacy_RefusesLiveSchema1(t *testing.T) {
	dir := t.TempDir()
	seedRealisticLegacy(t, dir)
	_, err := InspectLegacy(context.Background(), dir)
	require.ErrorIs(t, err, ErrLegacyLiveAuthority)
}

func TestInspectLegacy_TamperedBackupIsBlocking(t *testing.T) {
	dir, l := cutOver(t)
	bdir := l.History[len(l.History)-1].Backup
	if !filepath.IsAbs(bdir) {
		bdir = filepath.Join(dir, bdir)
	}
	require.NoError(t, os.WriteFile(filepath.Join(bdir, "data", "stray"), []byte("x"), 0o600))
	rep, err := InspectLegacy(context.Background(), dir)
	require.NoError(t, err)
	require.False(t, rep.Backup.Verified)
	require.NotEmpty(t, rep.Findings)
}

func TestInspectLegacy_DetectsMutation(t *testing.T) {
	dir, _ := cutOver(t)
	a, err := fingerprintTree(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o600))
	b, err := fingerprintTree(dir)
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}
