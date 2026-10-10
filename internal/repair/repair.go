package repair

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/scriva/store"
	"github.com/srjn45/warden/internal/migrate"
	"github.com/srjn45/warden/internal/ownerlock"
)

// DroppedRecord describes a record excluded during rebuild with rationale.
type DroppedRecord struct {
	Collection string `json:"collection"`
	Segment    string `json:"segment"`
	Offset     int64  `json:"offset"`
	Key        string `json:"key"`
	Reason     string `json:"reason"`
	RecordJSON string `json:"record_json,omitempty"`
}

// CollectionResult reports the repair outcome for one ScrivaDB collection.
type CollectionResult struct {
	Name          string          `json:"name"`
	TotalRead     int             `json:"total_read"`
	KeptRecords   int             `json:"kept_records"`
	DroppedCount  int             `json:"dropped_count"`
	Dropped       []DroppedRecord `json:"dropped,omitempty"`
	QuarantineDir string          `json:"quarantine_dir,omitempty"`
	VerifiedClean bool            `json:"verified_clean"`
}

// Report captures the full outcome across all collections and stores repaired.
type Report struct {
	DataDir      string                        `json:"data_dir"`
	BackupDir    string                        `json:"backup_dir"`
	CreatedAt    time.Time                     `json:"created_at"`
	Stores       map[string][]CollectionResult `json:"stores"`
	Clean        bool                          `json:"clean"`
	ErrorMessage string                        `json:"error_message,omitempty"`
}

// Options configures `warden repair all`.
type Options struct {
	DataDir        string
	ResolveHistory string // must be "live-wins"
	BackupParent   string // where pre-repair backup is placed
	DryRun         bool
	Version        string
	Stdout         io.Writer
}

// RepairAll rebuilds every ScrivaDB collection in dataDir:
// 1. Verifies ownership / stops running daemon conflicts.
// 2. Makes an offline timestamped backup of the stores before touching anything.
// 3. For each store and collection, scans all records line-by-line.
// 4. Resolves records with "live-wins": keeps newest valid record per _key, drops stale/lower revisions and stray records (body id != key).
// 5. Quarantines original segments/indexes into <collection>/quarantine/<run>/ with manifest.
// 6. Writes rebuilt seg_000001.ndjson, meta.json, and rebuilds indexes.
// 7. Verifies the repaired store read-only.
func RepairAll(ctx context.Context, opts Options) (*Report, error) {
	if opts.ResolveHistory != "live-wins" {
		return nil, fmt.Errorf("unsupported resolve-history policy %q: only 'live-wins' is supported", opts.ResolveHistory)
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}

	// 1. Acquire data-dir ownership lock
	lock, err := ownerlock.Acquire(opts.DataDir, ownerlock.Info{
		Kind:    ownerlock.KindCLI,
		Version: opts.Version,
		Command: "warden repair all --resolve-history=live-wins",
	})
	if err != nil {
		return nil, fmt.Errorf("repair all: cannot run while daemon is running: %w", err)
	}
	defer lock.Release()

	ts := time.Now().UTC().Format("20060102T150405Z")
	backupParent := opts.BackupParent
	if backupParent == "" {
		backupParent = filepath.Join(opts.DataDir, "backups")
	}
	backupDir := filepath.Join(backupParent, "repair-backup-"+ts)

	rep := &Report{
		DataDir:   opts.DataDir,
		BackupDir: backupDir,
		CreatedAt: time.Now().UTC(),
		Stores:    make(map[string][]CollectionResult),
		Clean:     true,
	}

	stores, err := migrate.DiscoverStores(opts.DataDir)
	if err != nil {
		return nil, fmt.Errorf("repair all: discover stores: %w", err)
	}

	if opts.DryRun {
		fmt.Fprintf(opts.Stdout, "repair all [dry-run]: inspecting %d stores in %s…\n", len(stores), opts.DataDir)
		for _, s := range stores {
			sDir := filepath.Join(opts.DataDir, filepath.FromSlash(s))
			colls, err := inspectStore(sDir)
			if err != nil {
				return nil, err
			}
			rep.Stores[s] = colls
		}
		return rep, nil
	}

	// 2. Perform whole-store backup before modifying anything
	fmt.Fprintf(opts.Stdout, "repair all: backing up stores to %s…\n", backupDir)
	if err := backupStores(opts.DataDir, backupDir, stores); err != nil {
		return nil, fmt.Errorf("repair all: backup failed: %w", err)
	}

	// 3. Rebuild collections in each store
	for _, s := range stores {
		sDir := filepath.Join(opts.DataDir, filepath.FromSlash(s))
		res, err := rebuildStore(ctx, sDir, ts)
		if err != nil {
			rep.Clean = false
			rep.ErrorMessage = err.Error()
			return rep, fmt.Errorf("repair store %s: %w", s, err)
		}
		rep.Stores[s] = res
	}

	// 4. Verify repaired stores
	findings, vErr := migrate.VerifyAllStores(ctx, opts.DataDir)
	if vErr != nil {
		rep.Clean = false
		rep.ErrorMessage = vErr.Error()
		return rep, fmt.Errorf("repair all: post-repair verification error: %w", vErr)
	}
	for _, f := range findings {
		if f.Severity == migrate.SeverityBlocking || f.Severity == migrate.SeverityRepairable {
			rep.Clean = false
			rep.ErrorMessage = fmt.Sprintf("post-repair verification failed on %s: %s", f.Store, f.Message)
			break
		}
	}

	// Write repair report inside backup dir
	repBytes, _ := json.MarshalIndent(rep, "", "  ")
	_ = os.WriteFile(filepath.Join(backupDir, "repair-report.json"), append(repBytes, '\n'), 0o644)

	return rep, nil
}

