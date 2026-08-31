// Package autopilot implements the master switch for warden's autopilot mode:
// the Controller (per-plan run registry + enable/disable state machine), the
// enable-time preflight, and the plan-file schema. This is the S1 "inert" core —
// the switch exists end-to-end on every surface but spawns no brain yet (that is
// S3). See docs/specs/autopilot.md (§2, §3, §5, §5.1, §10) for the contracts.
package autopilot

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// planSchemaVersion is the only plan-file schema version S1 understands.
const planSchemaVersion = 1

// PlanStatusComplete is the only special Plan.Status value (autopilot.md §2.1):
// a run the brain has declared done. Preflight SKIPS a complete plan — it is not
// registered as an active run — while an empty/absent (or any other) status means
// active. It is the in-place completion marker CompleteRun writes.
const PlanStatusComplete = "complete"

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

	// Status records the run's lifecycle in the plan file itself (autopilot.md
	// §2.1). Empty/absent ⇒ active; "complete" ⇒ the run finished and preflight
	// skips it. Declared so a completed plan STILL strict-decodes (the KnownFields
	// decoder would otherwise reject the `status` key). Any non-"complete" value is
	// treated as active, so a hand-typed status never blocks a plan from loading.
	Status string `yaml:"status,omitempty"`
	// CompletedAt is the RFC3339 instant CompleteRun stamped alongside Status. Purely
	// informational (owner-facing); it never gates loading or validation.
	CompletedAt string `yaml:"completed_at,omitempty"`
}

// IsComplete reports whether the plan carries the completion marker (autopilot.md
// §2.1). A complete plan strict-decodes normally; preflight simply does not
// register it as an active run.
func (p Plan) IsComplete() bool {
	return strings.EqualFold(strings.TrimSpace(p.Status), PlanStatusComplete)
}

// PlanTask is one coarse task in a plan. Dependency edges (After) reference other
// task ids within the same plan.
type PlanTask struct {
	ID       string   `yaml:"id"`
	Prompt   string   `yaml:"prompt"`
	After    []string `yaml:"after"`
	Status   string   `yaml:"status,omitempty"`
	LandedPR int      `yaml:"landed_pr,omitempty"`
}

const (
	TaskStatusPending = "pending"
	TaskStatusActive  = "active"
	TaskStatusDone    = "done"
	TaskStatusFailed  = "failed"
)

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
		status := strings.ToLower(strings.TrimSpace(t.Status))
		if status == "" {
			p.Tasks[i].Status = TaskStatusPending
			status = TaskStatusPending
		}
		switch status {
		case TaskStatusPending, TaskStatusActive, TaskStatusDone, TaskStatusFailed:
			p.Tasks[i].Status = status
		default:
			return fmt.Errorf("plan: task %q has invalid status %q", id, t.Status)
		}
		if status == TaskStatusDone && t.LandedPR <= 0 {
			return fmt.Errorf("plan: task %q status done requires landed_pr", id)
		}
		if status != TaskStatusDone && t.LandedPR != 0 {
			return fmt.Errorf("plan: task %q landed_pr is only valid with status done", id)
		}
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

// writeTaskStatusAtomic updates one task node while preserving the owner's YAML
// comments and ordering, then atomically replaces the plan file.
func writeTaskStatusAtomic(path, taskID, status string, landedPR int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("plan: read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("plan: decode %s: %w", path, err)
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	var tasks *yaml.Node
	for i := 0; root.Kind == yaml.MappingNode && i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "tasks" {
			tasks = root.Content[i+1]
			break
		}
	}
	if tasks == nil || tasks.Kind != yaml.SequenceNode {
		return fmt.Errorf("plan: task %q not found", taskID)
	}
	found := false
	for _, task := range tasks.Content {
		if task.Kind != yaml.MappingNode {
			continue
		}
		id := ""
		for i := 0; i+1 < len(task.Content); i += 2 {
			if task.Content[i].Value == "id" {
				id = task.Content[i+1].Value
			}
		}
		if id != taskID {
			continue
		}
		found = true
		upsertMapScalar(task, "status", status)
		if landedPR > 0 {
			upsertMapScalar(task, "landed_pr", fmt.Sprint(landedPR))
			taskValue(task, "landed_pr").Tag = "!!int"
		} else {
			removeMapKey(task, "landed_pr")
		}
		break
	}
	if !found {
		return fmt.Errorf("plan: task %q not found", taskID)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("plan: encode %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, e := os.Stat(path); e == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".warden-plan-*")
	if err != nil {
		return fmt.Errorf("plan: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(buf.Bytes())
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("plan: write temporary file: %w", err)
	}
	// Refuse to overwrite owner steering that raced this read/modify/write.
	current, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("plan: re-read %s before replace: %w", path, err)
	}
	if sha256.Sum256(current) != sha256.Sum256(data) {
		return fmt.Errorf("plan: concurrent edit detected; retry task status update")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("plan: replace %s: %w", path, err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("plan: open parent directory: %w", err)
	}
	if err = dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("plan: sync parent directory: %w", err)
	}
	if err = dir.Close(); err != nil {
		return fmt.Errorf("plan: close parent directory: %w", err)
	}
	return nil
}

func taskValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
func removeMapKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// markPlanCompleteInPlace rewrites the plan file at path, adding (or updating)
// `status: complete` and `completed_at: <completedAt>` while preserving every
// other key, its ordering, and its comments (autopilot.md §2.1). It round-trips a
// *yaml.Node — decode → upsert the two mapping keys → re-encode — rather than
// re-marshaling a Plan struct, precisely so an owner's inline comments and field
// order survive the marker. completedAt is an RFC3339 timestamp the caller
// supplies (the Controller owns the clock).
func markPlanCompleteInPlace(path, completedAt string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("plan: read %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("plan: decode %s: %w", path, err)
	}
	// Decoding into a yaml.Node yields a DocumentNode wrapping the top-level
	// mapping; tolerate a bare mapping too (defensive) so the upsert always targets
	// the key/value node list.
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return fmt.Errorf("plan: %s is empty", path)
		}
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("plan: %s top level is not a YAML mapping", path)
	}
	upsertMapScalar(root, "status", PlanStatusComplete)
	upsertMapScalar(root, "completed_at", completedAt)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2) // match the plan template's 2-space style
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("plan: encode %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("plan: encode %s: %w", path, err)
	}
	// Preserve the file's existing permission bits (fall back to 0644).
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, buf.Bytes(), mode); err != nil {
		return fmt.Errorf("plan: write %s: %w", path, err)
	}
	return nil
}

// upsertMapScalar sets key to the string scalar value in a YAML mapping node,
// updating the existing value in place when key is already present (preserving its
// position and any surrounding comments) and appending a new key/value pair
// otherwise. A mapping node's Content is a flat list of alternating key/value
// nodes.
func upsertMapScalar(mapping *yaml.Node, key, value string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			v := mapping.Content[i+1]
			v.Kind = yaml.ScalarNode
			v.Tag = "!!str"
			v.Value = value
			v.Style = 0
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}
