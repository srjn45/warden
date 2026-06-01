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
	TS     time.Time `bson:"ts" json:"ts"`
	Type   string    `bson:"type" json:"type"`
	Detail string    `bson:"detail" json:"detail"`
}

type Session struct {
	ID              string    `bson:"_id" json:"id"`
	Type            Type      `bson:"type" json:"type"`
	Ticket          string    `bson:"ticket" json:"ticket"`         // optional
	TmuxSession     string    `bson:"tmux_session" json:"tmux_session"`
	Repo            string    `bson:"repo" json:"repo"`
	Worktree        string    `bson:"worktree" json:"worktree"`     // optional (empty = no worktree)
	Branch          string    `bson:"branch" json:"branch"`         // optional
	PR              string    `bson:"pr" json:"pr"`                 // optional (pr-review)
	Status          Status    `bson:"status" json:"status"`
	PID             int       `bson:"pid" json:"pid"`
	CreatedAt       time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt       time.Time `bson:"updated_at" json:"updated_at"`
	Events          []Event   `bson:"events" json:"events"`
	LastPaneExcerpt string    `bson:"last_pane_excerpt" json:"last_pane_excerpt"`
}
