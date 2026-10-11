package migrate

// Schema-2 migration contract: the read-only plan (source/target inventory and
// preflight findings) and the one-shot, immutable pre-migration backup and
// manifest that every later cutover phase hangs off.
//
// Authority: docs/specs/2026-10-10-warden-db-schema-2-migration-acceptance.md
// and docs/specs/2026-10-10-warden-db-data-source-inventory.md.
//
// Invariants this file enforces and later phases must keep:
//   - The daemon is stopped. BeginSchema2 takes the data-dir ownership lock for
//     its whole life; there is no online or dual-writer path.
//   - Nothing is ever overwritten, merged, salvaged or deleted. The backup and
//     the manifest are created exclusively (a collision is an error, not a
//     reuse) and the manifest is written last, read-only.
//   - A warden-db that already exists beside a schema-1 ledger is a blocking
//     finding, never something to adopt, merge into or replace.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
)

const (
	// Schema2ManifestFormat is the manifest layout version.
	Schema2ManifestFormat = 1
	// Schema2ManifestName is the manifest file inside the backup directory.
	Schema2ManifestName = "MANIFEST.json"
	// Schema2BackupPrefix prefixes every backup directory name.
	Schema2BackupPrefix = "pre-schema2-"
	// schema2BackupData is the subdirectory of the backup that mirrors the
	// data dir's relative paths.
	schema2BackupData = "data"
)

// Disposition is what the migration does with a source.
type Disposition string

const (
	// DispositionMigrated: normalized into warden-db by the cutover.
	DispositionMigrated Disposition = "migrated"
	// DispositionExternal: intentionally outside warden-db; preserved in place
	// and never rewritten by the migration.
	DispositionExternal Disposition = "external"
	// DispositionUnclassified: present but not in the inventory. Preserved in
	// place, reported, never touched.
	DispositionUnclassified Disposition = "unclassified"
)

// sourceSpec is one row of the legacy-source table (acceptance spec §3).
type sourceSpec struct {
	Path        string // data-dir relative, slash-separated
	Destination string // warden-db collection(s), or the external reason
	Disposition Disposition
	// SkipSuffix names files inside a migrated directory that stay external
	// (snapshot transcripts): they are not copied into the backup.
	SkipSuffix string
}

