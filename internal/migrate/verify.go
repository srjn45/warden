package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/backendstore"
)

// KnownStores lists the relative paths of standard ScrivaDB stores within a Warden data directory.
var KnownStores = []string{
	"backends",
	"agents-db",
	"terminals-db",
	"context",
	"inbox",
	"plans/plans-db",
	"plans/plan-exports-db",
	"projects",
	"sessions-db",
	"pipelines-db",
	"schedules-db",
	"snapshots-db",
	"known-prompts-db",
	"autopilots-db",
	"autopilot/runs-db",
}

// DiscoverStores finds all ScrivaDB store directories inside dataDir,
// checking both KnownStores and dynamically scanning for collection directories.
func DiscoverStores(dataDir string) ([]string, error) {
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		return nil, nil
	}

	found := make(map[string]bool)

	// 1. Check known store paths
	for _, rel := range KnownStores {
		full := filepath.Join(dataDir, filepath.FromSlash(rel))
		if fi, err := os.Stat(full); err == nil && fi.IsDir() {
			found[filepath.Clean(filepath.FromSlash(rel))] = true
		}
	}

	// 2. Discover any additional ScrivaDB store directories up to 3 levels deep
	err := filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil || rel == "." {
			return nil
		}

		base := filepath.Base(p)
		if strings.HasPrefix(base, ".") || base == "backups" || base == "tmp" || base == "scratch" {
			return filepath.SkipDir
		}

		// Don't recurse deeper than 3 levels from dataDir
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) > 3 {
			return filepath.SkipDir
		}

		// Check if this directory looks like a ScrivaDB store (contains collection directories with seg_*.ndjson or index.json)
		if isScrivaStore(p) {
			found[filepath.Clean(rel)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	res := make([]string, 0, len(found))
	for s := range found {
		res = append(res, filepath.ToSlash(s))
	}
	sort.Strings(res)
	return res, nil
}

// isScrivaStore checks if dir is a ScrivaDB store directory (has collection subdirectories containing segment or index files).
func isScrivaStore(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		subDir := filepath.Join(dir, e.Name())
		subEntries, err := os.ReadDir(subDir)
		if err != nil {
			continue
		}
		for _, se := range subEntries {
			name := se.Name()
			if (strings.HasPrefix(name, "seg_") && strings.HasSuffix(name, ".ndjson")) || name == "index.json" {
				return true
			}
		}
	}
	return false
}

// RepairCommandFor returns the suggested repair command for findings in store.
func RepairCommandFor(store string) string {
	store = filepath.ToSlash(store)
	switch {
	case store == "backends" || strings.HasPrefix(store, "backends/"):
		return "warden repair backends"
	case store == "agents-db" || strings.HasPrefix(store, "agents-db/") || store == "agents":
		return "warden repair agents"
	default:
		return "warden repair all --resolve-history=live-wins"
	}
}

// VerifyAllStores inspects every ScrivaDB store in dataDir using the engine's strictness,
// classifying findings as SeverityAuto, SeverityRepairable, or SeverityBlocking.
func VerifyAllStores(ctx context.Context, dataDir string) ([]Finding, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		return nil, nil
	}

	stores, err := DiscoverStores(dataDir)
	if err != nil {
		return nil, fmt.Errorf("verify all stores: discover: %w", err)
	}

	var findings []Finding

	for _, storeRel := range stores {
		fullPath := filepath.Join(dataDir, filepath.FromSlash(storeRel))

		// Special-cased verify for backend registry to preserve its recoverable vs ambiguous classification
		if storeRel == "backends" {
			backendFindings, err := verifyBackendsStore(ctx, fullPath)
			if err != nil {
				findings = append(findings, Finding{
					Severity: SeverityBlocking,
					Store:    "backends",
					Message:  fmt.Sprintf("backends: verification failed: %v", err),
					Command:  "warden repair backends",
				})
			} else {
				findings = append(findings, backendFindings...)
			}
			continue
		}

		storeFindings, err := verifyGenericStore(ctx, storeRel, fullPath)
		if err != nil {
			findings = append(findings, Finding{
				Severity: SeverityBlocking,
				Store:    storeRel,
				Message:  fmt.Sprintf("%s: verification failed: %v", storeRel, err),
				Command:  RepairCommandFor(storeRel),
			})
		} else {
			findings = append(findings, storeFindings...)
		}
	}

	return findings, nil
}

