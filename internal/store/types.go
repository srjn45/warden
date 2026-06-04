package store

import "time"

type Status string

const (
	StatusSpawning        Status = "spawning"
	StatusWorking         Status = "working"
	StatusWaitingForInput Status = "waiting_for_input"
	StatusIdle            Status = "idle"
	StatusDone            Status = "done"
	StatusErrored         Status = "errored"
	StatusOrphaned        Status = "orphaned"
)

func (s Status) Valid() bool {
	switch s {
	case StatusSpawning, StatusWorking, StatusWaitingForInput,
		StatusIdle, StatusDone, StatusErrored, StatusOrphaned:
		return true
	}
	return false
}

// Type is the kind of work an agent session is doing (design §2).
type Type string

const (
	TypeDevelopment    Type = "development"
	TypeAnalysis       Type = "analysis"
	TypeSpike          Type = "spike"
	TypePRReview       Type = "pr-review"
	TypeBuildkiteDebug Type = "buildkite-debug"
	TypeTestRun        Type = "test-run"
	TypeEnvTest       Type = "env-test"
	TypeOther          Type = "other"
)

// Valid reports whether t is one of the known task types.
func (t Type) Valid() bool {
	switch t {
	case TypeDevelopment, TypeAnalysis, TypeSpike, TypePRReview,
		TypeBuildkiteDebug, TypeTestRun, TypeEnvTest, TypeOther:
		return true
	}
	return false
}

// NormalizeType maps any input to a known Type, collapsing unknowns to "other".
func NormalizeType(s string) Type {
	t := Type(s)
	switch t {
	case TypeDevelopment, TypeAnalysis, TypeSpike, TypePRReview,
		TypeBuildkiteDebug, TypeTestRun, TypeEnvTest, TypeOther:
		return t
	}
	return TypeOther
}

// DefaultWorktree reports whether this type creates a git worktree by default.
// analysis/spike are opt-in (via --worktree) so they return false here.
func (t Type) DefaultWorktree() bool {
	switch t {
	case TypeDevelopment, TypePRReview:
		return true
	}
	return false
}

type Event struct {
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`
	Detail string    `json:"detail"`
}

type Session struct {
	ID              string    `json:"id"`
	Type            Type      `json:"type"`
	Ticket          string    `json:"ticket"` // optional
	TmuxSession     string    `json:"tmux_session"`
	ClaudeSessionID string    `json:"claude_session_id"` // pinned claude --session-id (UUID); deterministic transcript + future --resume
	Repo            string    `json:"repo"`
	Worktree        string    `json:"worktree"` // optional (empty = no worktree)
	Branch          string    `json:"branch"`   // optional
	PR              string    `json:"pr"`       // optional (pr-review)
	Prompt          string    `json:"prompt"`   // initial prompt (prompt-spawned agents)
	Workdir         string    `json:"workdir"`  // absolute cwd of the tmux session
	Subject         string    `json:"subject"`  // one-line auto summary of what it's doing
	Status          Status    `json:"status"`
	PID             int       `json:"pid"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Events          []Event   `json:"events"`
	LastPaneExcerpt string    `json:"last_pane_excerpt"`
	Supervised      bool      `json:"supervised"` // launched with --permission-mode acceptEdits (prompts) instead of bypass
	PipelineID      string    `json:"pipeline_id,omitempty"` // set for pipeline jobs (back-ref)
	JobID           string    `json:"job_id,omitempty"`      // set for pipeline jobs (back-ref)
}
