package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

func TestSchema3OwnershipVerificationCLI(t *testing.T) {
	old := verifySchema3Ownership
	t.Cleanup(func() { verifySchema3Ownership = old })
	var got []string
	verifySchema3Ownership = func(_ context.Context, dir, transaction, target, manifest string) error {
		got = []string{dir, transaction, target, manifest}
		return nil
	}
	cmd := newMigrateCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--verify-ownership", "--data-dir", "/data", "--transaction", "tx", "--target-sha", "sha256:target", "--manifest-sha", "sha256:manifest", "--json"})
	require.NoError(t, cmd.ExecuteContext(context.Background()))
	require.Equal(t, []string{"/data", "tx", "sha256:target", "sha256:manifest"}, got)
	require.JSONEq(t, `{"verified":true,"transaction":"tx"}`, out.String())

	cmd = newMigrateCmd()
	cmd.SetArgs([]string{"--verify-ownership", "--transaction", "tx"})
	require.ErrorContains(t, cmd.Execute(), "requires --transaction, --target-sha, and --manifest-sha")
}

func TestSchema3CLIPreviewAndMutationRefusal(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: 2}))
	db, err := wardendb.OpenOfflineStagingDir(filepath.Join(dir, "warden-db"))
	require.NoError(t, err)
	col, err := db.Collection("projects")
	require.NoError(t, err)
	_, _, err = col.InsertWithKey("p", map[string]any{"id": "p", "path": ""})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	ledger, err := os.ReadFile(filepath.Join(dir, schema.FileName))
	require.NoError(t, err)
	for _, action := range []string{"--check", "--plan"} {
		cmd := newMigrateCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{action, "--target", "3", "--data-dir", dir, "--json"})
		require.NoError(t, cmd.ExecuteContext(context.Background()))
		require.Contains(t, out.String(), `"policy": "ownership-v1"`)
		require.Contains(t, out.String(), `"target_digests"`)
	}
	for _, action := range []string{"--apply", "--resume", "--restore", "--staging-export"} {
		cmd := newMigrateCmd()
		cmd.SetArgs([]string{action, "--target", "3", "--data-dir", dir})
		require.ErrorIs(t, cmd.Execute(), migrate.ErrUpdaterTransactionRequired)
	}
	after, err := os.ReadFile(filepath.Join(dir, schema.FileName))
	require.NoError(t, err)
	require.Equal(t, ledger, after)
	_, err = os.Stat(filepath.Join(dir, "backups"))
	require.True(t, os.IsNotExist(err))
}
