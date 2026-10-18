package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/updater"
	"github.com/stretchr/testify/require"
)

// Representative v11.1.0 `migrate --check --json` findings, including the
// exact fields emitted by the shipped cutover and strict preflight code.
const v1110CompatReport = `{"data_dir":"/tmp/x","current_version":1,"target_version":2,"clean":false,"findings":[
{"severity":"repairable","class":"schema2_cutover_pending","store":"warden-db","message":"the data dir is at schema 1 and must be cut over to the single warden-db (schema 2) before this release can run; ` + "`wd update`" + ` does this for you, a manual install must run ` + "`warden migrate --apply`" + ` with the daemon stopped","command":"warden daemon stop && warden migrate --apply"},
{"severity":"auto","store":"owner","message":"owner lock will be taken automatically"},
{"severity":"repairable","store":"foo","message":"unclassified path in the data dir; preserved in place and not migrated"},
{"severity":"repairable","class":"corruption","store":"savings/ledger.jsonl","message":"savings/ledger.jsonl: line 2 (offset 91): malformed JSON record; will be quarantined during cutover (sha256:0123456789ab)","details":"malformed savings records will be preserved in quarantine/savings/ during migration; exact raw bytes and provenance remain recoverable"}
]}`

func fakeTargetBin(t *testing.T, report string, exit string) string {
	t.Helper()
	dir := t.TempDir()
	rep := filepath.Join(dir, "report.json")
	require.NoError(t, os.WriteFile(rep, []byte(report), 0o600))
	bin := filepath.Join(dir, "warden")
	script := "#!/bin/sh\ncat " + rep + "\nexit " + exit + "\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	return bin
}

func TestTargetPreflightAcceptsV1110CompatibleReport(t *testing.T) {
	t.Parallel()
	for _, exit := range []string{"0", "1"} {
		bin := fakeTargetBin(t, v1110CompatReport, exit)
		res, err := targetBinaryPreflight(context.Background(), bin, t.TempDir())
		require.NoError(t, err)
		require.Empty(t, res.Blockers)
		require.Empty(t, res.RepairCommand)
		require.Equal(t, []string{
			"warden-db: schema-2 cutover will run after the binary swap",
			"owner lock will be taken automatically",
			"foo: unclassified path in the data dir; preserved in place and not migrated",
			"savings/ledger.jsonl: line 2 (offset 91): malformed JSON record; will be quarantined during cutover (sha256:0123456789ab)",
		}, res.Notes)
	}
}

func TestTargetPreflightStillBlocksGenuineProblems(t *testing.T) {
	t.Parallel()
	report := `{"findings":[
{"severity":"repairable","class":"schema2_cutover_pending","store":"warden-db","message":"pending"},
{"severity":"repairable","store":"context","message":"latent conflict","command":"warden repair all --resolve-history=live-wins"},
{"severity":"blocking","store":"plans","message":"corrupt segment"}]}`
	bin := fakeTargetBin(t, report, "1")
	res, err := targetBinaryPreflight(context.Background(), bin, t.TempDir())
	require.NoError(t, err)
	require.Equal(t, []string{"context: latent conflict", "plans: corrupt segment"}, res.Blockers)
	require.Equal(t, "warden repair all --resolve-history=live-wins", res.RepairCommand)
	require.Len(t, res.Notes, 1)
}

func TestUpdateRecoveryGuidanceOnlyForBackendFailures(t *testing.T) {
	t.Parallel()
	unrelated := &updater.PreflightError{Blockers: []string{"plans: corrupt segment"}}
	require.NotContains(t, updateRecoveryGuidance(unrelated).Error(), "repair backends")

	backend := &updater.PreflightError{Blockers: []string{"backends: registry unreadable"}, RepairCommand: "warden repair backends"}
	require.Contains(t, updateRecoveryGuidance(backend).Error(), "repair backends")

	byPath := &updater.PreflightError{Blockers: []string{"/d/backends/x: damaged"}}
	require.Contains(t, updateRecoveryGuidance(byPath).Error(), "repair backends")
}