func verifyBackendsStore(ctx context.Context, fullPath string) ([]Finding, error) {
	rep, err := backendstore.Verify(ctx, fullPath)
	if err != nil {
		return nil, err
	}
	var findings []Finding
	for _, c := range rep.Collections {
		switch c.Verdict {
		case backendstore.VerdictClean:
			continue
		case backendstore.VerdictRecoverable:
			findings = append(findings, Finding{
				Severity: SeverityAuto,
				Store:    "backends/" + c.Name,
				Message:  fmt.Sprintf("backends/%s: %s (auto-recoverable derived index / stale revision)", c.Name, strings.Join(c.Codes, ", ")),
				Details:  strings.Join(c.Severities, ", "),
			})
		case backendstore.VerdictAmbiguous:
			sev := SeverityRepairable
			if slices.Contains(c.Severities, string(engine.SeverityDataCorruption)) {
				sev = SeverityBlocking
			}
			why := strings.Join(c.Reasons, "; ")
			findings = append(findings, Finding{
				Severity: sev,
				Store:    "backends/" + c.Name,
				Message:  fmt.Sprintf("backends/%s: %s (%s)", c.Name, strings.Join(c.Codes, ", "), why),
				Details:  strings.Join(c.Severities, ", "),
				Command:  "warden repair backends",
			})
		}
	}
	return findings, nil
}

func verifyGenericStore(ctx context.Context, storeRel, fullPath string) ([]Finding, error) {
	rep, err := engine.VerifyDir(ctx, fullPath, engine.VerifyOptions{Mode: engine.VerifyFull})
	if err != nil {
		return nil, err
	}

	var findings []Finding

	// Database-level findings (e.g. locks)
	for _, f := range rep.Findings {
		if f.Severity == engine.SeverityInfo {
			continue
		}
		findings = append(findings, classifyEngineFinding(storeRel, "", f))
	}

	// Collection-level findings
	for _, cr := range rep.Collections {
		for _, f := range cr.Findings {
			if f.Severity == engine.SeverityInfo {
				continue
			}
			findings = append(findings, classifyEngineFinding(storeRel, cr.Name, f))
		}
	}

	return findings, nil
}

func classifyEngineFinding(storeRel, colName string, f engine.Finding) Finding {
	target := storeRel
	if colName != "" {
		target = storeRel + "/" + colName
	}
	repairCmd := RepairCommandFor(storeRel)

	switch f.Severity {
	case engine.SeverityRepairableIndex:
		return Finding{
			Severity: SeverityAuto,
			Store:    target,
			Message:  fmt.Sprintf("%s: %s", target, f.Message),
			Details:  string(f.Code),
		}
	case engine.SeverityConflict:
		loc := ""
		if f.Location.ID > 0 {
			loc = fmt.Sprintf(" (id: %d)", f.Location.ID)
		} else if f.Location.Segment != "" {
			loc = fmt.Sprintf(" (%s)", f.Location.Segment)
		}
		return Finding{
			Severity: SeverityRepairable,
			Store:    target,
			Message:  fmt.Sprintf("%s: %s", target, f.Message),
			Details:  fmt.Sprintf("code: %s%s", f.Code, loc),
			Command:  repairCmd,
		}
	case engine.SeverityDataCorruption:
		return Finding{
			Severity: SeverityBlocking,
			Store:    target,
			Message:  fmt.Sprintf("%s: %s", target, f.Message),
			Details:  string(f.Code),
			Command:  repairCmd,
		}
	default:
		// Fallback for any unknown non-info severity
		return Finding{
			Severity: SeverityBlocking,
			Store:    target,
			Message:  fmt.Sprintf("%s: %s", target, f.Message),
			Details:  string(f.Code),
			Command:  repairCmd,
		}
	}
}
