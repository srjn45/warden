package store

import (
	"strings"
	"time"
)

// NormalizeTags cleans a set of tag labels (#30) into the canonical form stored
// on a Session: each tag is trimmed, lowercased, blanks are dropped, and
// duplicates are collapsed while preserving first-seen order. Returns nil for an
// all-empty input so untagged sessions stay nil (and JSON-omitted), keeping the
// field backward-compatible with records that predate tags.
func NormalizeTags(tags []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// HasTag reports whether the session carries tag (matched after normalization,
// so it is case- and whitespace-insensitive).
func (s *Session) HasTag(tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return false
	}
	for _, t := range s.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

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

// SessionKind discriminates an AI agent session from a plain terminal session.
// A terminal is mechanically an ordinary tracked tmux session — recovery,
// attach, persistence, and the collaboration daemon's file-conflict awareness
// all apply — but it has no transcript, cost, state, or approvals, so it is
// excluded from every AI-centric aggregation (see Session.IsTerminal). The zero
// value ("") is an agent, so records that predate this field need no migration.
type SessionKind string

const (
	KindAgent    SessionKind = "agent"    // default; empty string == agent (back-compat)
	KindTerminal SessionKind = "terminal" // a plain ${SHELL:-bash} pane, not an AI agent
)

// Type is the kind of work an agent session is doing (design §2).
type Type string

const (
	TypeDevelopment  Type = "development"
	TypeAnalysis     Type = "analysis"
	TypeSpike        Type = "spike"
	TypePRReview     Type = "pr-review"
	TypeCode         Type = "code"
	TypeDocs         Type = "docs"
	TypeWebsite      Type = "website"
	TypeDebugCI      Type = "debug-ci"
	TypeTests        Type = "tests"
	TypeOther        Type = "other"
	TypeResearch     Type = "research"
	TypeArchitecture Type = "architecture"
	TypeDesign       Type = "design"
	TypeCodeReview   Type = "code-review"
	TypeMonitorCI    Type = "monitor-ci"
	TypeMergePR      Type = "merge-pr"
	TypeRelease      Type = "release"
)

// Builtin returns all canonical task types.
func Builtin() []Type {
	return []Type{
		TypeDevelopment, TypeAnalysis, TypeSpike, TypePRReview,
		TypeResearch, TypeArchitecture, TypeDesign, TypeCodeReview,
		TypeDocs, TypeMonitorCI, TypeDebugCI, TypeMergePR, TypeRelease,
	}
}

// Valid reports whether t is a known task type: a built-in, or a custom type a
// plugin registered via the lookup seam (#47). With no plugins installed this is
// exactly the built-in set, so existing behavior is unchanged.
func (t Type) Valid() bool {
	if t.Builtin() {
		return true
	}
	_, ok := lookupCustomType(string(t))
	return ok
}

// NormalizeType maps any input to a known Type, collapsing unknowns to "other".
// Legacy values are mapped to their new equivalents for backward compat.
func NormalizeType(s string) Type {
	switch s {
	case "buildkite-debug":
		return TypeDebugCI
	case "test-run", "env-test":
		return TypeTests
	case "code", "website", "tests":
		return TypeDevelopment
	case "other":
		return ""
	}
	t := Type(s)
	if t.Builtin() {
		return t
	}
	// A plugin-registered custom type is preserved as-is rather than collapsed to
	// "other"; unknown names still collapse (the historical default).
	if _, ok := lookupCustomType(s); ok {
		return t
	}
	return ""
}

// DefaultWorktree reports whether spawning this type creates a git worktree by
// default. Phase 0a isolates every write-agent — development, pr-review, code,
// docs, website, debug-ci, tests — so parallel write-agents never collide in the
// shared repo; the caller opts a write-agent out with --in-repo (honored in
// wantWorktree, except for pr-review which is structurally a separate checkout).
// The investigation types (analysis/spike) and the free-form catch-all (other)
// stay opt-in via --worktree and return false here.
func (t Type) DefaultWorktree() bool {
	switch t {
	case TypeDevelopment, TypePRReview, TypeCode, TypeDocs, TypeWebsite, TypeDebugCI, TypeTests:
		return true
	}
	// A plugin-registered custom type uses its declared isolation policy. Unknown
	// names (no plugin) fall through to false, the historical default.
	if pol, ok := lookupCustomType(string(t)); ok {
		return pol.Worktree
	}
	return false
}

type Event struct {
	TS     time.Time `json:"ts"`
	Type   string    `json:"type"`
	Detail string    `json:"detail"`
}

type Session struct {
	ID              string      `json:"id"`
	Name            string      `json:"name,omitempty"` // optional human-friendly alias (max 32 chars, alphanumeric + hyphens/underscores)
	Type            Type        `json:"type"`
	Ticket          string      `json:"ticket"` // optional
	TmuxSession     string      `json:"tmux_session"`
	Backend         string      `json:"backend,omitempty"` // agent backend id (claude, aider, …); empty ⇒ "claude" (back-compat, no store migration)
	Kind            SessionKind `json:"kind,omitempty"`    // "" ⇒ agent (back-compat); "terminal" ⇒ plain shell, excluded from AI-centric surfaces
	ClaudeSessionID string      `json:"claude_session_id"` // pinned backend session id (claude --session-id; UUID); deterministic transcript + --resume
	Repo            string      `json:"repo"`
	Worktree        string      `json:"worktree"`                   // optional (empty = no worktree)
	Branch          string      `json:"branch"`                     // optional
	WorktreeCreated bool        `json:"worktree_created,omitempty"` // warden ran `git worktree add` (vs adopted a pre-existing one)
	BranchCreated   bool        `json:"branch_created,omitempty"`   // warden/gh created Branch (vs checked out a user branch)
	PR              string      `json:"pr"`                         // optional (pr-review)
	Prompt          string      `json:"prompt"`                     // initial prompt (prompt-spawned agents)
	Workdir         string      `json:"workdir"`                    // absolute cwd of the tmux session
	Subject         string      `json:"subject"`                    // one-line auto summary of what it's doing
	Tags            []string    `json:"tags,omitempty"`             // optional free-form labels for grouping/filtering (#30); nil/empty for untagged sessions
	Status          Status      `json:"status"`
	PID             int         `json:"pid"`
	ExitCode        *int        `json:"exit_code,omitempty"` // process exit status when recovered: nil=unknown (orphaned/pre-feature), 0=clean, non-zero=crash
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Events          []Event     `json:"events"`
	LastPaneExcerpt string      `json:"last_pane_excerpt"`
	AutoRestart     bool        `json:"auto_restart,omitempty"`    // opt-in: auto-resume this agent when it errors (capped)
	RestartCount    int         `json:"restart_count,omitempty"`   // consecutive auto-restart attempts since last sustained-healthy run
	LastRestartAt   *time.Time  `json:"last_restart_at,omitempty"` // when the most recent auto-restart fired
	PermissionMode  string      `json:"permission_mode,omitempty"` // explicit mode override; empty = use global default
	Role            string      `json:"role,omitempty"`            // built-in role (persona + default flags); empty = "general" (no persona)
	AutoApprove     bool        `json:"auto_approve,omitempty"`    // opt-in: auto-approve yes/no prompts (always option 1)
	ForceCompact    *bool       `json:"force_compact,omitempty"`   // per-agent force-compact override; nil = inherit global token_force_compact
	PipelineID      string      `json:"pipeline_id,omitempty"`     // set for pipeline jobs (back-ref)
	JobID           string      `json:"job_id,omitempty"`          // set for pipeline jobs (back-ref)
	ScheduleID      string      `json:"schedule_id,omitempty"`     // set for schedule-fired runs (back-ref to the schedule that spawned this); agent-mode and pipeline-mode job sessions alike
	ScheduleName    string      `json:"schedule_name,omitempty"`   // operator-facing name of that schedule (== ScheduleID today, carried for display)
	ParentID        string      `json:"parent_id,omitempty"`       // id of the agent that spawned this one; empty = root (operator/CLI spawn)
	Model           string      `json:"model,omitempty"`           // claude model (opus/sonnet/haiku or full ID)
	// ProjectID back-refs the first-class project (projectstore) this agent belongs
	// to; empty = ungrouped. It is the PARENT project's canonical id: an agent
	// running in a git worktree links to its repo's project here and keeps its own
	// worktree path in Worktree — a worktree is never a project of its own. Optional
	// (omitempty), so records that predate the field read as ungrouped.
	ProjectID string `json:"project_id,omitempty"`
	// Hibernated marks an agent that was gracefully terminated because its project
	// was closed (IDE-like hibernation, docs/specs/2026-08-28-project-centric-ui.md
	// Phase 4). Its process is gone but its worktree + transcript are kept, so
	// reopening the project restores it right where it left off. The daemon sets it
	// on CloseProject and clears it on reopen after RestoreSession; a naturally
	// finished agent never carries it, so reopen only revives the ones close killed.
	Hibernated bool `json:"hibernated,omitempty"`

	ContextTokens    int        `json:"context_tokens,omitempty"`     // latest context-window fill; 0 = unknown (no model turn yet)
	ContextState     string     `json:"context_state,omitempty"`      // "" | ok | warning | critical
	ContextCheckedAt time.Time  `json:"context_checked_at,omitempty"` // when ContextTokens was last refreshed
	LastCompactAt    *time.Time `json:"last_compact_at,omitempty"`    // when warden last auto-sent /compact (cooldown guard)

	// Rate limit fields
	RateLimitedAt       *time.Time `json:"rate_limited_at,omitempty"`        // when limit was first hit
	RateLimitRestoreAt  *time.Time `json:"rate_limit_restore_at,omitempty"`  // scheduled resume time
	RateLimitRetryCount int        `json:"rate_limit_retry_count,omitempty"` // number of retry attempts
}

// IsTerminal reports whether this session is a plain terminal (a ${SHELL:-bash}
// pane) rather than an AI agent. Terminals are excluded from every AI-centric
// surface — spend, savings, metrics, insights/digest, state detection,
// approvals, and name/summary classification — because they have no transcript,
// cost, or state; they still participate in recovery, attach, persistence, and
// file-conflict awareness like any tracked tmux session. The zero value is an
// agent, so records that predate the Kind field read as agents.
func (s *Session) IsTerminal() bool { return s.Kind == KindTerminal }

// Context-fill states stored in Session.ContextState. They mirror
// ctxtokens.State but are duplicated here to keep store free of that import.
const (
	ContextOK       = "ok"
	ContextWarning  = "warning"
	ContextCritical = "critical"
)
