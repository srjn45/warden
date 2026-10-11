package schema

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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

	// schema-2 ledger: an old binary says "newer", with schema-2 guidance.
	require.NoError(t, Save(d, &Ledger{SchemaVersion: 2, BinaryVersion: "v99"}))
	_, err = Boot(d, "v1")
	require.ErrorAs(t, err, &ge)
	require.Equal(t, VerdictDataNewer, ge.Verdict)
	require.Contains(t, ge.Error(), "schema 2 (warden-db)")
	require.Contains(t, ge.Error(), "no automatic downgrade")
}

func TestGuard_CleanDirs(t *testing.T) {
	d := t.TempDir()
	require.NoError(t, Guard(d), "empty dir passes and is not stamped")
	_, err := os.Stat(Path(d))
	require.True(t, os.IsNotExist(err))
	require.NoError(t, Save(d, &Ledger{SchemaVersion: 1}))
	require.NoError(t, Guard(d))
}
