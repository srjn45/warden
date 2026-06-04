// Package pipeline models a DAG of agent jobs and the pure logic that drives it:
// validation, reconcile planning, prompt composition, YAML parsing, and a
// file-backed store. All decision logic is side-effect-free; the daemon's
// Executor performs the actual spawns.
package pipeline

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/store"
)

type Status string

const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusStalled  Status = "stalled"
	StatusCanceled Status = "canceled"
)

type JobStatus string

const (
	JobPending JobStatus = "pending"
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobFailed  JobStatus = "failed"
	JobSkipped JobStatus = "skipped"
)

// Job is one node in the DAG. The first block is author-supplied; the second is
// filled at runtime by the executor and emit.
type Job struct {
	ID         string   `json:"id" yaml:"id"`
	Prompt     string   `json:"prompt" yaml:"prompt"`
	DependsOn  []string `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
	Handoff    string   `json:"handoff,omitempty" yaml:"handoff,omitempty"`
	Worktree   string   `json:"worktree" yaml:"worktree"` // none | fresh | from:<jobid>
	Supervised bool     `json:"supervised,omitempty" yaml:"supervised,omitempty"`
	Type       string   `json:"type,omitempty" yaml:"type,omitempty"`

	SessionID string    `json:"session_id,omitempty" yaml:"-"`
	Status    JobStatus `json:"status,omitempty" yaml:"-"`
	Output    string    `json:"output,omitempty" yaml:"-"`
	Branch    string    `json:"branch,omitempty" yaml:"-"`
}

type Pipeline struct {
	ID     string `json:"id" yaml:"-"` // == Name; stable key
	Name   string `json:"name" yaml:"name"`
	Repo   string `json:"repo" yaml:"repo"`
	Status Status `json:"status" yaml:"-"`
	Jobs   []Job  `json:"jobs" yaml:"jobs"`
}

// Job returns a pointer to the job with id, or nil.
func (p *Pipeline) Job(id string) *Job {
	for i := range p.Jobs {
		if p.Jobs[i].ID == id {
			return &p.Jobs[i]
		}
	}
	return nil
}

// ParseWorktree splits a worktree spec into (mode, fromJob). "from:impl" ->
// ("from","impl"); "fresh"/"none" -> (mode, "").
func ParseWorktree(s string) (mode, fromJob string) {
	if strings.HasPrefix(s, "from:") {
		return "from", strings.TrimPrefix(s, "from:")
	}
	return s, ""
}

// Validate checks the DAG is well-formed: safe unique ids, non-empty prompts,
// known dependency + from-ref targets, valid worktree modes, and no cycles.
func Validate(p *Pipeline) error {
	if err := store.SafeID(p.Name); err != nil {
		return fmt.Errorf("invalid pipeline name %q: must have no '/', '\\', ':', or '..'", p.Name)
	}
	if p.Repo == "" {
		return fmt.Errorf("pipeline repo is required")
	}
	if len(p.Jobs) == 0 {
		return fmt.Errorf("pipeline has no jobs")
	}
	ids := map[string]bool{}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if err := store.SafeID(j.ID); err != nil {
			return fmt.Errorf("invalid job id %q: must have no '/', '\\', ':', or '..'", j.ID)
		}
		if ids[j.ID] {
			return fmt.Errorf("duplicate job id %q", j.ID)
		}
		ids[j.ID] = true
		if strings.TrimSpace(j.Prompt) == "" {
			return fmt.Errorf("job %q: prompt is required", j.ID)
		}
		switch mode, _ := ParseWorktree(j.Worktree); mode {
		case "none", "fresh", "from":
		default:
			return fmt.Errorf("job %q: invalid worktree %q (want none|fresh|from:<job>)", j.ID, j.Worktree)
		}
	}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		for _, dep := range j.DependsOn {
			if dep == j.ID {
				return fmt.Errorf("job %q depends on itself", j.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("job %q depends on unknown job %q", j.ID, dep)
			}
		}
		if mode, from := ParseWorktree(j.Worktree); mode == "from" {
			if !ids[from] {
				return fmt.Errorf("job %q worktree references unknown job %q", j.ID, from)
			}
		}
	}
	return detectCycle(p)
}

// detectCycle reports a dependency cycle via DFS over the depends_on edges.
func detectCycle(p *Pipeline) error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		color[id] = gray
		j := p.Job(id)
		for _, dep := range j.DependsOn {
			switch color[dep] {
			case gray:
				return fmt.Errorf("dependency cycle through job %q", dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}
	for i := range p.Jobs {
		if color[p.Jobs[i].ID] == white {
			if err := visit(p.Jobs[i].ID); err != nil {
				return err
			}
		}
	}
	return nil
}
