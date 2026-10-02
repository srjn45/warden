package agentstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/capacity"
	"github.com/srjn45/warden/internal/store"
)

// CapacityDomain and QuotaBinding are re-exported from the canonical persisted
// entity package so callers working with Agent need no provider implementation.
type CapacityDomain = capacity.CapacityDomain
type QuotaBinding = capacity.QuotaBinding

// Agent is the canonical AI-worker entity. Kind (and IsTerminal) are
// intentionally absent — terminal panes are a separate entity managed by
// terminalstore. All AI-agent operational fields from the legacy Session model
// are carried forward with two canonical renames:
//
//   - Backend  → AiCli          (json: "ai_cli")
//   - ClaudeSessionID → AICLISessionID (json: "ai_cli_session_id")
//
// Both legacy JSON field names are accepted on decode (AiCli wins if both are
// present) and both are emitted on encode during the alias window so existing
// readers that have not yet migrated still see a recognised field name.
type Agent struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name,omitempty"`
	Type             store.Type             `json:"type"`
	Ticket           string                 `json:"ticket"`
	TmuxSession      string                 `json:"tmux_session"`
	AiCli            string                 `json:"ai_cli,omitempty"`  // canonical (legacy: "backend")
	AICLISessionID   string                 `json:"ai_cli_session_id"` // canonical (legacy: "claude_session_id")
	Repo             string                 `json:"repo"`
	Worktree         string                 `json:"worktree"`
	Branch           string                 `json:"branch"`
	WorktreeCreated  bool                   `json:"worktree_created,omitempty"`
	BranchCreated    bool                   `json:"branch_created,omitempty"`
	PR               string                 `json:"pr"`
	Prompt           string                 `json:"prompt"`
	Workdir          string                 `json:"workdir"`
	Subject          string                 `json:"subject"`
	Tags             []string               `json:"tags,omitempty"`
	Status           store.Status           `json:"status"`
	PID              int                    `json:"pid"`
	ExitCode         *int                   `json:"exit_code,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	UpdatedAt        time.Time              `json:"updated_at"`
	Events           []store.Event          `json:"events"`
	LastPaneExcerpt  string                 `json:"last_pane_excerpt"`
	AutoRestart      bool                   `json:"auto_restart,omitempty"`
	RestartCount     int                    `json:"restart_count,omitempty"`
	LastRestartAt    *time.Time             `json:"last_restart_at,omitempty"`
	PermissionMode   string                 `json:"permission_mode,omitempty"`
	ExecutionProfile store.ExecutionProfile `json:"execution_profile,omitempty"`
	Role             string                 `json:"role,omitempty"`
	Task             string                 `json:"task,omitempty"`
	AutoApprove      bool                   `json:"auto_approve,omitempty"`
	ForceCompact     *bool                  `json:"force_compact,omitempty"`
	PipelineID       string                 `json:"pipeline_id,omitempty"`
	JobID            string                 `json:"job_id,omitempty"`
	PlanID           string                 `json:"plan_id,omitempty"`
	ScheduleID       string                 `json:"schedule_id,omitempty"`
	ScheduleName     string                 `json:"schedule_name,omitempty"`
	ParentID         string                 `json:"parent_id,omitempty"`
	// ChildAgents and ChildPipelines preserve nil-vs-empty-slice semantics:
	// nil = legacy record (no authoritative list), []string{} = authoritative empty.
	ChildAgents     []string               `json:"child_agents,omitempty"`
	ChildPipelines  []string               `json:"child_pipelines,omitempty"`
	AutopilotRunID  string                 `json:"autopilot_run_id,omitempty"`
	AutopilotSlot   string                 `json:"autopilot_slot,omitempty"`
	AutopilotTaskID string                 `json:"autopilot_task_id,omitempty"`
	Model           string                 `json:"model,omitempty"`
	QuotaBinding    *capacity.QuotaBinding `json:"quota_binding,omitempty"`
	ProjectID       string                 `json:"project_id,omitempty"`
	Hibernated      bool                   `json:"hibernated,omitempty"`

	ContextTokens    int        `json:"context_tokens,omitempty"`
	ContextState     string     `json:"context_state,omitempty"`
	ContextCheckedAt time.Time  `json:"context_checked_at,omitempty"`
	LastCompactAt    *time.Time `json:"last_compact_at,omitempty"`

	RateLimitedAt             *time.Time             `json:"rate_limited_at,omitempty"`
	RateLimitRestoreAt        *time.Time             `json:"rate_limit_restore_at,omitempty"`
	RateLimitRetryCount       int                    `json:"rate_limit_retry_count,omitempty"`
	BackendRecoveryGeneration uint64                 `json:"backend_recovery_generation,omitempty"`
	BackendRecovery           *store.BackendRecovery `json:"backend_recovery,omitempty"`
}

// MarshalJSON emits both the canonical (ai_cli, ai_cli_session_id) and the
// deprecated (backend, claude_session_id) field names during the alias window
// so readers that have not yet migrated still find a field they recognise.
// ChildAgents and ChildPipelines are emitted as null-omitted vs explicit-empty
// to preserve the authoritative-empty distinction.
func (a Agent) MarshalJSON() ([]byte, error) {
	type plain Agent
	optional := func(ids []string) *[]string {
		if ids == nil {
			return nil
		}
		return &ids
	}
	return json.Marshal(struct {
		plain
		// alias-window: legacy names for backward-compatible readers
		Backend         string `json:"backend,omitempty"`
		ClaudeSessionID string `json:"claude_session_id"`
		// nil-vs-empty authoritative semantics for hierarchy lists
		ChildAgents    *[]string `json:"child_agents,omitempty"`
		ChildPipelines *[]string `json:"child_pipelines,omitempty"`
	}{
		plain:           plain(a),
		Backend:         a.AiCli,
		ClaudeSessionID: a.AICLISessionID,
		ChildAgents:     optional(a.ChildAgents),
		ChildPipelines:  optional(a.ChildPipelines),
	})
}

// UnmarshalJSON reads both the canonical field names (ai_cli,
// ai_cli_session_id) and their deprecated aliases (backend,
// claude_session_id). When both canonical and alias are present the canonical
// value wins (spec §4, canonical-wins rule).
func (a *Agent) UnmarshalJSON(data []byte) error {
	type plain Agent
	var aux struct {
		plain
		// legacy aliases accepted during the migration window
		Backend         string `json:"backend"`
		ClaudeSessionID string `json:"claude_session_id"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*a = Agent(aux.plain)
	if a.AiCli == "" && aux.Backend != "" {
		a.AiCli = aux.Backend
	}
	if a.AICLISessionID == "" && aux.ClaudeSessionID != "" {
		a.AICLISessionID = aux.ClaudeSessionID
	}
	return nil
}

