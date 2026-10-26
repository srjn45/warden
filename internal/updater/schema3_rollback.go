package updater

import (
	"context"
	"fmt"
	"path/filepath"

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
	// The old process must acquire the owner lock itself. Never start while the
	// updater still owns it, and never treat absence of a supervisor as success.
	if err = t.close(); err != nil {
		return RollbackResult{}, err
	}
	if err = opts.Service.Restart(ctx); err != nil {
		return RollbackResult{}, fmt.Errorf("schema3: restart paired old service: %w", err)
	}
	waiter := &txn{opts: Options{ReadyTimeout: opts.ReadyTimeout}, svc: opts.Service, probe: opts.Prober, clock: opts.Clock}
	if err = waiter.waitReady(ctx, stripV(t.manifest.OldVersion), 2); err != nil {
		// Even a failed old readiness start is explicitly stopped. Leave the
		// verified restored generation and transaction evidence for recovery.
		if stopErr := opts.Service.Stop(ctx); stopErr != nil {
			return RollbackResult{}, fmt.Errorf("schema3: old readiness failed: %v; stopping old service failed: %w", err, stopErr)
		}
		return RollbackResult{}, fmt.Errorf("schema3: paired old readiness failed: %w", err)
	}
	l, err := schema.Load(t.dir)
	if err != nil || l.SchemaVersion != 2 || l.InProgress != nil {
		return RollbackResult{}, fmt.Errorf("schema3: paired old readiness ledger mismatch")
	}
	result := RollbackResult{
		PriorVersion: t.manifest.TargetVersion, TargetVersion: t.manifest.OldVersion,
		SchemaChanged: true, DataRestored: true,
		Message: "rollback complete: restored the selected transaction's full binary and data generation; verified old version and schema",
	}
	fmt.Fprintln(opts.Stdout, result.Message)
	return result, nil
}
