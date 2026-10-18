package schema

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/srjn45/warden/internal/adoption"
)

// Schema2 is the data format whose authority is the single warden-db store
// (docs/specs/2026-10-10-warden-db-schema-2-migration-acceptance.md). A binary
// whose SchemaVersion is below it must never open a data dir that is at, or is
// becoming, schema 2: it would start schema-1 stores beside warden-db and fork
// the data.
const Schema2 = 2

// WardenDBDir is the schema-2 store directory inside the data dir. It mirrors
// wardendb.DirName (this package cannot import wardendb); a migrate test pins
// the two together.
const WardenDBDir = "warden-db"

// Verdict is one row of the daemon boot guard table (spec §3).
type Verdict int

const (
	// VerdictStart: data and binary agree — start.
	VerdictStart Verdict = iota
	// VerdictDataNewer: the data was written by a newer warden.
	VerdictDataNewer
	// VerdictNeedsMigration: data is older but this binary can migrate it.
	VerdictNeedsMigration
	// VerdictUnsupported: data is older than this binary can migrate from.
	VerdictUnsupported
	// VerdictInterrupted: the journal shows a migration that never finished.
	VerdictInterrupted
	// VerdictSchema2Residue: warden-db exists but the ledger does not say
	// schema 2 (absent, or still schema 1). Whatever left it — an aborted
	// migration, a restored ledger, a manual copy — the data dir has two
	// candidate authorities, and this binary will not pick one.
	VerdictSchema2Residue
)

func (v Verdict) String() string {
	switch v {
	case VerdictStart:
		return "start"
	case VerdictDataNewer:
		return "data_newer"
	case VerdictNeedsMigration:
		return "needs_migration"
	case VerdictUnsupported:
		return "unsupported_upgrade"
	case VerdictInterrupted:
		return "interrupted_migration"
	case VerdictSchema2Residue:
		return "schema2_residue"
	}
	return "unknown"
}

// Decide evaluates the guard table for a data dir at schema `data` against a
// binary that writes `binary` and can migrate from `minSchema`. An interrupted
// migration outranks every version comparison: the version in the ledger says
// nothing reliable about half-migrated data.
func Decide(data, binary, minSchema int, inProgress bool) Verdict {
	switch {
	case inProgress:
		return VerdictInterrupted
	case data == binary:
		return VerdictStart
	case data > binary:
		return VerdictDataNewer
	case data >= minSchema:
		return VerdictNeedsMigration
	default:
		return VerdictUnsupported
	}
}

// GuardError is a boot refusal. Its message is the operator's next step.
type GuardError struct {
	Verdict Verdict
	DataDir string
	Ledger  Ledger
	// Binary and Min are the refusing binary's SchemaVersion and MinSchema.
	Binary, Min int
}

func (e *GuardError) Error() string {
	head := fmt.Sprintf("refusing to start: data dir %s is at schema %d, this warden reads schema %d",
		e.DataDir, e.Ledger.SchemaVersion, e.Binary)
	switch e.Verdict {
	case VerdictDataNewer:
		by := ""
		if e.Ledger.BinaryVersion != "" {
			by = " (" + e.Ledger.BinaryVersion + ")"
		}
		if e.Ledger.SchemaVersion >= Schema2 {
			return head + fmt.Sprintf("\nthe data was migrated to schema 2 (warden-db) by a newer warden%s; "+
				"this binary only knows the schema-1 stores and will not open them — it would fork the data. "+
				"Upgrade this binary (`wd update`). Schema 2 has no automatic downgrade; the pre-migration "+
				"copy of the schema-1 stores is under %s", by, filepath.Join(e.DataDir, "backups"))
		}
		return head + fmt.Sprintf("\nthe data was written by a newer warden%s; upgrade this binary (`wd update`) — "+
			"an older warden must not open it", by)
	case VerdictNeedsMigration:
		if e.Binary >= Schema2 && e.Ledger.SchemaVersion < Schema2 {
			return head + "\nthe data is still on the schema-1 stores and must be cut over to the single warden-db first. " +
				"With the daemon stopped, run `warden migrate --plan` to inspect, then `warden migrate --apply` " +
				"(`wd update` from a release that knows the cutover does this for you). " +
				"A verified, immutable copy of the schema-1 stores is taken first and nothing is deleted"
		}
		return head + "\nthe data needs migrating first: run `warden migrate` (or `wd update`, which migrates for you)"
	case VerdictUnsupported:
		return head + fmt.Sprintf("\nunsupported direct upgrade: this warden can migrate from schema %d at the oldest; "+
			"upgrade through the intermediate releases (`wd update --plan` shows the path)", e.Min)
	case VerdictInterrupted:
		ip := e.Ledger.InProgress
		what := "a migration"
		if ip != nil && ip.Migration != "" {
			what = "migration " + ip.Migration
			if ip.Step != "" {
				what += " (step " + ip.Step + ")"
			}
		}
		return fmt.Sprintf("refusing to start: %s on data dir %s was interrupted\n"+
			"finish it with `warden migrate --resume`, or go back to the pre-migration snapshot with `warden migrate --restore`",
			what, e.DataDir)
	case VerdictSchema2Residue:
		return fmt.Sprintf("refusing to start: data dir %s holds %s/ but its schema ledger is at schema %d, not %d\n"+
			"this looks like a schema-2 migration that did not finish, or a ledger restored over migrated data. "+
			"Nothing was opened or changed. Run `warden migrate --plan` to inspect; do not delete %s/ or the "+
			"legacy stores by hand — the pre-migration backup under %s is the rollback source",
			e.DataDir, WardenDBDir, e.Ledger.SchemaVersion, Schema2, WardenDBDir, filepath.Join(e.DataDir, "backups"))
	}
	return head
}

