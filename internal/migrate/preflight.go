package migrate

// Schema-2 strict preflight: the full offline verification that runs before the
// cutover creates a backup or touches anything. It extends the read-only
// inventory (PlanSchema2) with:
//
//   - a strict verification of every discovered legacy store. Unlike
//     `warden doctor`, nothing is "auto-recoverable" here: a derived index that
//     would be rebuilt at open, a duplicate key, a torn tail or an unreadable
//     file all refuse the cutover, because migrating would otherwise have to
//     salvage, merge or guess;
//   - layout checks (a source of the wrong shape, a Scriva store outside the
//     inventory), duplicate-identity checks across flat files, and unsafe
//     external artifact checks (symlinks, special files, transcript pointers
//     leaving the data dir).
//
// Preflight is read-only: it never repairs, rebuilds, quarantines or deletes.
// Every blocking finding carries a stable Class so scripts and the operator
// can tell *why* the cutover was refused.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/ownerlock"
	"github.com/srjn45/warden/internal/wardendb"
)

// FindingClass is the stable machine-readable reason for a preflight finding.
type FindingClass string

const (
	ClassCorruption         FindingClass = "corruption"          // unreadable / torn / bad data
	ClassAmbiguity          FindingClass = "ambiguity"           // conflicting history, cannot pick a truth
	ClassDuplicateIdentity  FindingClass = "duplicate_identity"  // same identity twice
	ClassUnsupportedLayout  FindingClass = "unsupported_layout"  // wrong shape / outside the inventory
	ClassUnsafeArtifact     FindingClass = "unsafe_artifact"     // symlink, special file, escaping pointer
	ClassStaleDerivedState  FindingClass = "stale_derived_state" // index/derived state needs repair first
	ClassUnverifiable       FindingClass = "unverifiable"        // verification itself failed
	ClassOwned              FindingClass = "owned"               // a process holds the data
	ClassMissingArtifactRef FindingClass = "missing_artifact"    // note: pointer target absent
)

// classifyEngineFinding2 maps a Scriva verify finding to a strict preflight class.
func classifyEngineFinding2(f engine.Finding) FindingClass {
	switch f.Code {
	case engine.CodeConflictDuplicateID, engine.CodeDuplicateIdentical,
		engine.CodeConflictIDReuse, engine.CodeSidxUniqueViolation:
		return ClassDuplicateIdentity
	case engine.CodeLockHeld, engine.CodeLockHeldByThisProc:
		return ClassOwned
	}
	switch f.Severity {
	case engine.SeverityDataCorruption:
		return ClassCorruption
	case engine.SeverityConflict:
		return ClassAmbiguity
	case engine.SeverityRepairableIndex:
		return ClassStaleDerivedState
	}
	return ClassAmbiguity
}

// strictRemediation is the operator-facing next step per class.
func strictRemediation(c FindingClass) string {
	switch c {
	case ClassCorruption:
		return "restore the store from a backup or run `warden repair` deliberately; the cutover never salvages corrupt data"
	case ClassAmbiguity:
		return "resolve the conflicting history deliberately (`warden repair all --resolve-history=<policy>`); the cutover never picks a winner"
	case ClassDuplicateIdentity:
		return "remove or reconcile the duplicate identity by hand; the cutover never merges or dedupes"
	case ClassStaleDerivedState:
		return "run `warden repair all` once so derived indexes are rebuilt before the cutover; preflight never rebuilds them"
	case ClassUnsupportedLayout:
		return "move the path aside by hand or restore the expected layout; the cutover supports only the documented legacy layout"
	case ClassUnsafeArtifact:
		return "replace the symlink/special file with a real file inside the data dir, or move it out; the cutover never follows it"
	case ClassOwned:
		return "stop the daemon (`warden daemon stop`) and any CLI using the data dir, then retry"
	case ClassUnverifiable:
		return "fix the I/O or permission problem named above, then retry"
	}
	return ""
}

