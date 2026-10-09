package backendstore

// Open/recovery layer for the backend registry (issue #841; contract in
// docs/specs/2026-10-09-backend-registry-integrity-contract.md). NewStore stays
// strictly fail-closed; this file adds the narrow, backup-first, offline path
// that recovers a registry whose only problem is provably stale revision
// history, and refuses (typed error, nothing mutated) everything else.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva"
	"github.com/srjn45/scriva/engine"
)

// RepairCommand is the operator-facing offline repair command (built by t5).
const RepairCommand = "warden repair backends"

// RepairDryRunCommand is the read-only counterpart of RepairCommand.
const RepairDryRunCommand = "warden repair backends --dry-run"

// registryCollections is every ScrivaDB collection opened by NewStore; the only
// ones the recovery layer will touch.
var registryCollections = []string{"backends", "models", "role_tiers", "handover_settings", "quotas", "rl_cooldowns"}

// ResolutionRule names the only rule recovery applies to revision regressions.
const ResolutionRule = "newest-revision-with-concordant-timestamp"

var (
	// ErrRecoveryRequired matches (errors.Is) every *RecoveryRequiredError.
	ErrRecoveryRequired = errors.New("backend registry requires operator recovery")
	// ErrOwned is returned when the registry directory is held by a live process;
	// recovery is offline-only.
	ErrOwned = errors.New("backend registry is open in another process; stop the daemon first")
)

// Verdict classifies one collection.
type Verdict string

const (
	VerdictClean       Verdict = "clean"
	VerdictRecoverable Verdict = "recoverable" // safe to repair automatically (backup first)
	VerdictAmbiguous   Verdict = "ambiguous"   // needs an operator; never auto-resolved
)

// DiscardedRevision is one stale history line recovery removes from a segment.
// Every one is recorded in the report and survives in the verified backup.
type DiscardedRevision struct {
	Collection  string `json:"collection"`
	Segment     string `json:"segment"`
	Line        int    `json:"line"` // 1-based
	ID          uint64 `json:"id"`
	Key         string `json:"key,omitempty"`
	Rev         uint64 `json:"rev"`
	TS          string `json:"ts"`
	WinnerRev   uint64 `json:"winner_rev"`
	WinnerTS    string `json:"winner_ts"`
	DiscardedAs string `json:"rule"`
}

// CollectionVerdict is the classification of one collection.
type CollectionVerdict struct {
	Name       string              `json:"name"`
	Verdict    Verdict             `json:"verdict"`
	Severities []string            `json:"severities,omitempty"`
	Codes      []string            `json:"codes,omitempty"`
	Reasons    []string            `json:"reasons,omitempty"`
	Stale      []DiscardedRevision `json:"stale,omitempty"`
}

// Report is the read-only result of Verify.
type Report struct {
	Dir         string                  `json:"dir"`
	Time        time.Time               `json:"time"`
	Integrity   *engine.IntegrityReport `json:"integrity,omitempty"`
	Collections []CollectionVerdict     `json:"collections"`
}

// Clean reports that no collection needs any action.
func (r *Report) Clean() bool {
	for _, c := range r.Collections {
		if c.Verdict != VerdictClean {
			return false
		}
	}
	return true
}

// Recoverable reports that action is needed and all of it is safe.
func (r *Report) Recoverable() bool {
	need := false
	for _, c := range r.Collections {
		switch c.Verdict {
		case VerdictAmbiguous:
			return false
		case VerdictRecoverable:
			need = true
		}
	}
	return need
}

func (r *Report) ambiguous() []CollectionVerdict {
	var out []CollectionVerdict
	for _, c := range r.Collections {
		if c.Verdict == VerdictAmbiguous {
			out = append(out, c)
		}
	}
	return out
}

// RecoveryRequiredError is returned when the registry cannot be opened or
// recovered automatically. Nothing on disk was changed by the attempt that
// produced it (a failed repair is rolled back from the backup first).
type RecoveryRequiredError struct {
	Collections   []CollectionVerdict // the ambiguous (or failed) collections
	Severities    []string
	ReportPath    string // JSON report, "" if it could not be written
	BackupPath    string // verified backup, "" if none was taken
	RepairCommand string
	Cause         error
}

