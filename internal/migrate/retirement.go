package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/adoption"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
)

// Retirement support window and soak defaults
// (docs/specs/2026-10-18-legacy-store-retirement-evidence.md §2).
const (
	// DefaultSupportWindow is how long after an install's cutover its legacy
	// stores and backup must be retained before retirement may be considered.
	DefaultSupportWindow = 90 * 24 * time.Hour
	// DefaultMinRuntimeBoots is the fewest daemon boots on schema 2 that count
	// as evidence the install actually ran on warden-db.
	DefaultMinRuntimeBoots = 3
)

// RetirementOptions tunes Evaluate. Zero values take the documented defaults.
type RetirementOptions struct {
	Now           time.Time
	SupportWindow time.Duration
	MinBoots      int
}

// Criterion IDs are stable: operators and automation key on them.
const (
	CritSchema2Active   = "schema2_active"
	CritWardenDBPresent = "warden_db_present"
	CritBackupVerified  = "backup_verified"
	CritMigrationOK     = "migration_succeeded"
	CritNoUnresolved    = "no_unresolved_failures"
	CritNoRollback      = "no_rollback"
	CritNoLegacyOpens   = "zero_legacy_runtime_opens"
	CritRuntimeBoots    = "schema2_runtime_observed"
	CritSupportWindow   = "support_window_elapsed"
)

// Criterion is one auditable entry condition.
type Criterion struct {
	ID     string `json:"id"`
	Met    bool   `json:"met"`
	Detail string `json:"detail"`
}

// RetirementReport is the machine-readable entry-criteria verdict. Eligible
// only means the operator MAY start retirement; nothing is ever deleted by
// evaluating it.
type RetirementReport struct {
	DataDir           string         `json:"data_dir"`
	EvaluatedAt       time.Time      `json:"evaluated_at"`
	Eligible          bool           `json:"eligible"`
	CutoverAt         *time.Time     `json:"cutover_at,omitempty"`
	WindowEndsAt      *time.Time     `json:"window_ends_at,omitempty"`
	Criteria          []Criterion    `json:"criteria"`
	Blockers          []string       `json:"blockers,omitempty"`
	LegacyPresent     []string       `json:"legacy_present,omitempty"`
	Counts            map[string]int `json:"event_counts"`
	MalformedLogLines int            `json:"malformed_log_lines,omitempty"`
}