// schema2Sources is the mandatory coverage list. A top-level name not matched
// here (see classifyTopLevel) is reported as unclassified.
var schema2Sources = []sourceSpec{
	{"agents-db", "agents (+archived)", DispositionMigrated, ""},
	{"closed", "agents (archived)", DispositionMigrated, ""},
	{"sessions-db", "agents|terminals", DispositionMigrated, ""},
	{"sessions", "agents|terminals", DispositionMigrated, ""},
	{"terminals-db", "terminals", DispositionMigrated, ""},
	{"projects", "projects, project_groups", DispositionMigrated, ""},
	{"plans/plans-db", "plans", DispositionMigrated, ""},
	{"plans/plan-exports-db", "plan_exports", DispositionMigrated, ""},
	{"pipelines-db", "pipelines, pipeline_jobs", DispositionMigrated, ""},
	{"pipelines", "pipelines, pipeline_jobs", DispositionMigrated, ""},
	{"autopilots-db", "autopilot_runs", DispositionMigrated, ""},
	{"autopilot", "autopilot_runs", DispositionMigrated, ""},
	{"context", "context_entries", DispositionMigrated, ""},
	{"inbox", "messages, id_sequences", DispositionMigrated, ""},
	{"backends", "backends, backend_models, backend_roles, backend_settings", DispositionMigrated, ""},
	{"usage-snapshots", "usage_snapshots", DispositionMigrated, ""},
	{"quota-impact", "quota_impact_fences", DispositionMigrated, ""},
	{"snapshots-db", "snapshots", DispositionMigrated, ""},
	{"snapshots", "snapshots (metadata); *.transcript external", DispositionMigrated, ".transcript"},
	{"savings", "savings_events, savings_calibration", DispositionMigrated, ""},
	{"spend", "spend_records", DispositionMigrated, ""},
	{"schedules-db", "schedules", DispositionMigrated, ""},
	{"schedules.json", "schedules", DispositionMigrated, ""},
	{"known-prompts-db", "known_prompts", DispositionMigrated, ""},
	{"plan-sync-hub", "plan_sync_envelopes", DispositionMigrated, ""},
	{"metrics", "metrics_samples", DispositionMigrated, ""},

	{"plans/pending", "plan YAML export (inert)", DispositionExternal, ""},
	{"plans/in_progress", "plan YAML export (inert)", DispositionExternal, ""},
	{"plans/completed", "plan YAML export (inert)", DispositionExternal, ""},
	{"plans/archived", "plan YAML export (inert)", DispositionExternal, ""},
	{schema.FileName, "schema ledger", DispositionExternal, ""},
	{ownerlock.FileName, "ownership lock", DispositionExternal, ""},
	{"audit.jsonl", "append-only audit stream", DispositionExternal, ""},
	{"prompts", "runtime artifact", DispositionExternal, ""},
	{"hints", "runtime artifact", DispositionExternal, ""},
	{"exits", "runtime artifact", DispositionExternal, ""},
	{"settings", "runtime artifact", DispositionExternal, ""},
	{"session-logs", "runtime artifact", DispositionExternal, ""},
	{"ratelimit-captures", "runtime artifact", DispositionExternal, ""},
	{"backups", "maintenance artifact", DispositionExternal, ""},
	{"quarantine", "maintenance artifact", DispositionExternal, ""},
	{"tmp", "maintenance artifact", DispositionExternal, ""},
}

// externalTopLevel names top-level entries that are never data sources:
// config, importer sentinels, and operator-made copies of stores.
func externalTopLevel(name string) (string, bool) {
	switch {
	case strings.HasPrefix(name, "."):
		return "importer sentinel or hidden runtime file", true
	case strings.HasPrefix(name, "config.yaml"):
		return "configuration", true
	case strings.Contains(name, ".bak-"), strings.Contains(name, ".corrupt-"),
		strings.Contains(name, "-backup"), strings.Contains(name, "-backups"):
		return "operator or repair backup", true
	}
	return "", false
}

// InventoryEntry is one source (or target) path as found on disk.
type InventoryEntry struct {
	Path        string      `json:"path"`
	Destination string      `json:"destination,omitempty"`
	Disposition Disposition `json:"disposition"`
	Present     bool        `json:"present"`
	Files       int         `json:"files,omitempty"`
	Bytes       int64       `json:"bytes,omitempty"`
	// Symlinks lists symlinks found under the entry. They are never followed
	// and make the plan blocking.
	Symlinks []string `json:"symlinks,omitempty"`
}

// TargetInventory describes where schema-2 data will land.
type TargetInventory struct {
	Dir         string   `json:"dir"`
	Exists      bool     `json:"exists"`
	Collections []string `json:"collections"`
}

// LedgerView is the ledger state the plan was computed against.
type LedgerView struct {
	Present       bool   `json:"present"`
	SchemaVersion int    `json:"schema_version"`
	BinaryVersion string `json:"binary_version,omitempty"`
	Interrupted   bool   `json:"interrupted,omitempty"`
}

// Schema2Plan is the read-only answer to "what would the cutover do". It
// writes nothing and acquires nothing.
type Schema2Plan struct {
	DataDir   string           `json:"data_dir"`
	From      int              `json:"from"`
	To        int              `json:"to"`
	Ledger    LedgerView       `json:"ledger"`
	Sources   []InventoryEntry `json:"sources"`
	Target    TargetInventory  `json:"target"`
	BackupDir string           `json:"backup_dir"`
	Findings  []Finding        `json:"findings"`
	// Blocked is true when any finding is blocking: BeginSchema2 refuses.
	Blocked bool `json:"blocked"`
}

