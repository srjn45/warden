package autopilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MigrateLegacyPlans copies legacy config-listed plans into each repository's
// plans directory and registers the copy. Sources are deliberately retained so
// older warden versions and config remain usable during the deprecation window.
// Repeated calls are safe: identical destinations and registrations are no-ops.
func MigrateLegacyPlans(ctx context.Context, env Env, c *Controller, configured []string, baseDir string, out io.Writer) ([]string, error) {
	var errs []string
	effective := make([]string, 0, len(configured)+1)
	rootLegacy := filepath.Join(baseDir, "autopilot.plan.yaml")
	if _, err := os.Stat(rootLegacy); err == nil && !containsPlan(configured, rootLegacy, baseDir) {
		configured = append(configured, rootLegacy)
	}
	for _, configuredPath := range configured {
		src := strings.TrimSpace(configuredPath)
		if src == "" {
			continue
		}
		if !filepath.IsAbs(src) {
			src = filepath.Join(baseDir, src)
		}
		src = canonicalPath(src)
		repo, err := env.GitToplevel(ctx, filepath.Dir(src))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: resolve repository: %v", configuredPath, err))
			effective = append(effective, src)
			continue
		}
		repo = canonicalPath(repo)
		planPath := src
		plansDir := filepath.Join(repo, "plans")
		if rel, err := filepath.Rel(plansDir, src); err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			name := legacyPlanName(src)
			planPath = canonicalPath(filepath.Join(plansDir, name+".yaml"))
			if err := copyPlanIdempotent(src, planPath); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", configuredPath, err))
				effective = append(effective, src)
				continue
			}
			fmt.Fprintf(out, "warning: migrated deprecated autopilot plan %s to %s; register plans directly and remove autopilot.plans[] from config\n", src, planPath)
		}
		if err := c.relocateStoredRun(repo, src, planPath); err != nil {
			errs = append(errs, fmt.Sprintf("%s: relocate stored run: %v", configuredPath, err))
			effective = append(effective, src)
			continue
		}
		if _, err := c.Register(ctx, RegisterRequest{Name: defaultRunName(planPath), Repo: repo, PlanFile: planPath}); err != nil {
			errs = append(errs, fmt.Sprintf("%s: register: %v", configuredPath, err))
			effective = append(effective, src)
			continue
		}
		effective = append(effective, planPath)
	}
	if len(errs) > 0 {
		return effective, fmt.Errorf("legacy autopilot plan migration: %s", strings.Join(errs, "; "))
	}
	return effective, nil
}

// relocateStoredRun preserves a Phase 1 durable record created for the legacy
// source path while changing its identity to the copied named plan path.
func (c *Controller) relocateStoredRun(repo, src, dst string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	oldID := RunID(repo, src)
	r, ok := c.runs[oldID]
	if !ok || src == dst {
		return nil
	}
	newID := RunID(repo, dst)
	if _, exists := c.runs[newID]; exists {
		return nil
	}
	plan, err := LoadPlan(dst)
	if err != nil {
		return err
	}
	delete(c.runs, oldID)
	r.runID, r.name, r.planFile, r.absPlanFile, r.plan = newID, defaultRunName(dst), dst, dst, plan
	c.runs[newID] = r
	if c.store == nil {
		return nil
	}
	rec := c.recordLocked(r)
	if old, err := c.store.Get(oldID); err == nil {
		rec.CreatedAt = old.CreatedAt
	}
	if err := c.store.Create(rec); err != nil && !errors.Is(err, ErrRunExists) {
		return err
	}
	return c.store.Delete(oldID)
}

func containsPlan(plans []string, target, baseDir string) bool {
	for _, plan := range plans {
		if !filepath.IsAbs(plan) {
			plan = filepath.Join(baseDir, plan)
		}
		if canonicalPath(plan) == canonicalPath(target) {
			return true
		}
	}
	return false
}

func legacyPlanName(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if base == "autopilot.plan" {
		return "default"
	}
	return base
}

func copyPlanIdempotent(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read legacy plan: %w", err)
	}
	if existing, err := os.ReadFile(dst); err == nil {
		if bytes.Equal(existing, data) {
			return nil
		}
		return fmt.Errorf("migration destination %s already exists with different contents", dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
