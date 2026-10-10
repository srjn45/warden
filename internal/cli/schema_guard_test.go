package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/srjn45/warden/internal/schema"
)

// runDaemonRefused boots `warden daemon` against a data dir holding ledger and
// returns the startup error. The guard must stop it before any store opens.
func runDaemonRefused(t *testing.T, ledger schema.Ledger) (dataDir string, err error) {
	t.Helper()
	dataDir = t.TempDir()
	require.NoError(t, schema.Save(dataDir, &ledger))
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf("data_dir: %q\naddr: 127.0.0.1:0\n", dataDir)), 0o600))

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"daemon", "--config", cfgPath})
	return dataDir, root.Execute()
}

func TestDaemonBootGuardRefusesBeforeOpeningStores(t *testing.T) {
	cases := map[string]struct {
		ledger  schema.Ledger
		verdict schema.Verdict
	}{
		"data newer than binary": {
			schema.Ledger{SchemaVersion: schema.SchemaVersion + 1, BinaryVersion: "99.0.0"}, schema.VerdictDataNewer},
		"interrupted migration": {
			schema.Ledger{SchemaVersion: schema.SchemaVersion, InProgress: &schema.InProgress{Migration: "0002-x"}}, schema.VerdictInterrupted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir, err := runDaemonRefused(t, tc.ledger)
			var ge *schema.GuardError
			require.ErrorAs(t, err, &ge)
			require.Equal(t, tc.verdict, ge.Verdict)
			// Nothing was opened, imported or created: only the ledger (and the
			// released ownership lock) may exist.
			for _, store := range []string{"agents-db", "sessions-db", "context", "inbox", "pipelines-db", "backends"} {
				require.NoDirExists(t, filepath.Join(dataDir, store), "store %s was touched by a refused boot", store)
			}
		})
	}
}

func TestBinarySchemaVersion(t *testing.T) {
	script := func(body string) string {
		p := filepath.Join(t.TempDir(), "warden")
		require.NoError(t, os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
		return p
	}
	ctx := context.Background()

	v, err := binarySchemaVersion(ctx, script(`[ "$1 $2" = "version --json" ] || exit 2
echo '{"version":"9.29.0","schema_version":7,"min_schema":5}'`))
	require.NoError(t, err)
	require.Equal(t, 7, v)

	// A release that predates the ledger has no schema_version: 0.
	v, err = binarySchemaVersion(ctx, script(`echo '{"version":"9.20.0"}'`))
	require.NoError(t, err)
	require.Zero(t, v)

	_, err = binarySchemaVersion(ctx, script(`exit 1`))
	require.Error(t, err)
	_, err = binarySchemaVersion(ctx, script(`echo not-json`))
	require.Error(t, err)
}