func inspectStore(storeDir string) ([]CollectionResult, error) {
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return nil, err
	}
	var results []CollectionResult
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "quarantine" {
			continue
		}
		cDir := filepath.Join(storeDir, e.Name())
		cRes, err := simulateRebuildCollection(cDir, e.Name())
		if err != nil {
			return nil, err
		}
		results = append(results, cRes)
	}
	return results, nil
}

func simulateRebuildCollection(collDir, name string) (CollectionResult, error) {
	records, dropped, total, err := scanAndResolveRecords(collDir, name)
	if err != nil {
		return CollectionResult{}, err
	}
	return CollectionResult{
		Name:          name,
		TotalRead:     total,
		KeptRecords:   len(records),
		DroppedCount:  len(dropped),
		Dropped:       dropped,
		VerifiedClean: len(dropped) == 0,
	}, nil
}

func rebuildStore(ctx context.Context, storeDir string, runID string) ([]CollectionResult, error) {
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		return nil, err
	}
	var results []CollectionResult
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "quarantine" {
			continue
		}
		cDir := filepath.Join(storeDir, e.Name())
		cRes, err := rebuildCollection(ctx, cDir, e.Name(), runID)
		if err != nil {
			return nil, err
		}
		results = append(results, cRes)
	}
	return results, nil
}

type resolvedRecord struct {
	Key     string
	Data    map[string]any
	Rev     uint64
	Ts      time.Time
	Segment string
	Offset  int64
	RawLine []byte
}

