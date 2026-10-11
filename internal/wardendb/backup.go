package wardendb

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/ownerlock"
)

// Backup layout: <dest>/MANIFEST.json describes <dest>/data/, which mirrors the
// warden-db directory. The manifest is written last, so a directory without
// one is an incomplete backup and is refused by VerifyBackup and Restore.
const (
	BackupManifestName = "MANIFEST.json"
	BackupDataDir      = "data"
	// BackupFormat is the manifest format this package writes and reads.
	BackupFormat = 1
)

// Backup and restore audit actions.
const (
	ActionBackup         = "wardendb.backup"
	ActionRestore        = "wardendb.restore"
	ActionRestoreRefused = "wardendb.restore_refused"
)

// StageRestore marks an IntegrityError raised for the contents of a backup.
const StageRestore = "restore"

var (
	// ErrBackupInvalid: the backup is incomplete, was altered, or was written
	// for another format or schema version.
	ErrBackupInvalid = errors.New("wardendb: invalid backup")
	// ErrBackupDestination: the destination is not an empty directory outside
	// warden-db.
	ErrBackupDestination = errors.New("wardendb: unusable backup destination")
	// ErrRestoreRefused: restoring would overwrite existing data, or the
	// backup cannot be restored safely.
	ErrRestoreRefused = errors.New("wardendb: restore refused")
)

// BackupKind says how a backup was taken.
type BackupKind string

const (
	// BackupSnapshot: taken from the owning handle while it served writes, as
	// one transaction-consistent cut (the engine's snapshot). Derived indexes
	// are not included; the engine rebuilds them on the first open.
	BackupSnapshot BackupKind = "snapshot"
	// BackupCopy: an exact copy of every file of a directory at rest, taken
	// under exclusive ownership.
	BackupCopy BackupKind = "copy"
)

// BackupFile is one file of a backup. Path is slash-separated and relative to
// the warden-db directory.
type BackupFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// BackupCollection lists the files of one collection directory.
type BackupCollection struct {
	Name  string       `json:"name"`
	Files []BackupFile `json:"files"`
}

// BackupManifest describes a backup. It holds no record data.
type BackupManifest struct {
	Format        int        `json:"format"`
	SchemaVersion int        `json:"schema_version"`
	Kind          BackupKind `json:"kind"`
	CreatedAt     time.Time  `json:"created_at"`
	// Source is the warden-db directory the backup was taken from.
	Source      string             `json:"source"`
	Collections []BackupCollection `json:"collections"`
	// Files are the database-level files (the transaction coordinator's).
	Files []BackupFile `json:"files"`
	// Checksum is the SHA-256 of the manifest with this field empty.
	Checksum string `json:"checksum"`
}

func (m *BackupManifest) allFiles() []BackupFile {
	out := append([]BackupFile(nil), m.Files...)
	for _, c := range m.Collections {
		out = append(out, c.Files...)
	}
	return out
}

func (m *BackupManifest) sum() string {
	c := *m
	c.Checksum = ""
	b, _ := json.Marshal(&c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// newManifest groups files (keyed by slash path) into a sealed manifest.
// collections names every collection directory, including empty ones.
func newManifest(kind BackupKind, source string, now time.Time, files []BackupFile, collections []string) *BackupManifest {
	m := &BackupManifest{
		Format: BackupFormat, SchemaVersion: SchemaVersion, Kind: kind,
		CreatedAt: now.UTC(), Source: source, Files: []BackupFile{}, Collections: []BackupCollection{},
	}
	byCol := map[string][]BackupFile{}
	for _, c := range collections {
		byCol[c] = []BackupFile{}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		if i := strings.IndexByte(f.Path, '/'); i > 0 {
			byCol[f.Path[:i]] = append(byCol[f.Path[:i]], f)
		} else {
			m.Files = append(m.Files, f)
		}
	}
	names := make([]string, 0, len(byCol))
	for n := range byCol {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m.Collections = append(m.Collections, BackupCollection{Name: n, Files: byCol[n]})
	}
	m.Checksum = m.sum()
	return m
}

// lockFile reports the ownership bookkeeping files, which are never data.
func lockFile(rel string) bool { return rel == ownerlock.FileName || rel == "LOCK" }

// inside reports whether p is dir or lies under it.
func inside(dir, p string) bool {
	a, err1 := filepath.Abs(dir)
	b, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return true
	}
	rel, err := filepath.Rel(a, b)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// prepareDest makes dest an empty directory outside dir and returns the
// cleanup to run when the backup fails.
func prepareDest(dir, dest string) (func(), error) {
	if dest == "" {
		return nil, fmt.Errorf("%w: empty path", ErrBackupDestination)
	}
	if inside(dir, dest) {
		return nil, fmt.Errorf("%w: %s is inside %s", ErrBackupDestination, dest, dir)
	}
	created := false
	ents, err := os.ReadDir(dest)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBackupDestination, err)
		}
		created = true
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrBackupDestination, err)
	case len(ents) > 0:
		return nil, fmt.Errorf("%w: %s is not empty", ErrBackupDestination, dest)
	}
	return func() {
		if created {
			_ = os.RemoveAll(dest)
			return
		}
		_ = os.RemoveAll(filepath.Join(dest, BackupDataDir))
		_ = os.Remove(filepath.Join(dest, BackupManifestName))
	}, nil
}

