// Package agentbackend defines warden's neutral contract for driving a console
// coding agent. Warden core (lifecycle, digest, savings, approval, ctxtokens,
// spend, repl) talks only to the Backend interface and the neutral types here —
// it never references a concrete agent binary (claude, aider, agy, …) directly.
// Each console agent is normalized into this contract by one adapter under
// backends/.
//
// Phase 0 (this package's introduction) wires only the command-string and
// transcript-location seams through the interface (LaunchCmd / ResumeCmd /
// HeadlessCmd / TranscriptPath); the semantic methods (ParseTranscript /
// DetectState / ParseApproval / Pricing / SystemPromptFlag) are defined and
// implemented by the Claude adapter but are not yet routed through the interface
// by core — that capability-gated wiring lands in Phase 1 alongside the second
// backend (design §5).
package agentbackend

import (
	"io"
	"time"
)

// State is an agent's coarse run state inferred from its captured tmux pane.
// It is deliberately small: warden layers its own richer status machine
// (rate-limited, stuck, orphaned, …) on top of this neutral signal.
type State string

const (
	// StateUnknown means the pane carried no signal this adapter recognizes;
	// the caller should keep the agent's current status rather than guess.
	StateUnknown State = ""
	// StateIdle means the agent is at rest awaiting work (positively detected).
	StateIdle State = "idle"
	// StateWorking means the agent is actively streaming a turn.
	StateWorking State = "working"
	// StateNeedsInput means the agent is blocked on an approval / prompt.
	StateNeedsInput State = "needs_input"
)

// LaunchOpts is the neutral input for a fresh agent session. The caller
// (lifecycle) resolves Model to a concrete id and validates Mode before calling;
// the adapter is responsible only for shaping these into the agent's command and
// shell-quoting any value that is typed into a tmux pane.
type LaunchOpts struct {
	SessionID string // session id to assign (when Caps.SessionIDControl); pinned for deterministic transcript + resume
	Name      string // display label for the session (warden uses the agent id)
	Model     string // already-resolved model id (aliases expanded, default applied) — empty only if the backend has no model flag
	Mode      string // permission/approval mode (one of Caps.PermissionModes)
}

// ResumeOpts is the neutral input for resuming an existing session by id.
type ResumeOpts struct {
	SessionID string // the id of the conversation to resume
	Name      string // display label re-applied on resume
	Model     string // already-resolved model id
	Mode      string // permission/approval mode
}

// Turn is warden's neutral transcript record. Backends normalize their own
// transcript format (JSONL, markdown, SQLite rows, …) INTO a slice of these so
// digest / narration / savings can be written once against a single shape.
type Turn struct {
	Role      string    // "user" | "assistant" | "tool"
	Text      string    // concatenated message text
	ToolName  string    // tool invoked on this turn, if any
	Files     []string  // files touched on this turn (for digest "what changed")
	Timestamp time.Time // best-effort; zero when the source carries none
}

// Approval is warden's neutral view of an agent's pending approval prompt,
// normalized from each agent's prompt UI (Claude's box-drawing+numbered options,
// Aider's y/n, …).
type Approval struct {
	Action            string   // e.g. "Bash(rm -rf node_modules)"; "" if not found
	Question          string   // e.g. "Do you want to proceed?"; "" if not found
	Options           []string // option labels, top-down, 1-indexed by position
	SelectedIdx       int      // 1-based index of the highlighted option; 0 if none
	AffirmativeIdx    int      // 1-based least-privilege "yes"; 0 = none found
	AffirmativeSticky bool     // true when the affirmative is a standing/"don't ask again" grant
}

// Price is one model's published per-million-token rates.
type Price struct {
	InputPerMTok  float64
	OutputPerMTok float64
}

// PricingTable carries a backend's model pricing for spend & savings. Default is
// the rate applied to an unrecognized model id (a conservative fallback).
type PricingTable struct {
	Models  map[string]Price // model id / family -> rates
	Default Price            // fallback for an unrecognized model
}

// Caps declares which warden features a backend can support. Core gates features
// on these so a non-full backend degrades (design §5) rather than crashing.
type Caps struct {
	Resume               bool     // can resume a prior session by id
	Headless             bool     // has a non-interactive one-shot mode
	ModelSelection       bool     // accepts a model flag
	PermissionModes      []string // backend-native approval modes
	StructuredTranscript bool     // parseable transcript on disk (gates digests / savings / token counting)
	SystemPromptInject   bool     // accepts an injected system-prompt addendum
	SessionIDControl     bool     // warden can assign the session id (vs. discover the agent-generated one)
}

// Backend describes how to drive one console coding agent. All strings returned
// by LaunchCmd/ResumeCmd are typed into a tmux pane and executed by a shell, so
// adapters MUST shell-quote any untrusted value they embed.
type Backend interface {
	// Identity.
	ID() string          // "claude", "aider", "antigravity", …
	DisplayName() string // human-readable
	Binary() string      // "claude", "aider", "agy"
	InstallHint() string // how to install the binary

	// Launch / resume — strings typed into a tmux pane and run by a shell.
	LaunchCmd(opts LaunchOpts) string
	ResumeCmd(opts ResumeOpts) (cmd string, ok bool) // ok=false ⇒ no resume support

	// Headless one-shot (classify / summarize). ok=false ⇒ caller uses the local-LLM path.
	HeadlessCmd(prompt string) (argv []string, ok bool)

	// Transcript: where it lives + how to parse it into warden's neutral turns.
	TranscriptPath(projectsDir, workdir, sessionID string) (path string, ok bool)
	ParseTranscript(r io.Reader) ([]Turn, error)

	// State detection from a captured tmux pane.
	DetectState(pane string) State
	ParseApproval(pane string) (*Approval, bool)

	// Optional system-prompt addendum, returned as a fragment appended to LaunchCmd.
	SystemPromptFlag(text string) (fragment string, ok bool)

	// Pricing / tokenizer metadata for spend & savings (ok=false ⇒ degrade).
	Pricing() (PricingTable, bool)

	Capabilities() Caps
}
