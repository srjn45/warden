package daemon

// Small typed request/response builders used by the daemon's HTTP route tests.
// The production wire types are generated from openapi.yaml into the oapi
// package; these mirrors exist only to keep the table-driven tests readable.

import (
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/branchtrack"
	"github.com/srjn45/warden/internal/collab"
	"github.com/srjn45/warden/internal/snapshot"
	"github.com/srjn45/warden/internal/store"
)

// EventRequest is the body for POST /events (sent by hooks).
type EventRequest struct {
	Session string `json:"session"`
	Type    string `json:"type"`
	Detail  string `json:"detail"`
}

// AdoptRequest is the body for POST /adopt.
type AdoptRequest struct {
	Cwd         string `json:"cwd"`
	SessionID   string `json:"session_id"`
	TmuxSession string `json:"tmux_session"`
}

type adoptResponse struct {
	Session *store.Session `json:"session"`
	Warning string         `json:"warning,omitempty"`
}

// InputRequest is the body for POST /sessions/{id}/input.
type InputRequest struct {
	Text string `json:"text"`
}

// OutputResponse is the body for GET /sessions/{id}/output.
type OutputResponse struct {
	Output string `json:"output"`
}

// GitRequest is the shared body for the commit/push/sync endpoints.
type GitRequest struct {
	Session string `json:"session,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Message string `json:"message,omitempty"`
	Base    string `json:"base,omitempty"`
}

// CheckRequest is the body for POST /check.
type CheckRequest struct {
	Session string `json:"session,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Name    string `json:"name,omitempty"`
}

// ApproveRequest is the body for POST /sessions/{id}/approve.
type ApproveRequest struct {
	Option      int    `json:"option"`
	Fingerprint string `json:"fingerprint"`
}

type approvalsResponse struct {
	Enabled   bool            `json:"enabled"`
	Approvals []approval.View `json:"approvals"`
}

type branchesResponse struct {
	Branches []branchtrack.BranchStatus `json:"branches"`
}

type conflictsResponse struct {
	Conflicts []collab.Conflict `json:"conflicts"`
}

type snapshotListResponse struct {
	Snapshots []*snapshot.Snapshot `json:"snapshots"`
}

type dirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DirListing is the body of GET /fs/dirs.
type DirListing struct {
	Path    string     `json:"path"`
	Parent  string     `json:"parent"`
	Entries []dirEntry `json:"entries"`
}
