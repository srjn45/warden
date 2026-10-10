package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/srjn45/scriva/store"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/ownerlock"
)

func repairBackendsCmdFor(t *testing.T, dataDir string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfg, []byte(fmt.Sprintf("data_dir: %q\n", dataDir)), 0o600))
	cmd := newRepairBackendsCmd()
	cmd.SetContext(t.Context())
	cmd.Flags().String("config", cfg, "")
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return cmd, stdout, stderr
}

// buildTestRegistry populates a store in <dataDir>/backends with revisions and returns it.
func buildTestRegistry(t *testing.T, dataDir string) {
	t.Helper()
	dir := filepath.Join(dataDir, "backends")
	s, err := backendstore.NewStore(dir)
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.Upsert(backendstore.Backend{ID: "claude", Installed: true, BinaryPath: "/usr/bin/claude", DetectedAt: now, Tier: backendstore.TierSubscription, Enabled: true}))
	require.NoError(t, s.Upsert(backendstore.Backend{ID: "custom", Installed: true, DetectedAt: now, Tier: backendstore.TierFree, Enabled: true}))
	for _, tier := range []string{backendstore.TierFree, backendstore.TierSubscription, backendstore.TierPayPerUse, backendstore.TierSubscription} {
		require.NoError(t, s.SetTier("claude", tier))
	}
	require.NoError(t, s.SetDefault("claude"))
	require.NoError(t, s.Close())
}

// injectStaleRegressions appends an older revision update with an older timestamp to backends.
func injectStaleRegressions(t *testing.T, dataDir string) int {
	t.Helper()
	cdir := filepath.Join(dataDir, "backends", "backends")
	segs, err := filepath.Glob(filepath.Join(cdir, "seg_*.ndjson"))
	require.NoError(t, err)
	require.NotEmpty(t, segs)
	last := segs[len(segs)-1]
	raw, err := os.ReadFile(last)
	require.NoError(t, err)

	type line struct {
		ID  uint64 `json:"id"`
		Op  string `json:"op"`
		Rev uint64 `json:"rev"`
	}
	maxRev := map[uint64]uint64{}
	older := map[uint64][]byte{}
	for _, l := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var e line
		require.NoError(t, json.Unmarshal(l, &e))
		if e.Rev > maxRev[e.ID] {
			maxRev[e.ID] = e.Rev
		}
		if e.Op == "update" {
			if _, ok := older[e.ID]; !ok {
				older[e.ID] = append([]byte(nil), l...)
			}
		}
	}
	var extra []byte
	n := 0
	for id, l := range older {
		var e line
		require.NoError(t, json.Unmarshal(l, &e))
		if e.Rev < maxRev[id] {
			extra = append(extra, l...)
			extra = append(extra, '\n')
			n++
		}
	}
	f, err := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.Write(extra)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	idx, _ := filepath.Glob(filepath.Join(cdir, "index.json"))
	sidx, _ := filepath.Glob(filepath.Join(cdir, "sidx_*.json"))
	for _, p := range append(idx, sidx...) {
		require.NoError(t, os.Remove(p))
	}
	return n
}

// injectAmbiguousRegressions appends an update with a future timestamp and lower revision.
func injectAmbiguousRegressions(t *testing.T, dataDir string) {
	t.Helper()
	cdir := filepath.Join(dataDir, "backends", "backends")
	segs, err := filepath.Glob(filepath.Join(cdir, "seg_*.ndjson"))
	require.NoError(t, err)
	last := segs[len(segs)-1]

	entry := store.Entry{
		ID:   1,
		Op:   store.OpUpdate,
		Ts:   time.Now().UTC().Add(100 * time.Hour),
		Rev:  1,
		Data: map[string]any{"_key": "claude", "id": "claude", "tier": "free", "enabled": false},
	}
	b, err := store.Encode(entry)
	require.NoError(t, err)

	f, err := os.OpenFile(last, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.Write(b)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	idx, _ := filepath.Glob(filepath.Join(cdir, "index.json"))
	sidx, _ := filepath.Glob(filepath.Join(cdir, "sidx_*.json"))
	for _, p := range append(idx, sidx...) {
		require.NoError(t, os.Remove(p))
	}
}

func TestRepairBackendsAbsentStore(t *testing.T) {
	dataDir := t.TempDir()
	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	require.NoError(t, cmd.RunE(cmd, nil))
	require.Contains(t, stdout.String(), "absent")
	require.Contains(t, stdout.String(), "first start")
}

func TestRepairBackendsCleanStore(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)

	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	err := cmd.RunE(cmd, nil)
	require.NoError(t, err)
	require.Contains(t, stdout.String(), "clean")
	require.Equal(t, 0, repairExitOK)
}