// HasTag reports whether the agent carries tag, matched after normalization
// (case- and whitespace-insensitive).
func (a *Agent) HasTag(tag string) bool {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tag == "" {
		return false
	}
	for _, t := range a.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// ToSession converts the canonical Agent entity into the legacy Session DTO
// used at external boundaries (REST API, OpenAPI responses, CLI compatibility).
func (a *Agent) ToSession() *store.Session {
	if a == nil {
		return nil
	}
	return &store.Session{
		ID:                        a.ID,
		Name:                      a.Name,
		Type:                      a.Type,
		Ticket:                    a.Ticket,
		TmuxSession:               a.TmuxSession,
		AiCli:                     a.AiCli,
		Backend:                   a.AiCli, // deprecated wire mirror for dual-emit
		AICLISessionID:            a.AICLISessionID,
		ClaudeSessionID:           a.AICLISessionID, // deprecated wire mirror for dual-emit
		Repo:                      a.Repo,
		Worktree:                  a.Worktree,
		Branch:                    a.Branch,
		WorktreeCreated:           a.WorktreeCreated,
		BranchCreated:             a.BranchCreated,
		PR:                        a.PR,
		Prompt:                    a.Prompt,
		Workdir:                   a.Workdir,
		Subject:                   a.Subject,
		Tags:                      append([]string{}, a.Tags...),
		Status:                    a.Status,
		PID:                       a.PID,
		ExitCode:                  a.ExitCode,
		CreatedAt:                 a.CreatedAt,
		UpdatedAt:                 a.UpdatedAt,
		Events:                    append([]store.Event{}, a.Events...),
		LastPaneExcerpt:           a.LastPaneExcerpt,
		AutoRestart:               a.AutoRestart,
		RestartCount:              a.RestartCount,
		LastRestartAt:             a.LastRestartAt,
		PermissionMode:            a.PermissionMode,
		ExecutionProfile:          a.ExecutionProfile,
		Role:                      a.Role,
		Task:                      a.Task,
		AutoApprove:               a.AutoApprove,
		ForceCompact:              a.ForceCompact,
		PipelineID:                a.PipelineID,
		JobID:                     a.JobID,
		PlanID:                    a.PlanID,
		ScheduleID:                a.ScheduleID,
		ScheduleName:              a.ScheduleName,
		ParentID:                  a.ParentID,
		ChildAgents:               a.ChildAgents,
		ChildPipelines:            a.ChildPipelines,
		AutopilotRunID:            a.AutopilotRunID,
		AutopilotSlot:             a.AutopilotSlot,
		AutopilotTaskID:           a.AutopilotTaskID,
		Model:                     a.Model,
		QuotaBinding:              a.QuotaBinding,
		ProjectID:                 a.ProjectID,
		Hibernated:                a.Hibernated,
		ContextTokens:             a.ContextTokens,
		ContextState:              a.ContextState,
		ContextCheckedAt:          a.ContextCheckedAt,
		LastCompactAt:             a.LastCompactAt,
		RateLimitedAt:             a.RateLimitedAt,
		RateLimitRestoreAt:        a.RateLimitRestoreAt,
		RateLimitRetryCount:       a.RateLimitRetryCount,
		BackendRecoveryGeneration: a.BackendRecoveryGeneration,
		BackendRecovery:           a.BackendRecovery,
	}
}

// FromSession converts a legacy Session DTO into a canonical Agent entity.
func FromSession(s *store.Session) *Agent {
	if s == nil {
		return nil
	}
	return &Agent{
		ID:                        s.ID,
		Name:                      s.Name,
		Type:                      s.Type,
		Ticket:                    s.Ticket,
		TmuxSession:               s.TmuxSession,
		AiCli:                     firstNonEmpty(s.AiCli, s.Backend),
		AICLISessionID:            firstNonEmpty(s.AICLISessionID, s.ClaudeSessionID),
		Repo:                      s.Repo,
		Worktree:                  s.Worktree,
		Branch:                    s.Branch,
		WorktreeCreated:           s.WorktreeCreated,
		BranchCreated:             s.BranchCreated,
		PR:                        s.PR,
		Prompt:                    s.Prompt,
		Workdir:                   s.Workdir,
		Subject:                   s.Subject,
		Tags:                      append([]string{}, s.Tags...),
		Status:                    s.Status,
		PID:                       s.PID,
		ExitCode:                  s.ExitCode,
		CreatedAt:                 s.CreatedAt,
		UpdatedAt:                 s.UpdatedAt,
		Events:                    append([]store.Event{}, s.Events...),
		LastPaneExcerpt:           s.LastPaneExcerpt,
		AutoRestart:               s.AutoRestart,
		RestartCount:              s.RestartCount,
		LastRestartAt:             s.LastRestartAt,
		PermissionMode:            s.PermissionMode,
		ExecutionProfile:          s.ExecutionProfile,
		Role:                      s.Role,
		Task:                      s.Task,
		AutoApprove:               s.AutoApprove,
		ForceCompact:              s.ForceCompact,
		PipelineID:                s.PipelineID,
		JobID:                     s.JobID,
		PlanID:                    s.PlanID,
		ScheduleID:                s.ScheduleID,
		ScheduleName:              s.ScheduleName,
		ParentID:                  s.ParentID,
		ChildAgents:               s.ChildAgents,
		ChildPipelines:            s.ChildPipelines,
		AutopilotRunID:            s.AutopilotRunID,
		AutopilotSlot:             s.AutopilotSlot,
		AutopilotTaskID:           s.AutopilotTaskID,
		Model:                     s.Model,
		QuotaBinding:              s.QuotaBinding,
		ProjectID:                 s.ProjectID,
		Hibernated:                s.Hibernated,
		ContextTokens:             s.ContextTokens,
		ContextState:              s.ContextState,
		ContextCheckedAt:          s.ContextCheckedAt,
		LastCompactAt:             s.LastCompactAt,
		RateLimitedAt:             s.RateLimitedAt,
		RateLimitRestoreAt:        s.RateLimitRestoreAt,
		RateLimitRetryCount:       s.RateLimitRetryCount,
		BackendRecoveryGeneration: s.BackendRecoveryGeneration,
		BackendRecovery:           s.BackendRecovery,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// CapacityBindingState labels records without a daemon-owned binding as legacy.
// It intentionally derives from the persisted nil value so reading an old record
// never writes a guessed account/profile or bucket claim back to storage.
func (a *Agent) CapacityBindingState() string {
	if a == nil || a.QuotaBinding == nil {
		return capacity.LegacyUnbound
	}
	return "bound"
}

// AgentStore defines the persistence interface for AI agents.
type AgentStore interface {
	Insert(ctx context.Context, a *Agent) error
	Get(ctx context.Context, id string) (*Agent, error)
	GetByNameOrID(ctx context.Context, nameOrID string) (*Agent, error)
	List(ctx context.Context) ([]*Agent, error)
	ListClosed(ctx context.Context) ([]*Agent, error)
	ListClosedDegraded(ctx context.Context) ([]*Agent, int, error)
	Update(ctx context.Context, id string, fn func(*Agent) error) error
	UpdateStatus(ctx context.Context, id string, status store.Status) error
	UpdateStatusIf(ctx context.Context, id string, expected, next store.Status) (bool, error)
	FinalizeExit(ctx context.Context, id string, expected, next store.Status, code int) (bool, error)
	AppendEvent(ctx context.Context, id string, ev store.Event) error
	AppendEventStatus(ctx context.Context, id string, ev store.Event, status store.Status) error
	SetRestart(ctx context.Context, id string, count int, at time.Time) error
	UpdateContext(ctx context.Context, id string, tokens int, state string) error
	StampCompact(ctx context.Context, id string) error
	UpdateAutoApprove(ctx context.Context, id string, enabled bool) error
	SetForceCompact(ctx context.Context, id string, v *bool) error
	UpdatePermissionMode(ctx context.Context, id string, mode string) error
	UpdateRole(ctx context.Context, id string, role string) error
	ClearWorktree(ctx context.Context, id string) error
	SetRateLimit(ctx context.Context, id string, restoreAt time.Time, retryCount int) error
	ClearRateLimit(ctx context.Context, id string) error
	SetSessionID(ctx context.Context, id, sessionID string) error
	Ping(ctx context.Context) error
	Archive(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	Close() error
}