func (e *RecoveryRequiredError) Error() string {
	var parts []string
	for _, c := range e.Collections {
		p := c.Name
		if len(c.Codes) > 0 {
			p += " [" + strings.Join(c.Codes, ",") + "]"
		}
		if len(c.Reasons) > 0 {
			p += ": " + c.Reasons[0]
		}
		parts = append(parts, p)
	}
	msg := fmt.Sprintf("backend registry needs operator recovery (severity %s): %s", strings.Join(e.Severities, ","), strings.Join(parts, "; "))
	if e.ReportPath != "" {
		msg += "; report: " + e.ReportPath
	}
	if e.BackupPath != "" {
		msg += "; backup: " + e.BackupPath
	}
	if e.Cause != nil {
		msg += "; cause: " + e.Cause.Error()
	}
	return msg + "; stop the daemon and run `" + e.RepairCommand + "`"
}

func (e *RecoveryRequiredError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrRecoveryRequired, e.Cause}
	}
	return []error{ErrRecoveryRequired}
}

// Options configures Open and Repair.
type Options struct {
	// BackupDir is the parent for backups and reports. It must be outside the
	// registry directory. Default: <registry dir>/../backend-registry-backups.
	BackupDir string
	// Logger receives one audit record per recovery action (default slog.Default()).
	Logger *slog.Logger
	// Now is the clock (default time.Now).
	Now func() time.Time
}

func (o Options) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now().UTC()
	}
	return time.Now().UTC()
}

func (o Options) backupParent(dir string) string {
	if o.BackupDir != "" {
		return o.BackupDir
	}
	return filepath.Join(filepath.Dir(filepath.Clean(dir)), "backend-registry-backups")
}

// Result is the outcome of Repair / a recovering Open.
type Result struct {
	Recovered  bool                // false: registry was already clean (no-op)
	BackupPath string              // verified backup ("" when nothing was done)
	ReportPath string              // JSON audit report ("" when nothing was done)
	Discarded  []DiscardedRevision // stale revisions removed
	Before     *Report
}

// Open opens the registry strictly. If strict open fails on an integrity
// finding it runs Repair (backup-first) and reopens strictly. Ambiguous
// findings return a *RecoveryRequiredError and leave the store untouched.
func Open(dir string, opts Options) (*Store, *Result, error) {
	s, err := NewStore(dir)
	if err == nil {
		return s, nil, nil
	}
	if !errors.Is(err, engine.ErrIntegrity) {
		return nil, nil, err
	}
	log := opts.logger()
	log.Warn("backend registry failed strict open; attempting backup-first recovery", "dir", dir, "error", err)
	res, rerr := Repair(context.Background(), dir, opts)
	if rerr != nil {
		var rre *RecoveryRequiredError
		if errors.As(rerr, &rre) && rre.Cause == nil {
			rre.Cause = err
		}
		return nil, nil, rerr
	}
	s, err = NewStore(dir)
	if err != nil {
		return nil, res, &RecoveryRequiredError{
			RepairCommand: RepairCommand, BackupPath: res.BackupPath, ReportPath: res.ReportPath,
			Severities: []string{string(engine.SeverityConflict)}, Cause: fmt.Errorf("strict reopen after recovery failed: %w", err),
		}
	}
	return s, res, nil
}

