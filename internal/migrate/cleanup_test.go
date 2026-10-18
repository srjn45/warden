package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/adoption"
	"github.com/srjn45/warden/internal/audit"
)

// eligibleDir is a cut-over data dir that satisfies every retirement criterion.
func eligibleDir(t *testing.T) (string, CleanupOptions) {
	dir, l := cutOver(t)
	recordBoots(t, dir, 3)
	return dir, CleanupOptions{Now: l.History[0].At.Add(DefaultSupportWindow + time.Hour), BinaryVersion: "11.0.0"}
}

func TestCleanup_DryRunChangesNothing(t *testing.T) {
	dir, opt := eligibleDir(t)
	want, _ := CleanupCandidates(dir)
	require.NotEmpty(t, want)
	before, err := fingerprintTree(dir)
	require.NoError(t, err)

	opt.DryRun = true
	rep, err := CleanupLegacy(context.Background(), dir, opt)
	require.NoError(t, err)
	require.False(t, rep.Applied)
	require.Equal(t, want, rep.Paths)
	after, err := fingerprintTree(dir)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestCleanup_RefusesWithoutConfirm(t *testing.T) {
	dir, opt := eligibleDir(t)
	rep, err := CleanupLegacy(context.Background(), dir, opt)
	require.ErrorIs(t, err, ErrCleanupUnconfirmed)
	require.False(t, rep.Applied)
	left, _ := CleanupCandidates(dir)
	require.Equal(t, rep.Paths, left)
}

func TestCleanup_RefusesIneligible(t *testing.T) {
	dir, opt := eligibleDir(t)
	opt.Confirm = true
	opt.Now = opt.Now.Add(-DefaultSupportWindow) // window not elapsed
	_, err := CleanupLegacy(context.Background(), dir, opt)
	require.ErrorIs(t, err, ErrCleanupNotEligible)
	require.Contains(t, err.Error(), CritSupportWindow)
	left, _ := CleanupCandidates(dir)
	require.NotEmpty(t, left)
}

func TestCleanup_RefusesCorruptBackup(t *testing.T) {
	dir, opt := eligibleDir(t)
	opt.Confirm = true
	matches, err := filepath.Glob(filepath.Join(dir, "backups", "pre-schema2-*", "*"))
	require.NoError(t, err)
	// corrupt one backed-up file (not the manifest)
	corrupted := false
	filepath.WalkDir(filepath.Dir(matches[0]), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !corrupted && d.Name() != Schema2ManifestName {
			require.NoError(t, os.Chmod(p, 0o600))
			require.NoError(t, os.WriteFile(p, []byte("tampered"), 0o600))
			corrupted = true
		}
		return nil
	})
	require.True(t, corrupted)
	_, err = CleanupLegacy(context.Background(), dir, opt)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "backup") || strings.Contains(err.Error(), "retirement"), err.Error())
	left, _ := CleanupCandidates(dir)
	require.NotEmpty(t, left, "nothing moves when the backup does not verify")
}

func TestCleanup_RefusesMissingWardenDB(t *testing.T) {
	dir, opt := eligibleDir(t)
	opt.Confirm = true
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "warden-db")))
	_, err := CleanupLegacy(context.Background(), dir, opt)
	require.Error(t, err)
	left, _ := CleanupCandidates(dir)
	require.NotEmpty(t, left)
}

func TestCleanup_VerifiedCleanupQuarantinesAuditsAndIsIdempotent(t *testing.T) {
	dir, opt := eligibleDir(t)
	opt.Confirm = true
	opt.Actor = "tester"
	want, _ := CleanupCandidates(dir)

	rep, err := CleanupLegacy(context.Background(), dir, opt)
	require.NoError(t, err)
	require.True(t, rep.Applied)
	require.Equal(t, want, rep.Paths)
	for _, p := range want {
		require.NoFileExists(t, filepath.Join(dir, filepath.FromSlash(p)))
		_, err := os.Lstat(filepath.Join(rep.Quarantine, filepath.FromSlash(p)))
		require.NoError(t, err, "moved, not deleted: %s", p)
	}
	require.DirExists(t, filepath.Join(dir, "warden-db"))

	evs, err := audit.Read(filepath.Join(dir, "audit.jsonl"), audit.Filter{})
	require.NoError(t, err)
	var found bool
	for _, e := range evs {
		if e.Action == audit.ActionLegacyCleanup {
			found = true
			require.Equal(t, "tester", e.Actor)
			require.Equal(t, strings.Join(want, ","), e.Detail["paths"])
		}
	}
	require.True(t, found, "audit event recorded")
	log, err := adoption.Read(dir)
	require.NoError(t, err)
	require.Equal(t, 1, log.Count(adoption.LegacyCleanup, time.Time{}))

	// Re-run: gate still passes (backup + warden-db intact), nothing to move.
	opt.Now = opt.Now.Add(time.Second)
	again, err := CleanupLegacy(context.Background(), dir, opt)
	require.NoError(t, err)
	require.False(t, again.Applied)
	require.Empty(t, again.Paths)
	log, _ = adoption.Read(dir)
	require.Equal(t, 1, log.Count(adoption.LegacyCleanup, time.Time{}), "no-op re-run records nothing")
}