// backupDirName is the generation-stamped backup directory name.
func backupDirName(t time.Time) string {
	return Schema2BackupPrefix + t.UTC().Format("20060102T150405Z")
}

// PlanSchema2 inspects dataDir without modifying it.
func PlanSchema2(dataDir string, now time.Time) (*Schema2Plan, error) {
	p := &Schema2Plan{
		DataDir:   dataDir,
		From:      schema.SchemaVersion,
		To:        schema.Schema2,
		BackupDir: filepath.Join(dataDir, "backups", backupDirName(now)),
		Target: TargetInventory{
			Dir:         filepath.Join(dataDir, wardendb.DirName),
			Collections: wardendb.Catalogue(),
		},
	}
	block := func(store, msg, details string) {
		p.Findings = append(p.Findings, Finding{Severity: SeverityBlocking, Store: store, Message: msg, Details: details})
	}

	if fi, err := os.Stat(dataDir); err != nil || !fi.IsDir() {
		block("", fmt.Sprintf("data dir %s is not a directory", dataDir), "check --data-dir / the configured data_dir")
		p.Blocked = true
		return p, nil
	}

	// Daemon must be stopped: probe now (a read-only look); BeginSchema2 takes
	// the real lock.
	switch oe, err := ownerlock.Probe(dataDir); {
	case err != nil:
		return nil, fmt.Errorf("schema2 plan: probe ownership lock: %w", err)
	case oe != nil:
		block("owner.lock", "the data dir is owned by a running warden process", oe.Error()+"\n"+oe.NextStep())
	}

	// Ledger.
	switch l, err := schema.Load(dataDir); {
	case errors.Is(err, schema.ErrNoLedger):
		block(schema.FileName, "no schema ledger", "start this warden once so the data dir is stamped at schema 1, then retry")
	case err != nil:
		block(schema.FileName, "schema ledger is unreadable", err.Error()+"\nrestore it from a backup; the migration never guesses the data format")
	default:
		p.Ledger = LedgerView{Present: true, SchemaVersion: l.SchemaVersion, BinaryVersion: l.BinaryVersion, Interrupted: l.MigrationInterrupted()}
		switch {
		case l.MigrationInterrupted():
			block(schema.FileName, "a previous migration was interrupted ("+l.InProgress.Migration+")",
				"finish it with `warden migrate --resume`, or go back with `warden migrate --restore`")
		case l.SchemaVersion >= schema.Schema2:
			block(schema.FileName, fmt.Sprintf("data dir is already at schema %d; nothing to migrate", l.SchemaVersion), "")
		case l.SchemaVersion != p.From:
			block(schema.FileName, fmt.Sprintf("data dir is at schema %d; the schema-2 cutover starts from schema %d", l.SchemaVersion, p.From),
				"run `warden migrate --apply` first")
		}
	}

	// Target: warden-db must not exist. Never adopt, merge into or replace it.
	if _, err := os.Lstat(p.Target.Dir); err == nil {
		p.Target.Exists = true
		block(wardendb.DirName, wardendb.DirName+"/ already exists beside a schema-1 ledger",
			"the migration will not merge into, adopt, or overwrite it. Inspect it (`warden repair warden-db` verifies it); "+
				"move it aside by hand only if you know it is leftover, then retry. The legacy stores are untouched")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("schema2 plan: stat %s: %w", p.Target.Dir, err)
	}

	// Existing backups are never reused or removed; one at the planned path
	// would make creation fail, so say so now.
	if _, err := os.Lstat(p.BackupDir); err == nil {
		block("backups", "backup directory "+p.BackupDir+" already exists",
			"backups are never overwritten or reused; retry in a few seconds")
	}

	// Source inventory.
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("schema2 plan: read %s: %w", dataDir, err)
	}
	known := map[string]bool{}
	for _, s := range schema2Sources {
		known[strings.SplitN(s.Path, "/", 2)[0]] = true
		e, err := inventoryEntry(dataDir, s)
		if err != nil {
			block(s.Path, "cannot inventory "+s.Path, err.Error())
			continue
		}
		p.Sources = append(p.Sources, e)
		if len(e.Symlinks) > 0 {
			block(s.Path, "symlinks inside a data source", strings.Join(e.Symlinks, ", ")+
				"\nsymlinks are never followed during migration; replace them with real files or move them out")
		}
	}
	// wardendb itself is the target, not an unclassified source.
	known[wardendb.DirName] = true
	for _, de := range entries {
		name := de.Name()
		if known[name] {
			continue
		}
		reason, ext := externalTopLevel(name)
		e := InventoryEntry{Path: name, Present: true, Disposition: DispositionUnclassified}
		if ext {
			e.Disposition, e.Destination = DispositionExternal, reason
		} else {
			p.Findings = append(p.Findings, Finding{Severity: SeverityRepairable, Store: name,
				Message: "unclassified path in the data dir; preserved in place and not migrated"})
		}
		p.Sources = append(p.Sources, e)
	}
	sort.Slice(p.Sources, func(i, j int) bool { return p.Sources[i].Path < p.Sources[j].Path })

	for _, f := range p.Findings {
		if f.Severity == SeverityBlocking {
			p.Blocked = true
		}
	}
	return p, nil
}