// Verify is the read-only offline check: a full scriva verification plus the
// recoverable/ambiguous classification. It never writes to dir. The directory
// need not be unowned (the lock is reported as an Info finding).
func Verify(ctx context.Context, dir string) (*Report, error) {
	rep := &Report{Dir: dir, Time: time.Now().UTC()}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	ir, err := engine.VerifyDir(ctx, dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		return nil, err
	}
	rep.Integrity = ir
	inScope := map[string]bool{}
	for _, c := range registryCollections {
		inScope[c] = true
	}
	for _, cr := range ir.Collections {
		v := CollectionVerdict{Name: cr.Name, Verdict: VerdictClean}
		sevs, codes := map[string]bool{}, map[string]bool{}
		conflictsOnlyRevision, anyConflict, corrupt := true, false, false
		for _, f := range cr.Findings {
			if f.Severity == engine.SeverityInfo {
				continue
			}
			sevs[string(f.Severity)], codes[string(f.Code)] = true, true
			switch f.Severity {
			case engine.SeverityConflict:
				anyConflict = true
				if f.Code != engine.CodeConflictRevision {
					conflictsOnlyRevision = false
				}
			case engine.SeverityDataCorruption:
				corrupt = true
			}
		}
		if len(sevs) == 0 {
			rep.Collections = append(rep.Collections, v)
			continue
		}
		v.Severities, v.Codes = sortedKeys(sevs), sortedKeys(codes)
		v.Verdict = VerdictRecoverable // derived-index damage: scriva rebuild is lossless
		switch {
		case !inScope[cr.Name]:
			v.Verdict, v.Reasons = VerdictAmbiguous, []string{"collection is outside the backend registry; not handled here"}
		case corrupt:
			v.Verdict, v.Reasons = VerdictAmbiguous, []string{"segment bytes are damaged (data corruption); salvage is never automatic"}
		case anyConflict && !conflictsOnlyRevision:
			v.Verdict, v.Reasons = VerdictAmbiguous, []string{"history conflict other than revision regression (duplicate/reused/deleted id)"}
		case anyConflict:
			stale, reasons := classifyRegressions(dir, cr.Name)
			v.Stale = stale
			if len(reasons) > 0 {
				v.Verdict, v.Reasons, v.Stale = VerdictAmbiguous, reasons, nil
			}
		}
		rep.Collections = append(rep.Collections, v)
	}
	return rep, nil
}

