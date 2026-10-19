package ptyobs

import "time"

// DangerBanner is the conspicuous text every surface shows while a session's
// posture is dangerously-execute-all.
const DangerBanner = "DANGER: dangerously-execute-all - Warden auto-answers every confidently classified permission prompt for this session."

// TrailEntry is one explainable state transition.
type TrailEntry struct {
	At       time.Time `json:"at"`
	From     string    `json:"from"`
	To       string    `json:"to"`
	Reason   string    `json:"reason,omitempty"`
	Evidence []string  `json:"evidence,omitempty"`
	Seq      uint64    `json:"seq"`
}

// RuntimeView is the operator-facing runtime status of one session: the same
// document every client (TUI, web, CLI, MCP) renders.
type RuntimeView struct {
	SessionID string `json:"session_id"`
	// Mode is the daemon's runtime.pty mode (off|shadow|on).
	Mode string `json:"mode"`
	// Supervised is false when no event stream backs the session (runtime off
	// at spawn); the remaining fields are then zero.
	Supervised    bool   `json:"supervised"`
	Authoritative bool   `json:"authoritative"`
	Presentation  string `json:"presentation,omitempty"`

	// State is the live classifier state; Reason is the typed waiting reason.
	State  string    `json:"state,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	// Evidence is the evidence behind the latest transition; Trail the bounded
	// transition history, newest last.
	Evidence []string     `json:"evidence,omitempty"`
	Trail    []TrailEntry `json:"trail,omitempty"`
	// QuietSeconds is how long the terminal has been silent. Advisory only:
	// silence is never a state.
	QuietSeconds float64 `json:"quiet_seconds,omitempty"`

	// InputOwner is who owns the input line: human, operator, warden or free.
	InputOwner    string `json:"input_owner,omitempty"`
	HumanLineOpen bool   `json:"human_line_open,omitempty"`

	// Posture is the effective permission posture; Danger is set (with the
	// banner) for dangerously-execute-all.
	Posture      string `json:"posture"`
	Danger       bool   `json:"danger"`
	DangerBanner string `json:"danger_banner,omitempty"`

	// Route and LastOutcome describe the most recent decision, from the trace.
	Route       string `json:"route,omitempty"`
	LastOutcome string `json:"last_outcome,omitempty"`

	// Prompt is the permission prompt currently on screen, summarised.
	Prompt *PermissionSummary `json:"prompt,omitempty"`
}
