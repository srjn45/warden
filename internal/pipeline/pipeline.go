// Package pipeline models a DAG of agent jobs and the pure logic that drives it:
// validation, reconcile planning, prompt composition, YAML parsing, and a
// file-backed store. All decision logic is side-effect-free; the daemon's
// Executor performs the actual spawns.
package pipeline

import (
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/store"
)

type Status string

const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusPaused   Status = "paused"
	StatusDone     Status = "done"
	StatusStalled  Status = "stalled"
	StatusCanceled Status = "canceled"
)

type JobStatus string

const (
	JobPending        JobStatus = "pending"
	JobRunning        JobStatus = "running"
	JobDone           JobStatus = "done"
	JobFailed         JobStatus = "failed"
	JobSkipped        JobStatus = "skipped"
	JobNeedsAttention JobStatus = "needs_attention"
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
	RunIf      string   `json:"run_if,omitempty" yaml:"run_if,omitempty"` // success (default) | failure | always
	Role       string   `json:"role,omitempty" yaml:"role,omitempty"`
	Tier       string   `json:"tier,omitempty" yaml:"tier,omitempty"`
	Backend    string   `json:"backend,omitempty" yaml:"backend,omitempty"`
	Model      string   `json:"model,omitempty" yaml:"model,omitempty"`

	SessionID string         `json:"session_id,omitempty" yaml:"-"`
	Status    JobStatus      `json:"status,omitempty" yaml:"-"`
	Output    string         `json:"output,omitempty" yaml:"-"`
	Branch    string         `json:"branch,omitempty" yaml:"-"`
	Workdir   string         `json:"workdir,omitempty" yaml:"-"`
	System    bool           `json:"system,omitempty" yaml:"-"`
	Digest    *digest.Digest `json:"digest,omitempty" yaml:"-"` // completion snapshot (nil until reaped)
}

type Pipeline struct {
	ID     string `json:"id" yaml:"-"` // == Name; stable key
	Name   string `json:"name" yaml:"name"`
	Repo   string `json:"repo" yaml:"repo"`
	Status Status `json:"status" yaml:"-"`
	Jobs   []Job  `json:"jobs" yaml:"jobs"`
	// Tags are stamped onto every job agent this pipeline spawns. Not spec-
	// authored (yaml:"-"): the daemon sets them at creation time — today to
	// inherit autopilot's ownership tags from the creating agent, so a pipeline a
	// manager escalates to stays inside its run's fence.
	Tags []string `json:"tags,omitempty" yaml:"-"`
	// ScheduleID/ScheduleName are set (yaml:"-", daemon-stamped at creation) when a
	// schedule fired this pipeline, so every job session it spawns can back-ref the
	// origin schedule via Session.ScheduleID — the pipeline-mode analogue of the
	// agent-mode fire tagging.
	ScheduleID   string `json:"schedule_id,omitempty" yaml:"-"`
	ScheduleName string `json:"schedule_name,omitempty" yaml:"-"`
	// ProjectID back-refs the first-class project (projectstore) this pipeline
	// belongs to; empty = ungrouped. Daemon-stamped at creation (yaml:"-", not
	// spec-authored), the pipeline-mode analogue of Session.ProjectID: it groups a
	// pipeline and all its job agents under one parent project in the cockpit/TUI.
	ProjectID string `json:"project_id,omitempty" yaml:"-"`
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

// HasLiveJobs reports whether any job is still running or awaiting input.
func (p *Pipeline) HasLiveJobs() bool {
	for i := range p.Jobs {
		if p.Jobs[i].Status == JobRunning || p.Jobs[i].Status == JobNeedsAttention {
			return true
		}
	}
	return false
}

// IsCancelable reports whether a pipeline can still be canceled. A finished
// pipeline (done/canceled) cannot — it can only be deleted. A stalled pipeline
// (a job failed) is still cancelable only while it has live jobs left to
// terminate on parallel branches; once nothing is live it is effectively
// finished and only deletion remains.
func (p *Pipeline) IsCancelable() bool {
	switch p.Status {
	case StatusDone, StatusCanceled:
		return false
	case StatusStalled:
		return p.HasLiveJobs()
	default: // pending | running | paused
		return true
	}
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
		if strings.TrimSpace(j.Prompt) == "" && !j.System {
			return fmt.Errorf("job %q: prompt is required", j.ID)
		}
		switch mode, _ := ParseWorktree(j.Worktree); mode {
		case "none", "fresh", "from", "pipeline":
		default:
			return fmt.Errorf("job %q: invalid worktree %q (want none|fresh|from:<job>)", j.ID, j.Worktree)
		}
		switch j.RunIf {
		case "", "success", "failure", "always":
		default:
			return fmt.Errorf("job %q: invalid run_if %q (want success|failure|always)", j.ID, j.RunIf)
		}
		if j.Tier != "" && !backendstore.ModelTier(j.Tier).Valid() {
			return fmt.Errorf("job %q: invalid tier %q (want tier-1|tier-2|tier-3)", j.ID, j.Tier)
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
