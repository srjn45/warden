package migrate

// Schema-1 flat-file sources. Unlike the ScrivaDB stores these are plain
// JSON/JSONL files written by the compat-mode stores (usage-snapshots,
// quota-impact, savings, spend, metrics) plus the audit stream. They are
// imported into the shared warden-db collections under the SAME keys the
// shared runtime stores would have generated, so a migrated record is
// indistinguishable from a native one. Sources are only ever read.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva/query"

	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/capacity"
)

// flatDirSources lists the migrated flat directories and the file names the
// loader understands inside each. Anything else holding data blocks the export
// (never silently ignored).
var flatDirSources = map[string][]string{
	"usage-snapshots": {"usage-snapshots.jsonl"},
	"quota-impact":    {"quota-impact-fences.jsonl"},
	"savings":         {"ledger.jsonl", "calibration.json"},
	"spend":           {"spend.json"},
}

func loadFlatSources(ctx context.Context, dataDir string, set *stagingSet) error {
	for dir, allowed := range flatDirSources {
		if err := blockUnknownFlatFiles(dataDir, dir, allowed, ".jsonl", set); err != nil {
			return err
		}
	}
	if err := blockUnknownFlatFiles(dataDir, "metrics", nil, ".jsonl", set); err != nil {
		return err
	}
	steps := []func() error{
		func() error {
			return loadJSONL(dataDir, "usage-snapshots/usage-snapshots.jsonl", "usage_snapshots", set, func(n int, line []byte, m map[string]any) (string, error) {
				var snap backendusage.UsageSnapshot
				if err := json.Unmarshal(line, &snap); err != nil {
					return "", err
				}
				return backendusage.SnapshotKey(snap.Revision), nil
			})
		},
		func() error {
			return loadJSONLDedup(dataDir, "quota-impact/quota-impact-fences.jsonl", "quota_impact_fences", set, func(line []byte) (string, error) {
				var rec capacity.FenceRecord
				if err := json.Unmarshal(line, &rec); err != nil {
					return "", err
				}
				return capacity.FenceKey(rec), nil
			})
		},
		func() error {
			return loadSavingsLedger(dataDir, set)
		},
		func() error {
			return loadSingletonJSON(dataDir, "savings/calibration.json", "savings_calibration", "current", set)
		},
		func() error { return loadSpend(dataDir, set) },
		func() error { return loadMetrics(ctx, dataDir, set) },
		func() error { return loadAudit(dataDir, set) },
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// blockUnknownFlatFiles refuses a flat source directory that holds data the
// loader has no mapping for.
func blockUnknownFlatFiles(dataDir, dir string, allowed []string, allowedSuffix string, set *stagingSet) error {
	root := filepath.Join(dataDir, filepath.FromSlash(dir))
	des, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	for _, de := range des {
		name := de.Name()
		if ok[name] || (allowed == nil && strings.HasSuffix(name, allowedSuffix) && !de.IsDir()) {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			return err
		}
		if de.IsDir() || fi.Size() > 0 {
			set.block(dir, dir+"/"+name+": file has no safe staging mapping", ClassUnsupportedLayout)
		}
	}
	return nil
}

// readJSONLines returns each non-blank line with its 1-based line number.
func readJSONLines(path string) ([][]byte, []int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var lines [][]byte
	var nums []int
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), line...))
		nums = append(nums, n)
	}
	return lines, nums, sc.Err()
}

func decodeObject(line []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("not a JSON object")
	}
	return m, nil
}

