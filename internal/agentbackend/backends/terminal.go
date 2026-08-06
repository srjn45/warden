package backends

import (
	"io"

	"github.com/srjn45/warden/internal/agentbackend"
)

func init() { agentbackend.Register(Terminal{}) }

// Terminal is the Backend adapter for a plain interactive shell — NOT an AI
// agent. It gives warden a first-class way to open an ordinary terminal in an
// agent's directory and manage it with the same lifecycle every other backend
// gets: an isolated worktree (or the shared repo), a tmux pane you can attach
// to, git commit/push/sync, snapshots, teardown, and the cockpit/TUI listing.
//
// The use case is the "human seat" beside the fleet: a shell parked in a repo or
// worktree — to run builds, poke at a failing test, or drive git by hand — that
// warden tracks and tears down like any managed agent, without an AI CLI in the
// loop.
//
// Everything AI-specific degrades by design (design §5): no resume, no headless
// one-shot, no model, no transcript, no pricing, no approval parsing, no
// system-prompt injection. DetectState always returns Unknown — a shell has no
// stable "working"/"idle" marker warden could scrape — so the poller keeps the
// agent's status as-is and learns it finished only from the exit-code capture
// the lifecycle appends (the user typing `exit`). The task prompt is
// intentionally ignored: a shell would EXECUTE whatever text it's fed, so warden
// never types the prompt into it (Terminal deliberately does NOT implement
// PromptSeeder).
type Terminal struct{}

// --- Identity ---------------------------------------------------------------

func (Terminal) ID() string          { return "terminal" }
func (Terminal) DisplayName() string { return "Terminal (plain shell)" }

// Binary reports "sh" — the POSIX shell every supported platform ships. It is
// only used for install detection (autopilot's LookPath) and error messages;
// the actual shell warden launches is $SHELL, falling back to bash then sh (see
// LaunchCmd). "sh" is guaranteed present, so a Terminal backend always reads as
// installed.
func (Terminal) Binary() string { return "sh" }

func (Terminal) InstallHint() string {
	return "No install needed — warden opens your $SHELL (falls back to bash)."
}

// --- Launch / resume --------------------------------------------------------

// LaunchCmd returns the command typed into the tmux pane: launch the user's
// interactive login shell. The pane is already created with the agent's worktree
// as its working directory (as for every backend), so the shell opens directly
// "on that directory" with no cd needed. It is a NESTED shell (not `exec`) on
// purpose: the lifecycle appends an exit-code capture (`; printf '%s' "$?" > …`)
// after this command, and only a non-exec'd child returns control to the outer
// pane shell so that capture runs when the user types `exit` — which is how
// warden learns the terminal session ended. SessionID/Name/Model/Mode are all
// irrelevant to a shell and ignored.
func (Terminal) LaunchCmd(agentbackend.LaunchOpts) string {
	// $SHELL is the user's configured login shell; bash is the fallback when it is
	// unset. Started with a tty on stdin, the shell is interactive automatically —
	// no -i needed — which keeps this portable across bash/zsh.
	return `${SHELL:-bash}`
}

// ResumeCmd reports no resume support: a shell holds no session warden can pin,
// so rotate/handoff re-open a fresh terminal instead (Caps.Resume=false).
func (Terminal) ResumeCmd(agentbackend.ResumeOpts) (string, bool) { return "", false }

// LaunchPromptArg returns "" — a plain terminal has no task prompt. warden never
// seeds the prompt into the shell (that would run it as a command), so nothing
// is appended to the launch line and Terminal does not implement PromptSeeder.
func (Terminal) LaunchPromptArg(string) string { return "" }

// HeadlessCmd reports no headless one-shot: a shell has no non-interactive
// classify/summarize mode, so warden's offload path uses the local-LLM fallback
// (Terminal is never the default backend anyway).
func (Terminal) HeadlessCmd(string) ([]string, bool) { return nil, false }

// --- Transcript -------------------------------------------------------------

// TranscriptPath reports no transcript: a shell writes no conversation log warden
// can read, so digest/savings/token-counting degrade to "no transcript"
// (Caps.StructuredTranscript=false).
func (Terminal) TranscriptPath(_, _, _ string) (string, bool) { return "", false }

// ParseTranscript returns no turns: there is no transcript to normalize.
func (Terminal) ParseTranscript(io.Reader) ([]agentbackend.Turn, error) { return nil, nil }

// --- State / approval -------------------------------------------------------

// DetectState always returns Unknown: a shell prints no stable state marker, so
// warden keeps the agent's current status and infers completion from the
// exit-code capture rather than the pane. This mirrors the conservative stance of
// the interactive backends that have no positive working/idle marker (crush /
// goose / opencode).
func (Terminal) DetectState(string) agentbackend.State { return agentbackend.StateUnknown }

// ParseApproval finds nothing: a plain terminal has no approval UI for warden to
// answer.
func (Terminal) ParseApproval(string) (*agentbackend.Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no system-prompt injection: there is nothing to inject
// a persona/hints into (Caps.SystemPromptInject=false). Terminal likewise does
// not implement ContextInjector — dropping a rules file a shell never reads would
// only litter the worktree.
func (Terminal) SystemPromptFlag(string) (string, bool) { return "", false }

// Pricing reports none: a shell consumes no model tokens, so it is omitted from
// spend/savings entirely (Pricing ok=false).
func (Terminal) Pricing() (agentbackend.PricingTable, bool) {
	return agentbackend.PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities declares Terminal as the fully-degraded backend: an interactive
// pane and nothing else. Every AI feature is off, so core skips resume, headless,
// model selection, transcript digests, spend/savings, and system-prompt injection
// for a terminal agent (design §5).
func (Terminal) Capabilities() agentbackend.Caps {
	return agentbackend.Caps{
		Resume:               false,
		Headless:             false,
		ModelSelection:       false,
		PermissionModes:      nil,
		StructuredTranscript: false,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}
