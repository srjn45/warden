package planstore

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scan helpers below are a legacy migration boundary: they walk repository
// plans/**/*.yaml to upsert ScrivaDB records. They must not be invoked as
// authority for normal PlanService create/read/list/update/transition paths
// (docs/specs/2026-09-30-scrivadb-canonical-plans.md §6.1 / §6.5).

// PlanTaskDef is a single task entry read from a plan YAML file.
type PlanTaskDef struct {
	ID       string   `yaml:"id"`
	Prompt   string   `yaml:"prompt"`
	After    []string `yaml:"after"`
	Status   string   `yaml:"status"`
	LandedPR int      `yaml:"landed_pr"`
}

// ReadPlanTasks parses the tasks: array from a plan YAML file.
// Returns tasks in declaration order. Returns nil and an error on I/O or YAML failures.
func ReadPlanTasks(absPath string) ([]PlanTaskDef, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Tasks []PlanTaskDef `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc.Tasks, nil
}

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
		discovered := discoverPlanBranches(ctx, rootDir, name, id)
		if existing == nil {
			// Create new record.
			p := &Plan{
				ID:        id,
				ProjectID: projectID,
				Name:      name,
				FilePath:  c.filePath,
				Status:    c.status,
				Branches:  discovered,
			}
			if createErr := s.Create(ctx, p); createErr != nil && createErr != ErrExists {
				return upserted, createErr
			}
			upserted++
		} else {
			merged := mergeBranches(existing.Branches, discovered)
			needUpdate := existing.FilePath != c.filePath || existing.Status != c.status || !equalStrings(existing.Branches, merged)
			if !needUpdate {
				continue
			}
			// Update FilePath and Status; merge discovered branches.
			// Never touch execution links or TaskProgress.
			if updateErr := s.Update(ctx, id, func(p *Plan) error {
				p.FilePath = c.filePath
				p.Status = c.status
				p.Branches = merged
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

// discoverPlanBranches lists local git branches that appear to belong to a plan.
// Best-effort: a missing git repo or git failure yields a nil list.
func discoverPlanBranches(ctx context.Context, root, planName, planID string) []string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "for-each-ref", "--format=%(refname:short)", "refs/heads/").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if b := strings.TrimSpace(line); b != "" {
			names = append(names, b)
		}
	}
	return matchPlanBranches(planName, planID, names)
}

// matchPlanBranches returns local branch names that look associated with a plan.
// Integration branches (autopilot/*) and trunk (main/master) are excluded.
func matchPlanBranches(planName, planID string, branches []string) []string {
	slug := strings.ToLower(planSlug(planName))
	id := strings.ToLower(strings.TrimSpace(planID))
	if slug == "" && id == "" {
		return nil
	}
	var matches []string
	for _, b := range branches {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		lb := strings.ToLower(b)
		if lb == "main" || lb == "master" || strings.HasPrefix(lb, "autopilot/") {
			continue
		}
		if (slug != "" && (lb == slug || strings.Contains(lb, slug))) ||
			(id != "" && strings.Contains(lb, id)) {
			matches = append(matches, b)
		}
	}
	return matches
}

func mergeBranches(existing, discovered []string) []string {
	if len(existing) == 0 && len(discovered) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing)+len(discovered))
	out := make([]string, 0, len(existing)+len(discovered))
	for _, b := range existing {
		if b == "" {
			continue
		}
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = struct{}{}
		out = append(out, b)
	}
	for _, b := range discovered {
		if b == "" {
			continue
		}
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = struct{}{}
		out = append(out, b)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
