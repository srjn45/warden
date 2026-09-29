package agentstore

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/store"
)

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
	ID              string        `json:"id"`
	Name            string        `json:"name,omitempty"`
	Type            store.Type    `json:"type"`
	Ticket          string        `json:"ticket"`
	TmuxSession     string        `json:"tmux_session"`
	AiCli           string        `json:"ai_cli,omitempty"`  // canonical (legacy: "backend")
	AICLISessionID  string        `json:"ai_cli_session_id"` // canonical (legacy: "claude_session_id")
	Repo            string        `json:"repo"`
	Worktree        string        `json:"worktree"`
	Branch          string        `json:"branch"`
	WorktreeCreated bool          `json:"worktree_created,omitempty"`
	BranchCreated   bool          `json:"branch_created,omitempty"`
	PR              string        `json:"pr"`
	Prompt          string        `json:"prompt"`
	Workdir         string        `json:"workdir"`
	Subject         string        `json:"subject"`
	Tags            []string      `json:"tags,omitempty"`
	Status          store.Status  `json:"status"`
	PID             int           `json:"pid"`
	ExitCode        *int          `json:"exit_code,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Events          []store.Event `json:"events"`
	LastPaneExcerpt string        `json:"last_pane_excerpt"`
	AutoRestart     bool          `json:"auto_restart,omitempty"`
	RestartCount    int           `json:"restart_count,omitempty"`
	LastRestartAt   *time.Time    `json:"last_restart_at,omitempty"`
	PermissionMode  string        `json:"permission_mode,omitempty"`
	Role            string        `json:"role,omitempty"`
	Task            string        `json:"task,omitempty"`
	AutoApprove     bool          `json:"auto_approve,omitempty"`
	ForceCompact    *bool         `json:"force_compact,omitempty"`
	PipelineID      string        `json:"pipeline_id,omitempty"`
	JobID           string        `json:"job_id,omitempty"`
	PlanID          string        `json:"plan_id,omitempty"`
	ScheduleID      string        `json:"schedule_id,omitempty"`
	ScheduleName    string        `json:"schedule_name,omitempty"`
	ParentID        string        `json:"parent_id,omitempty"`
	// ChildAgents and ChildPipelines preserve nil-vs-empty-slice semantics:
	// nil = legacy record (no authoritative list), []string{} = authoritative empty.
	ChildAgents     []string `json:"child_agents,omitempty"`
	ChildPipelines  []string `json:"child_pipelines,omitempty"`
	AutopilotRunID  string   `json:"autopilot_run_id,omitempty"`
	AutopilotSlot   string   `json:"autopilot_slot,omitempty"`
	AutopilotTaskID string   `json:"autopilot_task_id,omitempty"`
	Model           string   `json:"model,omitempty"`
	ProjectID       string   `json:"project_id,omitempty"`
	Hibernated      bool     `json:"hibernated,omitempty"`

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