// EvaluateRetirement checks every entry criterion against the data dir. It is
// read-only.
func EvaluateRetirement(dataDir string, opt RetirementOptions) (*RetirementReport, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	if opt.SupportWindow <= 0 {
		opt.SupportWindow = DefaultSupportWindow
	}
	if opt.MinBoots <= 0 {
		opt.MinBoots = DefaultMinRuntimeBoots
	}
	rep := &RetirementReport{DataDir: dataDir, EvaluatedAt: opt.Now, Counts: map[string]int{}}
	add := func(id string, met bool, format string, a ...any) {
		rep.Criteria = append(rep.Criteria, Criterion{ID: id, Met: met, Detail: fmt.Sprintf(format, a...)})
	}

	log, err := adoption.Read(dataDir)
	if err != nil {
		return nil, fmt.Errorf("retirement: read evidence log: %w", err)
	}
	rep.MalformedLogLines = log.Malformed
	for _, e := range log.Events {
		rep.Counts[string(e.Kind)]++
	}

	l, lerr := schema.Load(dataDir)
	var entry *schema.HistoryEntry
	fresh := false
	if lerr == nil {
		for i := range l.History {
			h := &l.History[i]
			if h.To >= schema.Schema2 && (h.Migration == Migration0002ID || h.Migration == schema.MigrationBaselineFresh) {
				entry = h
			}
		}
		fresh = entry != nil && entry.Migration == schema.MigrationBaselineFresh
	}

	// 1. schema 2 is the ledger's committed state, with no open journal.
	switch {
	case lerr != nil:
		add(CritSchema2Active, false, "schema ledger unreadable: %v", lerr)
	case l.SchemaVersion < schema.Schema2:
		add(CritSchema2Active, false, "ledger is schema %d", l.SchemaVersion)
	case l.InProgress != nil:
		add(CritSchema2Active, false, "a migration journal is open (%s)", l.InProgress.Migration)
	default:
		add(CritSchema2Active, true, "ledger is schema %d with no open journal", l.SchemaVersion)
	}

	// 2. warden-db exists.
	add(CritWardenDBPresent, schema.WardenDBPresent(dataDir) && !errors.Is(statErr(wardendb.Dir(dataDir)), fs.ErrNotExist),
		"%s", wardendb.Dir(dataDir))

	// 3. committed migration, from the ledger (authoritative) with telemetry
	// as corroboration for installs migrated before evidence existed.
	if entry == nil {
		add(CritMigrationOK, false, "no schema-2 cutover in the ledger history")
	} else {
		at := entry.At
		rep.CutoverAt = &at
		src := "ledger history only (predates evidence log)"
		if log.Count(adoption.MigrationApplied, time.Time{}) > 0 {
			src = "ledger history and telemetry"
		}
		add(CritMigrationOK, true, "%s at %s: %s", entry.Migration, at.Format(time.RFC3339), src)
		end := at.Add(opt.SupportWindow)
		rep.WindowEndsAt = &end
	}

	// 4. verified immutable backup (a fresh install has no legacy data).
	switch {
	case fresh:
		add(CritBackupVerified, true, "fresh install: no legacy data ever existed")
	case entry == nil || entry.Backup == "":
		add(CritBackupVerified, false, "ledger records no cutover backup")
	default:
		dir := entry.Backup
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(dataDir, filepath.FromSlash(dir))
		}
		if _, err := VerifySchema2Backup(dir); err != nil {
			add(CritBackupVerified, false, "%v", err)
		} else {
			add(CritBackupVerified, true, "backup %s verified against its manifest", entry.Backup)
		}
	}

	since := time.Time{}
	if rep.CutoverAt != nil {
		since = *rep.CutoverAt
	}
	// 5. no failure/refusal after the cutover committed.
	failures := log.Count(adoption.MigrationFailed, since) + log.Count(adoption.RecoveryFailed, since)
	if failures == 0 {
		add(CritNoUnresolved, true, "no migration or recovery failure since the cutover")
	} else {
		add(CritNoUnresolved, false, "%d migration/recovery failure(s) since the cutover", failures)
	}

	// 6. no rollback: updater rollback, restore, or an abandoned schema-2 target.
	rolls := log.Count(adoption.UpdaterRollback, since) + log.Count(adoption.RecoveryRestored, since)
	abandoned, _ := filepath.Glob(filepath.Join(dataDir, "backups", "abandoned-schema2-*"))
	if rolls == 0 && len(abandoned) == 0 {
		add(CritNoRollback, true, "no rollback or restore recorded")
	} else {
		add(CritNoRollback, false, "%d rollback/restore event(s), %d abandoned schema-2 target(s)", rolls, len(abandoned))
	}

	// 7. zero legacy runtime opens, ever.
	if n := log.Count(adoption.LegacyOpenRefused, time.Time{}); n == 0 {
		add(CritNoLegacyOpens, true, "no attempt to open a legacy store on schema 2")
	} else {
		add(CritNoLegacyOpens, false, "%d refused legacy-store open attempt(s)", n)
	}

	// 8. the daemon actually ran on schema 2.
	boots := log.Count(adoption.RuntimeBoot, since)
	add(CritRuntimeBoots, boots >= opt.MinBoots, "%d daemon boot(s) on schema 2 (need %d)", boots, opt.MinBoots)

	// 9. support window elapsed since this install's cutover.
	if rep.WindowEndsAt == nil {
		add(CritSupportWindow, false, "no cutover time to measure from")
	} else if opt.Now.Before(*rep.WindowEndsAt) {
		add(CritSupportWindow, false, "window ends %s", rep.WindowEndsAt.Format(time.RFC3339))
	} else {
		add(CritSupportWindow, true, "window ended %s", rep.WindowEndsAt.Format(time.RFC3339))
	}

	rep.Eligible = true
	for _, c := range rep.Criteria {
		if !c.Met {
			rep.Eligible = false
			rep.Blockers = append(rep.Blockers, c.ID)
		}
	}
	rep.LegacyPresent = legacyPresent(dataDir)
	return rep, nil
}

func statErr(p string) error { _, err := os.Lstat(p); return err }

// legacyPresent lists migrated legacy source paths still on disk: the cleanup
// candidates. Informational; retirement never runs from this list.
func legacyPresent(dataDir string) []string {
	var out []string
	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dataDir, filepath.FromSlash(s.Path))); err == nil {
			out = append(out, s.Path)
		}
	}
	sort.Strings(out)
	return out
}

// Summary renders the report for a terminal.
func (r *RetirementReport) Summary() string {
	var b strings.Builder
	verdict := "NOT ELIGIBLE"
	if r.Eligible {
		verdict = "ELIGIBLE (operator may begin retirement; nothing is deleted automatically)"
	}
	fmt.Fprintf(&b, "legacy-store retirement: %s\n", verdict)
	for _, c := range r.Criteria {
		mark := "FAIL"
		if c.Met {
			mark = "ok  "
		}
		fmt.Fprintf(&b, "  [%s] %-28s %s\n", mark, c.ID, c.Detail)
	}
	if len(r.LegacyPresent) > 0 {
		fmt.Fprintf(&b, "legacy stores still on disk: %s\n", strings.Join(r.LegacyPresent, ", "))
	}
	return b.String()
}