func TestRepairBackendsOwnedStoreRefused(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)

	orig := probeDataDirOwner
	probeDataDirOwner = func(dir string) (*ownerlock.OwnedError, error) {
		return &ownerlock.OwnedError{
			Dir: dir,
			Owner: &ownerlock.Owner{
				PID:     12345,
				Kind:    ownerlock.KindDaemon,
				Launch:  ownerlock.LaunchSystemd,
				Version: "9.27.0",
				Addr:    "127.0.0.1:8765",
			},
		}, nil
	}
	t.Cleanup(func() { probeDataDirOwner = orig })

	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	require.Equal(t, repairExitOwned, ExitCode(err))
	require.Contains(t, stdout.String(), "owner:  daemon pid 12345 (launched: systemd)")
	require.Contains(t, stdout.String(), "systemctl --user stop warden")
}

func TestRepairBackendsDryRunRecoverable(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)
	require.Positive(t, injectStaleRegressions(t, dataDir))

	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	require.NoError(t, cmd.Flags().Set("dry-run", "true"))
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	require.Equal(t, repairExitRepairable, ExitCode(err))
	require.Contains(t, stdout.String(), "safely recoverable")
	require.Contains(t, stdout.String(), "dry-run (read-only; nothing was changed)")
	require.Contains(t, stdout.String(), "stale revisions to discard:")
}

func TestRepairBackendsDryRunJSON(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)
	require.Positive(t, injectStaleRegressions(t, dataDir))

	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	require.NoError(t, cmd.Flags().Set("dry-run", "true"))
	require.NoError(t, cmd.Flags().Set("json", "true"))
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	require.Equal(t, repairExitRepairable, ExitCode(err))

	var doc repairBackendsOutput
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &doc))
	require.Equal(t, "dry-run", doc.Mode)
	require.Equal(t, repairStatusRecoverable, doc.Status)
	require.Equal(t, repairExitRepairable, doc.ExitCode)
	require.False(t, doc.Mutated)
	require.NotEmpty(t, doc.Collections)
	require.Equal(t, "backends", doc.Collections[0].Name)
	require.Equal(t, classSafelyRecoverable, doc.Collections[0].Classification)
	require.Positive(t, doc.Collections[0].StaleRevisions)
}

func TestRepairBackendsNonInteractiveRequiresYes(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)
	require.Positive(t, injectStaleRegressions(t, dataDir))

	origStdin := stdinIsInteractive
	stdinIsInteractive = func(io.Reader) bool { return false }
	t.Cleanup(func() { stdinIsInteractive = origStdin })

	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	require.Equal(t, repairExitUnconfirmed, ExitCode(err))
	require.Contains(t, stdout.String(), "cancelled")
	require.Contains(t, stdout.String(), "--yes")
}

func TestRepairBackendsInteractivePromptDeclined(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)
	require.Positive(t, injectStaleRegressions(t, dataDir))

	origStdin := stdinIsInteractive
	stdinIsInteractive = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsInteractive = origStdin })

	cmd, stdout, stderr := repairBackendsCmdFor(t, dataDir)
	cmd.SetIn(strings.NewReader("n\n"))
	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	require.Equal(t, repairExitUnconfirmed, ExitCode(err))
	require.Contains(t, stderr.String(), "Repair the backend registry")
	require.Contains(t, stdout.String(), "cancelled")
}