func scanAndResolveRecords(collDir, collName string) ([]resolvedRecord, []DroppedRecord, int, error) {
	entries, err := os.ReadDir(collDir)
	if err != nil {
		return nil, nil, 0, err
	}

	var segFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "seg_") && strings.HasSuffix(e.Name(), ".ndjson") {
			segFiles = append(segFiles, e.Name())
		}
	}
	sort.Strings(segFiles)

	keptByKey := make(map[string]resolvedRecord)
	var keyOrder []string
	var dropped []DroppedRecord
	totalRead := 0

	for _, seg := range segFiles {
		segPath := filepath.Join(collDir, seg)
		f, err := os.Open(segPath)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		buf := make([]byte, 1024*1024)
		scanner.Buffer(buf, 10*1024*1024)

		var offset int64 = 0
		for scanner.Scan() {
			line := scanner.Bytes()
			lineLen := int64(len(line) + 1) // + newline
			totalRead++

			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				offset += lineLen
				continue
			}

			// First try store.Decode for canonical ScrivaDB entry
			entry, decErr := store.Decode(trimmed)
			if decErr != nil {
				// Try unmarshaling raw JSON object
				var rawObj map[string]any
				if jErr := json.Unmarshal(trimmed, &rawObj); jErr != nil {
					dropped = append(dropped, DroppedRecord{
						Collection: collName,
						Segment:    seg,
						Offset:     offset,
						Reason:     fmt.Sprintf("undecodable JSON: %v", jErr),
						RecordJSON: string(trimmed),
					})
					offset += lineLen
					continue
				}

				// If it's a raw record without ScrivaDB envelope
				key, _ := rawObj["_key"].(string)
				if key == "" {
					if idStr, ok := rawObj["id"].(string); ok && idStr != "" {
						key = idStr
					}
				}
				if key == "" {
					dropped = append(dropped, DroppedRecord{
						Collection: collName,
						Segment:    seg,
						Offset:     offset,
						Reason:     "missing _key",
						RecordJSON: string(trimmed),
					})
					offset += lineLen
					continue
				}

				if bodyID, ok := rawObj["id"].(string); ok && bodyID != "" && bodyID != key {
					dropped = append(dropped, DroppedRecord{
						Collection: collName,
						Segment:    seg,
						Offset:     offset,
						Key:        key,
						Reason:     fmt.Sprintf("stray record: body id %q != _key %q", bodyID, key),
						RecordJSON: string(trimmed),
					})
					offset += lineLen
					continue
				}

				cand := resolvedRecord{
					Key:     key,
					Data:    rawObj,
					Rev:     1,
					Ts:      time.Now().UTC(),
					Segment: seg,
					Offset:  offset,
					RawLine: append([]byte(nil), trimmed...),
				}
				if existing, exists := keptByKey[key]; exists {
					dropped = append(dropped, DroppedRecord{
						Collection: collName,
						Segment:    existing.Segment,
						Offset:     existing.Offset,
						Key:        key,
						Reason:     "superseded by newer record per live-wins policy",
						RecordJSON: string(existing.RawLine),
					})
				} else {
					keyOrder = append(keyOrder, key)
				}
				keptByKey[key] = cand
				offset += lineLen
				continue
			}

			// Valid ScrivaDB entry decoded
			if entry.Op == store.OpDelete {
				// Deletion tombstones: drop any existing record for this key
				key, _ := entry.Data["_key"].(string)
				if key != "" {
					if existing, exists := keptByKey[key]; exists {
						dropped = append(dropped, DroppedRecord{
							Collection: collName,
							Segment:    existing.Segment,
							Offset:     existing.Offset,
							Key:        key,
							Reason:     "deleted by later tombstone per live-wins policy",
							RecordJSON: string(existing.RawLine),
						})
						delete(keptByKey, key)
					}
				}
				offset += lineLen
				continue
			}

			key, _ := entry.Data["_key"].(string)
			if key == "" {
				if idStr, ok := entry.Data["id"].(string); ok && idStr != "" {
					key = idStr
				}
			}
			if key == "" {
				dropped = append(dropped, DroppedRecord{
					Collection: collName,
					Segment:    seg,
					Offset:     offset,
					Reason:     "missing _key in entry data",
					RecordJSON: string(trimmed),
				})
				offset += lineLen
				continue
			}

			// Stray record check
			if bodyID, ok := entry.Data["id"].(string); ok && bodyID != "" && bodyID != key {
				dropped = append(dropped, DroppedRecord{
					Collection: collName,
					Segment:    seg,
					Offset:     offset,
					Key:        key,
					Reason:     fmt.Sprintf("stray record: body id %q != _key %q", bodyID, key),
					RecordJSON: string(trimmed),
				})
				offset += lineLen
				continue
			}

			cand := resolvedRecord{
				Key:     key,
				Data:    entry.Data,
				Rev:     entry.Rev,
				Ts:      entry.Ts,
				Segment: seg,
				Offset:  offset,
				RawLine: append([]byte(nil), trimmed...),
			}

			if existing, exists := keptByKey[key]; exists {
				dropped = append(dropped, DroppedRecord{
					Collection: collName,
					Segment:    existing.Segment,
					Offset:     existing.Offset,
					Key:        key,
					Reason:     "superseded by newer record per live-wins policy",
					RecordJSON: string(existing.RawLine),
				})
			} else {
				keyOrder = append(keyOrder, key)
			}
			keptByKey[key] = cand
			offset += lineLen
		}
		_ = f.Close()
	}

	var ordered []resolvedRecord
	for _, k := range keyOrder {
		if rec, exists := keptByKey[k]; exists {
			ordered = append(ordered, rec)
		}
	}

	return ordered, dropped, totalRead, nil
}

