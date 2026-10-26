package updater

// Schema-3 transaction machinery is deliberately private and not wired into
// update until cutover provides complete format-3 readers and readiness. Public
// migrate commands cannot obtain this authority. There is no environment bypass.

import (
	"bufio"
	"context"
	"crypto/sha256"
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

	"github.com/srjn45/warden/internal/migrate/schema3"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/schema"
)

const schema3BackupPrefix = "pre-schema3-"
const schema3CapabilityVersion = 1

type schema3File struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}
type schema3Manifest struct {
	Format           int             `json:"format"`
	Transaction      string          `json:"transaction"`
	Migration        string          `json:"migration"`
	Policy           string          `json:"policy"`
	From             int             `json:"source_format"`
	To               int             `json:"target_format"`
	Capability       int             `json:"capability_version"`
	DataDir          string          `json:"data_dir"`
	CreatedAt        time.Time       `json:"created_at"`
	BinaryPath       string          `json:"binary_path"`
	OldVersion       string          `json:"source_version"`
	TargetVersion    string          `json:"target_version"`
	OldBinaryHash    string          `json:"source_binary_sha256"`
	TargetBinaryHash string          `json:"target_binary_sha256"`
	LedgerHash       string          `json:"source_ledger_sha256"`
	Files            []schema3File   `json:"files"`
	RestorePaths     []string        `json:"restore_paths"`
	DeletePaths      []string        `json:"restore_deletion_set"`
	External         []string        `json:"external_untouched"`
	SourceIndexes    schema3.Indexes `json:"source_indexes"`
	TargetIndexes    schema3.Indexes `json:"required_target_indexes"`
	Report           *schema3.Report `json:"mapping"`
	SourceTree       string          `json:"source_tree"`
}
type schema3Journal struct {
	Seq          int    `json:"seq"`
	Previous     string `json:"previous"`
	ManifestHash string `json:"manifest_hash"`
	Transaction  string `json:"transaction"`
	Step         string `json:"step"`
	Complete     bool   `json:"complete"`
	Source       string `json:"source"`
	Staging      string `json:"staging"`
	Parked       string `json:"parked"`
	TargetBinary string `json:"target_binary_sha256"`
	Input        string `json:"input"`
	Output       string `json:"output"`
}
type schema3Transaction struct {
	dir, id, bundle, stage, park, journal string
	lock                                  *ownerlock.Lock
	manifest                              *schema3Manifest
	manifestHash                          string
	records                               []schema3Journal
	fault                                 func(string) error
}

func schema3Path(data, id string) (*schema3Transaction, error) {
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return nil, fmt.Errorf("schema3: unsafe transaction id")
	}
	abs, err := filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if canonical != abs {
		return nil, fmt.Errorf("schema3: symlink data directory refused")
	}
	t := &schema3Transaction{dir: abs, id: id}
	t.bundle = filepath.Join(abs, "backups", schema3BackupPrefix+id)
	t.stage = filepath.Join(abs, ".schema3-staging-"+id)
	t.park = filepath.Join(abs, ".schema2-parked-"+id)
	t.journal = filepath.Join(abs, ".schema3-transaction-"+id+".jsonl")
	return t, nil
}