// Repair is the explicit, backup-first, offline repair. Sequence: verify and
// classify; refuse (typed error, nothing mutated) when any collection is
// ambiguous; take a verified backup; remove provably stale revision lines;
// run scriva's rebuild; verify clean; on any failure restore the backup
// byte-identically. A clean registry is a no-op.
func Repair(ctx context.Context, dir string, opts Options) (*Result, error) {
	log := opts.logger()
	if err := checkAuthority(dir); err != nil {
		return nil, err
	}
	lock, err := acquireDirLock(dir)
	if err != nil {
		return nil, err
	}
	rep, err := Verify(ctx, dir)
	if err != nil {
		_ = lock.release()
		return nil, err
	}
	res := &Result{Before: rep}
	if rep.Clean() {
		_ = lock.release()
		log.Info("backend registry recovery: nothing to do", "dir", dir)
		return res, nil
	}
	parent := opts.backupParent(dir)
	stamp := opts.now().Format("20060102T150405Z")
	reportPath := filepath.Join(parent, "backend-registry-report-"+stamp+".json")
	writeReport := func(extra map[string]any) string {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return ""
		}
		doc := map[string]any{"report": rep, "rule": ResolutionRule}
		maps.Copy(doc, extra)
		b, _ := json.MarshalIndent(doc, "", "  ")
		if os.WriteFile(reportPath, b, 0o600) != nil {
			return ""
		}
		return reportPath
	}
	for _, c := range rep.Collections {
		if c.Verdict != VerdictClean {
			log.Warn("backend registry finding", "collection", c.Name, "verdict", c.Verdict, "severities", c.Severities, "codes", c.Codes, "reasons", c.Reasons, "stale_revisions", len(c.Stale))
		}
	}
	if !rep.Recoverable() {
		_ = lock.release()
		amb := rep.ambiguous()
		path := writeReport(nil)
		log.Error("backend registry recovery refused: ambiguous history; nothing was changed", "dir", dir, "report", path)
		return nil, &RecoveryRequiredError{Collections: amb, Severities: severitiesOf(amb), ReportPath: path, RepairCommand: RepairCommand}
	}

	backup, err := makeBackup(ctx, dir, parent, stamp)
	if err != nil {
		_ = lock.release()
		return nil, fmt.Errorf("backend registry recovery aborted before any change: %w", err)
	}
	log.Info("backend registry backup verified", "backup", backup)
	res.BackupPath = backup

	fail := func(cause error) (*Result, error) {
		rerr := restoreBackup(dir, backup)
		log.Error("backend registry recovery failed; restored from backup", "backup", backup, "cause", cause, "restore_error", rerr)
		path := writeReport(map[string]any{"failure": cause.Error()})
		_ = lock.release()
		return nil, &RecoveryRequiredError{Collections: nonClean(rep), Severities: severitiesOf(nonClean(rep)),
			ReportPath: path, BackupPath: backup, RepairCommand: RepairCommand, Cause: cause}
	}

	var touched []string
	for _, c := range rep.Collections {
		if c.Verdict == VerdictClean {
			continue
		}
		touched = append(touched, c.Name)
		if len(c.Stale) > 0 {
			if err := dropStale(dir, c.Name, c.Stale); err != nil {
				return fail(err)
			}
			res.Discarded = append(res.Discarded, c.Stale...)
			log.Info("backend registry stale revisions removed", "collection", c.Name, "count", len(c.Stale), "rule", ResolutionRule)
		}
	}
	_ = lock.release() // scriva's Repair takes/probes the engine lock itself
	if _, err := engine.Repair(ctx, dir, engine.RepairOptions{
		Collections: touched, BackupDir: filepath.Join(parent, "engine-"+stamp), OnConflict: engine.ConflictAbort, Now: opts.now(),
	}); err != nil {
		lock, _ = acquireDirLock(dir)
		return fail(fmt.Errorf("scriva rebuild: %w", err))
	}
	lock, _ = acquireDirLock(dir)
	_ = lock.release() // scriva's own open below takes the engine lock
	if err := rebuildKeyIndexes(dir, touched); err != nil {
		lock, _ = acquireDirLock(dir)
		return fail(fmt.Errorf("rebuild _key index: %w", err))
	}
	lock, _ = acquireDirLock(dir)
	after, err := engine.VerifyDir(ctx, dir, engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		return fail(err)
	}
	if !after.Clean() {
		return fail(fmt.Errorf("post-repair verification not clean: max severity %s (%v)", after.MaxSeverity(), after.Codes()))
	}
	res.Recovered = true
	res.ReportPath = writeReport(map[string]any{"result": "recovered", "backup": backup, "discarded": res.Discarded})
	_ = lock.release()
	log.Info("backend registry recovered", "dir", dir, "backup", backup, "report", res.ReportPath, "discarded_revisions", len(res.Discarded))
	return res, nil
}

