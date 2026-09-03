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
	"context"
	"io"
	"sort"
	"strings"
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

// Cost returns the dollar cost for a token pair, resolving full model IDs by
// family substring before falling back to the backend's conservative default.
func (t PricingTable) Cost(model string, inputTokens, outputTokens int) float64 {
	m := strings.ToLower(strings.TrimSpace(model))
	price := t.Default
	if exact, ok := t.Models[m]; ok {
		price = exact
	} else {
		families := make([]string, 0, len(t.Models))
		for family := range t.Models {
			families = append(families, family)
		}
		sort.Strings(families)
		for _, family := range families {
			if strings.Contains(m, strings.ToLower(family)) {
				price = t.Models[family]
				break
			}
		}
	}
	return float64(inputTokens)*price.InputPerMTok/1_000_000 +
		float64(outputTokens)*price.OutputPerMTok/1_000_000
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

	// LaunchPromptArg returns the fragment that seeds an initial task prompt onto
	// the launch command, given the absolute path of a file holding the prompt
	// (read back via "$(cat …)" so a multi-line prompt types as one physical line).
	// Agents disagree on how the first message is delivered: Claude takes it as a
	// trailing positional argument, while Aider takes it via --message. The adapter
	// owns the shell-quoting of promptFile. The returned fragment includes its own
	// leading space so it concatenates directly onto LaunchCmd.
	LaunchPromptArg(promptFile string) string

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

// PromptSeeder is an optional Backend extension implemented by agents whose
// interactive UI takes the initial task prompt only as typed input AFTER the
// program has started — it has neither a launch-line positional nor a prompt flag
// (Crush and Goose's interactive REPLs; Aider's interactive REPL). For such a
// backend, LaunchPromptArg returns "" (there is nothing to put on the launch line)
// and the lifecycle instead launches the bare interactive agent, waits for its
// input to become ready, then types the prompt into the pane and presses Enter
// (via the same bracketed-paste path as an operator's message). Backends that seed
// the prompt on the launch line (Claude positional, Aider --message historically,
// OpenCode --prompt, …) do NOT implement this interface.
type PromptSeeder interface {
	// PromptText returns the literal text to type into the started UI for the given
	// task prompt. ok=false disables post-launch seeding for this invocation (e.g.
	// an empty prompt — an interactive agent the operator drives by hand).
	PromptText(prompt string) (text string, ok bool)

	// ReadyMarker is a substring that appears in the agent's captured tmux pane once
	// its input is ready to receive the prompt. The lifecycle polls the pane for it
	// before typing; an empty marker falls back to a fixed settle delay.
	ReadyMarker() string
}

// InputReadiness is an optional backend extension for TUIs that expose a
// positive, backend-specific indication that their composer can accept input.
// It is used by destructive control flows (currently force-compact) after an
// interrupt; a generic stored "idle" status is not a sufficient acknowledgement
// because it may be stale or inferred from a temporarily quiet pane.
type InputReadiness interface {
	InputReady(pane string) bool
}

// Reviewer is an optional Backend extension implemented by agents that expose a
// NON-INTERACTIVE, diff-scoped code review as a first-class subcommand (Codex:
// `codex review --uncommitted|--base <branch>|--commit <sha>`). It is the
// agent-native counterpart to warden's configured `.warden/check.yml` checks and
// its spawned `pr-review` agent: instead of running project test commands or
// standing up a whole reviewer session, warden asks the backend's OWN reviewer to
// read the working diff and report findings. Additive and on-top — a backend that
// does not implement it is simply not offered `wd review` (the verb reports the
// backend has no native review and points at `pr-review`/`wd check`).
type Reviewer interface {
	// ReviewCmd returns the argv for a one-shot review run in the agent's workdir.
	// Scope selects what to review (uncommitted working tree, or a base branch).
	// When opts.Structured is set, the adapter returns a NON-INTERACTIVE, parseable
	// form whose machine-readable result the caller pairs with StructuredReviewer to
	// read back neutral findings (codex: `codex exec review`, whose native review
	// output persists to the rollout); otherwise it returns the prose/streamed form
	// (codex: plain `codex review`). ok=false ⇒ this backend offers no native review.
	ReviewCmd(opts ReviewOpts) (argv []string, ok bool)
}

// ReviewOpts is the neutral input for a Reviewer.ReviewCmd call. The CLI verb
// (`wd review`) populates it from its flags; an unset Scope means "uncommitted"
// (the agent's working tree).
type ReviewOpts struct {
	Scope      string // "uncommitted" (default) | "base"
	Base       string // base branch when Scope=="base"
	Structured bool   // request a machine-readable result (pair with StructuredReviewer); false ⇒ prose stream
	Prompt     string // optional extra review instructions
}

// StructuredReviewer is an optional companion to Reviewer for agents whose NATIVE
// review emits a machine-readable result the caller can normalize into neutral
// findings. It is the seam behind `wd review --json`: the CLI runs the Structured
// ReviewCmd form, then asks the backend to read back its own structured output as a
// neutral ReviewFindings. The result shape is the BACKEND'S — warden does not impose
// a schema on the agent (verified against codex v0.142.3: `codex review` ignores a
// caller `--output-schema` and owns its `review_output` structure, persisted to the
// session rollout); this seam normalizes that native shape into warden's neutral one.
// Additive and on-top: a backend without it still offers prose `wd review`, just not
// `--json`.
type StructuredReviewer interface {
	// ParseReviewResult locates the structured result of the review that just ran in
	// workdir and normalizes it into neutral findings. It is meaningful only right
	// after a Structured ReviewCmd run in the same workdir. ok=false ⇒ no structured
	// result was found (e.g. the model produced none, or the run did not complete a
	// review) — the caller reports a clean "no structured review output" rather than
	// erroring. A non-nil error signals a read/parse failure the caller surfaces.
	ParseReviewResult(workdir string) (findings ReviewFindings, ok bool, err error)
}

// ReviewFindings is warden's neutral, machine-readable result of a backend's native
// diff review — the normalization target a StructuredReviewer maps its own review
// output INTO, so `wd review --json` emits one neutral JSON shape regardless of which
// backend produced it. warden OWNS this neutral shape (not the schema the agent emits).
type ReviewFindings struct {
	Summary  string          `json:"summary"`           // overall human-readable explanation
	Verdict  string          `json:"verdict,omitempty"` // optional overall verdict in the backend's phrasing (codex: overall_correctness)
	Findings []ReviewFinding `json:"findings"`          // zero or more located findings (never nil in JSON: emitted as [])
}

// ReviewFinding is one located issue from a structured review.
type ReviewFinding struct {
	File     string `json:"file,omitempty"`     // repo-relative path when the backend gives a location; "" otherwise
	Line     int    `json:"line,omitempty"`     // 1-based start line; 0 when the backend gives no line
	Severity string `json:"severity,omitempty"` // neutral "info" | "warning" | "error" (backend signal mapped onto these)
	Title    string `json:"title,omitempty"`    // short headline, when the backend separates it from the body
	Message  string `json:"message"`            // the finding text
}

// ModelLister is an optional Backend extension for agents whose model set is a
// live, multi-vendor menu discoverable at runtime (Antigravity: `agy models`;
// Cursor: `--list-models`) rather than a hard-coded alias table. It surfaces the
// real, currently-available menu to warden's model picker — the agent-native
// counterpart to the static `lifecycle/models.go` alias set — so the operator can
// see exactly which ids the backend will accept on `--model` right now. Listing the
// menu is a metadata read, not a generation request: implementations run the
// backend's own list subcommand and return ok=false on any command error so the verb
// degrades cleanly. Additive and on-top — a backend without it (Claude, with a static
// model set) keeps the current resolved-id behavior and is simply not offered the
// live menu (`wd models` reports it and points at `--model` with a known id).
type ModelLister interface {
	// ListModels runs the backend's native model-menu command and returns the live
	// menu as a clean slice of model ids (one per entry, order preserved as the
	// backend lists them). ok=false ⇒ no live menu is available (command missing or
	// errored) — the caller reports a clean degrade rather than erroring.
	ListModels() (models []string, ok bool)
}

// UsageLimit is one distinct provider-owned allowance/reset window returned by
// an agent backend's native usage interface.
type UsageLimit struct {
	ID               string     `json:"id"`
	Scope            string     `json:"scope"`
	Label            string     `json:"label"`
	ModelFamilies    []string   `json:"model_families"`
	Models           []string   `json:"models"`
	UsedPercent      *float64   `json:"used_percent"`
	RemainingPercent *float64   `json:"remaining_percent,omitempty"`
	DurationMinutes  *int       `json:"duration_minutes,omitempty"`
	ResetsAt         *time.Time `json:"resets_at"`
	LimitState       *string    `json:"limit_state,omitempty"`
}

// UsageAccount carries account/plan details returned alongside usage limits.
type UsageAccount struct {
	Plan        string `json:"plan,omitempty"`
	LoginMethod string `json:"login_method,omitempty"`
}

// UsageResult is the structured result returned by a UsageLimiter implementation.
type UsageResult struct {
	Status     string        `json:"status"` // "ok", "rate_limited", "unauthenticated", "unavailable", etc.
	Account    *UsageAccount `json:"account,omitempty"`
	Usage      []UsageLimit  `json:"usage"`
	ErrorCode  string        `json:"error_code,omitempty"`
	ErrorMsg   string        `json:"error_message,omitempty"`
	ObservedAt time.Time     `json:"observed_at"`
}

// UsageLimiter is an optional Backend extension for agents that expose a backend-native
// interface for retrieving provider usage limits and quotas (e.g. Antigravity models RPC,
// Cursor Connect-RPC, Codex app-server).
type UsageLimiter interface {
	// FetchUsage queries the provider's live usage limits. ok=false indicates the backend
	// has no supported structured usage interface.
	FetchUsage(ctx context.Context) (result UsageResult, ok bool)
}

// SessionIDDiscoverer is an optional Backend extension implemented by agents that
// mint their OWN session id at launch (Caps.SessionIDControl=false) — warden
// cannot pin the id up front, so LaunchCmd ignores LaunchOpts.SessionID and the
// agent generates a UUID of its own. For such a backend warden leaves
// Session.ClaudeSessionID empty at spawn (the dir-scoped transcript fallback:
// newest rollout in the workdir), then, once the agent has written its first
// transcript, discovers the agent-generated id post-launch and pins it ONCE
// (the poller persists it to the session). After that, the same id flows into
// TranscriptPath / ResumeCmd so transcript reads and resume key off the exact id
// instead of dir-scoping — exact-id fidelity even with more than one session per
// workdir. Backends that let warden assign the id (Claude, SessionIDControl=true)
// do NOT implement this interface: by construction the discovery path never runs
// for them (the id is already pinned at spawn).
type SessionIDDiscoverer interface {
	// DiscoverSessionID locates the agent-generated session id for the agent
	// running in workdir, given the backend's transcript root (projectsDir, the
	// Claude-specific root; ignored by backends that resolve their own home, same
	// as TranscriptPath's projectsDir argument). ok=false when no id can be found
	// yet (the agent has not written a transcript) — the caller keeps the empty id
	// and retries on a later tick, so this is safe to call repeatedly. The
	// signature mirrors TranscriptPath(projectsDir, workdir, sessionID) minus the
	// not-yet-known id.
	DiscoverSessionID(projectsDir, workdir string) (id string, ok bool)
}

// SessionForker is an optional Backend extension implemented by agents that can
// BRANCH a recorded session into a new DIVERGENT one (Codex: `codex fork <id>`).
// It complements warden's snapshot (linear worktree+transcript rollback) and its
// rotate/handoff (which hand off the TASK but drop the conversation) with
// CONVERSATIONAL forking — explore an alternative reasoning path from a recorded
// session WITHOUT discarding the original, as a new warden-managed agent.
//
// A fork is structurally a managed spawn whose launch command is the fork verb, so
// it returns a tmux-pane command string exactly like LaunchCmd/ResumeCmd (NOT an
// argv): the spawn path types it into the new agent's pane and appends the same
// hint/prompt/exit suffixes. The initial prompt is delivered via the existing
// LaunchPromptArg seam (file-backed positional), NOT through this method.
//
// Additive and on-top: a backend that does not implement it is simply not forkable
// (a spawn with fork_from set against such a backend is rejected with a clear
// message). Claude implements none of this — by construction the fork path never
// runs for it, keeping Claude's launch byte-identical and regression-locked.
type SessionForker interface {
	// ForkCmd returns the launch command that forks SourceSessionID into a new
	// session, run in the new agent's worktree. ok=false ⇒ the source id is empty
	// or this backend cannot fork the given input (the caller reports a clean
	// "cannot fork" rather than launching a bare agent).
	ForkCmd(opts ForkOpts) (cmd string, ok bool)
}

// ForkOpts is the neutral input for a SessionForker.ForkCmd call. SourceSessionID
// is the BACKEND'S recorded session id (the source agent's pinned id, e.g. codex's
// rollout UUID) — never warden's agent id. Model/Mode mirror LaunchOpts and are
// resolved by the caller before the call.
type ForkOpts struct {
	SourceSessionID string // backend session id to fork from (REQUIRED; ok=false if empty)
	Name            string // display label for the new session (warden agent id)
	Model           string // already-resolved model id
	Mode            string // permission/approval mode
	// Workdir is the fork's OWN worktree (a fresh sibling off the source's branch).
	// A fork inherently runs in a different cwd than the source's recorded one
	// (dir-scoped discover-then-pin needs each agent its own cwd), and codex, seeing
	// the mismatch, otherwise prompts an interactive "Choose working directory" picker
	// whose default is the SOURCE dir (the wrong, unsafe choice). The adapter sets it
	// to the fork worktree so the implementation can pin the working root explicitly
	// (codex: `-C <dir>`) and suppress that picker. Verified live, codex 0.142.3.
	Workdir string
}

// ContextInjector is an optional Backend extension implemented by agents that have
// NO launch-time system-prompt flag (Caps.SystemPromptInject=false) but DO read a
// rules file (AGENTS.md) from their working directory on startup — Codex is the
// pilot. It is the file-drop counterpart to SystemPromptFlag's
// `--append-system-prompt` fragment: lifecycle assembles warden's
// collab/git/pipeline addendum once and, for a backend that injects, writes that
// SAME text into the worktree post-creation / pre-launch so the agent reads
// warden's coordination hints when it starts. A backend that implements neither
// SystemPromptFlag (returns ok=false) nor this interface contributes nothing — the
// addendum is simply skipped, exactly as it is today (design §5.3 / §6 step 5).
//
// Backends with a launch-time flag (Claude, SystemPromptInject=true) do NOT
// implement this interface: by construction the injection path never runs for them
// (the addendum already rides the launch command via SystemPromptFlag), which keeps
// Claude's launch byte-identical and regression-locked.
type ContextInjector interface {
	// InjectContext delivers text (warden's already-assembled addendum) into the
	// agent's workdir by writing the backend's rules file. The implementation MUST be
	// idempotent — relaunch/resume re-invokes it, so it replaces any prior
	// warden-managed section in place rather than duplicating it — and MUST preserve
	// a user's pre-existing rules file (merge a clearly-delimited warden block, never
	// clobber). An empty workdir or text is a no-op (returns nil).
	InjectContext(workdir, text string) error
}

// SystemPromptFiler is an optional Backend extension for a flag-based backend
// (SystemPromptInject=true) that can take its system-prompt addendum from a FILE
// read at launch instead of as an inline literal. It is the file-backed
// counterpart to SystemPromptFlag: same flag, but the value is a shell command
// substitution that reads the file when the launch command runs.
//
// Why this exists: warden's collab/git/pipeline addendum is ~1.6 KB. Typed inline
// after the other launch flags, the whole command exceeds the tty canonical-mode
// line limit (MAX_CANON / MAX_INPUT = 1024 bytes on macOS/BSD; larger on Linux),
// so the kernel silently DROPS every byte past 1024 — the command is truncated
// mid-flag and the agent never starts. File-backing the addendum (the same
// mechanism the prompt already uses via "$(cat …)") keeps the typed line tiny and
// platform-independent. A backend that does not implement this falls back to the
// inline SystemPromptFlag, so behaviour is unchanged where the file path is
// unavailable (e.g. no hints dir configured).
type SystemPromptFiler interface {
	// SystemPromptFileFlag returns the launch fragment (leading space, so it
	// concatenates onto LaunchCmd) that injects the contents of path as a
	// system-prompt addendum, reading path when the launch command runs. ok=false
	// means the backend cannot file-back and the caller should use SystemPromptFlag.
	SystemPromptFileFlag(path string) (fragment string, ok bool)
}
