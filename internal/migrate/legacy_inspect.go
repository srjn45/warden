package migrate

// Read-only inspection of retained schema-1 (legacy) stores.
//
// After the schema-2 cutover the legacy stores are kept on disk for the
// support window (see retirement.go) purely as evidence. This inspector lets an
// operator or developer look at them and compare them with the canonical
// warden-db. It is never an authority: it refuses a data dir whose ledger is
// not at schema 2 (there the legacy stores ARE the live authority), it opens
// legacy and canonical stores only through private copies, and it proves after
// the fact that the data dir is byte-for-byte untouched.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva/query"
	"github.com/srjn45/warden/internal/schema"
	"github.com/srjn45/warden/internal/wardendb"
)

// ErrLegacyLiveAuthority: the data dir has not cut over to schema 2, so the
// legacy stores are the live authority and must not be inspected as "legacy".
var ErrLegacyLiveAuthority = errors.New("migrate: legacy stores are the live authority on this data dir (not at schema 2); refusing legacy inspection")

// ErrLegacyInspectMutated: the data dir changed while it was being inspected.
// This is a bug in the inspector (or a concurrent writer), never expected.
var ErrLegacyInspectMutated = errors.New("migrate: data dir changed during read-only legacy inspection")

// legacyIDSample caps how many differing ids are listed per collection.
const legacyIDSample = 5

// LegacyCollection compares one warden-db collection with what the retained
// legacy stores map to.
type LegacyCollection struct {
	Name          string   `json:"name"`
	LegacyRecords int      `json:"legacy_records"`
	CanonRecords  int      `json:"canonical_records"`
	OnlyLegacy    int      `json:"only_legacy"`
	OnlyCanonical int      `json:"only_canonical"`
	OnlyLegacyIDs []string `json:"only_legacy_ids,omitempty"`
	OnlyCanonIDs  []string `json:"only_canonical_ids,omitempty"`
	// Match: identical key sets. Content may still have changed since the
	// cutover; canonical-only records are expected after runtime use.
	Match bool `json:"match"`
}

