package wardendb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/scriva/engine"
)

// Repair audit actions.
const (
	// ActionRepair is emitted once per completed Repair, changed or not.
	ActionRepair = "wardendb.repair"
	// ActionRepairRefused is emitted whenever Repair declines or cannot finish.
	ActionRepairRefused = "wardendb.repair_refused"
)

var (
	// ErrRepairBackupRequired: no usable backup location was given. Repair
	// never mutates a database it could not back up first.
	ErrRepairBackupRequired = errors.New("wardendb: repair requires a backup directory outside warden-db")
	// ErrRepairRefused is wrapped by every error Repair returns for a database
	// it would not, or could not completely, repair.
	ErrRepairRefused = errors.New("wardendb: repair refused")
)

// repairJournalName is the engine's restart record of an interrupted repair.
const repairJournalName = "REPAIR_JOURNAL.json"

// RepairOptions configures Repair and, through WithRepair, OpenForRepair.
type RepairOptions struct {
	// BackupDir is the directory the pre-repair backups are written under. It
	// is required and must lie outside warden-db.
	BackupDir string
	// Salvage permits repairing damaged segment bytes (ClassSegmentDamage):
	// the valid records move to a new segment and the damaged original is
	// quarantined inside the collection directory, never deleted. Whatever the
	// damaged regions held is lost to the live database. Without it a database
	// with damaged segments is refused untouched.
	Salvage bool
	// OnResult, if set, receives the result of the repair run by OpenForRepair
	// (also when it is refused). Repair itself ignores it.
	OnResult func(*RepairResult)
}

// WithRepair supplies the repair settings OpenForRepair requires. Open ignores
// it: no Option makes Open tolerant.
func WithRepair(ro RepairOptions) Option {
	return func(c *config) { c.repair = &ro }
}

// RepairOutcome is what Repair did with one collection.
type RepairOutcome string

const (
	// RepairUnchanged: nothing in the collection needed repair.
	RepairUnchanged RepairOutcome = "unchanged"
	// RepairRepaired: the collection was changed; see Actions.
	RepairRepaired RepairOutcome = "repaired"
	// RepairRefused: the collection needs repair and was left untouched.
	RepairRefused RepairOutcome = "refused"
)

// RepairAction is one change made to a collection.
type RepairAction struct {
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
}

// CollectionRepair reports what Repair did with one collection.
type CollectionRepair struct {
	Name    string        `json:"name"`
	Outcome RepairOutcome `json:"outcome"`
	// Reason says why the collection was refused.
	Reason string `json:"reason,omitempty"`
	// Classes are the finding classes present before the repair.
	Classes []FindingClass `json:"classes"`
	Actions []RepairAction `json:"actions"`
	// Salvage results: the segment the valid records were moved to, the
	// damaged originals moved to QuarantineDir (relative to the collection
	// directory), and the number of damaged regions whose contents are lost.
	NewSegment    string   `json:"new_segment,omitempty"`
	Quarantined   []string `json:"quarantined,omitempty"`
	QuarantineDir string   `json:"quarantine_dir,omitempty"`
	LostRegions   int      `json:"lost_regions,omitempty"`
}