// safeRel validates a slash-separated relative path from an archive or a
// manifest: it must stay inside the directory it is joined to.
func safeRel(p string) error {
	if p == "" || path.IsAbs(p) || strings.ContainsRune(p, '\\') || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("unsafe path %q", p)
	}
	return nil
}

// writeHashed streams r into a new file at dst and returns its size and hash.
func writeHashed(dst string, r io.Reader) (int64, string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, "", err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, hex.EncodeToString(h.Sum(nil)), err
}

func copyHashed(src, dst string) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = in.Close() }()
	return writeHashed(dst, in)
}

func hashFile(p string) (int64, string, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return n, hex.EncodeToString(h.Sum(nil)), err
}

func writeManifest(dest string, m *BackupManifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dest, BackupManifestName+".tmp")
	if _, _, err := writeHashed(tmp, strings.NewReader(string(b))); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dest, BackupManifestName))
}

func backupEvent(mon *monitor, m *BackupManifest, dest string) {
	mon.emit(ActionBackup, "", map[string]string{
		"kind":        string(m.Kind),
		"destination": dest,
		"collections": strconv.Itoa(len(m.Collections)),
		"files":       strconv.Itoa(len(m.allFiles())),
	})
}

// Backup writes a consistent backup of the whole database to dest while the
// handle keeps serving writes. It reuses the engine's snapshot, which is one
// transaction-consistent cut across every collection and the transaction
// coordinator, and records every file with its SHA-256 in the manifest. dest
// must be an empty or absent directory outside warden-db. A failed backup
// leaves nothing behind.
func (db *DB) Backup(ctx context.Context, dest string) (*BackupManifest, error) {
	handle, err := db.Handle()
	if err != nil {
		return nil, err
	}
	cleanup, err := prepareDest(db.dir, dest)
	if err != nil {
		return nil, err
	}
	m, err := func() (*BackupManifest, error) {
		pr, pw := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = pw.CloseWithError(handle.Engine().SnapshotTo(pw))
		}()
		files, err := extractSnapshot(ctx, pr, filepath.Join(dest, BackupDataDir))
		_ = pr.CloseWithError(err)
		<-done
		if err != nil {
			return nil, err
		}
		// The snapshot covers the collections the engine has open. Refuse to
		// call the backup whole if a collection directory was left out.
		got := map[string]bool{}
		for _, f := range files {
			if i := strings.IndexByte(f.Path, '/'); i > 0 {
				got[f.Path[:i]] = true
			}
		}
		ents, err := os.ReadDir(db.dir)
		if err != nil {
			return nil, err
		}
		var cols []string
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			cols = append(cols, e.Name())
			segs, _ := filepath.Glob(filepath.Join(db.dir, e.Name(), "seg_*.ndjson"))
			if len(segs) > 0 && !got[e.Name()] {
				return nil, fmt.Errorf("collection %q is on disk but not in the engine snapshot", e.Name())
			}
		}
		m := newManifest(BackupSnapshot, db.dir, time.Now(), files, cols)
		return m, writeManifest(dest, m)
	}()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("wardendb: backup %s: %w", db.dir, err)
	}
	backupEvent(db.mon, m, dest)
	return m, nil
}

