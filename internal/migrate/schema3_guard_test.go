package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/migrate/schema3"
	"github.com/srjn45/warden/internal/schema"
	"github.com/stretchr/testify/require"
)

func TestSchema3StandaloneMutationGuardLeavesSourceUnchanged(t *testing.T) {
	for _, format := range []int{2, 3} {
		dir := t.TempDir()
		require.NoError(t, schema.Save(dir, &schema.Ledger{SchemaVersion: format, InProgress: &schema.InProgress{Migration: schema3.MigrationID, TransactionID: "txn", Snapshot: "never-overlay-this"}}))
		before, err := os.ReadFile(filepath.Join(dir, schema.FileName))
		require.NoError(t, err)
		env := Env{DataDir: dir, Context: context.Background()}
		runner := NewRunner(nil)
		require.ErrorIs(t, runner.Apply(env, 3), ErrUpdaterTransactionRequired)
		require.ErrorIs(t, runner.Resume(env), ErrUpdaterTransactionRequired)
		require.ErrorIs(t, runner.Restore(env), ErrUpdaterTransactionRequired)
		after, err := os.ReadFile(filepath.Join(dir, schema.FileName))
		require.NoError(t, err)
		require.Equal(t, before, after)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Len(t, entries, 1, "even adoption logs must not be written on standalone refusal")
	}
}
