package planstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Legacy YAML helpers — migration / export boundary only.
//
// These functions parse or mutate repository plan YAML under plans/**. They
// must NOT be called from the normal PlanService create/read/list/update/
// transition/finalize path (see docs/specs/2026-09-30-scrivadb-canonical-plans.md
// §6.5). Callers: explicit legacy import (Phase 5), sync_to_repo export
// (Phase 6/7), and golden/fixture tests.

// LegacyPlanDocument is the v1 repository plan YAML body (pre-export-envelope).
type LegacyPlanDocument struct {
	Version     int              `yaml:"version"`
	Name        string           `yaml:"name"`
	Goal        string           `yaml:"goal"`
	Constraints []string         `yaml:"constraints,omitempty"`
	Tasks       []LegacyPlanTask `yaml:"tasks"`
	DoneWhen    []string         `yaml:"done_when,omitempty"`
}

// LegacyPlanTask is one task entry in a legacy plan YAML document.
type LegacyPlanTask struct {
	ID     string   `yaml:"id"`
	Prompt string   `yaml:"prompt"`
	After  []string `yaml:"after,omitempty"`
}

// LegacyMarshalPlanYAML encodes a legacy plan document.
func LegacyMarshalPlanYAML(doc LegacyPlanDocument) ([]byte, error) {
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode plan yaml: %w", err)
	}
	return b, nil
}

// LegacyLoadPlanDocument reads and parses a plan YAML file at abs.
func LegacyLoadPlanDocument(abs string) (LegacyPlanDocument, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return LegacyPlanDocument{}, err
	}
	var doc LegacyPlanDocument
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return LegacyPlanDocument{}, err
	}
	return doc, nil
}

// LegacyWritePlanYAMLAtomic writes doc to abs via temp file + rename.
func LegacyWritePlanYAMLAtomic(abs string, doc LegacyPlanDocument) error {
	body, err := LegacyMarshalPlanYAML(doc)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, filepath.Base(abs)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp plan file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck // best-effort cleanup if rename does not happen
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp plan file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp plan file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp plan file: %w", err)
	}
	if err := os.Rename(tmpPath, abs); err != nil {
		return fmt.Errorf("overwrite plan file: %w", err)
	}
	return nil
}

// LegacyMovePlanFile renames a replica YAML into plans/<status>/<filename>.
// Export/migration only — never call from PlanService lifecycle transitions.
func LegacyMovePlanFile(p *Plan, root string, to PlanStatus) (string, error) {
	filename := filepath.Base(p.FilePath)
	if filename == "." || filename == string(filepath.Separator) || filename == "" {
		filename = planFilename(p.Name)
	}
	newRel := filepath.Join("plans", string(to), filename)
	if p.FilePath == newRel {
		return newRel, nil
	}
	src := filepath.Join(root, p.FilePath)
	dst := filepath.Join(root, newRel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("move plan file to %s: %w", newRel, err)
	}
	return newRel, nil
}

// LegacyTaskSpecsToDocTasks converts TaskSpecs into legacy YAML task nodes.
func LegacyTaskSpecsToDocTasks(tasks []TaskSpec) []LegacyPlanTask {
	out := make([]LegacyPlanTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, LegacyPlanTask{
			ID:     strings.TrimSpace(t.ID),
			Prompt: t.Prompt,
			After:  t.After,
		})
	}
	return out
}

// LegacyReadPlanTasksFromFile loads YAML tasks for a plan via FilePath.
// Migration/export boundary only — normal completion uses Plan.Tasks.
func LegacyReadPlanTasksFromFile(p *Plan, projectRoot string) ([]PlanTaskDef, error) {
	if p == nil || p.FilePath == "" {
		return nil, nil
	}
	var abs string
	if filepath.IsAbs(p.FilePath) {
		abs = p.FilePath
	} else if projectRoot != "" {
		abs = filepath.Join(projectRoot, p.FilePath)
	} else {
		var err error
		abs, err = filepath.Abs(p.FilePath)
		if err != nil {
			return nil, err
		}
	}
	return ReadPlanTasks(abs)
}
