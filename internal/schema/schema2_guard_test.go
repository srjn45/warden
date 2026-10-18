package schema

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/adoption"
)

func TestGuard_Schema2Residue(t *testing.T) {
	// ledger-less dir holding warden-db: refuse, write nothing.
	d := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(d, WardenDBDir), 0o700))
	_, err := Boot(d, "v1")
	var ge *GuardError
	require.ErrorAs(t, err, &ge)
	require.Equal(t, VerdictSchema2Residue, ge.Verdict)
	_, statErr := os.Stat(Path(d))
	require.True(t, os.IsNotExist(statErr), "refused boot must not stamp a ledger")
	require.ErrorAs(t, Guard(d), &ge)
	require.Contains(t, ge.Error(), "Nothing was opened or changed")

	// schema-1 ledger + warden-db: refuse.
	require.NoError(t, Save(d, &Ledger{SchemaVersion: 1}))
	_, err = Boot(d, "v1")
	require.ErrorAs(t, err, &ge)
	require.Equal(t, VerdictSchema2Residue, ge.Verdict)

	// schema-2 ledger: a schema-1 binary (the previous major) says "newer", with
	// schema-2 guidance, and does so from the ledger alone — before any store.
	require.NoError(t, Save(d, &Ledger{SchemaVersion: 2, BinaryVersion: "v99"}))
	err = check(d, &Ledger{SchemaVersion: 2, BinaryVersion: "v99"}, 1, 1)
	require.ErrorAs(t, err, &ge)
	require.Equal(t, VerdictDataNewer, ge.Verdict)
	require.Contains(t, ge.Error(), "schema 2 (warden-db)")
	require.Contains(t, ge.Error(), "no automatic downgrade")

	// ...while this (schema-2) binary starts on it.
	res, err := Boot(d, "v1")
	require.NoError(t, err)
	require.Equal(t, 2, res.Ledger.SchemaVersion)
}

func TestGuard_SchemaOneDataNeedsCutover(t *testing.T) {
	d := t.TempDir()
	require.NoError(t, Save(d, &Ledger{SchemaVersion: 1}))
	var ge *GuardError
	for name, err := range map[string]error{"Guard": Guard(d), "Boot": func() error { _, e := Boot(d, "v1"); return e }()} {
		require.ErrorAs(t, err, &ge, name)
		require.Equal(t, VerdictNeedsMigration, ge.Verdict, name)
		require.Contains(t, ge.Error(), "warden migrate --apply", name)
		require.Contains(t, ge.Error(), "nothing is deleted", name)
	}
}

func TestGuard_CleanDirs(t *testing.T) {
	d := t.TempDir()
	require.NoError(t, Guard(d), "empty dir passes and is not stamped")
	_, err := os.Stat(Path(d))
	require.True(t, os.IsNotExist(err))
	require.NoError(t, Save(d, &Ledger{SchemaVersion: Schema2}))
	require.NoError(t, Guard(d))
}

func TestBoot_RecordsRuntimeBootOnSchema2Only(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Save(dir, &Ledger{SchemaVersion: Schema2, BinaryVersion: "11", History: []HistoryEntry{}}))
	_, err := Boot(dir, "11.0.0")
	require.NoError(t, err)
	log, err := adoption.Read(dir)
	require.NoError(t, err)
	require.Equal(t, 1, log.Count(adoption.RuntimeBoot, time.Time{}))
}
