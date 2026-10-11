package migrate

// The schema-2 cutover is intentionally a single journaled operation.  The
// staging directory is named from the immutable backup manifest, so retrying
// after a crash can only resume the same candidate; it can never adopt an
// unrelated warden-db directory.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
)

const Migration0002ID = "0002-schema2-cutover"

func init() { _ = Register(Migration0002) }

var Migration0002 = Migration{
	ID: Migration0002ID, From: 1, To: schema.Schema2, Kind: KindData,
	Run: runSchema2Cutover,
	Verify: func(env Env) error {
		if _, err := os.Stat(wardendb.Dir(env.DataDir)); err != nil {
			return fmt.Errorf("schema-2 target missing: %w", err)
		}
		return nil
	},
}

func runSchema2Cutover(env Env) error {
	ctx := env.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ledger, err := schema.Load(env.DataDir)
	if err != nil {
		return err
	}
	var manifest *Schema2Manifest
	if ledger.InProgress != nil && ledger.InProgress.Snapshot != "" {
		backup := ledger.InProgress.Snapshot
		if !filepath.IsAbs(backup) {
			backup = filepath.Join(env.DataDir, filepath.FromSlash(backup))
		}
		manifest, err = VerifySchema2Backup(backup)
		if err != nil {
			return fmt.Errorf("schema-2 resume refuses invalid backup: %w", err)
		}
	} else {
		run, err := BeginSchema2Locked(env.DataDir, env.BinaryVersion, time.Now())
		if err != nil {
			return err
		}
		manifest = run.Manifest
		rel, err := filepath.Rel(env.DataDir, manifest.Backup.Dir)
		if err != nil {
			return err
		}
		if err := env.Snapshot(filepath.ToSlash(rel)); err != nil {
			return fmt.Errorf("record schema-2 backup: %w", err)
		}
	}

	staging := filepath.Join(env.DataDir, ".schema2-staging-"+manifest.ID)
	target := wardendb.Dir(env.DataDir)
	if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
		if err := env.Step("staging"); err != nil {
			return err
		}
		if _, err := ExportSchema2Staging(ctx, env.DataDir, StagingExportOptions{TargetDir: staging, Now: time.Now()}); err != nil {
			return err
		}
		if _, err := VerifySchema2Equivalence(ctx, env.DataDir, staging); err != nil {
			return fmt.Errorf("schema-2 staging verification: %w", err)
		}
		if err := env.Step("promoting"); err != nil {
			return err
		}
		// staging and target are siblings, therefore Rename is same-filesystem
		// and atomic. Sync the parent to make the namespace update durable.
		if err := os.Rename(staging, target); err != nil {
			return fmt.Errorf("promote verified schema-2 staging: %w", err)
		}
		if err := syncDirectory(env.DataDir); err != nil {
			return err
		}
		if err := env.Step("promoted"); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("inspect schema-2 target: %w", err)
	} else if ledger.InProgress == nil || (ledger.InProgress.Step != "promoting" && ledger.InProgress.Step != "promoted") {
		return errors.New("schema-2 target appeared outside this journaled promotion; refusing to adopt it")
	}
	if _, err := VerifySchema2Equivalence(ctx, env.DataDir, target); err != nil {
		return fmt.Errorf("schema-2 promoted target verification: %w", err)
	}
	return nil
}

func syncDirectory(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
