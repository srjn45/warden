// Package schema owns the data-format ledger (<data>/schema.json) and the
// daemon boot guard built on it (docs/specs/2026-10-09-update-process.md §3).
//
// The ledger records which data format a data dir is at, as a monotonically
// increasing integer that is deliberately separate from the release semver:
// many releases share one value, and it bumps only when stored data or config
// format changes. Each binary embeds the format it writes (SchemaVersion) and
// the oldest format it can migrate from (MinSchema). The daemon compares the
// two at boot and refuses to open stores it must not touch; it never migrates.
// Migrations run only from `wd update` / `warden migrate`.
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	// SchemaVersion is the data format this binary reads and writes.
	SchemaVersion = 1
	// MinSchema is the oldest data format this binary can migrate from.
	MinSchema = 1
	// LegacyBaseline is the format stamped on a pre-ledger install. Every
	// release that predates the ledger wrote this format (their one-time boot
	// importers are idempotent and still run at boot), so it is a constant, not
	// a function of which sentinels are present. It stays 1 forever: once
	// SchemaVersion moves on, a still-unstamped install is stamped here and then
	// has to migrate like any other schema-1 data dir.
	LegacyBaseline = 1

	// FileName is the ledger's name inside the data dir.
	FileName = "schema.json"

	// Baseline migration IDs recorded in the one history entry a stamp writes.
	MigrationBaselineFresh  = "baseline-fresh"
	MigrationBaselineLegacy = "baseline-legacy"
)

// Ledger is the decoded <data>/schema.json.
type Ledger struct {
	SchemaVersion int `json:"schema_version"`
	// BinaryVersion is the warden version that last wrote the ledger (the stamp
	// or a migration). The daemon never rewrites an existing ledger, so this is
	// not "the binary currently running".
	BinaryVersion string         `json:"binary_version"`
	History       []HistoryEntry `json:"history"`
	// InProgress is the migration journal: set before a migration step, cleared
	// after. Non-nil at boot means a migration was interrupted.
	InProgress *InProgress `json:"in_progress"`
}

// HistoryEntry is one applied step: the baseline stamp or a migration.
type HistoryEntry struct {
	From      int       `json:"from"`
	To        int       `json:"to"`
	Migration string    `json:"migration"`
	At        time.Time `json:"at"`
	Backup    string    `json:"backup,omitempty"`
	// Sentinels lists the legacy importer sentinels present when the baseline
	// was inferred (baseline-legacy entries only).
	Sentinels []string `json:"sentinels,omitempty"`
}

// InProgress is the journal record of a migration that has started.
type InProgress struct {
	Migration string `json:"migration"`
	Step      string `json:"step,omitempty"`
	Snapshot  string `json:"snapshot,omitempty"`
}

// ErrNoLedger is returned by Load when the data dir has no schema.json.
var ErrNoLedger = errors.New("schema: no ledger")

// CorruptError reports a ledger that exists but cannot be trusted. It is never
// overwritten automatically: guessing a version here could let the daemon open
// data it does not understand.
type CorruptError struct {
	Path string
	Err  error
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("schema ledger %s is unreadable: %v\n"+
		"the daemon will not guess the data format; restore the file from a backup, or move it aside "+
		"to have a pre-ledger install re-stamped at its baseline", e.Path, e.Err)
}

func (e *CorruptError) Unwrap() error { return e.Err }

// Path returns the ledger path for a data dir.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Load reads the ledger. It returns ErrNoLedger when the file is absent and a
// *CorruptError when it is present but invalid.
func Load(dataDir string) (*Ledger, error) {
	p := Path(dataDir)
	raw, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNoLedger
	}
	if err != nil {
		return nil, fmt.Errorf("schema: read %s: %w", p, err)
	}
	var l Ledger
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, &CorruptError{Path: p, Err: err}
	}
	if l.SchemaVersion < 1 {
		return nil, &CorruptError{Path: p, Err: fmt.Errorf("schema_version %d is not a positive integer", l.SchemaVersion)}
	}
	return &l, nil
}

// Save atomically replaces the ledger (temp file + fsync + rename), so a crash
// leaves either the old ledger or the new one, never a torn file.
func Save(dataDir string, l *Ledger) error {
	tmp, err := writeTemp(dataDir, l)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, Path(dataDir)); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("schema: commit ledger: %w", err)
	}
	syncDir(dataDir)
	return nil
}

// create writes the ledger only if none exists, reporting whether it did. The
// hard link makes "absent → present" atomic and exclusive, which is what keeps
// a stamp exactly-once even if two processes race past the ownership lock.
func create(dataDir string, l *Ledger) (bool, error) {
	tmp, err := writeTemp(dataDir, l)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	final := Path(dataDir)
	if err := os.Link(tmp, final); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		// Filesystems without hard links: fall back to a checked rename.
		if _, serr := os.Lstat(final); serr == nil {
			return false, nil
		}
		if rerr := os.Rename(tmp, final); rerr != nil {
			return false, fmt.Errorf("schema: commit ledger: %w", rerr)
		}
	}
	syncDir(dataDir)
	return true, nil
}

// writeTemp writes the encoded ledger to a fsynced temp file beside the final
// path (same filesystem, so the commit is a rename/link) and returns its name.
func writeTemp(dataDir string, l *Ledger) (string, error) {
	if l == nil {
		return "", errors.New("schema: nil ledger")
	}
	out := *l
	if out.History == nil {
		out.History = []HistoryEntry{}
	}
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("schema: encode ledger: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("schema: data dir: %w", err)
	}
	f, err := os.CreateTemp(dataDir, "."+FileName+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("schema: temp ledger: %w", err)
	}
	name := f.Name()
	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("schema: write ledger: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(raw); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("schema: write ledger: %w", err)
	}
	return name, nil
}

