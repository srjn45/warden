package daemon

import (
	"github.com/srajanpathak/agentctl/internal/store"
)

// EventRequest is the body for POST /events (sent by hooks).
type EventRequest struct {
	Session string `json:"session"` // tmux session name == session id
	Type    string `json:"type"`
	Detail  string `json:"detail"`
}

// SpawnRequest is the body for POST /spawn.
type SpawnRequest struct {
	Type     string `json:"type"`     // required; normalized via store.NormalizeType
	Ticket   string `json:"ticket"`   // optional; becomes the id when present
	Repo     string `json:"repo"`     // required
	Branch   string `json:"branch"`   // optional; development branch / pr-review checkout
	PR       string `json:"pr"`       // optional; pr-review
	Worktree bool   `json:"worktree"` // analysis/spike opt-in
}

// CleanupRequest is the body for POST /cleanup.
type CleanupRequest struct {
	ID    string `json:"id"`
	Force bool   `json:"force"`
	Hard  bool   `json:"hard"`
}

// InputRequest is the body for POST /sessions/{id}/input.
type InputRequest struct {
	Text string `json:"text"`
}

// OutputResponse is the body for GET /sessions/{id}/output.
type OutputResponse struct {
	Output string `json:"output"`
}

// errorResponse is the standard error envelope.
type errorResponse struct {
	Error string `json:"error"`
}

// sessionsResponse wraps a list for GET /sessions.
type sessionsResponse struct {
	Sessions []*store.Session `json:"sessions"`
}
