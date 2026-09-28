package planstore

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// statusDirs is the ordered set of subdirectories under plans/ whose name
// encodes plan status. Files in any other directory are ignored.
var statusDirs = []struct {
	dir    string
	status PlanStatus
}{
	{"pending", PlanStatusPending},
	{"in_progress", PlanStatusInProgress},
	{"completed", PlanStatusCompleted},
	{"archived", PlanStatusArchived},
}

// ScanProject walks <rootDir>/plans/ for plan YAML files and upserts each
// found plan into store. It returns the number of plans upserted (created or
// updated) and any scan error.
//
// Status is inferred from the subdirectory name. Flat plans/*.yaml files are
// treated as pending. For each plan:
//   - If no record exists → create with the inferred status.
//   - If a record exists → update FilePath and Status only; execution links
//     (AutopilotRunID, PipelineID, OrchestratorID) and TaskProgress are never
//     overwritten.
//
// ScanProject is idempotent: a second call on an unchanged tree produces no
// net change.
func ScanProject(ctx context.Context, s *Store, projectID, rootDir string) (int, error) {
	plansDir := filepath.Join(rootDir, "plans")
	if _, err := os.Stat(plansDir); os.IsNotExist(err) {
		return 0, nil
	}

	type planCandidate struct {
		filePath string // relative to rootDir
		status   PlanStatus
	}
	var candidates []planCandidate

	// Walk subdirectories that encode status.
	for _, sd := range statusDirs {
		dir := filepath.Join(plansDir, sd.dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		for _, e := range entries {
			if e.IsDir() || !isYAML(e.Name()) {
				continue
			}
			rel := filepath.Join("plans", sd.dir, e.Name())
			candidates = append(candidates, planCandidate{filePath: rel, status: sd.status})
		}
	}

	// Walk flat plans/*.yaml files (treated as pending).
	flatEntries, err := os.ReadDir(plansDir)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	for _, e := range flatEntries {
		if e.IsDir() || !isYAML(e.Name()) {
			continue
		}
		rel := filepath.Join("plans", e.Name())
		candidates = append(candidates, planCandidate{filePath: rel, status: PlanStatusPending})
	}

	upserted := 0
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return upserted, err
		}
		absPath := filepath.Join(rootDir, c.filePath)
		name := readPlanName(absPath)
		if name == "" {
			slog.Debug("planstore: skipping plan file with no name", "path", absPath)
			continue
		}
		id := PlanID(projectID, name)
		existing, err := s.Get(ctx, id)
		if err != nil && err != ErrNotFound {
			return upserted, err
		}
		if existing == nil {
			// Create new record.
			p := &Plan{
				ID:        id,
				ProjectID: projectID,
				Name:      name,
				FilePath:  c.filePath,
				Status:    c.status,
			}
			if createErr := s.Create(ctx, p); createErr != nil && createErr != ErrExists {
				return upserted, createErr
			}
			upserted++
		} else if existing.FilePath != c.filePath || existing.Status != c.status {
			// Update FilePath and Status only — never touch execution links or TaskProgress.
			if updateErr := s.Update(ctx, id, func(p *Plan) error {
				p.FilePath = c.filePath
				p.Status = c.status
				return nil
			}); updateErr != nil {
				return upserted, updateErr
			}
			upserted++
		}
	}
	return upserted, nil
}

// readPlanName reads just the `name:` field from a YAML file without a full
// parse. It scans lines until it finds one starting with "name:" at the top
// level (no leading spaces) and returns the trimmed value. Returns the
// filename stem if no name: field is found.
func readPlanName(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "name:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		// Strip optional YAML inline comment.
		if idx := strings.Index(val, " #"); idx >= 0 {
			val = strings.TrimSpace(val[:idx])
		}
		// Strip surrounding quotes.
		val = strings.Trim(val, `"'`)
		if val != "" {
			return val
		}
	}
	_ = sc.Err() // scan errors fall through to the filename-stem fallback
	// Fall back to filename stem.
	return stemName(filepath.Base(path))
}

// stemName returns the filename without extension.
func stemName(filename string) string {
	ext := filepath.Ext(filename)
	if ext != "" {
		return strings.TrimSuffix(filename, ext)
	}
	return filename
}

func isYAML(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".yaml" || ext == ".yml"
}