// extractSnapshot unpacks the engine's gzip-compressed tar into dataDir.
func extractSnapshot(ctx context.Context, r io.Reader, dataDir string) ([]BackupFile, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	var files []BackupFile
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := safeRel(hdr.Name); err != nil {
			return nil, err
		}
		n, sum, err := writeHashed(filepath.Join(dataDir, filepath.FromSlash(hdr.Name)), tr)
		if err != nil {
			return nil, err
		}
		files = append(files, BackupFile{Path: hdr.Name, Size: n, SHA256: sum})
	}
}

// Backup writes a backup of the warden-db under dataDir, which must not be
// open anywhere: Backup takes exclusive ownership for the duration (a live
// owner yields *LockedError; use DB.Backup on the owner instead) and copies
// every file exactly as it sits on disk. It never modifies the source. The
// copy is taken whatever the health of the directory, so that a damaged
// database can be preserved before a repair; Restore verifies what it is
// asked to restore. dest must be an empty or absent directory outside
// warden-db. A failed backup leaves nothing behind.
func Backup(ctx context.Context, dataDir, dest string, opts ...Option) (*BackupManifest, error) {
	cfg := toolConfig("backup", opts)
	dir := Dir(dataDir)
	mon := &monitor{dir: dir, auditor: cfg.auditor, metrics: cfg.metrics}
	lock, err := acquireExisting(dir, cfg, mon)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	m, err := copyBackup(ctx, dir, dest)
	if err != nil {
		return nil, err
	}
	backupEvent(mon, m, dest)
	return m, nil
}

// toolConfig resolves the options of an offline entry point. Its owner kind
// defaults to the CLI: it is never the daemon's long-lived handle.
func toolConfig(command string, opts []Option) config {
	cfg := config{owner: ownerlock.Info{Kind: ownerlock.KindCLI, Command: "warden-db " + command}}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	return cfg
}

// acquireExisting takes exclusive ownership of a warden-db directory that
// must already exist. It creates nothing but the ownership lock file.
func acquireExisting(dir string, cfg config, mon *monitor) (*ownerlock.Lock, error) {
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, dir)
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("wardendb: %s is not a directory", dir)
	}
	return acquire(dir, cfg, mon)
}

func acquire(dir string, cfg config, mon *monitor) (*ownerlock.Lock, error) {
	lock, err := ownerlock.Acquire(dir, cfg.owner)
	if err != nil {
		if owned, ok := errors.AsType[*ownerlock.OwnedError](err); ok {
			return nil, mon.locked(&LockedError{Dir: dir, Owner: owned.Owner, cause: err})
		}
		return nil, fmt.Errorf("wardendb: lock %s: %w", dir, err)
	}
	return lock, nil
}

// copyBackup copies every file under dir into dest. The caller holds the
// ownership lock, so the files are at rest.
func copyBackup(ctx context.Context, dir, dest string) (*BackupManifest, error) {
	cleanup, err := prepareDest(dir, dest)
	if err != nil {
		return nil, err
	}
	var files []BackupFile
	var cols []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && !strings.Contains(rel, "/") {
				cols = append(cols, rel)
			}
			return nil
		}
		if lockFile(rel) || !d.Type().IsRegular() {
			return nil
		}
		n, sum, err := copyHashed(p, filepath.Join(dest, BackupDataDir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		files = append(files, BackupFile{Path: rel, Size: n, SHA256: sum})
		return nil
	})
	var m *BackupManifest
	if err == nil {
		if err = os.MkdirAll(filepath.Join(dest, BackupDataDir), 0o700); err == nil {
			m = newManifest(BackupCopy, dir, time.Now(), files, cols)
			err = writeManifest(dest, m)
		}
	}
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("wardendb: backup %s: %w", dir, err)
	}
	return m, nil
}

