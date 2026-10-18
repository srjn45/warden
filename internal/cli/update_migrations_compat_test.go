package cli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/schema"
	"github.com/stretchr/testify/require"
)

func stubUpdateMigrationSeams(t *testing.T, target int, run func(context.Context, string, string, io.Writer) error) *int {
	t.Helper()
	oldSchema, oldRun := targetBinarySchema, runTargetMigrate
	calls := 0
	targetBinarySchema = func(context.Context, string) (int, error) { return target, nil }
	runTargetMigrate = func(ctx context.Context, bin, dir string, out io.Writer) error {
		calls++
		return run(ctx, bin, dir, out)
	}
	t.Cleanup(func() { targetBinarySchema, runTargetMigrate = oldSchema, oldRun })
	return &calls
}

func legacySchemaFixture(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	require.NoError(t, schema.Save(d, &schema.Ledger{SchemaVersion: 1, BinaryVersion: "10.0.1"}))
	return d
}

func TestUpdateMigrationsRunsTargetApplyAndVerifiesLedger(t *testing.T) {
	d := legacySchemaFixture(t)
	calls := stubUpdateMigrationSeams(t, 2, func(_ context.Context, _ string, dir string, _ io.Writer) error {
		return schema.Save(dir, &schema.Ledger{SchemaVersion: 2, BinaryVersion: "11.1.0"})
	})

	err := runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), d, "/fixture/warden", io.Discard)
	require.NoError(t, err)
	require.Equal(t, 1, *calls)

	l, err := schema.Load(d)
	require.NoError(t, err)
	require.Equal(t, 2, l.SchemaVersion)
}

func TestUpdateMigrationsFailureAndIncompleteApplyAreFatal(t *testing.T) {
	t.Run("apply failure", func(t *testing.T) {
		d := legacySchemaFixture(t)
		stubUpdateMigrationSeams(t, 2, func(context.Context, string, string, io.Writer) error {
			return errors.New("strict preflight refused")
		})
		err := runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), d, "/fixture/warden", io.Discard)
		require.ErrorContains(t, err, "previous binary and data are restored")
	})

	t.Run("ledger not advanced", func(t *testing.T) {
		d := legacySchemaFixture(t)
		stubUpdateMigrationSeams(t, 2, func(context.Context, string, string, io.Writer) error { return nil })
		err := runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), d, "/fixture/warden", io.Discard)
		require.ErrorContains(t, err, "ledger is at schema 1")
	})
}

func TestUpdateMigrationsSkipsSafeCasesAndRefusesInterrupted(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		calls := stubUpdateMigrationSeams(t, 2, func(context.Context, string, string, io.Writer) error {
			return errors.New("must not run")
		})
		err := runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), t.TempDir(), "/fixture/warden", io.Discard)
		require.NoError(t, err)
		require.Zero(t, *calls)
	})

	t.Run("older target", func(t *testing.T) {
		d := legacySchemaFixture(t)
		calls := stubUpdateMigrationSeams(t, 0, func(context.Context, string, string, io.Writer) error {
			return errors.New("must not run")
		})
		err := runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), d, "/fixture/warden", io.Discard)
		require.NoError(t, err)
		require.Zero(t, *calls)
	})

	t.Run("interrupted", func(t *testing.T) {
		d := legacySchemaFixture(t)
		l, err := schema.Load(d)
		require.NoError(t, err)
		l.InProgress = &schema.InProgress{Migration: "0002-schema2-cutover", Step: "promoting"}
		require.NoError(t, schema.Save(d, l))
		calls := stubUpdateMigrationSeams(t, 2, func(context.Context, string, string, io.Writer) error { return nil })
		err = runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), d, "/fixture/warden", io.Discard)
		require.ErrorContains(t, err, "was interrupted")
		require.Zero(t, *calls)
	})
}

func TestUpdateMigrationsPreservesSnapshotMarker(t *testing.T) {
	d := legacySchemaFixture(t)
	l, err := schema.Load(d)
	require.NoError(t, err)
	l.InProgress = &schema.InProgress{Step: "snapshot", Snapshot: "snapshots/fixture"}
	require.NoError(t, schema.Save(d, l))
	stubUpdateMigrationSeams(t, 2, func(_ context.Context, _ string, dir string, _ io.Writer) error {
		return schema.Save(dir, &schema.Ledger{SchemaVersion: 2, BinaryVersion: "11.1.0"})
	})

	require.NoError(t, runUpdateMigrations(context.Background(), filepath.Join(t.TempDir(), "config.yaml"), d, "/fixture/warden", io.Discard))
}