// Check applies the boot guard table to a ledger using this binary's embedded
// SchemaVersion and MinSchema. nil means start.
func Check(dataDir string, l *Ledger) error {
	return check(dataDir, l, SchemaVersion, MinSchema)
}

func check(dataDir string, l *Ledger, binary, minSchema int) error {
	v := Decide(l.SchemaVersion, binary, minSchema, l.MigrationInterrupted())
	// Two candidate authorities outrank both "start" and "needs migration": a
	// cutover would refuse to merge into an existing warden-db, so pointing the
	// operator at `migrate --apply` here would be wrong. An interrupted
	// migration still outranks this (its journal explains the warden-db).
	if (v == VerdictStart || v == VerdictNeedsMigration) && l.SchemaVersion < Schema2 && WardenDBPresent(dataDir) {
		v = VerdictSchema2Residue
	}
	if v == VerdictStart {
		return nil
	}
	return &GuardError{Verdict: v, DataDir: dataDir, Ledger: *l, Binary: binary, Min: minSchema}
}

// BootResult is what the daemon learns from the guard on a permitted start.
type BootResult struct {
	Ledger *Ledger
	// Stamped is true when this boot wrote the baseline ledger.
	Stamped bool
	// StampErr is set when the baseline was inferred but could not be written.
	// Boot proceeds on the inferred ledger — an install that ran before the
	// ledger existed must keep running — and the stamp is retried next boot.
	StampErr error
}

// Boot is the daemon's hook: stamp a ledger-less data dir, then apply the guard
// table. It runs before any store is opened and performs no migration. A
// non-nil error (a *GuardError or a *CorruptError) means the daemon must exit
// without touching the data.
func Boot(dataDir, binaryVersion string) (BootResult, error) {
	// A ledger-less dir that already holds warden-db must not be "stamped
	// fresh" at schema 1: the stamp would bless it and the schema-1 stores
	// would be opened next to it. Refuse before writing anything.
	if _, err := Load(dataDir); errors.Is(err, ErrNoLedger) && WardenDBPresent(dataDir) {
		return BootResult{}, &GuardError{Verdict: VerdictSchema2Residue, DataDir: dataDir,
			Ledger: Ledger{}, Binary: SchemaVersion, Min: MinSchema}
	}
	l, stamped, err := Ensure(dataDir, binaryVersion)
	res := BootResult{Ledger: l, Stamped: stamped}
	if err != nil {
		var se *StampError
		if !errors.As(err, &se) {
			return BootResult{}, err
		}
		res.StampErr = err
	}
	if err := Check(dataDir, l); err != nil {
		return BootResult{}, err
	}
	if l != nil && l.SchemaVersion >= Schema2 {
		// Adoption heartbeat; best-effort, never blocks a boot.
		_ = adoption.Record(dataDir, adoption.Event{Kind: adoption.RuntimeBoot, BinaryVersion: binaryVersion, ToSchema: l.SchemaVersion})
	}
	return res, nil
}

// WardenDBPresent reports whether the schema-2 store directory exists. An
// unstatable path counts as present: when unsure, refuse.
func WardenDBPresent(dataDir string) bool {
	_, err := os.Lstat(filepath.Join(dataDir, WardenDBDir))
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// Guard is the read-only form of Boot for CLI paths that open stores outside
// the daemon: it loads the ledger and applies the same table, but never stamps,
// migrates or writes. A ledger-less dir passes unless it holds warden-db. A nil
// error means this binary may open the legacy stores.
func Guard(dataDir string) error {
	l, err := Load(dataDir)
	switch {
	case errors.Is(err, ErrNoLedger):
		if WardenDBPresent(dataDir) {
			return &GuardError{Verdict: VerdictSchema2Residue, DataDir: dataDir, Binary: SchemaVersion, Min: MinSchema}
		}
		return nil
	case err != nil:
		return err
	}
	return Check(dataDir, l)
}