// inventoryEntry stats one source without following symlinks.
func inventoryEntry(dataDir string, s sourceSpec) (InventoryEntry, error) {
	e := InventoryEntry{Path: s.Path, Destination: s.Destination, Disposition: s.Disposition}
	root := filepath.Join(dataDir, filepath.FromSlash(s.Path))
	fi, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return e, nil
	}
	if err != nil {
		return e, err
	}
	e.Present = true
	if fi.Mode()&fs.ModeSymlink != 0 {
		e.Symlinks = append(e.Symlinks, s.Path)
		return e, nil
	}
	if !fi.IsDir() {
		e.Files, e.Bytes = 1, fi.Size()
		return e, nil
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dataDir, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			e.Symlinks = append(e.Symlinks, filepath.ToSlash(rel))
		case d.Type().IsRegular():
			if s.SkipSuffix != "" && strings.HasSuffix(d.Name(), s.SkipSuffix) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			e.Files++
			e.Bytes += info.Size()
		}
		return nil
	})
	return e, err
}

// BackupFile is one copied file. Path is data-dir relative, slash-separated.
type BackupFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}

// BackupMeta is the immutable record of the pre-migration copy.
type BackupMeta struct {
	Dir        string       `json:"dir"`
	CreatedAt  time.Time    `json:"created_at"`
	Files      []BackupFile `json:"files"`
	TotalBytes int64        `json:"total_bytes"`
	// LedgerSHA256 is the checksum of schema.json at backup time, so a later
	// restore can prove which ledger the backup belongs to.
	LedgerSHA256 string `json:"ledger_sha256,omitempty"`
}

// Schema2Manifest is the migration manifest. It is created once, exclusively,
// after the backup is complete, and is read-only on disk: its existence means
// "this backup is whole". Phase journalling is later work and lives elsewhere.
type Schema2Manifest struct {
	Format        int              `json:"format"`
	ID            string           `json:"id"`
	From          int              `json:"from"`
	To            int              `json:"to"`
	CreatedAt     time.Time        `json:"created_at"`
	BinaryVersion string           `json:"binary_version"`
	DataDir       string           `json:"data_dir"`
	Sources       []InventoryEntry `json:"sources"`
	Target        TargetInventory  `json:"target"`
	Backup        BackupMeta       `json:"backup"`
}

// ErrSchema2Blocked is returned by BeginSchema2 when preflight is blocking.
var ErrSchema2Blocked = errors.New("migrate: schema-2 preflight is blocking")

