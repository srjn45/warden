package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/migrate/schema3"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/wardendb"
	"github.com/stretchr/testify/require"
)

type pairedRestoreService struct {
	stopped bool
	stopErr error
	starts  int
	onStart func() error
}

func (s *pairedRestoreService) State(context.Context) ServiceState {
	return ServiceState{Kind: ServiceSystemd, Active: !s.stopped}
}
func (s *pairedRestoreService) Stop(context.Context) error {
	if s.stopErr != nil {
		return s.stopErr
	}
	s.stopped = true
	return nil
}
func (s *pairedRestoreService) Restart(context.Context) error {
	s.starts++
	s.stopped = false
	if s.onStart != nil {
		return s.onStart()
	}
	return nil
}
func (s *pairedRestoreService) Exited(context.Context) bool      { return s.stopped }
func (*pairedRestoreService) Diagnostics(context.Context) string { return "isolated service" }

func TestSchema3RollbackSelectsPairedTransactionAfterNewWrites(t *testing.T) {
	ctx := context.Background()
	tr, old, target := schema3Fixture(t)
	beginSchema3Fixture(t, tr, old, target)
	require.NoError(t, tr.promote(ctx))
	require.NoError(t, tr.finish(ctx, Health{Status: "ok", Version: "target-test", SchemaVersion: 3}, true))
	// A real target-only collection and post-update business write must both
	// disappear when restoring the complete old generation.
	db, err := wardendb.OpenOfflineStagingDir(filepath.Join(tr.dir, "warden-db"))
	require.NoError(t, err)
	col, err := db.Collection("projects")
	require.NoError(t, err)
	_, _, err = col.InsertWithKey("later", map[string]any{"id": "later", "name": "post-update"})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	require.NoError(t, os.WriteFile(old, []byte("target binary"), 0755))
	require.NoError(t, os.WriteFile(old+".bak", []byte("unrelated backup binary"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(tr.dir, "backups", "2999-unrelated"), 0700))
	require.NoError(t, tr.close())
	svc := &pairedRestoreService{}
	svc.onStart = func() error {
		// Service startup must occur after updater releases exclusive ownership.
		lock, err := ownerlock.Acquire(tr.dir, ownerlock.Info{Kind: ownerlock.KindDaemon})
		if err != nil {
			return err
		}
		return lock.Release()
	}
	probe := &fakeProbe{fn: func() (Health, error) {
		if svc.stopped {
			return Health{}, errors.New("stopped")
		}
		return Health{Status: "ok", Version: "old-test", SchemaVersion: 2}, nil
	}}
	opts := RollbackOptions{
		DataDir: tr.dir, InstallBin: old, TransactionID: tr.id, AcceptDataLoss: true,
		Service: svc, Prober: probe, Clock: newFakeClock(),
	}
	result, err := Rollback(opts)
	require.NoError(t, err)
	require.True(t, result.DataRestored)
	require.Equal(t, "old-test", result.TargetVersion)
	require.Equal(t, 1, svc.starts)
	got, err := os.ReadFile(old)
	require.NoError(t, err)
	require.Equal(t, "old binary", string(got))
	g, _, err := schema3.Read(ctx, filepath.Join(tr.dir, "warden-db"))
	require.NoError(t, err)
	require.NotContains(t, g["projects"], "later")
	require.NotContains(t, g, "plan_tasks")
	require.DirExists(t, filepath.Join(tr.dir, ".schema3-abandoned-"+tr.id))
}

func TestSchema3RollbackRefusesUnsafeSelectionOrStop(t *testing.T) {
	for _, mode := range []string{"no transaction", "no consent", "stop failed", "still answering", "wrong old version", "wrong old schema"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			tr, old, target := schema3Fixture(t)
			beginSchema3Fixture(t, tr, old, target)
			require.NoError(t, tr.promote(ctx))
			require.NoError(t, os.WriteFile(old, []byte("target binary"), 0755))
			require.NoError(t, tr.close())
			svc := &pairedRestoreService{}
			if mode == "stop failed" {
				svc.stopErr = errors.New("cannot stop")
			}
			probe := &fakeProbe{fn: func() (Health, error) {
				if svc.stopped && mode != "still answering" {
					return Health{}, errors.New("stopped")
				}
				h := Health{Status: "ok", Version: "old-test", SchemaVersion: 2}
				if mode == "wrong old version" {
					h.Version = "wrong"
				}
				if mode == "wrong old schema" {
					h.SchemaVersion = 3
				}
				return h, nil
			}}
			opts := RollbackOptions{DataDir: tr.dir, InstallBin: old, TransactionID: tr.id, AcceptDataLoss: true, Force: true, Service: svc, Prober: probe, Clock: newFakeClock(), ReadyTimeout: time.Second}
			if mode == "no transaction" {
				opts.TransactionID = ""
			}
			if mode == "no consent" {
				opts.AcceptDataLoss = false
			}
			_, err := Rollback(opts)
			require.Error(t, err)
			if mode == "wrong old version" || mode == "wrong old schema" {
				require.True(t, svc.stopped, "failed readiness must stop the restored daemon")
			} else {
				got, e := os.ReadFile(old)
				require.NoError(t, e)
				require.Equal(t, "target binary", string(got), "refused rollback must not replace binary")
				require.Zero(t, svc.starts)
			}
		})
	}
}

func TestSchema3RestoreResumeDoesNotRequireTargetBinary(t *testing.T) {
	ctx := context.Background()
	tr, old, target := schema3Fixture(t)
	beginSchema3Fixture(t, tr, old, target)
	require.NoError(t, tr.promote(ctx))
	tr.fault = func(p string) error {
		if p == "after/restore-binary" {
			return errors.New("power loss after paired binary replacement")
		}
		return nil
	}
	require.Error(t, tr.restore(ctx, old))
	tr.fault = nil
	require.NoError(t, os.Remove(target))
	require.NoError(t, tr.loadForRestore())
	require.NoError(t, tr.restore(ctx, old))
	got, err := os.ReadFile(old)
	require.NoError(t, err)
	require.Equal(t, "old binary", string(got))
}
