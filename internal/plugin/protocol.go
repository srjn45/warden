// Package plugin is warden's thin extension seam (#47): operators register
// external executables that (a) declare custom agent task types and (b) subscribe
// to lifecycle hook events. Plugins are invoked over a documented JSON-over-stdio
// protocol — request on stdin, response on stdout, bounded by a hard timeout —
// exactly mirroring warden's existing per-agent PreToolUse guard hooks (internal/cli
// hook guard/git-guard/check-guard). Hooks are advisory and FAIL OPEN: a broken,
// slow, or missing plugin logs and is skipped, never blocking or crashing an agent.
//
// The package is split into three concerns: this file (the wire protocol), the
// registry/descriptor (plugin.go), and the dispatcher (dispatcher.go). store
// consults the custom-type registry through a function-var seam (store.SetCustomTypeLookup)
// so store never imports plugin.
package plugin

import "github.com/srjn45/warden/internal/store"

// ProtocolVersion is the schema version stamped onto every Request and expected
// on every Response. It is bumped only on a breaking change to the wire shape; a
// plugin can read it to stay forward-compatible. The dispatcher does not reject a
// mismatching response version (fail-open posture) but records it.
const ProtocolVersion = 1

// HookEvent names a point in an agent's lifecycle at which subscribed plugins are
// invoked. The set is small and stable; the "pre-" events fire before the action
// and "post-" after it. Hooks are advisory only — a "pre-" event's response never
// blocks the action (fail-open), so these are notifications/observers, not gates.
type HookEvent string

const (
	EventPreSpawn    HookEvent = "pre-spawn"
	EventPostSpawn   HookEvent = "post-spawn"
	EventPreCommit   HookEvent = "pre-commit"
	EventPostCommit  HookEvent = "post-commit"
	EventPreCheck    HookEvent = "pre-check"
	EventPostCheck   HookEvent = "post-check"
	EventPreTeardown HookEvent = "pre-teardown"
)

// AllEvents is the canonical ordered set of hook events, used to validate a
// plugin's subscriptions and to render `wd plugin list`.
var AllEvents = []HookEvent{
	EventPreSpawn, EventPostSpawn,
	EventPreCommit, EventPostCommit,
	EventPreCheck, EventPostCheck,
	EventPreTeardown,
}

// ValidEvent reports whether e is one of the known hook events.
func ValidEvent(e HookEvent) bool {
	for _, x := range AllEvents {
		if x == e {
			return true
		}
	}
	return false
}

// SessionMeta is the stable, read-only slice of an agent's record handed to a
// plugin. It is a deliberately small projection of store.Session: a plugin sees
// what it needs to act (id, type, repo/worktree/branch/workdir) without warden
// committing to expose the full mutable record over the wire.
type SessionMeta struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Repo     string `json:"repo"`
	Worktree string `json:"worktree"`
	Branch   string `json:"branch"`
	Workdir  string `json:"workdir"`
}

// MetaFromSession projects a store.Session into the wire SessionMeta. A nil
// session yields the zero value so callers need not branch.
func MetaFromSession(s *store.Session) SessionMeta {
	if s == nil {
		return SessionMeta{}
	}
	return SessionMeta{
		ID:       s.ID,
		Type:     string(s.Type),
		Repo:     s.Repo,
		Worktree: s.Worktree,
		Branch:   s.Branch,
		Workdir:  s.Workdir,
	}
}

// Request is the JSON object written to a plugin's stdin for one hook invocation.
// Payload carries event-specific extras (e.g. a commit message or check name);
// it is free-form so the schema can grow without a version bump.
type Request struct {
	ProtocolVersion int               `json:"protocol_version"`
	Event           HookEvent         `json:"event"`
	Session         SessionMeta       `json:"session"`
	Payload         map[string]string `json:"payload,omitempty"`
}

// Response is the JSON object a plugin writes to stdout. It is purely advisory:
// OK/Message are recorded by the dispatcher but never change warden's control
// flow (fail-open). Message is logged so an operator can see what a plugin said.
type Response struct {
	ProtocolVersion int    `json:"protocol_version"`
	OK              bool   `json:"ok"`
	Message         string `json:"message,omitempty"`
}