// PreflightSchema2 is PlanSchema2 plus strict verification of every discovered
// legacy store. It is read-only and holds no lock; BeginSchema2 re-runs it
// under the ownership lock. The returned plan is Blocked when anything refuses.
func PreflightSchema2(ctx context.Context, dataDir string, now time.Time) (*Schema2Plan, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := PlanSchema2(dataDir, now)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dataDir); err != nil || !fi.IsDir() {
		return p, nil // PlanSchema2 already reported it
	}
	pf := &preflight{ctx: ctx, dataDir: dataDir, plan: p}
	pf.verifyStores()
	pf.checkLayouts()
	pf.checkExternals()
	pf.checkFlatFiles()
	pf.checkOwnerAgain()

	for _, f := range p.Findings {
		if f.Severity == SeverityBlocking {
			p.Blocked = true
		}
	}
	return p, nil
}

type preflight struct {
	ctx     context.Context
	dataDir string
	plan    *Schema2Plan
}

func (pf *preflight) add(sev Severity, class FindingClass, store, msg, details string) {
	if details == "" && sev == SeverityBlocking {
		details = strictRemediation(class)
	}
	pf.plan.Findings = append(pf.plan.Findings, Finding{
		Severity: sev, Class: string(class), Store: store, Message: msg, Details: details,
	})
}

func (pf *preflight) block(class FindingClass, store, msg, details string) {
	pf.add(SeverityBlocking, class, store, msg, details)
}

// migratedSource reports whether rel is (inside) a migrated source.
func migratedSource(rel string) bool {
	for _, s := range schema2Sources {
		if s.Disposition == DispositionMigrated && (rel == s.Path || strings.HasPrefix(rel, s.Path+"/")) {
			return true
		}
	}
	return false
}

func hasKnownStoreName(rel string) bool {
	for _, s := range schema2Sources {
		if rel == s.Path {
			return true
		}
	}
	return false
}

// verifyStores strictly verifies every discovered Scriva store. Operator copies
// and maintenance dirs are skipped (DiscoverStores skips backups/quarantine/
// tmp; externalTopLevel names are filtered here).
func (pf *preflight) verifyStores() {
	stores, err := DiscoverStores(pf.dataDir)
	if err != nil {
		pf.block(ClassUnverifiable, "", "cannot discover legacy stores", err.Error())
		return
	}
	for _, rel := range stores {
		top := strings.SplitN(rel, "/", 2)[0]
		if top == wardendb.DirName {
			continue // existing warden-db is already a blocking plan finding
		}
		if _, ext := externalTopLevel(top); ext {
			continue
		}
		if !migratedSource(rel) {
			// A store the inventory does not know would be silently left behind.
			pf.block(ClassUnsupportedLayout, rel, rel+": a ScrivaDB store outside the migration inventory",
				"its data would not be migrated and would be orphaned; "+strictRemediation(ClassUnsupportedLayout))
			continue
		}
		pf.verifyStore(rel)
	}
}

func (pf *preflight) verifyStore(rel string) {
	full := filepath.Join(pf.dataDir, filepath.FromSlash(rel))
	rep, err := engine.VerifyDir(pf.ctx, full, engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		pf.block(ClassUnverifiable, rel, rel+": verification failed: "+err.Error(), "")
		return
	}
	emit := func(col string, f engine.Finding) {
		if f.Severity == engine.SeverityInfo {
			return
		}
		target := rel
		if col != "" {
			target = rel + "/" + col
		}
		class := classifyEngineFinding2(f)
		loc := ""
		switch {
		case f.Location.ID > 0:
			loc = fmt.Sprintf(" (id %d)", f.Location.ID)
		case f.Location.Segment != "":
			loc = " (" + f.Location.Segment + ")"
		}
		pf.block(class, target, fmt.Sprintf("%s: %s%s", target, f.Message, loc),
			fmt.Sprintf("code: %s\n%s", f.Code, strictRemediation(class)))
	}
	for _, f := range rep.Findings {
		emit("", f)
	}
	for _, c := range rep.Collections {
		for _, f := range c.Findings {
			emit(c.Name, f)
		}
	}
}