// LegacyBackupEvidence is the cutover backup check recorded in the ledger.
type LegacyBackupEvidence struct {
	Path     string `json:"path,omitempty"`
	Verified bool   `json:"verified"`
	Files    int    `json:"files,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// LegacyInspection is the machine-readable read-only report.
type LegacyInspection struct {
	DataDir  string `json:"data_dir"`
	Schema   int    `json:"schema_version"`
	ReadOnly bool   `json:"read_only"`
	// Authority is always "none": the inspector never serves live data.
	Authority   string               `json:"authority"`
	CutoverAt   *time.Time           `json:"cutover_at,omitempty"`
	Sources     []InventoryEntry     `json:"legacy_sources"`
	Collections []LegacyCollection   `json:"collections"`
	Backup      LegacyBackupEvidence `json:"backup"`
	Findings    []Finding            `json:"findings,omitempty"`
	// Unchanged proves the data dir fingerprint was identical before and after.
	Unchanged bool `json:"unchanged"`
}

// InspectLegacy reads the retained legacy stores of a schema-2 data dir and
// compares them with warden-db. It writes nothing to dataDir.
func InspectLegacy(ctx context.Context, dataDir string) (*LegacyInspection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	l, err := schema.Load(dataDir)
	if err != nil {
		return nil, fmt.Errorf("migrate: legacy inspection: schema ledger unreadable: %w", err)
	}
	if l.SchemaVersion < schema.Schema2 || l.InProgress != nil {
		return nil, ErrLegacyLiveAuthority
	}
	before, err := fingerprintTree(dataDir)
	if err != nil {
		return nil, err
	}
	rep := &LegacyInspection{DataDir: dataDir, Schema: l.SchemaVersion, ReadOnly: true, Authority: "none"}

	var entry *schema.HistoryEntry
	for i := range l.History {
		h := &l.History[i]
		if h.To >= schema.Schema2 && (h.Migration == Migration0002ID || h.Migration == schema.MigrationBaselineFresh) {
			entry = h
		}
	}
	if entry != nil {
		at := entry.At
		rep.CutoverAt = &at
	}

	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated {
			continue
		}
		e, err := inventoryEntry(dataDir, s)
		if err != nil {
			return nil, err
		}
		if e.Present {
			rep.Sources = append(rep.Sources, e)
		}
	}

	rep.Backup = inspectBackup(dataDir, entry)
	if entry != nil && entry.Migration != schema.MigrationBaselineFresh && !rep.Backup.Verified {
		rep.Findings = append(rep.Findings, Finding{Severity: SeverityBlocking, Store: "backup", Message: rep.Backup.Detail})
	}

	if len(rep.Sources) > 0 {
		if err := compareLegacyToCanonical(ctx, dataDir, rep); err != nil {
			return nil, err
		}
	}

	after, err := fingerprintTree(dataDir)
	if err != nil {
		return nil, err
	}
	rep.Unchanged = before == after
	if !rep.Unchanged {
		return rep, ErrLegacyInspectMutated
	}
	return rep, nil
}

func inspectBackup(dataDir string, entry *schema.HistoryEntry) LegacyBackupEvidence {
	switch {
	case entry == nil || entry.Backup == "":
		if entry != nil && entry.Migration == schema.MigrationBaselineFresh {
			return LegacyBackupEvidence{Verified: true, Detail: "fresh install: no legacy data ever existed"}
		}
		return LegacyBackupEvidence{Detail: "ledger records no cutover backup"}
	}
	dir := entry.Backup
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(dataDir, filepath.FromSlash(dir))
	}
	m, err := VerifySchema2Backup(dir)
	if err != nil {
		return LegacyBackupEvidence{Path: entry.Backup, Detail: err.Error()}
	}
	return LegacyBackupEvidence{Path: entry.Backup, Verified: true, Files: len(m.Backup.Files), Detail: "verified against its manifest"}
}

// compareLegacyToCanonical maps the retained legacy stores on a private copy
// and diffs their key sets against a private copy of warden-db.
func compareLegacyToCanonical(ctx context.Context, dataDir string, rep *LegacyInspection) error {
	tmp, err := os.MkdirTemp("", "warden-legacy-inspect-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyMigratedSources(ctx, dataDir, tmp); err != nil {
		return fmt.Errorf("copy legacy sources for inspection: %w", err)
	}
	set, err := buildStagingSet(ctx, tmp)
	if err != nil {
		return err
	}
	for _, f := range set.findings {
		f.Message = strings.ReplaceAll(f.Message, tmp, "<data-dir>")
		f.Details = strings.ReplaceAll(f.Details, tmp, "<data-dir>")
		rep.Findings = append(rep.Findings, f)
	}
	legacy := map[string]map[string]bool{}
	for _, row := range set.rows {
		if legacy[row.collection] == nil {
			legacy[row.collection] = map[string]bool{}
		}
		legacy[row.collection][row.key] = true
	}

	canon := map[string]map[string]bool{}
	src := wardendb.Dir(dataDir)
	if _, err := os.Stat(src); err == nil {
		cp := filepath.Join(tmp, "canonical-warden-db")
		if err := copyTree(src, cp); err != nil {
			return fmt.Errorf("copy warden-db for inspection: %w", err)
		}
		db, err := wardendb.OpenOfflineLegacyDir(cp)
		if err != nil {
			return fmt.Errorf("open warden-db copy: %w", err)
		}
		defer db.Close()
		for _, name := range wardendb.Catalogue() {
			if _, err := os.Stat(filepath.Join(cp, name)); err != nil {
				continue
			}
			col, err := db.Collection(name)
			if err != nil {
				return fmt.Errorf("warden-db collection %s: %w", name, err)
			}
			rows, err := col.Scan(query.MatchAll)
			if err != nil {
				return err
			}
			keys := map[string]bool{}
			for _, r := range rows {
				if k := str(r.Data["_key"]); k != "" {
					keys[k] = true
				}
			}
			canon[name] = keys
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	names := map[string]bool{}
	for n := range legacy {
		names[n] = true
	}
	for n := range canon {
		names[n] = true
	}
	for _, name := range sortedKeys(names) {
		c := LegacyCollection{Name: name, LegacyRecords: len(legacy[name]), CanonRecords: len(canon[name])}
		for k := range legacy[name] {
			if !canon[name][k] {
				c.OnlyLegacy++
				c.OnlyLegacyIDs = append(c.OnlyLegacyIDs, k)
			}
		}
		for k := range canon[name] {
			if !legacy[name][k] {
				c.OnlyCanonical++
				c.OnlyCanonIDs = append(c.OnlyCanonIDs, k)
			}
		}
		sort.Strings(c.OnlyLegacyIDs)
		sort.Strings(c.OnlyCanonIDs)
		if len(c.OnlyLegacyIDs) > legacyIDSample {
			c.OnlyLegacyIDs = c.OnlyLegacyIDs[:legacyIDSample]
		}
		if len(c.OnlyCanonIDs) > legacyIDSample {
			c.OnlyCanonIDs = c.OnlyCanonIDs[:legacyIDSample]
		}
		c.Match = c.OnlyLegacy == 0 && c.OnlyCanonical == 0
		rep.Collections = append(rep.Collections, c)
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		return copyPlain(p, filepath.Join(dst, rel))
	})
}

// fingerprintTree summarises every entry's path, type, size and mtime. Any
// create, delete, rewrite or touch of the data dir changes it.
func fingerprintTree(root string) (string, error) {
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(&b, "%s|%v|%d|%d\n", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		return nil
	})
	return b.String(), err
}

// Summary renders the report for a terminal.
func (r *LegacyInspection) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "legacy inspection for %s (schema %d) — READ-ONLY, authority: none\n", r.DataDir, r.Schema)
	if r.CutoverAt != nil {
		fmt.Fprintf(&b, "cutover: %s\n", r.CutoverAt.Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "backup: verified=%v %s\n", r.Backup.Verified, r.Backup.Detail)
	if len(r.Sources) == 0 {
		b.WriteString("no legacy stores on disk\n")
	}
	for _, s := range r.Sources {
		fmt.Fprintf(&b, "  legacy %-24s %5d files %10d bytes -> %s\n", s.Path, s.Files, s.Bytes, s.Destination)
	}
	for _, c := range r.Collections {
		mark := "match"
		if !c.Match {
			mark = "DIFF "
		}
		fmt.Fprintf(&b, "  [%s] %-24s legacy=%d canonical=%d only-legacy=%d only-canonical=%d\n", mark, c.Name, c.LegacyRecords, c.CanonRecords, c.OnlyLegacy, c.OnlyCanonical)
	}
	for _, f := range r.Findings {
		fmt.Fprintf(&b, "  [%s] %s: %s\n", f.Severity, f.Store, f.Message)
	}
	b.WriteString("canonical-only records are expected after runtime use; nothing was changed.\n")
	return b.String()
}
