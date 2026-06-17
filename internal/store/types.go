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
	StatusRateLimited     Status = "rate_limited"
)

func (s Status) Valid() bool {
	switch s {
	case StatusSpawning, StatusWorking, StatusWaitingForInput,
		StatusIdle, StatusDone, StatusErrored, StatusOrphaned,
		StatusRateLimited:
		return true
	}
	return false
}

// Type is the kind of work an agent session is doing (design §2).
type Type string

const (
	TypeDevelopment Type = "development"
	TypeAnalysis    Type = "analysis"
	TypeSpike       Type = "spike"
	TypePRReview    Type = "pr-review"
	TypeCode        Type = "code"
	TypeDocs        Type = "docs"
	TypeWebsite     Type = "website"
	TypeDebugCI     Type = "debug-ci"
	TypeTests       Type = "tests"
	TypeOther       Type = "other"
)

// Valid reports whether t is one of the known task types.
func (t Type) Valid() bool {
	switch t {
	case TypeDevelopment, TypeAnalysis, TypeSpike, TypePRReview,
		TypeCode, TypeDocs, TypeWebsite, TypeDebugCI, TypeTests, TypeOther:
		return true
	}
	return false
}

// NormalizeType maps any input to a known Type, collapsing unknowns to "other".
// Legacy values are mapped to their new equivalents for backward compat.
func NormalizeType(s string) Type {
	switch s {
	case "buildkite-debug":
		return TypeDebugCI
	case "test-run", "env-test":
		return TypeTests
	}
	t := Type(s)
	switch t {
	case TypeDevelopment, TypeAnalysis, TypeSpike, TypePRReview,
		TypeCode, TypeDocs, TypeWebsite, TypeDebugCI, TypeTests, TypeOther:
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
	ID              string     `json:"id"`
	Name            string     `json:"name,omitempty"` // optional human-friendly alias (max 32 chars, alphanumeric + hyphens/underscores)
	Type            Type       `json:"type"`
	Ticket          string     `json:"ticket"` // optional
	TmuxSession     string     `json:"tmux_session"`
	ClaudeSessionID string     `json:"claude_session_id"` // pinned claude --session-id (UUID); deterministic transcript + future --resume
	Repo            string     `json:"repo"`
	Worktree        string     `json:"worktree"` // optional (empty = no worktree)
	Branch          string     `json:"branch"`   // optional
	PR              string     `json:"pr"`       // optional (pr-review)
	Prompt          string     `json:"prompt"`   // initial prompt (prompt-spawned agents)
	Workdir         string     `json:"workdir"`  // absolute cwd of the tmux session
	Subject         string     `json:"subject"`  // one-line auto summary of what it's doing
	Status          Status     `json:"status"`
	PID             int        `json:"pid"`
	ExitCode        *int       `json:"exit_code,omitempty"` // process exit status when recovered: nil=unknown (orphaned/pre-feature), 0=clean, non-zero=crash
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	Events          []Event    `json:"events"`
	LastPaneExcerpt string     `json:"last_pane_excerpt"`
	Supervised      bool       `json:"supervised"`                // launched with --permission-mode acceptEdits (prompts) instead of bypass
	AutoApprove     bool       `json:"auto_approve,omitempty"`    // per-session auto-approval override (overrides AutoApproveGlobal)
	AutoRestart     bool       `json:"auto_restart,omitempty"`    // opt-in: auto-resume this agent when it errors (capped)
	RestartCount    int        `json:"restart_count,omitempty"`   // consecutive auto-restart attempts since last sustained-healthy run
	LastRestartAt   *time.Time `json:"last_restart_at,omitempty"` // when the most recent auto-restart fired
	PipelineID      string     `json:"pipeline_id,omitempty"`     // set for pipeline jobs (back-ref)
	JobID           string     `json:"job_id,omitempty"`          // set for pipeline jobs (back-ref)

	ContextTokens    int        `json:"context_tokens,omitempty"`     // latest context-window fill; 0 = unknown (no model turn yet)
	ContextState     string     `json:"context_state,omitempty"`      // "" | ok | warning | critical
	ContextCheckedAt time.Time  `json:"context_checked_at,omitempty"` // when ContextTokens was last refreshed
	LastCompactAt    *time.Time `json:"last_compact_at,omitempty"`    // when warden last auto-sent /compact (cooldown guard)

	// Rate limit fields
	RateLimitedAt       *time.Time `json:"rate_limited_at,omitempty"`        // when limit was first hit
	RateLimitRestoreAt  *time.Time `json:"rate_limit_restore_at,omitempty"`  // scheduled resume time
	RateLimitRetryCount int        `json:"rate_limit_retry_count,omitempty"` // number of retry attempts
}

// Context-fill states stored in Session.ContextState. They mirror
// ctxtokens.State but are duplicated here to keep store free of that import.
const (
	ContextOK       = "ok"
	ContextWarning  = "warning"
	ContextCritical = "critical"
)
