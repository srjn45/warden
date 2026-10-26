package updater

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/srjn45/warden/internal/schema"
)

// rollbackSchema3 is the public rollback adapter for the private updater
// transaction. It is deliberately separate from the generic snapshot path.
func rollbackSchema3(ctx context.Context, opts RollbackOptions) (RollbackResult, error) {
	if !opts.AcceptDataLoss {
		return RollbackResult{}, fmt.Errorf("schema3: rollback discards all writes since the selected update; explicit --accept-data-loss required (--yes does not authorize data loss)")
	}
	t, err := schema3Path(opts.DataDir, opts.TransactionID)
	if err != nil {
		return RollbackResult{}, err
	}
	// Read-only preflight fails before touching a service for missing/tampered
	// bundles. Repeat verification under exclusive ownership before restoration.
	t.manifestHash, err = schema3HashFile(t.planPath())
	if err != nil {
		return RollbackResult{}, err
	}
	if err = t.verifyBundle(); err != nil {
		return RollbackResult{}, err
	}
	bin, err := filepath.Abs(opts.InstallBin)
	if err != nil {
		return RollbackResult{}, err
	}
	if bin != t.manifest.BinaryPath {
		return RollbackResult{}, fmt.Errorf("schema3: installed binary path differs from selected transaction")
	}
	if err = acquireSchema3(ctx, t, opts.Service, opts.Prober); err != nil {
		return RollbackResult{}, err
	}
	defer func() {
		if t.lock != nil {
			_ = t.close()
		}
	}()
	if err = t.loadForRestore(); err != nil {
		return RollbackResult{}, err
	}
	if err = t.restore(ctx, bin); err != nil {
		return RollbackResult{}, err
	}
	if err = restartPairedOld(ctx, t, opts.Service, opts.Prober, opts.Clock, opts.ReadyTimeout); err != nil {
		return RollbackResult{}, err
	}
	result := RollbackResult{
		PriorVersion: t.manifest.TargetVersion, TargetVersion: t.manifest.OldVersion,
		SchemaChanged: true, DataRestored: true,
		Message: "rollback complete: restored the selected transaction's full binary and data generation; verified old version and schema",
	}
	fmt.Fprintln(opts.Stdout, result.Message)
	return result, nil
}

// restartPairedOld starts the restored old binary and proves it is the exact
// old version on schema 2. The updater must hand over the owner lock first, so
// the old process acquires it itself, and absence of a supervisor is a failure.
// A failed old readiness leaves the service stopped with the verified restored
// generation and transaction evidence intact.
func restartPairedOld(ctx context.Context, t *schema3Transaction, svc ServiceController, probe Prober, clock Clock, ready time.Duration) error {
	if err := t.close(); err != nil {
		return err
	}
	if err := svc.Restart(ctx); err != nil {
		return fmt.Errorf("schema3: restart paired old service: %w", err)
	}
	waiter := &txn{opts: Options{ReadyTimeout: ready}, svc: svc, probe: probe, clock: clock}
	if err := waiter.waitReady(ctx, stripV(t.manifest.OldVersion), 2); err != nil {
		if stopErr := svc.Stop(ctx); stopErr != nil {
			return fmt.Errorf("schema3: old readiness failed: %v; stopping old service failed: %w", err, stopErr)
		}
		return fmt.Errorf("schema3: paired old readiness failed: %w", err)
	}
	l, err := schema.Load(t.dir)
	if err != nil || l.SchemaVersion != 2 || l.InProgress != nil {
		return fmt.Errorf("schema3: paired old readiness ledger mismatch")
	}
	return nil
}