func TestRepairBackendsApplyWithYesSuccess(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)
	require.Positive(t, injectStaleRegressions(t, dataDir))

	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	require.NoError(t, cmd.Flags().Set("yes", "true"))
	backupDir := t.TempDir()
	require.NoError(t, cmd.Flags().Set("backup-dir", backupDir))

	require.NoError(t, cmd.RunE(cmd, nil))
	require.Contains(t, stdout.String(), "repaired")
	require.Contains(t, stdout.String(), "discarded stale revisions:")
	require.Contains(t, stdout.String(), "backup:")
	require.Contains(t, stdout.String(), "verified: size and SHA-256")

	// Store is now clean and verifiable
	rep, err := backendstore.Verify(context.Background(), filepath.Join(dataDir, "backends"))
	require.NoError(t, err)
	require.True(t, rep.Clean())

	// Can be opened cleanly
	s, err := backendstore.NewStore(filepath.Join(dataDir, "backends"))
	require.NoError(t, err)
	defer s.Close()
	list, err := s.List()
	require.NoError(t, err)
	require.NotEmpty(t, list)
}

func TestRepairBackendsAmbiguousRefused(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)
	injectAmbiguousRegressions(t, dataDir)

	cmd, stdout, _ := repairBackendsCmdFor(t, dataDir)
	require.NoError(t, cmd.Flags().Set("yes", "true"))
	backupDir := t.TempDir()
	require.NoError(t, cmd.Flags().Set("backup-dir", backupDir))

	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	require.Equal(t, repairExitRecoveryRequired, ExitCode(err))
	require.Contains(t, stdout.String(), "recovery-required")
	require.Contains(t, stdout.String(), "AMBIGUOUS, recovery required")
	require.Contains(t, stdout.String(), "report:")
}

func TestDoctorCheckBackendRegistry(t *testing.T) {
	dataDir := t.TempDir()
	// Absent
	res := checkBackendRegistry(context.Background(), dataDir)
	require.True(t, res.ok)
	require.Contains(t, res.detail, "no registry yet")

	// Clean
	buildTestRegistry(t, dataDir)
	res = checkBackendRegistry(context.Background(), dataDir)
	require.True(t, res.ok)
	require.Contains(t, res.detail, "clean")

	// Recoverable
	injectStaleRegressions(t, dataDir)
	res = checkBackendRegistry(context.Background(), dataDir)
	require.False(t, res.ok)
	require.Contains(t, res.detail, "safely recoverable findings")
	require.Contains(t, res.detail, backendstore.RepairCommand)

	// Ambiguous
	injectAmbiguousRegressions(t, dataDir)
	res = checkBackendRegistry(context.Background(), dataDir)
	require.False(t, res.ok)
	require.Contains(t, res.detail, "RECOVERY REQUIRED")
	require.Contains(t, res.detail, backendstore.RepairDryRunCommand)
	require.Contains(t, res.detail, backendstore.RepairCommand)
}

// Beside a running daemon the persisted registry index legitimately trails its
// writes. doctor must not call that "recoverable" (nor promise a repair on next
// start): it is a pass, and it is only a pass while the directory is owned.
func TestDoctorCheckBackendRegistryLiveIndexLagIsNotAWarning(t *testing.T) {
	dataDir := t.TempDir()
	buildTestRegistry(t, dataDir)

	owner, err := backendstore.NewStore(filepath.Join(dataDir, "backends"))
	require.NoError(t, err)
	for _, tier := range []string{backendstore.TierFree, backendstore.TierSubscription, backendstore.TierFree} {
		require.NoError(t, owner.SetTier("claude", tier))
	}

	res := checkBackendRegistry(context.Background(), dataDir)
	require.True(t, res.ok, "index lag beside the owner is not a finding: %s", res.detail)
	require.NotContains(t, res.detail, "safely recoverable")
	require.NotContains(t, res.detail, "next start")

	// Update preflight must not list the lag as something to repair either.
	pre, err := backendPreflight(context.Background(), filepath.Join(dataDir, "backends"))
	require.NoError(t, err)
	require.Empty(t, pre.Blockers)
	require.Empty(t, pre.Notes, "live index lag is not an update note")

	require.NoError(t, owner.Close())
}