// Schema2Run is a started cutover: lock held, backup and manifest on disk.
// The caller (the cutover phases) must Release it.
type Schema2Run struct {
	Manifest *Schema2Manifest
	lock     *ownerlock.Lock
}

// Release drops the ownership lock.
func (r *Schema2Run) Release() error {
	if r == nil || r.lock == nil {
		return nil
	}
	l := r.lock
	r.lock = nil
	return l.Release()
}

// BeginSchema2 takes the ownership lock (refusing while a daemon or another CLI
// holds the data dir), re-plans under it, and, if nothing blocks, creates the
// verified backup and the manifest. It changes no legacy data and does not
// touch the ledger or warden-db.
func BeginSchema2(dataDir, binaryVersion string, now time.Time) (*Schema2Run, error) {
	lock, err := ownerlock.Acquire(dataDir, ownerlock.Info{
		Kind: ownerlock.KindCLI, Version: binaryVersion, Command: "warden migrate schema-2",
	})
	if err != nil {
		return nil, fmt.Errorf("schema-2 migration needs the daemon stopped: %w", err)
	}
	run, err := beginSchema2Locked(dataDir, binaryVersion, now)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	run.lock = lock
	return run, nil
}

// BeginSchema2Locked is BeginSchema2 for callers which already own dataDir's
// owner lock (the migration runner).  It deliberately does not acquire a
// second lock, but retains every preflight/backup invariant of BeginSchema2.
func BeginSchema2Locked(dataDir, binaryVersion string, now time.Time) (*Schema2Run, error) {
	return beginSchema2Locked(dataDir, binaryVersion, now)
}

func beginSchema2Locked(dataDir, binaryVersion string, now time.Time) (*Schema2Run, error) {
	run := &Schema2Run{}
	fail := func(err error) (*Schema2Run, error) { _ = run.Release(); return nil, err }

	// Under our own lock the probe in PlanSchema2 reports "self" as unowned.
	plan, err := PreflightSchema2(context.Background(), dataDir, now)
	if err != nil {
		return fail(err)
	}
	if plan.Blocked {
		var msgs []string
		for _, f := range plan.Findings {
			if f.Severity == SeverityBlocking {
				msgs = append(msgs, f.Store+": "+f.Message)
			}
		}
		return fail(fmt.Errorf("%w: %s", ErrSchema2Blocked, strings.Join(msgs, "; ")))
	}

	idb := make([]byte, 6)
	if _, err := rand.Read(idb); err != nil {
		return fail(err)
	}
	m := &Schema2Manifest{
		Format: Schema2ManifestFormat, ID: "s2-" + hex.EncodeToString(idb),
		From: plan.From, To: plan.To, CreatedAt: now.UTC(), BinaryVersion: binaryVersion,
		DataDir: dataDir, Sources: plan.Sources, Target: plan.Target,
	}
	meta, err := writeBackup(dataDir, plan.BackupDir, now)
	if err != nil {
		return fail(err)
	}
	m.Backup = *meta
	if err := writeManifestOnce(plan.BackupDir, m); err != nil {
		return fail(err)
	}
	run.Manifest = m
	return run, nil
}