// VerifyBackup checks the backup at backupDir against its manifest: format and
// schema version, the manifest's own checksum, and the size and SHA-256 of
// every file, with no file missing and none unaccounted for. It returns the
// manifest of a backup that is exactly what was written.
func VerifyBackup(backupDir string) (*BackupManifest, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrBackupInvalid, backupDir, fmt.Sprintf(format, args...))
	}
	b, err := os.ReadFile(filepath.Join(backupDir, BackupManifestName))
	if err != nil {
		return nil, bad("manifest unreadable (incomplete backup?): %v", err)
	}
	var m BackupManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, bad("manifest does not parse: %v", err)
	}
	switch {
	case m.Format != BackupFormat:
		return nil, bad("manifest format %d, this build reads %d", m.Format, BackupFormat)
	case m.SchemaVersion != SchemaVersion:
		return nil, bad("schema version %d, this build hosts %d", m.SchemaVersion, SchemaVersion)
	case m.Kind != BackupSnapshot && m.Kind != BackupCopy:
		return nil, bad("unknown backup kind %q", m.Kind)
	case m.Checksum == "" || m.Checksum != m.sum():
		return nil, bad("manifest checksum mismatch")
	}
	data := filepath.Join(backupDir, BackupDataDir)
	want := map[string]bool{}
	for _, c := range m.Collections {
		if err := safeRel(c.Name); err != nil || strings.Contains(c.Name, "/") {
			return nil, bad("unsafe collection name %q", c.Name)
		}
		for _, f := range c.Files {
			if !strings.HasPrefix(f.Path, c.Name+"/") {
				return nil, bad("%s is listed under collection %q", f.Path, c.Name)
			}
		}
	}
	for _, f := range m.allFiles() {
		if err := safeRel(f.Path); err != nil || lockFile(f.Path) || want[f.Path] {
			return nil, bad("unsafe or duplicate entry %q", f.Path)
		}
		want[f.Path] = true
		n, sum, err := hashFile(filepath.Join(data, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, bad("%s: %v", f.Path, err)
		}
		if n != f.Size || sum != f.SHA256 {
			return nil, bad("%s does not match its manifest entry", f.Path)
		}
	}
	err = filepath.WalkDir(data, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(data, p)
		if !want[filepath.ToSlash(rel)] {
			return fmt.Errorf("%s is not in the manifest", filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, bad("%v", err)
	}
	return &m, nil
}

// RestoreOptions configures Restore. The zero value refuses to replace
// anything and refuses a backup that does not verify clean.
type RestoreOptions struct {
	// Overwrite permits restoring over a warden-db that already holds data.
	// The existing contents are moved aside (RestoreResult.Replaced), never
	// deleted. A database that is open anywhere is never overwritten.
	Overwrite bool
	// AllowCorrupt permits restoring a backup whose contents have blocking
	// integrity findings — the pre-repair backup of a damaged database, to
	// undo a repair. Open will refuse the restored directory.
	AllowCorrupt bool
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Dir      string
	Manifest *BackupManifest
	// Files is the number of files restored.
	Files int
	// Replaced is the directory the previous contents were moved to, or empty
	// when the target held no data.
	Replaced string
	// Verify is the read-only verification of the restored directory.
	Verify     *VerifyResult
	StartedAt  time.Time
	FinishedAt time.Time
}

// Restore restores the backup at backupDir into the warden-db under dataDir.
//
// It verifies the backup against its manifest first (VerifyBackup), takes
// exclusive ownership of the target (a live owner yields *LockedError and is
// never overwritten), and refuses a target that already holds data unless
// RestoreOptions.Overwrite is set. The files are staged beside the target,
// re-checked against the manifest and verified as a database before the
// target is touched: a backup with blocking integrity findings is refused with
// *IntegrityError unless AllowCorrupt is set. Only then are the previous
// contents moved aside and the staged files moved into place.
//
// A restored directory passes a strict Open. A snapshot backup carries no
// derived indexes, so it reports HealthDegraded until the engine has rebuilt
// them on that first open.
func Restore(ctx context.Context, backupDir, dataDir string, ro RestoreOptions, opts ...Option) (*RestoreResult, error) {
	if dataDir == "" {
		return nil, errors.New("wardendb: empty data directory")
	}
	cfg := toolConfig("restore", opts)
	dir := Dir(dataDir)
	mon := &monitor{dir: dir, auditor: cfg.auditor, metrics: cfg.metrics}
	res := &RestoreResult{Dir: dir, StartedAt: time.Now()}
	refuse := func(err error) (*RestoreResult, error) {
		mon.emit(ActionRestoreRefused, "", map[string]string{"backup": backupDir, "error": err.Error()})
		return nil, err
	}

	m, err := VerifyBackup(backupDir)
	if err != nil {
		return refuse(err)
	}
	if inside(dir, backupDir) {
		return refuse(fmt.Errorf("%w: backup %s is inside the target %s", ErrRestoreRefused, backupDir, dir))
	}
	for _, f := range m.allFiles() {
		// An interrupted compaction swap is recorded with paths into the
		// directory it was taken from; rolling it forward elsewhere would act
		// on the wrong files.
		if path.Base(f.Path) == "compact.manifest" {
			return refuse(fmt.Errorf("%w: backup holds an interrupted compaction (%s); it can only be put back by hand at %s", ErrRestoreRefused, f.Path, m.Source))
		}
	}
	res.Manifest = m

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("wardendb: create %s: %w", dir, err)
	}
	lock, err := acquire(dir, cfg, mon)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var existing []string
	for _, e := range ents {
		if !lockFile(e.Name()) {
			existing = append(existing, e.Name())
		}
	}
	if len(existing) > 0 && !ro.Overwrite {
		return refuse(fmt.Errorf("%w: %s already holds data (%d entries); set Overwrite to move it aside", ErrRestoreRefused, dir, len(existing)))
	}

	// Stage beside the target, on the same filesystem, so that moving the
	// files into place is a rename.
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	stage := filepath.Join(dataDir, "."+DirName+".restore-"+stamp)
	if err := os.Mkdir(stage, 0o700); err != nil {
		return nil, fmt.Errorf("wardendb: restore staging: %w", err)
	}
	staged := false
	defer func() {
		if !staged {
			_ = os.RemoveAll(stage)
		}
	}()
	for _, c := range m.Collections {
		if err := os.MkdirAll(filepath.Join(stage, c.Name), 0o700); err != nil {
			return nil, err
		}
	}
	for _, f := range m.allFiles() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel := filepath.FromSlash(f.Path)
		n, sum, err := copyHashed(filepath.Join(backupDir, BackupDataDir, rel), filepath.Join(stage, rel))
		if err != nil {
			return nil, fmt.Errorf("wardendb: restore %s: %w", f.Path, err)
		}
		if n != f.Size || sum != f.SHA256 {
			return refuse(fmt.Errorf("%w: %s: %s changed while it was being restored", ErrBackupInvalid, backupDir, f.Path))
		}
	}
	check, err := verifyDir(ctx, stage, true, LockStatus{State: LockUnowned})
	if err != nil {
		return nil, err
	}
	if check.Blocking > 0 && !ro.AllowCorrupt {
		ie := mon.integrityError(StageRestore, "", check.blockingFindings(), check.Blocking, nil)
		ie.Dir = backupDir
		return nil, mon.refuse(ie)
	}

	if len(existing) > 0 {
		res.Replaced = filepath.Join(dataDir, DirName+".replaced-"+stamp)
		if err := os.Mkdir(res.Replaced, 0o700); err != nil {
			return nil, fmt.Errorf("wardendb: restore: %w", err)
		}
		for _, name := range existing {
			if err := os.Rename(filepath.Join(dir, name), filepath.Join(res.Replaced, name)); err != nil {
				return nil, fmt.Errorf("wardendb: restore: moving the previous contents to %s failed part-way: %w", res.Replaced, err)
			}
		}
	}
	staged = true // from here on the staging directory holds the only copy in flight
	moved, err := os.ReadDir(stage)
	if err != nil {
		return nil, err
	}
	for _, e := range moved {
		if err := os.Rename(filepath.Join(stage, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return nil, fmt.Errorf("wardendb: restore: moving %s into place failed part-way; the rest is in %s: %w", e.Name(), stage, err)
		}
	}
	_ = os.Remove(stage)

	res.Files = len(m.allFiles())
	if res.Verify, err = verifyDir(ctx, dir, true, LockStatus{State: LockUnowned}); err != nil {
		return nil, err
	}
	res.FinishedAt = time.Now()
	mon.emit(ActionRestore, "", map[string]string{
		"backup":      backupDir,
		"kind":        string(m.Kind),
		"files":       strconv.Itoa(res.Files),
		"collections": strconv.Itoa(len(m.Collections)),
		"replaced":    res.Replaced,
		"health":      string(res.Verify.Health),
	})
	return res, nil
}