// RepairResult reports what a Repair run found, backed up and changed.
type RepairResult struct {
	Dir        string    `json:"dir"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Changed reports whether anything in the directory was modified.
	Changed bool `json:"changed"`
	// Resumed: an interrupted earlier repair was continued.
	Resumed bool `json:"resumed"`
	// BackupDir is the whole-database backup taken before the first mutation
	// (restorable with Restore and AllowCorrupt); empty when nothing was
	// touched. Backup is its manifest.
	BackupDir string          `json:"backup_dir,omitempty"`
	Backup    *BackupManifest `json:"backup,omitempty"`
	// EngineBackupDir is the engine's own verified copy of the collections it
	// repaired, with its repair report.
	EngineBackupDir string             `json:"engine_backup_dir,omitempty"`
	Collections     []CollectionRepair `json:"collections"`
	// Refused lists why the run was refused, in full or in part.
	Refused []string `json:"refused,omitempty"`
	// Before and After are the read-only verifications around the run. After
	// is nil when nothing was attempted. They carry engine messages and are
	// not serialized; use their Report.
	Before *VerifyResult `json:"-"`
	After  *VerifyResult `json:"-"`
}

func classesOf(s FindingSet) map[FindingClass]int {
	out := map[FindingClass]int{}
	for _, fs := range [][]Finding{s.Blocking, s.Recoverable} {
		for _, f := range fs {
			out[ClassOf(f)]++
		}
	}
	return out
}

func classList(m map[FindingClass]int) []FindingClass {
	out := make([]FindingClass, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// Repair is the only code path in this package that may change a damaged
// warden-db. Open and Register never call it.
//
// It requires exclusive ownership of the directory for the whole run (a live
// owner yields *LockedError) and a backup location (ErrRepairBackupRequired
// otherwise). It first verifies the directory read-only and decides, before
// anything is written anywhere, what it will do per finding class
// (FindingClass.RepairPolicy):
//
//   - ClassConflict or ClassTransaction anywhere: the whole run is refused.
//   - ClassSegmentDamage without RepairOptions.Salvage: the whole run is refused.
//   - ClassCrashLeftover: left alone; a strict Open reconciles it. A database
//     with nothing else is reported unchanged. An undecided transaction next
//     to findings that do need repair is refused: the engine will not repair
//     around an open transaction, and Open will not open around the damage.
//   - ClassDerived: rebuilt from the segments.
//   - ClassSegmentDamage with Salvage: valid records move to a new segment,
//     the damaged original is quarantined.
//
// A refusal leaves every byte as found and takes no backup. Otherwise a
// checksummed copy of the whole directory is written under BackupDir first;
// if that fails nothing is changed. The engine then repairs under its own
// directory lock, with its own verified backup of the collections it touches,
// refusing conflicting history and never guessing non-derivable settings.
//
// The result says what was changed. Every refusal, including a repair the
// engine could only partly complete or one that leaves blocking findings
// behind, returns the result together with an error wrapping ErrRepairRefused.
func Repair(ctx context.Context, dataDir string, ro RepairOptions, opts ...Option) (*RepairResult, error) {
	if dataDir == "" {
		return nil, errors.New("wardendb: empty data directory")
	}
	cfg := toolConfig("repair", opts)
	dir := Dir(dataDir)
	mon := &monitor{dir: dir, auditor: cfg.auditor, metrics: cfg.metrics}
	res := &RepairResult{Dir: dir, StartedAt: time.Now(), Collections: []CollectionRepair{}}
	refuse := func(err error, reasons ...string) (*RepairResult, error) {
		res.Refused = append(res.Refused, reasons...)
		if len(reasons) == 0 {
			res.Refused = append(res.Refused, err.Error())
		}
		res.FinishedAt = time.Now()
		mon.emit(ActionRepairRefused, "", map[string]string{
			"reasons": strings.Join(res.Refused, "; "),
			"changed": strconv.FormatBool(res.Changed),
			"backup":  res.BackupDir,
		})
		return res, err
	}

	if ro.BackupDir == "" {
		return refuse(ErrRepairBackupRequired)
	}
	if inside(dir, ro.BackupDir) {
		return refuse(fmt.Errorf("%w: %s is inside %s", ErrRepairBackupRequired, ro.BackupDir, dir))
	}
	lock, err := acquireExisting(dir, cfg, mon)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	before, err := verifyDir(ctx, dir, true, LockStatus{State: LockUnowned})
	if err != nil {
		return nil, err
	}
	res.Before = before
	if before.Lock.State != LockUnowned {
		return nil, mon.locked(&LockedError{Dir: dir, cause: engine.ErrDatabaseLocked})
	}

	// Decide, read-only, before anything is written anywhere.
	dbClasses := classesOf(before.Database)
	total := map[FindingClass]int{}
	for c, n := range dbClasses {
		total[c] += n
	}
	for _, c := range before.Collections {
		cls := classesOf(c.FindingSet)
		for k, n := range cls {
			total[k] += n
		}
		res.Collections = append(res.Collections, CollectionRepair{
			Name: c.Name, Outcome: RepairUnchanged, Classes: classList(cls), Actions: []RepairAction{},
		})
	}
	_, jerr := os.Stat(filepath.Join(dir, repairJournalName))
	resuming := jerr == nil
	work := total[ClassDerived] + total[ClassSegmentDamage]

	var reasons []string
	deny := func(class FindingClass, reason string) {
		if total[class] == 0 {
			return
		}
		reasons = append(reasons, fmt.Sprintf("%d %s finding(s): %s", total[class], class, reason))
		for i := range res.Collections {
			cr := &res.Collections[i]
			for _, c := range cr.Classes {
				if c == class {
					cr.Outcome, cr.Reason = RepairRefused, reason
				}
			}
		}
	}
	deny(ClassConflict, ClassConflict.RepairPolicy())
	deny(ClassTransaction, ClassTransaction.RepairPolicy())
	if !ro.Salvage {
		deny(ClassSegmentDamage, "refused: damaged segment bytes are only repaired when Salvage is set")
	}
	if (work > 0 || resuming) && dbClasses[ClassCrashLeftover] > 0 {
		reasons = append(reasons, "an undecided transaction is pending next to findings that need repair; neither can be resolved first")
	}
	if len(reasons) > 0 {
		for i := range res.Collections {
			if cr := &res.Collections[i]; cr.Outcome == RepairUnchanged && len(cr.Classes) > 0 {
				cr.Outcome, cr.Reason = RepairRefused, "the run was refused; nothing was changed"
			}
		}
		return refuse(fmt.Errorf("%w: %s: %s", ErrRepairRefused, dir, strings.Join(reasons, "; ")), reasons...)
	}
	if work == 0 && !resuming {
		res.FinishedAt = time.Now()
		mon.emit(ActionRepair, "", map[string]string{"changed": "false", "health": string(before.Health)})
		return res, nil
	}

	// Back the whole directory up before the first mutation, or refuse.
	dest := filepath.Join(ro.BackupDir, DirName+"-pre-repair-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
	m, err := copyBackup(ctx, dir, dest)
	if err != nil {
		return refuse(fmt.Errorf("%w: %s: no backup could be taken, nothing was changed: %w", ErrRepairRefused, dir, err))
	}
	res.Backup, res.BackupDir = m, dest
	backupEvent(mon, m, dest)

	rep, rerr := engine.Repair(ctx, dir, engine.RepairOptions{
		BackupDir:  ro.BackupDir,
		OnConflict: engine.ConflictAbort,
		Salvage:    ro.Salvage,
	})
	if rep != nil {
		res.Resumed, res.EngineBackupDir = rep.Resumed, rep.BackupDir
		for _, ec := range rep.Collections {
			cr := collectionRepair(res, ec.Name)
			cr.Actions = make([]RepairAction, 0, len(ec.Actions))
			for _, a := range ec.Actions {
				cr.Actions = append(cr.Actions, RepairAction{Kind: a.Kind, Target: a.Target})
			}
			switch ec.Status {
			case engine.RepairRepaired:
				cr.Outcome, res.Changed = RepairRepaired, true
			case engine.RepairBlocked:
				cr.Outcome, cr.Reason = RepairRefused, ec.Reason
				res.Refused = append(res.Refused, fmt.Sprintf("collection %q: %s", ec.Name, ec.Reason))
			}
			if s := ec.Salvage; s != nil {
				cr.NewSegment, cr.Quarantined, cr.QuarantineDir, cr.LostRegions = s.NewSegment, s.Quarantined, s.QuarantineDir, s.BadRegions
			}
		}
	}
	after, verr := verifyDir(ctx, dir, true, LockStatus{State: LockUnowned})
	if verr == nil {
		res.After = after
	}
	switch {
	case rerr != nil:
		// An interrupted run may have changed files without reporting them.
		res.Changed = res.Changed || rep == nil
		return refuse(fmt.Errorf("%w: %s: %w", ErrRepairRefused, dir, rerr))
	case verr != nil:
		return refuse(fmt.Errorf("%w: %s: verification after the repair failed: %w", ErrRepairRefused, dir, verr))
	case after.Blocking > 0:
		return refuse(fmt.Errorf("%w: %s: %d blocking finding(s) remain", ErrRepairRefused, dir, after.Blocking))
	}

	res.FinishedAt = time.Now()
	var repaired []string
	actions := 0
	for _, c := range res.Collections {
		if c.Outcome == RepairRepaired {
			repaired = append(repaired, c.Name)
		}
		actions += len(c.Actions)
	}
	mon.emit(ActionRepair, "", map[string]string{
		"changed":       strconv.FormatBool(res.Changed),
		"resumed":       strconv.FormatBool(res.Resumed),
		"salvage":       strconv.FormatBool(ro.Salvage),
		"collections":   strings.Join(repaired, ","),
		"actions":       strconv.Itoa(actions),
		"backup":        res.BackupDir,
		"engine_backup": res.EngineBackupDir,
		"health":        string(after.Health),
	})
	return res, nil
}

// collectionRepair returns the entry for name, adding one if needed.
func collectionRepair(res *RepairResult, name string) *CollectionRepair {
	for i := range res.Collections {
		if res.Collections[i].Name == name {
			return &res.Collections[i]
		}
	}
	res.Collections = append(res.Collections, CollectionRepair{Name: name, Outcome: RepairUnchanged, Classes: []FindingClass{}})
	return &res.Collections[len(res.Collections)-1]
}

// openForRepair backs OpenForRepair: an explicit Repair, then a strict Open.
func openForRepair(dataDir string, opts []Option) (*DB, error) {
	var cfg config
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if cfg.repair == nil {
		return nil, fmt.Errorf("%w: %s: pass WithRepair", ErrRepairBackupRequired, Dir(dataDir))
	}
	res, err := Repair(context.Background(), dataDir, *cfg.repair, opts...)
	if res != nil && cfg.repair.OnResult != nil {
		cfg.repair.OnResult(res)
	}
	if err != nil {
		return nil, err
	}
	return Open(dataDir, opts...)
}