// rebuildKeyIndexes recreates the unique _key index of each collection from the
// (now clean) segments. Scriva's Repair rebuilds persisted indexes but does not
// invent the lazily-created _key index: without it keyed reads miss every row
// and the next open would re-seed over the user's data.
func rebuildKeyIndexes(dir string, cols []string) error {
	db, err := scriva.Open(dir, scriva.WithSyncMode(engine.SyncModeNone))
	if err != nil {
		return err
	}
	for _, name := range cols {
		c, err := db.Collection(name)
		if err == nil {
			err = c.EnsureUniqueIndex(engine.KeyField)
		}
		if err != nil {
			_ = db.Close()
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return db.Close()
}

func nonClean(r *Report) []CollectionVerdict {
	var out []CollectionVerdict
	for _, c := range r.Collections {
		if c.Verdict != VerdictClean {
			out = append(out, c)
		}
	}
	return out
}

func severitiesOf(cs []CollectionVerdict) []string {
	set := map[string]bool{}
	for _, c := range cs {
		for _, s := range c.Severities {
			set[s] = true
		}
	}
	return sortedKeys(set)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- classification --------------------------------------------------------

type histEntry struct {
	ID   uint64          `json:"id"`
	Op   string          `json:"op"`
	TS   string          `json:"ts"`
	Rev  uint64          `json:"rev"`
	Data json.RawMessage `json:"data"`
}

type idState struct {
	maxRev  uint64
	maxTS   time.Time
	maxTSs  string
	maxData []byte
	hasDel  bool
	regress bool
	tsInv   bool
	stale   []DiscardedRevision
}

// classifyRegressions applies ResolutionRule to a collection. A regression line
// is provably stale iff it is an update with a lower revision than the id's
// newest AND an older timestamp than that newest line (append order, revision
// and wall clock all agree which write is last). Anything else -- equal-revision
// different content, a lower revision with a NEWER timestamp (a second writer's
// later, divergent write), a revision that is higher but older, deletes in an
// affected id, unparseable lines -- is ambiguous.
func classifyRegressions(dir, col string) ([]DiscardedRevision, []string) {
	cdir := filepath.Join(dir, col)
	if _, err := os.Stat(filepath.Join(cdir, "compact.manifest")); err == nil {
		return nil, []string{"pending compaction manifest; resolve it with scriva before recovery"}
	}
	segs, err := filepath.Glob(filepath.Join(cdir, "seg_*.ndjson"))
	if err != nil || len(segs) == 0 {
		return nil, []string{"no readable segments to classify"}
	}
	sort.Strings(segs)
	states := map[uint64]*idState{}
	var reasons []string
	for _, seg := range segs {
		raw, err := os.ReadFile(seg)
		if err != nil {
			return nil, []string{"segment unreadable: " + err.Error()}
		}
		for i, l := range bytes.Split(raw, []byte("\n")) {
			if len(bytes.TrimSpace(l)) == 0 {
				continue
			}
			var e histEntry
			if err := json.Unmarshal(l, &e); err != nil {
				return nil, []string{fmt.Sprintf("%s line %d does not parse: %v", filepath.Base(seg), i+1, err)}
			}
			ts, terr := time.Parse(time.RFC3339Nano, e.TS)
			st := states[e.ID]
			if st == nil {
				st = &idState{}
				states[e.ID] = st
			}
			switch e.Op {
			case "delete":
				st.hasDel = true
				continue
			case "insert", "update":
			default:
				return nil, []string{fmt.Sprintf("%s line %d has unknown op %q", filepath.Base(seg), i+1, e.Op)}
			}
			if terr != nil {
				return nil, []string{fmt.Sprintf("%s line %d has unparseable timestamp", filepath.Base(seg), i+1)}
			}
			switch {
			case st.maxRev == 0 || e.Rev > st.maxRev:
				if st.maxRev != 0 && ts.Before(st.maxTS) {
					st.tsInv = true
				}
				st.maxRev, st.maxTS, st.maxTSs, st.maxData = e.Rev, ts, e.TS, e.Data
			case e.Op == "update" && e.Rev == st.maxRev && bytes.Equal(e.Data, st.maxData):
				// identical repeat: scriva reports Info; keep.
			case e.Op == "update" && e.Rev < st.maxRev && ts.Before(st.maxTS):
				st.regress = true
				var k struct {
					Key string `json:"_key"`
				}
				_ = json.Unmarshal(e.Data, &k)
				st.stale = append(st.stale, DiscardedRevision{Collection: col, Segment: filepath.Base(seg), Line: i + 1, ID: e.ID,
					Key: k.Key, Rev: e.Rev, TS: e.TS, WinnerRev: st.maxRev, WinnerTS: st.maxTSs, DiscardedAs: ResolutionRule})
			default:
				reasons = append(reasons, fmt.Sprintf("id %d: %s line %d (rev %d, ts %s) is not provably older than rev %d (ts %s)",
					e.ID, filepath.Base(seg), i+1, e.Rev, e.TS, st.maxRev, st.maxTSs))
			}
		}
	}
	var stale []DiscardedRevision
	ids := make([]uint64, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		st := states[id]
		if st.regress && st.hasDel {
			reasons = append(reasons, fmt.Sprintf("id %d: revision regression on an id that was deleted", id))
		}
		if st.regress && st.tsInv {
			reasons = append(reasons, fmt.Sprintf("id %d: a higher revision carries an older timestamp; revision and wall clock disagree", id))
		}
		stale = append(stale, st.stale...)
	}
	if len(reasons) == 0 && len(stale) == 0 {
		reasons = append(reasons, "scriva reports a revision conflict that no stale line explains (e.g. two different updates at one revision)")
	}
	return stale, reasons
}

// dropStale rewrites the affected segments without the stale lines (atomic
// temp+rename) and removes the collection's derived index files so scriva's
// rebuild regenerates them.
func dropStale(dir, col string, stale []DiscardedRevision) error {
	cdir := filepath.Join(dir, col)
	drop := map[string]map[int]bool{}
	for _, s := range stale {
		if drop[s.Segment] == nil {
			drop[s.Segment] = map[int]bool{}
		}
		drop[s.Segment][s.Line] = true
	}
	for seg, lines := range drop {
		p := filepath.Join(cdir, seg)
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var out bytes.Buffer
		for i, l := range bytes.SplitAfter(raw, []byte("\n")) {
			if lines[i+1] {
				continue
			}
			out.Write(l)
		}
		tmp := p + ".recovery.tmp"
		if err := os.WriteFile(tmp, out.Bytes(), 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			return err
		}
	}
	derived, _ := filepath.Glob(filepath.Join(cdir, "sidx_*.json"))
	derived = append(derived, filepath.Join(cdir, "index.json"))
	for _, p := range derived {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ---- backup / restore ------------------------------------------------------

// makeBackup copies every collection directory of dir into
// <parent>/backends-backup-<stamp>/ and verifies each file by size and SHA-256.
// The LOCK file is not copied. Partial backups are removed.
func makeBackup(ctx context.Context, dir, parent, stamp string) (string, error) {
	if err := checkOutside(dir, parent); err != nil {
		return "", err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	var dst string
	for i := 0; ; i++ {
		name := "backends-backup-" + stamp
		if i > 0 {
			name = fmt.Sprintf("%s-%d", name, i)
		}
		dst = filepath.Join(parent, name)
		if err := os.Mkdir(dst, 0o700); err == nil {
			break
		} else if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	if err := copyDirVerified(ctx, dir, dst); err != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("backup failed verification: %w", err)
	}
	return dst, nil
}

func checkOutside(dir, parent string) error {
	a, err1 := filepath.Abs(dir)
	b, err2 := filepath.Abs(parent)
	if err1 != nil || err2 != nil {
		return errors.Join(err1, err2)
	}
	if b == a || strings.HasPrefix(b+string(os.PathSeparator), a+string(os.PathSeparator)) {
		return fmt.Errorf("backup directory %s must be outside the registry directory %s", parent, dir)
	}
	return nil
}

func copyDirVerified(ctx context.Context, src, dst string) error {
	return filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		if rel == "LOCK" {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		return copyFileVerified(p, target)
	})
}

func copyFileVerified(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
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
		return err
	}
	sum, size, err := hashFile(dst)
	if err != nil {
		return err
	}
	if size != n || !bytes.Equal(sum, h.Sum(nil)) {
		return fmt.Errorf("%s: backup copy does not match source", src)
	}
	return nil
}

func hashFile(p string) ([]byte, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return h.Sum(nil), n, err
}

// restoreBackup returns every collection directory in backup to dir,
// replacing whatever is there (the LOCK file is left alone).
func restoreBackup(dir, backup string) error {
	ents, err := os.ReadDir(backup)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		target := filepath.Join(dir, e.Name())
		if err := os.RemoveAll(target); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.MkdirAll(target, 0o700); err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, copyDirVerified(context.Background(), filepath.Join(backup, e.Name()), target))
	}
	return errors.Join(errs...)
}
