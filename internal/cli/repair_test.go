package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestApplySessionRepairBackupAndIdempotence(t *testing.T) {
	ctx := context.Background()
	data := filepath.Join(t.TempDir(), "data")
	fs, err := store.NewFileStore(data)
	require.NoError(t, err)
	require.NoError(t, fs.Insert(ctx, &store.Session{ID: "a1", Name: "n-a1", Status: store.StatusWorking}))
	require.NoError(t, fs.Close(ctx))
	report, err := store.DiagnoseSessions(ctx, data)
	require.NoError(t, err)

	for i := 1; i <= 2; i++ {
		backup := filepath.Join(t.TempDir(), "backup")
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		var out bytes.Buffer
		cmd.SetOut(&out)
		require.NoError(t, applySessionRepair(cmd, data, backup, report))
		_, err := os.Stat(filepath.Join(backup, "sessions-db"))
		require.NoError(t, err)
		after, err := store.DiagnoseSessions(ctx, data)
		require.NoError(t, err)
		require.Len(t, after.Active, 1)
		require.Equal(t, "a1", after.Active[0].ID)
	}
}

func TestApplySessionRepairRefusesOwnedStoreBeforeBackup(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	fs, err := store.NewFileStore(data)
	require.NoError(t, err)
	defer fs.Close(context.Background())
	backup := filepath.Join(t.TempDir(), "backup")
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err = applySessionRepair(cmd, data, backup, &store.RecoveryReport{})
	require.ErrorIs(t, err, store.ErrStoreOwned)
	_, statErr := os.Stat(backup)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestRepairSessionsExposesDocumentedDryRunFlag(t *testing.T) {
	flag := newRepairSessionsCmd().Flag("dry-run")
	require.NotNil(t, flag)
}

func TestCopyTreePreservesPermissionsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	dst := filepath.Join(root, "backup")
	require.NoError(t, os.Mkdir(src, 0o750))
	file := filepath.Join(src, "record")
	require.NoError(t, os.WriteFile(file, []byte("metadata"), 0o640))
	require.NoError(t, os.Chmod(file, 0o640))
	require.NoError(t, os.Symlink("record", filepath.Join(src, "record-link")))

	require.NoError(t, copyTree(src, dst))
	info, err := os.Stat(filepath.Join(dst, "record"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	link, err := os.Readlink(filepath.Join(dst, "record-link"))
	require.NoError(t, err)
	require.Equal(t, "record", link)
}

func TestRepairAllCmd_FlagsAndValidation(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("data_dir: "+tmp+"\n"), 0o644))

	cmd1 := newRepairAllCmd()
	require.NotNil(t, cmd1.Flag("resolve-history"))
	require.NotNil(t, cmd1.Flag("backup-dir"))
	require.NotNil(t, cmd1.Flag("dry-run"))
	require.NotNil(t, cmd1.Flag("json"))
	require.NotNil(t, cmd1.Flag("yes"))

	// Non-live-wins policy must fail
	cmd1.SetArgs([]string{"--config=" + cfgPath, "--resolve-history=abort", "--yes"})
	err := cmd1.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported --resolve-history policy")

	// Without --yes and non-interactive stdin, must refuse
	cmd2 := newRepairAllCmd()
	cmd2.SetIn(&bytes.Buffer{})
	cmd2.SetArgs([]string{"--config=" + cfgPath, "--resolve-history=live-wins"})
	err = cmd2.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires confirmation")

	// With --yes on empty store, executes cleanly
	cmd3 := newRepairAllCmd()
	cmd3.SetArgs([]string{"--config=" + cfgPath, "--resolve-history=live-wins", "--yes"})
	err = cmd3.Execute()
	require.NoError(t, err)

	// Dry run also executes cleanly without --yes
	cmd4 := newRepairAllCmd()
	cmd4.SetArgs([]string{"--config=" + cfgPath, "--resolve-history=live-wins", "--dry-run"})
	err = cmd4.Execute()
	require.NoError(t, err)
}
