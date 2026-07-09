// Package autopilot implements the master switch for warden's autopilot mode:
// the Controller (per-plan run registry + enable/disable state machine), the
// enable-time preflight, and the plan-file schema. This is the S1 "inert" core —
// the switch exists end-to-end on every surface but spawns no brain yet (that is
// S3). See docs/specs/autopilot.md (§2, §3, §5, §5.1, §10) for the contracts.
package autopilot

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// planSchemaVersion is the only plan-file schema version S1 understands.
const planSchemaVersion = 1

// Plan is the owner-authored brief the brain executes (autopilot.md §3). It is
// the source of truth for a run: a goal, optional constraints injected verbatim,
// optional coarse tasks (the brain authors/refines them when absent), and
// optional acceptance criteria the brain verifies before declaring the run
// complete. Unknown fields are rejected at decode so a steering typo surfaces at
// enable time rather than stalling a run days later.
type Plan struct {
	Version     int        `yaml:"version"`
	Goal        string     `yaml:"goal"`
	Constraints []string   `yaml:"constraints"`
	Tasks       []PlanTask `yaml:"tasks"`
	DoneWhen    []string   `yaml:"done_when"`
}

// PlanTask is one coarse task in a plan. Dependency edges (After) reference other
// task ids within the same plan.
type PlanTask struct {
	ID     string   `yaml:"id"`
	Prompt string   `yaml:"prompt"`
	After  []string `yaml:"after"`
}

// DecodePlan strict-decodes a v1 plan document (autopilot.md §3). Unknown fields
// are an error (typos surface now). It then validates the semantic shape: a
// non-empty goal, a recognized version, unique task ids, and dependency edges
// that reference tasks that exist.
func DecodePlan(data []byte) (Plan, error) {
	var p Plan
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // reject unknown fields — a steering typo must not slip through
	if err := dec.Decode(&p); err != nil {
		return Plan{}, fmt.Errorf("plan: decode: %w", err)
	}
	if err := p.validate(); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// LoadPlan reads and strict-decodes the plan file at path. A missing/unreadable
// file is a distinct, actionable error (it drives a preflight failure line).
func LoadPlan(path string) (Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Plan{}, fmt.Errorf("plan file not found: %s", path)
		}
		return Plan{}, fmt.Errorf("plan: read %s: %w", path, err)
	}
	p, err := DecodePlan(data)
	if err != nil {
		return Plan{}, fmt.Errorf("plan %s: %w", path, err)
	}
	return p, nil
}

// validate enforces the v1 semantic rules. An absent version defaults to 1 (the
// only version), so a plan omitting it still loads; any other value is rejected.
func (p *Plan) validate() error {
	if p.Version == 0 {
		p.Version = planSchemaVersion
	}
	if p.Version != planSchemaVersion {
		return fmt.Errorf("plan: unsupported version %d (want %d)", p.Version, planSchemaVersion)
	}
	if strings.TrimSpace(p.Goal) == "" {
		return fmt.Errorf("plan: goal is required")
	}
	ids := map[string]bool{}
	for i, t := range p.Tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" {
			return fmt.Errorf("plan: task[%d] has an empty id", i)
		}
		if ids[id] {
			return fmt.Errorf("plan: duplicate task id %q", id)
		}
		ids[id] = true
	}
	// Edges are checked in a second pass so forward references (a task depending
	// on one declared later in the list) are legal.
	var unknown []string
	for _, t := range p.Tasks {
		for _, dep := range t.After {
			if !ids[strings.TrimSpace(dep)] {
				unknown = append(unknown, fmt.Sprintf("%s→%s", t.ID, dep))
			}
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("plan: task dependency references unknown id(s): %s", strings.Join(unknown, ", "))
	}
	return nil
}
