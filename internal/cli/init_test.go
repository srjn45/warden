package cli

import (
	"bytes"
	"encoding/json"
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

	// Ledger was created at schema 1
	l, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
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

	require.NoError(t, cmd.Execute())

	var res initOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &res))
	require.Equal(t, "ok", res.Status)
	require.Equal(t, dataDir, res.DataDir)
	require.Equal(t, 1, res.SchemaVersion)
	require.True(t, res.Stamped)

	// Ledger recorded baseline-legacy
	l, err := schema.Load(dataDir)
	require.NoError(t, err)
	require.Equal(t, 1, l.SchemaVersion)
	require.Equal(t, schema.MigrationBaselineLegacy, l.History[0].Migration)
}
