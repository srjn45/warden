package updater

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/srjn45/warden/internal/schema"
)

// VerifySchema3Ownership is the target-side half of schema-3 update readiness.
// It is intentionally read-only: the updater keeps finalization authority, but
// the newly started binary proves that it is running the exact sealed target
// generation before the updater clears the ledger's pending transaction.
func VerifySchema3Ownership(ctx context.Context, dataDir, transaction, targetSHA, manifestSHA string) error {
	t, err := schema3Path(dataDir, transaction)
	if err != nil {
		return err
	}
	if targetSHA == "" || manifestSHA == "" {
		return fmt.Errorf("schema3: target and manifest hashes are required")
	}
	t.manifestHash = manifestSHA
	if got, err := schema3HashFile(t.planPath()); err != nil || got != manifestSHA {
		if err != nil {
			return fmt.Errorf("schema3: read planned manifest: %w", err)
		}
		return fmt.Errorf("schema3: planned manifest hash mismatch")
	}
	if err := t.verifyBundle(); err != nil {
		return err
	}
	if t.manifest.TargetBinaryHash != targetSHA {
		return fmt.Errorf("schema3: target binary hash does not match transaction")
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("schema3: locate running binary: %w", err)
	}
	bin, err = filepath.EvalSymlinks(bin)
	if err != nil {
		return fmt.Errorf("schema3: resolve running binary: %w", err)
	}
	if got, err := schema3HashFile(bin); err != nil || got != targetSHA {
		if err != nil {
			return fmt.Errorf("schema3: hash running binary: %w", err)
		}
		return fmt.Errorf("schema3: running binary does not match transaction target")
	}
	if err := t.readJournal(); err != nil {
		return err
	}
	if !t.done("ledger_committed") || t.started("restore_staged") {
		return fmt.Errorf("schema3: transaction is not awaiting target readiness")
	}
	l, err := schema.Load(t.dir)
	if err != nil {
		return err
	}
	if l.SchemaVersion != 3 || l.BinaryVersion != t.manifest.TargetVersion || l.InProgress == nil || *l.InProgress != *t.progress("ledger_committed") {
		return fmt.Errorf("schema3: ledger is not bound to this readiness transaction")
	}
	return t.verifyTarget(ctx, filepath.Join(t.dir, "warden-db"))
}
