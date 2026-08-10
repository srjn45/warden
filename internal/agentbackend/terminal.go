package agentbackend

import "io"

// terminalBackend is the launch adapter for a plain interactive shell — NOT an AI
// agent, and (since the cockpit stage-6 redesign) NOT a registered backend. A
// terminal is a first-class session Kind (store.KindTerminal), created via the
// session `kind` field rather than a backend id, so this adapter is deliberately
// absent from the registry: it never appears in agentbackend.IDs(), Get(),
// Detect(), or GET /api/v1/backends. It exists only so the lifecycle launch path
// can reuse the exact same degraded machinery every backend gets — an isolated
// tmux pane, git commit/push/sync, snapshots, teardown, and the cockpit listing —
// without an AI CLI in the loop.
//
// The use case is the "human seat" beside the fleet: a shell parked in a repo or
// worktree — to run builds, poke at a failing test, or drive git by hand — that
// warden tracks and tears down like any managed session.
//
// Everything AI-specific degrades by design: no resume, no headless one-shot, no
// model, no transcript, no pricing, no approval parsing, no system-prompt
// injection. DetectState always returns Unknown — a shell has no stable
// "working"/"idle" marker warden could scrape — so the poller (which also skips
// terminals by Kind) learns it finished only from the exit-code capture the
// lifecycle appends (the user typing `exit`). The task prompt is intentionally
// ignored: a shell would EXECUTE whatever text it's fed, so warden never types a
// prompt into it (terminalBackend deliberately does NOT implement PromptSeeder).
type terminalBackend struct{}

// TerminalBackend returns the internal launch adapter for a terminal-kind
// session. It is the one seam by which lifecycle resolves a terminal's launch
// command now that `terminal` is no longer in the backend registry — callers key
// on the session's Kind, not a backend id, to reach it.
func TerminalBackend() Backend { return terminalBackend{} }

// --- Identity ---------------------------------------------------------------

func (terminalBackend) ID() string          { return "terminal" }
func (terminalBackend) DisplayName() string { return "Terminal (plain shell)" }

// Binary reports "sh" — the POSIX shell every supported platform ships. It is
// only used for error messages now (a terminal is never install-detected: it is
// created by kind, not discovered as a backend); the actual shell warden launches
// is $SHELL, falling back to bash then sh (see LaunchCmd).
func (terminalBackend) Binary() string { return "sh" }

func (terminalBackend) InstallHint() string {
	return "No install needed — warden opens your $SHELL (falls back to bash)."
}

// --- Launch / resume --------------------------------------------------------

// LaunchCmd returns the command typed into the tmux pane: launch the user's
// interactive login shell. The pane is already created with the session's dir as
// its working directory, so the shell opens directly "on that directory" with no
// cd needed. It is a NESTED shell (not `exec`) on purpose: the lifecycle appends
// an exit-code capture (`; printf '%s' "$?" > …`) after this command, and only a
// non-exec'd child returns control to the outer pane shell so that capture runs
// when the user types `exit` — which is how warden learns the terminal session
// ended. SessionID/Name/Model/Mode are all irrelevant to a shell and ignored.
func (terminalBackend) LaunchCmd(LaunchOpts) string {
	// $SHELL is the user's configured login shell; bash is the fallback when it is
	// unset. Started with a tty on stdin, the shell is interactive automatically —
	// no -i needed — which keeps this portable across bash/zsh.
	return `${SHELL:-bash}`
}

// ResumeCmd reports no resume support: a shell holds no session warden can pin,
// so rotate/handoff re-open a fresh terminal instead (Caps.Resume=false).
func (terminalBackend) ResumeCmd(ResumeOpts) (string, bool) { return "", false }

// LaunchPromptArg returns "" — a plain terminal has no task prompt. warden never
// seeds the prompt into the shell (that would run it as a command), so nothing is
// appended to the launch line and terminalBackend does not implement PromptSeeder.
func (terminalBackend) LaunchPromptArg(string) string { return "" }

// HeadlessCmd reports no headless one-shot: a shell has no non-interactive
// classify/summarize mode, so warden's offload path uses the local-LLM fallback.
func (terminalBackend) HeadlessCmd(string) ([]string, bool) { return nil, false }

// --- Transcript -------------------------------------------------------------

// TranscriptPath reports no transcript: a shell writes no conversation log warden
// can read, so digest/savings/token-counting degrade to "no transcript"
// (Caps.StructuredTranscript=false).
func (terminalBackend) TranscriptPath(_, _, _ string) (string, bool) { return "", false }

// ParseTranscript returns no turns: there is no transcript to normalize.
func (terminalBackend) ParseTranscript(io.Reader) ([]Turn, error) { return nil, nil }

// --- State / approval -------------------------------------------------------

// DetectState always returns Unknown: a shell prints no stable state marker, so
// warden keeps the session's current status and infers completion from the
// exit-code capture rather than the pane.
func (terminalBackend) DetectState(string) State { return StateUnknown }

// ParseApproval finds nothing: a plain terminal has no approval UI to answer.
func (terminalBackend) ParseApproval(string) (*Approval, bool) { return nil, false }

// --- System prompt / pricing ------------------------------------------------

// SystemPromptFlag reports no system-prompt injection: there is nothing to inject
// a persona/hints into (Caps.SystemPromptInject=false). terminalBackend likewise
// does not implement ContextInjector — dropping a rules file a shell never reads
// would only litter the worktree.
func (terminalBackend) SystemPromptFlag(string) (string, bool) { return "", false }

// Pricing reports none: a shell consumes no model tokens, so it is omitted from
// spend/savings entirely (Pricing ok=false).
func (terminalBackend) Pricing() (PricingTable, bool) {
	return PricingTable{}, false
}

// --- Capabilities -----------------------------------------------------------

// Capabilities declares the terminal as the fully-degraded adapter: an
// interactive pane and nothing else. Every AI feature is off, so core skips
// resume, headless, model selection, transcript digests, spend/savings, and
// system-prompt injection for a terminal session.
func (terminalBackend) Capabilities() Caps {
	return Caps{
		Resume:               false,
		Headless:             false,
		ModelSelection:       false,
		PermissionModes:      nil,
		StructuredTranscript: false,
		SystemPromptInject:   false,
		SessionIDControl:     false,
	}
}