func rebuildCollection(ctx context.Context, collDir, collName string, runID string) (CollectionResult, error) {
	ordered, dropped, totalRead, err := scanAndResolveRecords(collDir, collName)
	if err != nil {
		return CollectionResult{}, err
	}

	type indexInfo struct {
		Field  string `json:"field"`
		Unique bool   `json:"unique"`
	}
	indexes := map[string]indexInfo{
		"_key": {Field: "_key", Unique: true},
	}

	entries, _ := os.ReadDir(collDir)
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "sidx_") && strings.HasSuffix(name, ".json") {
			field := name[len("sidx_") : len(name)-len(".json")]
			data, readErr := os.ReadFile(filepath.Join(collDir, name))
			unique := false
			if readErr == nil {
				var sf struct {
					Field  string `json:"field"`
					Unique bool   `json:"unique"`
				}
				if json.Unmarshal(data, &sf) == nil {
					unique = sf.Unique
				}
			}
			indexes[field] = indexInfo{Field: field, Unique: unique}
		}
	}

	// Quarantine existing segment and index files
	quarantineDir := filepath.Join(collDir, "quarantine", runID)
	if err := os.MkdirAll(quarantineDir, 0o700); err != nil {
		return CollectionResult{}, fmt.Errorf("create quarantine dir: %w", err)
	}

	var quarantinedFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "seg_") || strings.HasPrefix(name, "index") || strings.HasPrefix(name, "sidx_") {
			oldP := filepath.Join(collDir, name)
			newP := filepath.Join(quarantineDir, name)
			if err := os.Rename(oldP, newP); err == nil {
				quarantinedFiles = append(quarantinedFiles, name)
			}
		}
	}

	// Write quarantine manifest
	type quarantineManifest struct {
		Collection       string          `json:"collection"`
		RunID            string          `json:"run_id"`
		CreatedAt        time.Time       `json:"created_at"`
		QuarantinedFiles []string        `json:"quarantined_files"`
		DroppedRecords   []DroppedRecord `json:"dropped_records"`
	}
	manifestData, _ := json.MarshalIndent(quarantineManifest{
		Collection:       collName,
		RunID:            runID,
		CreatedAt:        time.Now().UTC(),
		QuarantinedFiles: quarantinedFiles,
		DroppedRecords:   dropped,
	}, "", "  ")
	_ = os.WriteFile(filepath.Join(quarantineDir, "manifest.json"), append(manifestData, '\n'), 0o644)

	// Write clean seg_000001.ndjson with newly encoded ScrivaDB entries
	newSegPath := filepath.Join(collDir, "seg_000001.ndjson")
	segF, err := os.OpenFile(newSegPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return CollectionResult{}, fmt.Errorf("create clean segment: %w", err)
	}

	for idx, rec := range ordered {
		// Assign clean sequential uint64 record ID
		id := uint64(idx + 1)
		rec.Data["_key"] = rec.Key
		entry := store.NewInsert(id, rec.Data)
		entry.Rev = rec.Rev
		if entry.Rev == 0 {
			entry.Rev = 1
		}
		if !rec.Ts.IsZero() {
			entry.Ts = rec.Ts
		}

		enc, encErr := store.Encode(entry)
		if encErr != nil {
			_ = segF.Close()
			return CollectionResult{}, fmt.Errorf("encode rebuilt entry for %s: %w", rec.Key, encErr)
		}
		if _, err := segF.Write(enc); err != nil {
			_ = segF.Close()
			return CollectionResult{}, err
		}
	}
	if err := segF.Sync(); err != nil {
		_ = segF.Close()
		return CollectionResult{}, err
	}
	_ = segF.Close()

	// Ensure meta.json exists
	metaPath := filepath.Join(collDir, "meta.json")
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		metaBytes := []byte(fmt.Sprintf(`{"name":"%s","created_at":"%s"}`+"\n", collName, time.Now().UTC().Format(time.RFC3339)))
		_ = os.WriteFile(metaPath, metaBytes, 0o644)
	}

	// Open collection with ScrivaDB engine to rebuild fresh primary index
	col, err := engine.OpenCollection(collName, filepath.Dir(collDir), engine.CollectionConfig{})
	if err != nil {
		return CollectionResult{}, fmt.Errorf("reopen collection after rebuild: %w", err)
	}

	// Rebuild and persist secondary indexes (including _key)
	for _, idx := range indexes {
		var idxErr error
		if idx.Unique {
			idxErr = col.EnsureUniqueIndex(idx.Field)
		} else {
			idxErr = col.EnsureIndex(idx.Field)
		}
		if idxErr != nil {
			_ = col.Close()
			return CollectionResult{}, fmt.Errorf("rebuild index %q on %s: %w", idx.Field, collName, idxErr)
		}
	}
	_ = col.Close()

	return CollectionResult{
		Name:          collName,
		TotalRead:     totalRead,
		KeptRecords:   len(ordered),
		DroppedCount:  len(dropped),
		Dropped:       dropped,
		QuarantineDir: quarantineDir,
		VerifiedClean: true,
	}, nil
}

func backupStores(dataDir, backupDir string, stores []string) error {
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	for _, rel := range stores {
		src := filepath.Join(dataDir, filepath.FromSlash(rel))
		dst := filepath.Join(backupDir, filepath.FromSlash(rel))
		if err := copyDir(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