// checkLayouts verifies every migrated source has the shape the importer
// expects: schedules.json is a regular file, every other source a directory.
func (pf *preflight) checkLayouts() {
	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated {
			continue
		}
		fi, err := os.Lstat(filepath.Join(pf.dataDir, filepath.FromSlash(s.Path)))
		if err != nil {
			continue // absent is fine; other errors surfaced by the inventory
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			continue // reported by the inventory as a blocking symlink
		}
		wantFile := strings.HasSuffix(s.Path, ".json")
		switch {
		case wantFile && !fi.Mode().IsRegular():
			pf.block(ClassUnsupportedLayout, s.Path, s.Path+" must be a regular file", "")
		case !wantFile && !fi.IsDir():
			pf.block(ClassUnsupportedLayout, s.Path, s.Path+" must be a directory", "")
		}
	}
}

// checkExternals refuses unsafe external artifacts: symlinks or special files
// at the top level or under the inert plan/runtime trees, and snapshot
// transcript pointers that escape the data dir or cross a symlink.
func (pf *preflight) checkExternals() {
	entries, err := os.ReadDir(pf.dataDir)
	if err != nil {
		pf.block(ClassUnverifiable, "", "cannot list data dir", err.Error())
		return
	}
	for _, de := range entries {
		name := de.Name()
		if name == wardendb.DirName || name == ownerlock.FileName {
			continue
		}
		if de.Type()&fs.ModeSymlink != 0 {
			if !hasKnownStoreName(name) { // known sources are covered by the inventory
				pf.block(ClassUnsafeArtifact, name, name+": top-level symlink in the data dir", "")
			}
			continue
		}
		if !de.Type().IsRegular() && !de.IsDir() {
			pf.block(ClassUnsafeArtifact, name, name+": special file in the data dir", "")
		}
	}
	for _, s := range schema2Sources {
		if s.Disposition != DispositionExternal || !isRuntimeTree(s.Path) {
			continue
		}
		root := filepath.Join(pf.dataDir, filepath.FromSlash(s.Path))
		fi, err := os.Lstat(root)
		if err != nil || !fi.IsDir() {
			if err == nil && fi.Mode()&fs.ModeSymlink != 0 {
				pf.block(ClassUnsafeArtifact, s.Path, s.Path+": symlinked external artifact", "")
			}
			continue
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(pf.dataDir, p)
			rel = filepath.ToSlash(rel)
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				pf.block(ClassUnsafeArtifact, rel, rel+": symlink inside an external artifact tree", "")
			case !d.Type().IsRegular() && !d.IsDir():
				pf.block(ClassUnsafeArtifact, rel, rel+": special file inside an external artifact tree", "")
			}
			return nil
		})
	}
	pf.checkTranscriptPointers()
}

func isRuntimeTree(path string) bool {
	switch path {
	case "plans/pending", "plans/in_progress", "plans/completed", "plans/archived",
		"prompts", "hints", "exits", "settings":
		return true
	}
	return false
}

// checkTranscriptPointers validates transcript_path in legacy snapshots/*.json.
func (pf *preflight) checkTranscriptPointers() {
	dir := filepath.Join(pf.dataDir, "snapshots")
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	root, _ := filepath.EvalSymlinks(pf.dataDir)
	for _, de := range files {
		if !de.Type().IsRegular() || !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			continue // reported by checkFlatFiles
		}
		var rec struct {
			TranscriptPath string `json:"transcript_path"`
		}
		if json.Unmarshal(b, &rec) != nil || rec.TranscriptPath == "" {
			continue
		}
		rel := "snapshots/" + de.Name()
		tp := rec.TranscriptPath
		if !filepath.IsAbs(tp) {
			tp = filepath.Join(pf.dataDir, tp)
		}
		tp = filepath.Clean(tp)
		if r, err := filepath.Rel(pf.dataDir, tp); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			pf.block(ClassUnsafeArtifact, rel, rel+": transcript pointer "+rec.TranscriptPath+" is outside the data dir", "")
			continue
		}
		fi, err := os.Lstat(tp)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			pf.add(SeverityRepairable, ClassMissingArtifactRef, rel,
				rel+": transcript "+rec.TranscriptPath+" is missing; the pointer is kept as-is", "")
		case err != nil:
			pf.block(ClassUnverifiable, rel, rel+": cannot stat transcript: "+err.Error(), "")
		case !fi.Mode().IsRegular():
			pf.block(ClassUnsafeArtifact, rel, rel+": transcript pointer is not a regular file (symlink or special)", "")
		default:
			if real, err := filepath.EvalSymlinks(tp); err != nil || !strings.HasPrefix(real, root+string(filepath.Separator)) {
				pf.block(ClassUnsafeArtifact, rel, rel+": transcript path crosses a symlink out of the data dir", "")
			}
		}
	}
}

