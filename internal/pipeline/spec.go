package pipeline

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ParseSpec decodes a pipeline YAML spec, applies defaults (worktree=none,
// type=development, all statuses pending), sets ID=Name, and validates the DAG.
func ParseSpec(data []byte) (*Pipeline, error) {
	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pipeline yaml: %w", err)
	}
	p.ID = p.Name
	p.Status = StatusPending
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if j.Worktree == "" {
			j.Worktree = "none"
		}
		if j.Type == "" {
			j.Type = "development"
		}
		if j.RunIf == "" {
			j.RunIf = "success"
		}
		j.Status = JobPending
	}
	if err := Validate(&p); err != nil {
		return nil, err
	}
	return &p, nil
}
