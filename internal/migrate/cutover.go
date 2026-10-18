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

	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
)

const Migration0002ID = "0002-schema2-cutover"

// ClassSchema2CutoverPending marks the informational finding that a schema-1
// data dir still has to be cut over. It is a Repairable (non-clean) finding so
// an updater that predates the cutover — and so cannot run it — blocks BEFORE
// swapping anything; an updater that does run the cutover recognises the class
// and treats it as a note.
const ClassSchema2CutoverPending = "schema2_cutover_pending"

func init() { _ = Register(Migration0002) }

// ErrRefusedBeforeChange marks a cutover failure that happened before any live
// state was touched: strict preflight, the staging export, or the equivalence
// proof of the staged copy. The runner uses it to put the ledger back exactly as
// it found it (a pure refusal must not leave a "migration interrupted" journal
// that stops the old binary from starting), whereas a failure after promotion
// stays journaled for `migrate --resume` / `--restore`.
var ErrRefusedBeforeChange = errors.New("migrate: refused before any live change")

var Migration0002 = Migration{
	ID: Migration0002ID, From: 1, To: schema.Schema2, Kind: KindData,
	Check: checkSchema2Cutover,
	Run:   runSchema2Cutover,
	Verify: func(env Env) error {
		if _, err := os.Stat(wardendb.Dir(env.DataDir)); err != nil {
			return fmt.Errorf("schema-2 target missing: %w", err)
		}
		return nil
	},
}

// checkSchema2Cutover is the read-only preflight of the cutover as `migrate
// --check` and the updater see it. It runs the same strict preflight as
// `migrate --plan`, with one difference: the updater checks BEFORE it stops the
// daemon, so "a daemon owns the data dir" and the index lag a live daemon
// causes are expected there and reported as notes. The cutover itself
// (BeginSchema2) re-runs the strict preflight under the ownership lock with no
// such leniency.
func checkSchema2Cutover(env Env) []Finding {
	ctx := env.Context
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := PreflightSchema2(ctx, env.DataDir, time.Now())
	if err != nil {
		return []Finding{{Severity: SeverityBlocking, Class: string(ClassUnverifiable), Store: "warden-db",
			Message: "schema-2 preflight failed: " + err.Error(), Details: strictRemediation(ClassUnverifiable)}}
	}
	owned := false
	for _, f := range plan.Findings {
		if f.Severity == SeverityBlocking && (f.Class == string(ClassOwned) || f.Store == ownerlock.FileName) {
			owned = true
		}
	}
	out := []Finding{{
		Severity: SeverityRepairable, Class: ClassSchema2CutoverPending, Store: "warden-db",
		Message: "the data dir is at schema 1 and must be cut over to the single warden-db (schema 2) before this release can run; " +
			"`wd update` does this for you, a manual install must run `warden migrate --apply` with the daemon stopped",
		Command: "warden daemon stop && warden migrate --apply",
	}}
	for _, f := range plan.Findings {
		if owned && f.Severity == SeverityBlocking && (f.Class == string(ClassOwned) || f.Store == ownerlock.FileName || f.Class == string(ClassStaleDerivedState)) {
			f.Severity = SeverityAuto
			f.Message += " (a running daemon is expected here; the updater stops it before the cutover, which re-verifies strictly)"
		}
		out = append(out, f)
	}
	return out
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
	freshRun := false
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
			return fmt.Errorf("%w: %w", ErrRefusedBeforeChange, err)
		}
		freshRun = true
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
		refuse := func(err error) error {
			if !freshRun || ctx.Err() != nil {
				// A resumed run keeps its journal and staging; so does an
				// interrupted one (cancellation is a crash, not a refusal).
				return err
			}
			// Our own scratch directory from this very call; the legacy sources
			// and the verified backup are untouched.
			_ = os.RemoveAll(staging)
			_ = os.RemoveAll(filepath.Join(env.DataDir, "quarantine", "savings"))
			return fmt.Errorf("%w: %w", ErrRefusedBeforeChange, err)
		}
		rep, err := ExportSchema2Staging(ctx, env.DataDir, StagingExportOptions{TargetDir: staging, Now: time.Now()})
		if err != nil {
			return refuse(err)
		}
		if _, err := VerifySchema2Equivalence(ctx, env.DataDir, staging); err != nil {
			return refuse(fmt.Errorf("schema-2 staging verification: %w", err))
		}
		if err := env.Step("promoting"); err != nil {
			return err
		}
		if len(rep.Quarantined) > 0 {
			if err := writeSavingsQuarantine(env.DataDir, rep.Quarantined); err != nil {
				return refuse(fmt.Errorf("write savings quarantine: %w; ensure data dir is writable and has free space", err))
			}
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
	if eq, err := VerifySchema2Equivalence(ctx, env.DataDir, target); err != nil {
		return fmt.Errorf("schema-2 promoted target verification: %w", err)
	} else if len(eq.Quarantined) > 0 {
		if err := writeSavingsQuarantine(env.DataDir, eq.Quarantined); err != nil {
			return fmt.Errorf("write savings quarantine on resume: %w", err)
		}
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