// syncDir makes the rename durable. Best effort: some filesystems refuse to
// fsync a directory, and the file contents are already synced.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// LegacySentinels are the marker files the pre-ledger boot importers write once
// their one-time import/migration has finished, relative to the data dir. Their
// presence is how a legacy install is recognised. They are read here, never
// written or removed: the importers still own them until they are ported into
// the migration registry.
var LegacySentinels = []string{
	".sessions-filedb-imported",               // internal/store: sessions JSON → ScrivaDB
	".provenance-migrated",                    // internal/store: old FileStore provenance pass
	".agents-from-sessions-imported",          // internal/agentstore: sessions-db → agents-db (active)
	".archived-agents-from-sessions-imported", // internal/agentstore: sessions-db → agents-db (closed)
	".terminals-from-sessions-imported",       // internal/terminalstore: sessions-db → terminals-db
	".pipelines-filedb-imported",              // internal/pipeline: pipelines JSON → ScrivaDB
	".schedules-filedb-imported",              // internal/schedule: schedules.json → ScrivaDB
	".snapshots-filedb-imported",              // internal/snapshot: snapshots JSON → ScrivaDB
	".autopilots-from-runs-imported",          // internal/autopilotstore: legacy runs → autopilots-db
	"backends/.autopilot-ladder-migrated",     // internal/backendstore: config ladder → registry
}

// legacyStorePaths are store locations whose presence marks a data dir as
// already in use even when no sentinel survives (e.g. after a partial factory
// reset, or a store that never had an importer).
var legacyStorePaths = []string{
	"sessions", "sessions-db", "agents-db", "closed", "terminals-db",
	"pipelines", "pipelines-db", "schedules.json", "schedules-db",
	"snapshots", "snapshots-db", "autopilot", "autopilots-db",
	"context", "inbox", "backends", "plans", "projects",
}

// Baseline is what InferBaseline concluded about a ledger-less data dir.
type Baseline struct {
	// Fresh is true when the dir holds no warden data: it is stamped at the
	// current SchemaVersion. Otherwise it is a legacy install at LegacyBaseline.
	Fresh   bool
	Version int
	// Sentinels are the LegacySentinels found, sorted.
	Sentinels []string
}

// InferBaseline classifies a data dir that has no ledger. It only stats paths.
// A path that cannot be statted counts as in-use data: when unsure, the dir is
// treated as a legacy install rather than declared fresh.
func InferBaseline(dataDir string) Baseline {
	var found []string
	unsure := false
	for _, rel := range LegacySentinels {
		switch _, err := os.Lstat(filepath.Join(dataDir, filepath.FromSlash(rel))); {
		case err == nil:
			found = append(found, rel)
		case !errors.Is(err, fs.ErrNotExist):
			unsure = true
		}
	}
	sort.Strings(found)
	if len(found) > 0 || unsure {
		return Baseline{Version: LegacyBaseline, Sentinels: found}
	}
	for _, rel := range legacyStorePaths {
		if _, err := os.Lstat(filepath.Join(dataDir, rel)); err == nil || !errors.Is(err, fs.ErrNotExist) {
			return Baseline{Version: LegacyBaseline}
		}
	}
	return Baseline{Fresh: true, Version: SchemaVersion}
}

// now is the stamp clock (overridden in tests).
var now = func() time.Time { return time.Now().UTC() }

// baselineLedger builds the ledger a stamp would write for b.
func baselineLedger(b Baseline, binaryVersion string) *Ledger {
	id := MigrationBaselineLegacy
	if b.Fresh {
		id = MigrationBaselineFresh
	}
	return &Ledger{
		SchemaVersion: b.Version,
		BinaryVersion: binaryVersion,
		History: []HistoryEntry{{
			From: 0, To: b.Version, Migration: id, At: now(), Sentinels: b.Sentinels,
		}},
	}
}

// Ensure returns the data dir's ledger, stamping one first if none exists: a
// fresh dir at SchemaVersion, a legacy install at LegacyBaseline. The stamp is
// written exactly once; an existing ledger is returned untouched. stamped
// reports whether this call wrote it.
func Ensure(dataDir, binaryVersion string) (l *Ledger, stamped bool, err error) {
	l, err = Load(dataDir)
	if err == nil {
		return l, false, nil
	}
	if !errors.Is(err, ErrNoLedger) {
		return nil, false, err
	}
	l = baselineLedger(InferBaseline(dataDir), binaryVersion)
	wrote, err := create(dataDir, l)
	if err != nil {
		return l, false, &StampError{Path: Path(dataDir), Err: err}
	}
	if !wrote {
		// Lost a race: someone stamped between our Load and create. Theirs wins.
		l, err = Load(dataDir)
		return l, false, err
	}
	return l, true, nil
}

// StampError means the baseline was inferred but could not be persisted. Ensure
// still returns the inferred ledger alongside it so a caller can carry on.
type StampError struct {
	Path string
	Err  error
}

func (e *StampError) Error() string { return fmt.Sprintf("schema: stamp %s: %v", e.Path, e.Err) }
func (e *StampError) Unwrap() error { return e.Err }