// writeBackup copies every migrated source (and the ledger) into dest/data.
// dest is created exclusively: an existing directory is an error, never reuse.
func writeBackup(dataDir, dest string, now time.Time) (*BackupMeta, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return nil, fmt.Errorf("schema2 backup: %w", err)
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return nil, fmt.Errorf("schema2 backup: create %s (existing backups are never reused): %w", dest, err)
	}
	meta := &BackupMeta{Dir: dest, CreatedAt: now.UTC()}
	copyFile := func(rel string) error {
		src := filepath.Join(dataDir, filepath.FromSlash(rel))
		dst := filepath.Join(dest, schema2BackupData, filepath.FromSlash(rel))
		bf, err := copyHashed(src, dst)
		if err != nil {
			return err
		}
		bf.Path = rel
		meta.Files = append(meta.Files, bf)
		meta.TotalBytes += bf.Size
		if rel == schema.FileName {
			meta.LedgerSHA256 = bf.SHA256
		}
		return nil
	}
	if _, err := os.Lstat(schema.Path(dataDir)); err == nil {
		if err := copyFile(schema.FileName); err != nil {
			return nil, err
		}
	}
	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated {
			continue
		}
		root := filepath.Join(dataDir, filepath.FromSlash(s.Path))
		fi, err := os.Lstat(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			if err := copyFile(s.Path); err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dataDir, p)
			rel = filepath.ToSlash(rel)
			switch {
			case d.IsDir():
				return nil
			case !d.Type().IsRegular():
				return fmt.Errorf("%s is not a regular file (symlinks and specials are never copied)", rel)
			case s.SkipSuffix != "" && strings.HasSuffix(d.Name(), s.SkipSuffix):
				return nil
			}
			return copyFile(rel)
		})
		if err != nil {
			return nil, fmt.Errorf("schema2 backup: %s: %w", s.Path, err)
		}
	}
	sort.Slice(meta.Files, func(i, j int) bool { return meta.Files[i].Path < meta.Files[j].Path })
	return meta, nil
}

// copyHashed copies src to dst (exclusive, read-only) and returns its record.
func copyHashed(src, dst string) (BackupFile, error) {
	var bf BackupFile
	in, err := os.Open(src)
	if err != nil {
		return bf, err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return bf, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return bf, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		return bf, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return bf, err
	}
	return BackupFile{Size: n, Mode: uint32(fi.Mode().Perm()), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// writeManifestOnce writes dir/MANIFEST.json read-only via temp + exclusive
// link, so it appears whole or not at all and can never replace another.
func writeManifestOnce(dir string, m *Schema2Manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o400); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmp.Name(), filepath.Join(dir, Schema2ManifestName)); err != nil {
		return fmt.Errorf("schema2 manifest: %w", err)
	}
	return nil
}

// ErrBackupInvalid: the backup is incomplete or does not match its manifest.
var ErrBackupInvalid = errors.New("migrate: schema-2 backup is not valid")

// VerifySchema2Backup re-reads backupDir and proves it matches its manifest:
// manifest present, every listed file present with the recorded size and
// checksum, and no file beyond those listed. A directory without a manifest is
// an incomplete backup and is refused.
func VerifySchema2Backup(backupDir string) (*Schema2Manifest, error) {
	bad := func(format string, a ...any) (*Schema2Manifest, error) {
		return nil, fmt.Errorf("%w: %s: %s", ErrBackupInvalid, backupDir, fmt.Sprintf(format, a...))
	}
	raw, err := os.ReadFile(filepath.Join(backupDir, Schema2ManifestName))
	if errors.Is(err, fs.ErrNotExist) {
		return bad("no %s (incomplete backup)", Schema2ManifestName)
	}
	if err != nil {
		return nil, err
	}
	var m Schema2Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return bad("manifest unreadable: %v", err)
	}
	if m.Format != Schema2ManifestFormat {
		return bad("manifest format %d, this build reads %d", m.Format, Schema2ManifestFormat)
	}
	want := map[string]BackupFile{}
	for _, f := range m.Backup.Files {
		want[f.Path] = f
	}
	root := filepath.Join(backupDir, schema2BackupData)
	seen := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		f, ok := want[rel]
		if !ok {
			return fmt.Errorf("unexpected file %s", rel)
		}
		got, err := hashFile(p)
		if err != nil {
			return err
		}
		if got != f.SHA256 {
			return fmt.Errorf("%s checksum mismatch", rel)
		}
		seen++
		return nil
	})
	if err != nil {
		return bad("%v", err)
	}
	if seen != len(want) {
		return bad("%d of %d files present", seen, len(want))
	}
	return &m, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