// acquireSchema3 verifies shutdown and obtains the sole owner lock before the
// first backup/mapping operation. ErrNoServiceManager is never accepted here.
func acquireSchema3(ctx context.Context, t *schema3Transaction, svc ServiceController, probe Prober) error {
	if svc == nil || probe == nil {
		return fmt.Errorf("schema3: explicit service and health controllers required")
	}
	state := svc.State(ctx)
	if state.Kind == ServiceManual || state.Kind == ServiceNone {
		return fmt.Errorf("schema3: managed stopped service required")
	}
	if err := svc.Stop(ctx); err != nil {
		return fmt.Errorf("schema3: stop: %w", err)
	}
	if !svc.Exited(ctx) {
		return fmt.Errorf("schema3: daemon exit unconfirmed")
	}
	if _, err := probe.Probe(ctx); err == nil {
		return fmt.Errorf("schema3: daemon still answers health")
	}
	lock, err := ownerlock.Acquire(t.dir, ownerlock.Info{Kind: ownerlock.KindCLI, Command: "warden update schema3"})
	if err != nil {
		return err
	}
	t.lock = lock
	return nil
}
func (t *schema3Transaction) close() error { err := t.lock.Release(); t.lock = nil; return err }
func (t *schema3Transaction) hit(point string) error {
	if t.fault != nil {
		return t.fault(point)
	}
	return nil
}
func (t *schema3Transaction) record(step string, complete bool, input, output string) error {
	if t.lock == nil {
		return fmt.Errorf("schema3: updater owner lock required")
	}
	phase := "intent"
	if complete {
		phase = "complete"
	}
	point := step + "/" + phase
	if err := t.hit("before/" + point); err != nil {
		return err
	}
	prev := ""
	if len(t.records) > 0 {
		prev = schema3.Digest(t.records[len(t.records)-1])
	}
	row := schema3Journal{len(t.records) + 1, prev, t.manifestHash, t.id, step, complete, filepath.Join(t.dir, "warden-db"), t.stage, t.park, t.manifest.TargetBinaryHash, input, output}
	if fi, err := os.Lstat(t.journal); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("schema3: unsafe journal path")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(t.journal, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(row)
	if err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = syncSchema3Dir(t.dir); err != nil {
		return err
	}
	t.records = append(t.records, row)
	return t.hit("after/" + point)
}
func schema3HashFile(p string) (string, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("schema3: unsafe file %s", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}
func schema3Inventory(root string) ([]schema3File, error) {
	var out []schema3File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == root {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		fi, e := d.Info()
		if e != nil {
			return e
		}
		kind := "file"
		hash := ""
		if d.IsDir() {
			kind = "directory"
		} else if d.Type().IsRegular() {
			hash, e = schema3HashFile(p)
			if e != nil {
				return e
			}
		} else {
			return fmt.Errorf("schema3: unsafe path %s", p)
		}
		out = append(out, schema3File{filepath.ToSlash(rel), kind, func() int64 {
			if kind == "directory" {
				return 0
			}
			return fi.Size()
		}(), uint32(fi.Mode().Perm()), hash})
		return nil
	})
	return out, err
}
func schema3Tree(root string) (string, error) {
	files, err := schema3Inventory(root)
	if err != nil {
		return "", err
	}
	for i := range files {
		if files[i].Type == "directory" {
			files[i].Size = 0
		}
	}
	return schema3.Digest(files), nil
}
func schema3CopyFile(src, dst string) error {
	if _, err := schema3HashFile(src); err != nil {
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	ce := out.Close()
	if err != nil {
		return err
	}
	return ce
}
func syncSchema3Dir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}
func writeSchema3Once(path string, v any) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		_, err = f.Write(append(raw, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return syncSchema3Dir(filepath.Dir(path))
}

// begin seals the immutable pre-journal source ledger, full database/config
// inventory, exact old binary and deterministic mapping before promotion.
func (t *schema3Transaction) begin(ctx context.Context, oldBin, targetBin, oldVersion, targetVersion string, configPaths []string, freeBytes uint64) error {
	if t.lock == nil {
		return fmt.Errorf("schema3: updater lock required")
	}
	if !filepath.IsAbs(oldBin) || !filepath.IsAbs(targetBin) {
		return fmt.Errorf("schema3: absolute binary paths required")
	}
	l, err := schema.Load(t.dir)
	if err != nil {
		return err
	}
	if l.SchemaVersion != 2 || l.InProgress != nil {
		return fmt.Errorf("schema3: exact committed schema 2 required")
	}
	if _, err = os.Lstat(t.journal); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("schema3: transaction already exists; resume through update")
	}
	source := filepath.Join(t.dir, "warden-db")
	if err = schema3.SafeTree(source); err != nil {
		return err
	}
	g, indexes, err := schema3.Read(ctx, source)
	if err != nil {
		return err
	}
	_, report, err := schema3.Map(g)
	if err != nil {
		return fmt.Errorf("schema3: mapping refused: %w; report=%s", err, string(mustSchema3JSON(report)))
	}
	oldHash, err := schema3HashFile(oldBin)
	if err != nil {
		return err
	}
	targetHash, err := schema3HashFile(targetBin)
	if err != nil {
		return err
	}
	ledgerHash, err := schema3HashFile(filepath.Join(t.dir, schema.FileName))
	if err != nil {
		return err
	}
	tree, err := schema3Tree(source)
	if err != nil {
		return err
	}
	paths := []string{"warden-db", schema.FileName}
	seen := map[string]bool{"warden-db": true, schema.FileName: true}
	for _, p := range configPaths {
		if p == "" || filepath.IsAbs(p) || filepath.Clean(p) != p || strings.Contains(p, "/") || strings.Contains(p, "\\") || seen[p] || p == "backups" || strings.HasPrefix(p, ".") {
			return fmt.Errorf("schema3: unsafe/duplicate restore path %q", p)
		}
		if _, err = schema3HashFile(filepath.Join(t.dir, p)); err != nil {
			return err
		}
		seen[p] = true
		paths = append(paths, p)
	}
	var required uint64
	for _, p := range paths {
		fi, e := os.Stat(filepath.Join(t.dir, p))
		if e != nil {
			return e
		}
		if fi.IsDir() {
			files, e := schema3Inventory(filepath.Join(t.dir, p))
			if e != nil {
				return e
			}
			for _, f := range files {
				if f.Type == "file" {
					required += uint64(f.Size)
				}
			}
		} else {
			required += uint64(fi.Size())
		}
	}
	fi, err := os.Stat(oldBin)
	if err != nil {
		return err
	}
	required = required*4 + uint64(fi.Size()) + 16<<20
	if freeBytes < required {
		return fmt.Errorf("schema3: insufficient space: need %d, available %d", required, freeBytes)
	}
	m := &schema3Manifest{Format: 1, Transaction: t.id, Migration: schema3.MigrationID, Policy: schema3.Policy, From: 2, To: 3, Capability: 1, DataDir: t.dir, CreatedAt: time.Now().UTC(), BinaryPath: oldBin, OldVersion: oldVersion, TargetVersion: targetVersion, OldBinaryHash: oldHash, TargetBinaryHash: targetHash, LedgerHash: ledgerHash, RestorePaths: paths, DeletePaths: []string{"warden-db"}, SourceIndexes: indexes, TargetIndexes: schema3.RequiredIndexes, Report: report, SourceTree: tree}
	t.manifest = m
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !seen[e.Name()] && e.Name() != "backups" && e.Name() != ownerlock.FileName {
			m.External = append(m.External, e.Name())
		}
	}
	// Compute the final manifest identity before any backup write. Persist its
	// immutable plan outside the bundle so interrupted copies can resume without
	// ever choosing a partially migrated generation as their baseline.
	m.Files, err = t.plannedFiles(oldBin)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	t.manifestHash = fmt.Sprintf("sha256:%x", sha256.Sum256(append(raw, '\n')))
	if err = writeSchema3Once(t.planPath(), m); err != nil {
		return err
	}
	if err = t.record("locked", false, tree, ""); err != nil {
		return err
	}
	if err = t.record("locked", true, tree, ""); err != nil {
		return err
	}
	if err = t.completeBackup(oldBin); err != nil {
		return err
	}
	for _, step := range []string{"mapped"} {
		if err = t.record(step, false, tree, schema3.Digest(report.Target)); err != nil {
			return err
		}
		if err = t.record(step, true, tree, schema3.Digest(report.Target)); err != nil {
			return err
		}
	}
	l.InProgress = t.progress("backed_up")
	return schema.Save(t.dir, l)
}
func mustSchema3JSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func (t *schema3Transaction) verifyBundle() error {
	path := filepath.Join(t.bundle, "MANIFEST.json")
	hash, err := schema3HashFile(path)
	if err != nil {
		return err
	}
	if t.manifestHash != "" && hash != t.manifestHash {
		return fmt.Errorf("schema3: manifest hash mismatch")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m schema3Manifest
	if err = json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.Format != 1 || m.Transaction != t.id || m.DataDir != t.dir || m.Migration != schema3.MigrationID || m.Policy != schema3.Policy || m.From != 2 || m.To != 3 || m.Capability != 1 {
		return fmt.Errorf("schema3: incompatible backup identity")
	}
	files, err := schema3Inventory(t.bundle)
	if err != nil {
		return err
	}
	actual := map[string]schema3File{}
	for _, f := range files {
		if f.Path != "MANIFEST.json" {
			if f.Type == "directory" {
				f.Size = 0
			}
			actual[f.Path] = f
		}
	}
	expected := map[string]schema3File{}
	for _, f := range m.Files {
		if f.Type == "directory" {
			f.Size = 0
		}
		if f.Path == "" || filepath.IsAbs(f.Path) || filepath.ToSlash(filepath.Clean(f.Path)) != f.Path || strings.HasPrefix(f.Path, "../") {
			return fmt.Errorf("schema3: unsafe manifest path")
		}
		if _, dup := expected[f.Path]; dup {
			return fmt.Errorf("schema3: duplicate manifest path")
		}
		expected[f.Path] = f
	}
	if schema3.Digest(actual) != schema3.Digest(expected) {
		return fmt.Errorf("schema3: backup inventory mismatch")
	}
	if got, err := schema3HashFile(filepath.Join(t.bundle, "bin", "warden")); err != nil || got != m.OldBinaryHash {
		return fmt.Errorf("schema3: paired binary mismatch")
	}
	if got, err := schema3HashFile(filepath.Join(t.bundle, "data", schema.FileName)); err != nil || got != m.LedgerHash {
		return fmt.Errorf("schema3: paired ledger mismatch")
	}
	t.manifest = &m
	t.manifestHash = hash
	return nil
}
func (t *schema3Transaction) load(targetBin string) error {
	if t.lock == nil {
		return fmt.Errorf("schema3: updater lock required")
	}
	plannedHash, err := schema3HashFile(t.planPath())
	if err != nil {
		return err
	}
	if t.manifestHash != "" && plannedHash != t.manifestHash {
		return fmt.Errorf("schema3: planned manifest identity changed")
	}
	t.manifestHash = plannedHash
	sealed := schema3Exists(filepath.Join(t.bundle, "MANIFEST.json"))
	if sealed {
		if err := t.verifyBundle(); err != nil {
			return err
		}
	} else {
		raw, err := os.ReadFile(t.planPath())
		if err != nil {
			return err
		}
		var m schema3Manifest
		if err = json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if m.Transaction != t.id || m.DataDir != t.dir || m.Policy != schema3.Policy || m.Format != 1 {
			return fmt.Errorf("schema3: invalid pending backup plan")
		}
		t.manifest = &m
		t.manifestHash, err = schema3HashFile(t.planPath())
		if err != nil {
			return err
		}
	}
	h, err := schema3HashFile(targetBin)
	if err != nil {
		return err
	}
	if h != t.manifest.TargetBinaryHash {
		return fmt.Errorf("schema3: wrong target binary")
	}
	f, err := os.Open(t.journal)
	if errors.Is(err, os.ErrNotExist) && !sealed {
		return t.completeBackup(t.manifest.BinaryPath)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 4<<20)
	prev := ""
	t.records = nil
	for sc.Scan() {
		var row schema3Journal
		if err = json.Unmarshal(sc.Bytes(), &row); err != nil {
			return fmt.Errorf("schema3: torn/corrupt journal retained: %w", err)
		}
		if row.Seq != len(t.records)+1 || row.Previous != prev || row.ManifestHash != t.manifestHash || row.Transaction != t.id || row.Staging != t.stage || row.Parked != t.park || row.Source != filepath.Join(t.dir, "warden-db") || row.TargetBinary != h {
			return fmt.Errorf("schema3: journal identity mismatch")
		}
		t.records = append(t.records, row)
		prev = schema3.Digest(row)
	}
	if err = sc.Err(); err != nil {
		return err
	}
	if !sealed {
		return t.completeBackup(t.manifest.BinaryPath)
	}
	return nil
}
func (t *schema3Transaction) done(step string) bool {
	for _, r := range t.records {
		if r.Step == step && r.Complete {
			return true
		}
	}
	return false
}
func schema3Exists(p string) bool { _, err := os.Lstat(p); return err == nil }
func (t *schema3Transaction) verifyTarget(ctx context.Context, dir string) error {
	g, indexes, err := schema3.Read(ctx, dir)
	if err != nil {
		return err
	}
	if err = schema3.VerifyIndexes(indexes, t.manifest.SourceIndexes, g); err != nil {
		return err
	}
	if schema3.Digest(schema3.Digests(g)) != schema3.Digest(t.manifest.Report.Target) {
		return fmt.Errorf("schema3: divergent target generation %s", dir)
	}
	return nil
}
func (t *schema3Transaction) promote(ctx context.Context) error {
	if t.lock == nil || t.manifest == nil {
		return fmt.Errorf("schema3: authenticated updater transaction required")
	}
	if err := t.verifyBundle(); err != nil {
		return err
	}
	source := filepath.Join(t.dir, "warden-db")
	if t.done("readiness_verified") {
		l, err := schema.Load(t.dir)
		if err != nil {
			return err
		}
		if l.SchemaVersion != 3 || l.InProgress != nil || l.BinaryVersion != t.manifest.TargetVersion {
			return fmt.Errorf("schema3: finalized ledger changed")
		}
		g, indexes, err := schema3.Read(ctx, source)
		if err != nil {
			return err
		}
		if err = schema3.ValidateTarget(g); err != nil {
			return err
		}
		return schema3.VerifyIndexes(indexes, t.manifest.SourceIndexes, g)
	}
	// Always regenerate from the sealed source, never a partially converted DB.
	g, idx, err := schema3.Read(ctx, filepath.Join(t.bundle, "data", "warden-db"))
	if err != nil {
		return err
	}
	mapped, report, err := schema3.Map(g)
	if err != nil {
		return err
	}
	if schema3.Digest(report) != schema3.Digest(t.manifest.Report) {
		return fmt.Errorf("schema3: mapping policy/output differs from manifest")
	}
	if !schema3Exists(t.park) {
		if tree, e := schema3Tree(source); e != nil || tree != t.manifest.SourceTree {
			return fmt.Errorf("schema3: source generation diverged")
		}
	}
	if !schema3Exists(t.park) || schema3Exists(t.stage) {
		if err = t.record("staged", false, t.manifest.SourceTree, schema3.Digest(report.Target)); err != nil {
			return err
		}
		if schema3Exists(t.stage) {
			if err = t.verifyTarget(ctx, t.stage); err != nil {
				return err
			}
		} else {
			if err = t.hit("before/staging-write"); err != nil {
				return err
			}
			if err = schema3.WriteStaging(ctx, t.stage, mapped, idx); err != nil {
				return err
			}
			if err = t.hit("after/staging-write"); err != nil {
				return err
			}
		}
		if err = t.record("staged", true, t.manifest.SourceTree, schema3.Digest(report.Target)); err != nil {
			return err
		}
		if err = t.record("verified", false, "", schema3.Digest(report.Target)); err != nil {
			return err
		}
		if err = t.verifyTarget(ctx, t.stage); err != nil {
			return err
		}
		if err = t.record("verified", true, "", schema3.Digest(report.Target)); err != nil {
			return err
		}
	}
	if !schema3Exists(t.park) {
		if err = t.record("old_generation_parked", false, t.manifest.SourceTree, ""); err != nil {
			return err
		}
		if err = t.hit("before/park-rename"); err != nil {
			return err
		}
		if err = os.Rename(source, t.park); err != nil {
			return err
		}
		if err = syncSchema3Dir(t.dir); err != nil {
			return err
		}
		if err = t.hit("after/park-rename"); err != nil {
			return err
		}
	}
	if tree, e := schema3Tree(t.park); e != nil || tree != t.manifest.SourceTree {
		return fmt.Errorf("schema3: parked generation diverged")
	}
	if !t.done("old_generation_parked") {
		if err = t.record("old_generation_parked", true, t.manifest.SourceTree, ""); err != nil {
			return err
		}
	}
	if schema3Exists(t.stage) {
		if schema3Exists(source) {
			return fmt.Errorf("schema3: both target and staging exist")
		}
		if err = t.record("promoted", false, "", schema3.Digest(report.Target)); err != nil {
			return err
		}
		if err = t.hit("before/promote-rename"); err != nil {
			return err
		}
		if err = os.Rename(t.stage, source); err != nil {
			return err
		}
		if err = syncSchema3Dir(t.dir); err != nil {
			return err
		}
		if err = t.hit("after/promote-rename"); err != nil {
			return err
		}
	}
	if err = t.verifyTarget(ctx, source); err != nil {
		return err
	}
	if !t.done("promoted") {
		if err = t.record("promoted", true, "", schema3.Digest(report.Target)); err != nil {
			return err
		}
	}
	l, err := schema.Load(t.dir)
	if err != nil {
		return err
	}
	if l.SchemaVersion != 2 && l.SchemaVersion != 3 {
		return fmt.Errorf("schema3: ledger changed")
	}
	if l.InProgress != nil && l.InProgress.Migration != schema3.MigrationID {
		return fmt.Errorf("schema3: conflicting ledger transaction")
	}
	if err = t.record("ledger_committed", false, t.manifest.LedgerHash, schema3.Digest(report.Target)); err != nil {
		return err
	}
	if l.SchemaVersion == 2 {
		l.SchemaVersion = 3
		l.BinaryVersion = t.manifest.TargetVersion
		l.History = append(l.History, schema.HistoryEntry{From: 2, To: 3, Migration: schema3.MigrationID, At: t.manifest.CreatedAt, Backup: t.bundle})
	}
	l.InProgress = t.progress("ledger_committed")
	if err = t.hit("before/ledger-save"); err != nil {
		return err
	}
	if err = schema.Save(t.dir, l); err != nil {
		return err
	}
	if err = t.hit("after/ledger-save"); err != nil {
		return err
	}
	return t.record("ledger_committed", true, t.manifest.LedgerHash, schema3.Digest(report.Target))
}

// restore is offline and transaction-paired. It preserves the failed target for
// evidence and installs a complete verified old generation, never an overlay.
// Caller must stop even a partly started target and reacquire ownership first.
func (t *schema3Transaction) restore(ctx context.Context, binaryPath string) error {
	if t.lock == nil {
		return fmt.Errorf("schema3: updater owner lock required")
	}
	if err := t.verifyBundle(); err != nil {
		return err
	}
	if binaryPath != t.manifest.BinaryPath {
		return fmt.Errorf("schema3: restore binary path differs from paired identity")
	}
	restore := filepath.Join(t.dir, ".schema2-restore-"+t.id)
	abandoned := filepath.Join(t.dir, ".schema3-abandoned-"+t.id)
	source := filepath.Join(t.dir, "warden-db")
	if err := t.record("restore_staged", false, t.manifest.SourceTree, ""); err != nil {
		return err
	}
	if !schema3Exists(restore) && !t.done("restore_promoted") {
		if tree, e := schema3Tree(source); e == nil && tree == t.manifest.SourceTree && schema3Exists(abandoned) {
			// Interrupted after the restore rename, before recording completion.
		} else {
			if err := schema3.CopyTree(filepath.Join(t.bundle, "data", "warden-db"), restore); err != nil {
				return err
			}
			if err := schema3.SyncTree(restore); err != nil {
				return err
			}
		}
	}
	if schema3Exists(restore) {
		if tree, e := schema3Tree(restore); e != nil || tree != t.manifest.SourceTree {
			return fmt.Errorf("schema3: restore staging differs")
		}
	}
	if err := t.record("restore_staged", true, t.manifest.SourceTree, ""); err != nil {
		return err
	}
	if !t.done("restore_promoted") {
		if err := t.record("restore_promoted", false, t.manifest.SourceTree, ""); err != nil {
			return err
		}
		if schema3Exists(restore) {
			if schema3Exists(source) {
				if schema3Exists(abandoned) {
					return fmt.Errorf("schema3: abandoned generation already exists")
				}
				if err := t.hit("before/restore-park"); err != nil {
					return err
				}
				if err := os.Rename(source, abandoned); err != nil {
					return err
				}
				if err := syncSchema3Dir(t.dir); err != nil {
					return err
				}
				if err := t.hit("after/restore-park"); err != nil {
					return err
				}
			}
			if err := t.hit("before/restore-promote"); err != nil {
				return err
			}
			if err := os.Rename(restore, source); err != nil {
				return err
			}
			if err := syncSchema3Dir(t.dir); err != nil {
				return err
			}
			if err := t.hit("after/restore-promote"); err != nil {
				return err
			}
		}
		if tree, e := schema3Tree(source); e != nil || tree != t.manifest.SourceTree {
			return fmt.Errorf("schema3: restored database differs")
		}
		if err := t.record("restore_promoted", true, t.manifest.SourceTree, ""); err != nil {
			return err
		}
	}
	for _, p := range t.manifest.RestorePaths {
		if p == "warden-db" {
			continue
		}
		if p == "" || filepath.Base(p) != p || p == "backups" || strings.HasPrefix(p, ".") {
			return fmt.Errorf("schema3: unsafe restore path")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		tmp := filepath.Join(t.dir, ".schema3-restore-file-"+t.id+"-"+p)
		if schema3Exists(tmp) {
			expected, e := schema3HashFile(filepath.Join(t.bundle, "data", p))
			if e != nil {
				return e
			}
			got, e := schema3HashFile(tmp)
			if e != nil || got != expected {
				return fmt.Errorf("schema3: divergent restore file")
			}
		} else if err := schema3CopyFile(filepath.Join(t.bundle, "data", p), tmp); err != nil {
			return err
		}
		if err := t.hit("before/restore/" + p); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(t.dir, p)); err != nil {
			return err
		}
		if err := syncSchema3Dir(t.dir); err != nil {
			return err
		}
		if err := t.hit("after/restore/" + p); err != nil {
			return err
		}
	}
	if err := t.record("restore_binary", false, t.manifest.OldBinaryHash, ""); err != nil {
		return err
	}
	tmp := binaryPath + ".schema3-restore-" + t.id
	if schema3Exists(tmp) {
		got, e := schema3HashFile(tmp)
		if e != nil || got != t.manifest.OldBinaryHash {
			return fmt.Errorf("schema3: divergent paired binary staging")
		}
	} else if err := schema3CopyFile(filepath.Join(t.bundle, "bin", "warden"), tmp); err != nil {
		return err
	}
	if err := t.hit("before/restore-binary"); err != nil {
		return err
	}
	if err := os.Rename(tmp, binaryPath); err != nil {
		return err
	}
	if err := syncSchema3Dir(filepath.Dir(binaryPath)); err != nil {
		return err
	}
	if err := t.hit("after/restore-binary"); err != nil {
		return err
	}
	if h, e := schema3HashFile(binaryPath); e != nil || h != t.manifest.OldBinaryHash {
		return fmt.Errorf("schema3: restored binary mismatch")
	}
	if err := t.record("restore_binary", true, t.manifest.OldBinaryHash, ""); err != nil {
		return err
	}
	return t.record("restored", true, t.manifest.SourceTree, t.manifest.OldBinaryHash)
}

// finish is called only after cutover's authenticated readiness start reports
// the exact target version, format 3 and successful ownership verification.
func (t *schema3Transaction) finish(ctx context.Context, h Health, ownershipOK bool) error {
	if t.lock == nil || t.manifest == nil {
		return fmt.Errorf("schema3: updater authority required")
	}
	if h.Status != "ok" || stripV(h.Version) != stripV(t.manifest.TargetVersion) || h.SchemaVersion != 3 || !ownershipOK {
		return fmt.Errorf("schema3: exact version/format/ownership readiness required")
	}
	if err := t.verifyTarget(ctx, filepath.Join(t.dir, "warden-db")); err != nil {
		return err
	}
	if err := t.record("readiness_verified", false, "", schema3.Digest(t.manifest.Report.Target)); err != nil {
		return err
	}
	l, err := schema.Load(t.dir)
	if err != nil {
		return err
	}
	if l.SchemaVersion != 3 || l.InProgress == nil || l.InProgress.Snapshot != t.bundle {
		return fmt.Errorf("schema3: readiness ledger mismatch")
	}
	l.InProgress = nil
	if err = schema.Save(t.dir, l); err != nil {
		return err
	}
	return t.record("readiness_verified", true, "", schema3.Digest(t.manifest.Report.Target))
}

func (t *schema3Transaction) planPath() string {
	return filepath.Join(t.dir, ".schema3-manifest-"+t.id+".json")
}
func (t *schema3Transaction) plannedFiles(oldBin string) ([]schema3File, error) {
	files := []schema3File{{Path: "bin", Type: "directory", Mode: 0700}, {Path: "data", Type: "directory", Mode: 0700}}
	for _, p := range t.manifest.RestorePaths {
		src := filepath.Join(t.dir, p)
		fi, err := os.Lstat(src)
		if err != nil {
			return nil, err
		}
		entry := schema3File{Path: "data/" + p, Type: "file", Mode: uint32(fi.Mode().Perm()), Size: fi.Size()}
		if fi.IsDir() {
			entry.Type = "directory"
			entry.Size = 0
			files = append(files, entry)
			children, err := schema3Inventory(src)
			if err != nil {
				return nil, err
			}
			for _, f := range children {
				f.Path = "data/" + p + "/" + f.Path
				files = append(files, f)
			}
		} else {
			entry.SHA256, err = schema3HashFile(src)
			if err != nil {
				return nil, err
			}
			files = append(files, entry)
		}
	}
	fi, err := os.Stat(oldBin)
	if err != nil {
		return nil, err
	}
	files = append(files, schema3File{Path: "bin/warden", Type: "file", Mode: uint32(fi.Mode().Perm()), Size: fi.Size(), SHA256: t.manifest.OldBinaryHash})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
func (t *schema3Transaction) completeBackup(oldBin string) error {
	if t.lock == nil || t.manifest == nil {
		return fmt.Errorf("schema3: updater authority required")
	}
	if tree, err := schema3Tree(filepath.Join(t.dir, "warden-db")); err != nil || tree != t.manifest.SourceTree {
		return fmt.Errorf("schema3: backup source changed")
	}
	if hash, err := schema3HashFile(filepath.Join(t.dir, schema.FileName)); err != nil || hash != t.manifest.LedgerHash {
		return fmt.Errorf("schema3: backup source ledger changed")
	}
	if err := t.record("backed_up", false, t.manifest.SourceTree, t.manifestHash); err != nil {
		return err
	}
	if err := t.hit("before/backup-create"); err != nil {
		return err
	}
	if fi, err := os.Lstat(filepath.Dir(t.bundle)); err == nil && !fi.IsDir() {
		return fmt.Errorf("schema3: unsafe backup parent")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.bundle), 0700); err != nil {
		return err
	}
	if !schema3Exists(t.bundle) {
		if err := os.Mkdir(t.bundle, 0700); err != nil {
			return err
		}
	} else if err := schema3.SafeTree(t.bundle); err != nil {
		return err
	}
	expected := map[string]schema3File{}
	for _, f := range t.manifest.Files {
		expected[f.Path] = f
	}
	if actual, err := schema3Inventory(t.bundle); err != nil {
		return err
	} else {
		for _, f := range actual {
			if e, ok := expected[f.Path]; !ok || schema3.Digest(e) != schema3.Digest(f) {
				return fmt.Errorf("schema3: divergent partial backup %s", f.Path)
			}
		}
	}
	for _, f := range t.manifest.Files {
		dst := filepath.Join(t.bundle, filepath.FromSlash(f.Path))
		if schema3Exists(dst) {
			continue
		}
		if err := t.hit("before/backup/" + strings.TrimPrefix(f.Path, "data/")); err != nil {
			return err
		}
		if f.Type == "directory" {
			if err := os.Mkdir(dst, os.FileMode(f.Mode)); err != nil {
				return err
			}
		} else {
			src := oldBin
			if f.Path != "bin/warden" {
				src = filepath.Join(t.dir, filepath.FromSlash(strings.TrimPrefix(f.Path, "data/")))
			}
			if hash, err := schema3HashFile(src); err != nil || hash != f.SHA256 {
				return fmt.Errorf("schema3: backup source identity changed %s", src)
			}
			if f.Path == "bin/warden" {
				if err := t.hit("before/backup/binary"); err != nil {
					return err
				}
			}
			if err := schema3CopyFile(src, dst); err != nil {
				return err
			}
			if f.Path == "bin/warden" {
				if err := t.hit("after/backup/binary"); err != nil {
					return err
				}
			}
		}
		if err := t.hit("after/backup/" + strings.TrimPrefix(f.Path, "data/")); err != nil {
			return err
		}
	}
	if err := schema3.SyncTree(t.bundle); err != nil {
		return err
	}
	if err := t.hit("before/manifest"); err != nil {
		return err
	}
	if err := writeSchema3Once(filepath.Join(t.bundle, "MANIFEST.json"), t.manifest); err != nil {
		return err
	}
	if err := t.hit("after/manifest"); err != nil {
		return err
	}
	if err := syncSchema3Dir(filepath.Dir(t.bundle)); err != nil {
		return err
	}
	if err := t.verifyBundle(); err != nil {
		return err
	}
	return t.record("backed_up", true, t.manifest.SourceTree, t.manifestHash)
}

func (t *schema3Transaction) progress(step string) *schema.InProgress {
	return &schema.InProgress{Migration: schema3.MigrationID, Step: step, Snapshot: t.bundle, TransactionID: t.id, ManifestSHA256: t.manifestHash, Journal: t.journal, SourceFormat: 2, TargetFormat: 3, TargetBinarySHA256: t.manifest.TargetBinaryHash}
}