// loadJSONL imports one record per line; keyFn derives the storage key. A line
// that does not decode blocks: the source is never partially imported.
func loadJSONL(dataDir, rel, target string, set *stagingSet, keyFn func(n int, line []byte, m map[string]any) (string, error)) error {
	lines, nums, err := readJSONLines(filepath.Join(dataDir, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	for i, line := range lines {
		m, err := decodeObject(line)
		if err != nil {
			set.block(target, fmt.Sprintf("%s: line %d: %v", rel, nums[i], err), ClassCorruption)
			continue
		}
		key, err := keyFn(i+1, line, m)
		if err != nil {
			set.block(target, fmt.Sprintf("%s: line %d: %v", rel, nums[i], err), ClassCorruption)
			continue
		}
		set.add(target, key, m)
	}
	return nil
}

// loadJSONLDedup is loadJSONL for insert-if-absent ledgers: a repeated key with
// an identical body collapses, a divergent one blocks.
func loadJSONLDedup(dataDir, rel, target string, set *stagingSet, keyFn func(line []byte) (string, error)) error {
	lines, nums, err := readJSONLines(filepath.Join(dataDir, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	seen := map[string]map[string]any{}
	for i, line := range lines {
		m, err := decodeObject(line)
		if err == nil {
			var key string
			if key, err = keyFn(line); err == nil {
				if prior, dup := seen[key]; dup {
					if !canonicalEqual(prior, m) {
						set.block(target, fmt.Sprintf("%s: line %d: key %s repeated with a different body", rel, nums[i], key), ClassAmbiguity)
					}
					continue
				}
				seen[key] = m
				set.add(target, key, m)
				continue
			}
		}
		set.block(target, fmt.Sprintf("%s: line %d: %v", rel, nums[i], err), ClassCorruption)
	}
	return nil
}

func loadSingletonJSON(dataDir, rel, target, key string, set *stagingSet) error {
	b, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	m, err := decodeObject(b)
	if err != nil {
		set.block(target, rel+": "+err.Error(), ClassCorruption)
		return nil
	}
	set.add(target, key, m)
	return nil
}

// loadSpend imports spend.json (session id -> gauge) keyed by session id.
func loadSpend(dataDir string, set *stagingSet) error {
	rel := "spend/spend.json"
	b, err := os.ReadFile(filepath.Join(dataDir, filepath.FromSlash(rel)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var all map[string]map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		set.block("spend_records", rel+": "+err.Error(), ClassCorruption)
		return nil
	}
	for id, entry := range all {
		if id == "" || entry == nil {
			set.block("spend_records", rel+": entry with empty id or body", ClassCorruption)
			continue
		}
		set.add("spend_records", id, entry)
	}
	return nil
}

// loadMetrics imports every daily metrics/YYYY-MM-DD.jsonl sample. The key
// mirrors metrics.Store.Record ("<taken_at unix-nano>:<id>") with a suffix
// derived from the line so reruns are deterministic; identical repeated
// samples collapse.
func loadMetrics(ctx context.Context, dataDir string, set *stagingSet) error {
	files, err := filepath.Glob(filepath.Join(dataDir, "metrics", "*.jsonl"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, fp := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := relPath(dataDir, fp)
		lines, nums, err := readJSONLines(fp)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for i, line := range lines {
			m, err := decodeObject(line)
			var at time.Time
			if err == nil {
				var s struct {
					TakenAt time.Time `json:"taken_at"`
				}
				err = json.Unmarshal(line, &s)
				at = s.TakenAt
			}
			if err != nil {
				set.block("metrics_samples", fmt.Sprintf("%s: line %d: %v", rel, nums[i], err), ClassCorruption)
				continue
			}
			sum := sha256.Sum256(line)
			key := fmt.Sprintf("%020d:%s", at.UnixNano(), hex.EncodeToString(sum[:4]))
			if seen[key] {
				continue
			}
			seen[key] = true
			set.add("metrics_samples", key, m)
		}
	}
	return nil
}

// loadAudit imports audit.jsonl into audit_events. The file itself stays where
// it is (external, append-only, never rewritten); the shared runtime reads only
// the collection, so without this copy the pre-migration trail would vanish
// from `warden audit`. Keys follow auditstore ("%020d-%012d-<id>") with the
// time component clamped to a running maximum, so key order is exactly file
// order and every imported event sorts before any event the new daemon logs.
func loadAudit(dataDir string, set *stagingSet) error {
	rel := "audit.jsonl"
	lines, nums, err := readJSONLines(filepath.Join(dataDir, rel))
	if err != nil {
		return err
	}
	var high int64
	for i, line := range lines {
		m, err := decodeObject(line)
		var ev struct {
			Time time.Time `json:"time"`
		}
		if err == nil {
			err = json.Unmarshal(line, &ev)
		}
		if err != nil {
			set.block("audit_events", fmt.Sprintf("%s: line %d: %v", rel, nums[i], err), ClassCorruption)
			continue
		}
		if ns := ev.Time.UnixNano(); !ev.Time.IsZero() && ns > high {
			high = ns
		}
		sum := sha256.Sum256(line)
		set.add("audit_events", fmt.Sprintf("%020d-%012d-%s", high, i+1, hex.EncodeToString(sum[:4])), m)
	}
	return nil
}

// legacySessionSources are the pre-agents-db session stores: the sessions-db
// ScrivaDB (collections "active"/"closed") and the older flat <id>.json files
// under sessions/ and closed/. The ledger (schema >= 1) means the boot/0001
// importers already copied them into agents-db / terminals-db; what is left is
// an inert, import-time copy that is frozen on disk (nothing writes it). They
// are not re-imported — doing so would resurrect agents deleted since — but
// they are never ignored silently either: ids that exist only there are
// reported, and the whole directories stay in place and in the backup.
func loadLegacySessionCopies(ctx context.Context, dataDir string, set *stagingSet) error {
	legacy := map[string]string{} // id -> where it was found
	note := func(id, where string) {
		if id != "" {
			if _, dup := legacy[id]; !dup {
				legacy[id] = where
			}
		}
	}

	dbDir := filepath.Join(dataDir, "sessions-db")
	db, err := openIfExists(dbDir)
	if err != nil {
		return err
	}
	if db != nil {
		defer db.Close()
		des, err := os.ReadDir(dbDir)
		if err != nil {
			return err
		}
		for _, de := range des {
			if !de.IsDir() || strings.HasPrefix(de.Name(), ".") {
				continue
			}
			col, err := db.Collection(de.Name())
			if err != nil {
				return err
			}
			rows, err := col.Scan(query.MatchAll)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if de.Name() != "active" && de.Name() != "closed" {
					set.block("sessions-db", "sessions-db/"+de.Name()+": collection has data but no safe staging mapping", ClassUnsupportedLayout)
					break
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				note(firstNonEmpty(str(r.Data["id"]), str(r.Data["_key"])), "sessions-db/"+de.Name())
			}
		}
	}
	for _, dir := range []string{"sessions", "closed"} {
		files, err := filepath.Glob(filepath.Join(dataDir, dir, "*.json"))
		if err != nil {
			return err
		}
		for _, fp := range files {
			b, err := os.ReadFile(fp)
			if err != nil {
				return err
			}
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				note(str(m["id"]), relPath(dataDir, fp))
			}
		}
	}

	var only []string
	for id := range legacy {
		_, a := set.agents[id]
		_, t := set.terms[id]
		if !a && !t {
			only = append(only, id)
		}
	}
	if len(only) == 0 {
		return nil
	}
	sort.Strings(only)
	shown := only
	if len(shown) > 5 {
		shown = shown[:5]
	}
	set.findings = append(set.findings, Finding{Severity: SeverityRepairable, Class: string(ClassMissingArtifactRef), Store: "sessions-db",
		Message: fmt.Sprintf("%d legacy session record(s) exist only in the pre-agents-db copies (e.g. %s): they were deleted or archived away after the import, "+
			"stay untouched in place and in the pre-migration backup, and are not carried into warden-db", len(only), strings.Join(shown, ", "))})
	return nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

type savingsLine struct {
	lineNum int
	offset  int64
	length  int
	raw     []byte
}

func readSavingsLinesWithOffsets(path string) ([]savingsLine, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64*1024)
	var lines []savingsLine
	var curOffset int64
	lineNum := 0
	maxLineLen := 16 * 1024 * 1024 // 16 MiB line safety limit
	for {
		lineNum++
		startOffset := curOffset
		rawWithDelim, readErr := r.ReadBytes('\n')
		if len(rawWithDelim) == 0 && readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read %s at line %d (offset %d): %w", path, lineNum, startOffset, readErr)
		}
		curOffset += int64(len(rawWithDelim))
		if len(rawWithDelim) > maxLineLen {
			return nil, fmt.Errorf("line %d (offset %d) exceeds 16MiB limit; line boundaries cannot be safely determined", lineNum, startOffset)
		}
		raw := bytes.TrimRight(rawWithDelim, "\r\n")
		if len(bytes.TrimSpace(raw)) == 0 {
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return nil, fmt.Errorf("read %s at line %d (offset %d): %w", path, lineNum, startOffset, readErr)
			}
			continue
		}
		lines = append(lines, savingsLine{
			lineNum: lineNum,
			offset:  startOffset,
			length:  len(raw),
			raw:     append([]byte(nil), raw...),
		})
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read %s at line %d (offset %d): %w", path, lineNum, startOffset, readErr)
		}
	}
	return lines, nil
}

func loadSavingsLedger(dataDir string, set *stagingSet) error {
	path := filepath.Join(dataDir, "savings", "ledger.jsonl")
	lines, err := readSavingsLinesWithOffsets(path)
	if err != nil {
		set.block("savings_events", fmt.Sprintf("savings/ledger.jsonl: %v", err), ClassCorruption)
		return nil
	}
	validCount := 0
	for _, sl := range lines {
		m, decErr := decodeObject(sl.raw)
		if decErr != nil {
			sha := sha256.Sum256(sl.raw)
			shaHex := hex.EncodeToString(sha[:])
			q := QuarantinedRecord{
				SourcePath: "savings/ledger.jsonl",
				Line:       sl.lineNum,
				Offset:     sl.offset,
				Length:     sl.length,
				RawBytes:   string(sl.raw),
				RawHex:     hex.EncodeToString(sl.raw),
				SHA256:     shaHex,
				Reason:     decErr.Error(),
			}
			set.quarantined = append(set.quarantined, q)
			set.findings = append(set.findings, Finding{
				Severity: SeverityRepairable,
				Class:    string(ClassCorruption),
				Store:    "savings/ledger.jsonl",
				Message:  fmt.Sprintf("savings/ledger.jsonl: line %d (offset %d): malformed JSON record quarantined (%s)", sl.lineNum, sl.offset, shaHex[:12]),
				Details:  fmt.Sprintf("raw: %q; reason: %v; preserved in quarantine", string(sl.raw), decErr),
			})
			continue
		}
		validCount++
		key := fmt.Sprintf("%012d", validCount)
		set.add("savings_events", key, m)
	}
	return nil
}

func writeSavingsQuarantine(dataDir string, records []QuarantinedRecord) error {
	return writeSavingsQuarantineDir(filepath.Join(dataDir, "quarantine", "savings"), records)
}

func writeSavingsQuarantineDir(quarDir string, records []QuarantinedRecord) error {
	if len(records) == 0 {
		return nil
	}
	if err := os.MkdirAll(quarDir, 0o700); err != nil {
		return fmt.Errorf("create quarantine dir %s: %w", quarDir, err)
	}

	for _, rec := range records {
		rawFileName := fmt.Sprintf("line_%06d_offset_%012d_%s.raw", rec.Line, rec.Offset, rec.SHA256[:8])
		rawPath := filepath.Join(quarDir, rawFileName)
		rawBytes, err := hex.DecodeString(rec.RawHex)
		if err != nil {
			rawBytes = []byte(rec.RawBytes)
		}
		sum := sha256.Sum256(rawBytes)
		if hex.EncodeToString(sum[:]) != rec.SHA256 {
			return fmt.Errorf("quarantine checksum mismatch for line %d: got %s, want %s", rec.Line, hex.EncodeToString(sum[:]), rec.SHA256)
		}
		if err := atomicWriteFile(rawPath, rawBytes, 0o600); err != nil {
			return fmt.Errorf("write quarantine raw file %s: %w", rawPath, err)
		}
	}

	var jsonlBuf bytes.Buffer
	for _, rec := range records {
		b, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("marshal quarantine record: %w", err)
		}
		jsonlBuf.Write(b)
		jsonlBuf.WriteByte('\n')
	}
	if err := atomicWriteFile(filepath.Join(quarDir, "records.jsonl"), jsonlBuf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("write quarantine records.jsonl: %w", err)
	}

	manifest := struct {
		SourcePath    string              `json:"source_path"`
		QuarantinedAt time.Time           `json:"quarantined_at"`
		TotalRecords  int                 `json:"total_records"`
		Records       []QuarantinedRecord `json:"records"`
	}{
		SourcePath:    "savings/ledger.jsonl",
		QuarantinedAt: time.Now().UTC(),
		TotalRecords:  len(records),
		Records:       records,
	}
	mb, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal quarantine manifest: %w", err)
	}
	if err := atomicWriteFile(filepath.Join(quarDir, "manifest.json"), append(mb, '\n'), 0o600); err != nil {
		return fmt.Errorf("write quarantine manifest.json: %w", err)
	}

	if err := syncDirectory(quarDir); err != nil {
		return err
	}
	parent := filepath.Dir(quarDir)
	if err := syncDirectory(parent); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(parent))
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
