package schema

import (
	"errors"
	"fmt"
)

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
		return head + fmt.Sprintf("\nthe data was written by a newer warden%s; upgrade this binary (`wd update`) — "+
			"an older warden must not open it", by)
	case VerdictNeedsMigration:
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
	return res, nil
}