// checkFlatFiles strictly parses the legacy JSON/JSONL sources that are not
// Scriva stores and refuses duplicate identities across flat record files.
func (pf *preflight) checkFlatFiles() {
	ids := map[string]string{} // "<source>\x00<id>" -> first file
	for _, s := range schema2Sources {
		if s.Disposition != DispositionMigrated || strings.HasSuffix(s.Path, "-db") ||
			s.Path == "backends" || s.Path == "projects" || s.Path == "context" || s.Path == "inbox" ||
			strings.HasPrefix(s.Path, "plans/") {
			continue
		}
		root := filepath.Join(pf.dataDir, filepath.FromSlash(s.Path))
		fi, err := os.Lstat(root)
		if err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			continue
		}
		visit := func(p string) {
			rel, _ := filepath.Rel(pf.dataDir, p)
			rel = filepath.ToSlash(rel)
			switch {
			case strings.HasSuffix(p, ".json"):
				b, err := os.ReadFile(p)
				if err != nil {
					pf.block(ClassUnverifiable, rel, rel+": unreadable: "+err.Error(), "")
					return
				}
				var v any
				if err := json.Unmarshal(b, &v); err != nil {
					pf.block(ClassCorruption, rel, rel+": invalid JSON: "+err.Error(), "")
					return
				}
				if m, ok := v.(map[string]any); ok && (s.Path == "sessions" || s.Path == "snapshots") {
					if id, _ := m["id"].(string); id != "" {
						k := s.Path + "\x00" + id
						if first, dup := ids[k]; dup {
							pf.block(ClassDuplicateIdentity, rel, fmt.Sprintf("%s: duplicate id %q (also in %s)", rel, id, first), "")
						} else {
							ids[k] = rel
						}
					}
				}
			case strings.HasSuffix(p, ".jsonl"):
				f, err := os.Open(p)
				if err != nil {
					pf.block(ClassUnverifiable, rel, rel+": unreadable: "+err.Error(), "")
					return
				}
				defer f.Close()
				sc := bufio.NewScanner(f)
				sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
				for n := 1; sc.Scan(); n++ {
					line := bytes.TrimSpace(sc.Bytes())
					if len(line) == 0 {
						continue
					}
					if !json.Valid(line) {
						pf.block(ClassCorruption, rel, fmt.Sprintf("%s: line %d is not valid JSON", rel, n), "")
						return
					}
				}
				if err := sc.Err(); err != nil {
					pf.block(ClassCorruption, rel, rel+": "+err.Error(), "")
				}
			}
		}
		if !fi.IsDir() {
			visit(root)
			continue
		}
		var paths []string
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() && !(s.SkipSuffix != "" && strings.HasSuffix(p, s.SkipSuffix)) {
				paths = append(paths, p)
			}
			return nil
		})
		sort.Strings(paths)
		for _, p := range paths {
			visit(p)
		}
	}
}

// checkOwnerAgain re-probes the ownership lock after the (potentially long)
// verification so a daemon started meanwhile is still refused.
func (pf *preflight) checkOwnerAgain() {
	oe, err := ownerlock.Probe(pf.dataDir)
	switch {
	case err != nil:
		pf.block(ClassUnverifiable, ownerlock.FileName, "cannot probe the ownership lock: "+err.Error(), "")
	case oe != nil:
		for _, f := range pf.plan.Findings {
			if f.Store == ownerlock.FileName && f.Severity == SeverityBlocking {
				return // already reported by the plan
			}
		}
		pf.block(ClassOwned, ownerlock.FileName,
			"a running warden process took the data dir during verification", oe.Error()+"\n"+oe.NextStep())
	}
}
