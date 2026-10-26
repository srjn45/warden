package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

func TestInitCmd_FreshDataDir(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")

	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--data-dir=" + dataDir})

	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), "warden data directory initialized:")

	// A fresh install is stamped directly at the current schema (warden-db) and
	// never builds the schema-1 per-store directories.
	l, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, schema.SchemaVersion, l.SchemaVersion)
	for _, legacy := range []string{"agents-db", "context", "inbox", "plans", "projects"} {
		require.NoDirExists(t, filepath.Join(dataDir, legacy), "fresh schema-2 init must not create legacy store dir %s", legacy)
	}
	require.Equal(t, version, l.BinaryVersion)
	require.Len(t, l.History, 1)
	require.Equal(t, schema.MigrationBaselineFresh, l.History[0].Migration)

	// Second run is idempotent
	var out2 bytes.Buffer
	cmd2 := newInitCmd()
	cmd2.SetOut(&out2)
	cmd2.SetArgs([]string{"--data-dir=" + dataDir})
	require.NoError(t, cmd2.Execute())
	require.Contains(t, out2.String(), "warden data directory ready:")
}

func TestInitCmd_LegacyDataDir(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	// Write legacy sentinel
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, ".sessions-filedb-imported"), []byte("ok\n"), 0o644))

	cmd := newInitCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--data-dir=" + dataDir, "--json"})

	err := cmd.Execute()
	require.ErrorContains(t, err, "requires the matching warden update transaction")
	// Init may stamp an unambiguous legacy baseline, but it must not attempt a
	// standalone schema-3 write. The paired updater owns that transition.
	l, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, schema.LegacyBaseline, l.SchemaVersion)
	require.Nil(t, l.InProgress)
	require.Equal(t, schema.MigrationBaselineLegacy, l.History[0].Migration)
	require.NoDirExists(t, filepath.Join(dataDir, schema.WardenDBDir))
	require.NoDirExists(t, filepath.Join(dataDir, "backups"))
}
