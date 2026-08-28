package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/handoff"
	"github.com/srjn45/warden/internal/router"
	"github.com/srjn45/warden/internal/store"
)

// SuccessorResolver resolves the backend+model a hot-swap should switch TO when the
// caller did not pin them explicitly. It is the narrow seam lifecycle depends on so
// switch.go stays testable without standing up a full store-backed router;
// *router.Resolver satisfies it directly. A tier resolution walks the quota-balanced
// weighted-headroom candidates and returns the highest-headroom eligible model.
type SuccessorResolver interface {
	ResolveTier(ctx context.Context, tier backendstore.ModelTier) (*router.Resolution, error)
	Resolve(ctx context.Context, opts router.ResolveOptions) (*router.Resolution, error)
}

// SwapReason names why a hot-swap fired. It is recorded in the handoff document and
// the agent's event log so an operator can see whether warden swapped for a full
// context window, an exhausted provider quota, or an explicit operator request.
type SwapReason string

const (
	// SwapReasonContextFill — the retiring agent's context window hit the fill
	// threshold (default 90%); its context would only grow toward a crash.
	SwapReasonContextFill SwapReason = "context_fill"
	// SwapReasonQuota — the retiring backend's provider quota hit the rolling
	// threshold (default 90%); further work risks a hard rate-limit stall.
	SwapReasonQuota SwapReason = "quota"
	// SwapReasonManual — an operator (or a Stage-4 CLI/MCP verb) requested the swap.
	SwapReasonManual SwapReason = "manual"
)

// SwapRequest carries the inputs for a mid-session hot-swap. The successor is chosen
// by the first of these that is set: an explicit Backend (+optional Model), else a
// Tier resolved through the Resolver, else — when only Model is set — the current
// backend with a new model. At least one selector must be present.
type SwapRequest struct {
	Backend string                 // explicit successor backend id (e.g. "antigravity"); "" ⇒ resolve or keep current
	Model   string                 // explicit successor model id; "" ⇒ backend default / resolver choice
	Tier    backendstore.ModelTier // resolve the successor via the router at this tier (used when Backend is "")
	Role    string                 // role to resolve the tier from when Tier is empty (router role→tier mapping)
	Reason  SwapReason             // why the swap fired (recorded); defaults to manual when empty
	Prompt  string                 // optional extra instruction appended to the successor's continuation prompt
}

// SwapResult reports what a completed hot-swap did: the resolved successor, the
// handoff file written, and the extracted context. The daemon persists the mutated
// session (Backend/Model/ClaudeSessionID) and can surface HandoffPath to the operator.
type SwapResult struct {
	Session      *store.Session  `json:"session"`
	Handoff      handoff.Handoff `json:"handoff"`
	HandoffPath  string          `json:"handoff_path"`
	FromBackend  string          `json:"from_backend"`
	FromModel    string          `json:"from_model,omitempty"`
	ToBackend    string          `json:"to_backend"`
	ToModel      string          `json:"to_model,omitempty"`
	Reason       SwapReason      `json:"reason"`
	ResolverUsed bool            `json:"resolver_used"` // true when the successor was chosen by the router (vs pinned)
}

// ErrNoSwapTarget is returned when a SwapRequest names no successor at all (no
// backend, no model, and no resolvable tier/role) — there is nothing to swap to.
var ErrNoSwapTarget = fmt.Errorf("hot-swap: no successor backend, model, or tier specified")

// ErrNoResolver is returned when a SwapRequest asks for tier/role resolution but no
// Resolver is wired — the swap cannot pick a successor, so it is refused rather than
// guessing.
var ErrNoResolver = fmt.Errorf("hot-swap: tier/role resolution requested but no resolver is configured")

// HotSwap retires the CLI process backing sess and launches a successor backend in
// the SAME worktree, carrying forward a structured context handoff so the new agent
// continues the work rather than starting cold. The flow is:
//
//  1. Extract context from the retiring agent's transcript (Goal, Decisions Log,
//     Modified Files, Immediate Next Step), enriched with the live git-diff state.
//  2. Persist it as `.warden/handoff-<id>.md` in the worktree.
//  3. Resolve the successor backend+model (explicit pin, or the quota-balanced
//     router by tier/role).
//  4. Retire the active CLI (kill the tmux session).
//  5. Launch the successor in the same worktree, injecting the handoff via the
//     backend's AGENTS.md rules file (ContextInjector) and seeding a continuation
//     prompt that points the successor at the handoff file.
//
// It mutates sess in place (Backend, Model, ClaudeSessionID) so the caller persists
// the record, and returns a SwapResult describing the swap. The worktree, branch,
// and agent id are preserved — a hot-swap changes WHO is driving, not WHERE. The
// retiring agent's in-flight turn is discarded (like force-compact / role-switch):
// a swap is an explicit, threshold- or operator-driven action.
func (l *Lifecycle) HotSwap(ctx context.Context, sess *store.Session, req SwapRequest) (*SwapResult, error) {
	if sess == nil {
		return nil, fmt.Errorf("hot-swap: nil session")
	}
	if fi, err := os.Stat(sess.Workdir); err != nil || !fi.IsDir() {
		return nil, ErrWorkdirMissing
	}

	fromBackend := normalizeBackendID(sess.Backend)
	fromModel := sess.Model

	// 1. Extract context from the retiring agent's transcript.
	h := l.extractHandoff(ctx, sess)
	h.SessionID = sess.ID
	h.Backend = fromBackend
	h.Model = fromModel
	if req.Reason == "" {
		req.Reason = SwapReasonManual
	}
	h.Reason = string(req.Reason)
	h.GeneratedAt = l.nowUTC().Format(time.RFC3339)

	// 3. Resolve the successor (before retiring the old CLI, so a resolution failure
	//    leaves the current agent running untouched).
	toBackendID, toModel, resolverUsed, err := l.resolveSuccessor(ctx, sess, req)
	if err != nil {
		return nil, err
	}
	toBackend := l.backendFor(toBackendID)
	h.SuccessorBackend = toBackend.ID()
	h.SuccessorModel = toModel
	h.SystemContext = l.swapSystemContext(sess, fromBackend, fromModel, toBackend.ID(), toModel)

	// 2. Persist the handoff markdown into the worktree.
	handoffPath, err := handoff.Write(sess.Workdir, h)
	if err != nil {
		// A failed handoff write is fatal: without it the successor would launch
		// blind. Leave the current agent running.
		return nil, fmt.Errorf("hot-swap: persist handoff: %w", err)
	}

	// 4. Retire the active CLI (kill the tmux session if it is alive).
	if _, err := l.run.Run(ctx, "", "tmux", "has-session", "-t", sess.TmuxSession); err == nil {
		l.killSession(sess.TmuxSession)
	}

	// 5. Launch the successor in the same worktree with the handoff injected.
	if err := l.launchSuccessor(ctx, sess, toBackend, toModel, handoffPath, h, req); err != nil {
		return nil, fmt.Errorf("hot-swap: launch successor %s: %w", toBackend.ID(), err)
	}

	// Mutate the session to reflect the new driver (caller persists).
	sess.Backend = toBackend.ID()
	sess.Model = toModel
	sess.UpdatedAt = l.nowUTC()

	return &SwapResult{
		Session:      sess,
		Handoff:      h,
		HandoffPath:  handoffPath,
		FromBackend:  fromBackend,
		FromModel:    fromModel,
		ToBackend:    toBackend.ID(),
		ToModel:      toModel,
		Reason:       req.Reason,
		ResolverUsed: resolverUsed,
	}, nil
}

// extractHandoff reads the retiring agent's transcript, parses it into neutral
// turns, and distils a Handoff (Goal / Decisions / Modified Files / Next Step),
// enriched with the live working-tree diff. Every step degrades gracefully: an
// unreadable/absent transcript yields an empty (but valid) handoff so a swap still
// proceeds — the successor gets the git-diff and system context even when the
// transcript is gone.
func (l *Lifecycle) extractHandoff(ctx context.Context, sess *store.Session) handoff.Handoff {
	turns := l.readTurns(sess)
	h := handoff.Extract(turns)
	h.GitDiff = strings.TrimSpace(l.GitNumstat(ctx, sess.Workdir))
	return h
}

// readTurns opens the retiring agent's transcript and parses it into warden's
// neutral turns via its backend adapter. It returns nil on any failure (no
// transcript path, unopenable file, parse error) — the caller treats an empty slice
// as "no recoverable transcript context", never as a hard error.
func (l *Lifecycle) readTurns(sess *store.Session) []agentbackend.Turn {
	path := l.transcriptPath(sess)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Debug("hot-swap: open transcript failed", "agent", sess.ID, "path", path, "err", err)
		return nil
	}
	defer f.Close()
	turns, err := l.backendFor(sess.Backend).ParseTranscript(f)
	if err != nil {
		slog.Debug("hot-swap: parse transcript failed", "agent", sess.ID, "path", path, "err", err)
		return nil
	}
	return turns
}

// resolveSuccessor decides the backend+model to swap to. Precedence:
//   - explicit Backend (with Model, or the backend's default when Model is empty);
//   - a Tier or Role, resolved through the quota-balanced router (Resolver);
//   - only a Model set ⇒ the current backend with the new model (a same-backend
//     model bump).
//
// It returns resolverUsed=true only when the router actually chose the successor.
func (l *Lifecycle) resolveSuccessor(ctx context.Context, sess *store.Session, req SwapRequest) (backendID, model string, resolverUsed bool, err error) {
	if req.Backend != "" {
		return req.Backend, req.Model, false, nil
	}
	if req.Tier != "" || req.Role != "" {
		if l.Resolver == nil {
			return "", "", false, ErrNoResolver
		}
		res, rErr := l.routerResolve(ctx, req)
		if rErr != nil {
			return "", "", false, fmt.Errorf("hot-swap: resolve successor: %w", rErr)
		}
		return res.BackendID, res.ModelID, true, nil
	}
	if req.Model != "" {
		// Same backend, new model.
		return normalizeBackendID(sess.Backend), req.Model, false, nil
	}
	return "", "", false, ErrNoSwapTarget
}

// routerResolve runs the configured resolver for req's tier/role. An explicit Tier
// takes precedence over a Role (the caller asked for a specific tier); otherwise the
// role is mapped to its default tier by the router.
func (l *Lifecycle) routerResolve(ctx context.Context, req SwapRequest) (*router.Resolution, error) {
	if req.Tier != "" {
		return l.Resolver.ResolveTier(ctx, req.Tier)
	}
	return l.Resolver.Resolve(ctx, router.ResolveOptions{Role: req.Role, AllowFallback: true})
}

// resolveSpawnTarget decides the backend+model for a FIRST spawn, mirroring
// resolveSuccessor but with one critical difference: a first spawn must ALWAYS
// succeed. Precedence (top wins):
//
//   - an explicit pinned backend (with model, or the backend's default when the
//     model is empty) — the caller chose it;
//   - an explicit or role-default model (a role's Defaults.Model is folded into
//     the request's model before this runs, so it lands here) on the default
//     backend — a model pin the router must not override;
//   - otherwise the quota-balanced router picks backend+model from
//     ResolveOptions{Role, Task, Tier, AllowFallback:true}.
//
// Unlike a hot-swap (which refuses with ErrNoResolver when nothing is wired), a
// first spawn DEGRADES gracefully: with no resolver, or when the resolver returns
// no eligible candidate / errors, it falls back to the passed backend+model
// (i.e. current behavior — the config default backend and an empty model). It
// never fails the spawn because the resolver is absent or empty.
func (l *Lifecycle) resolveSpawnTarget(ctx context.Context, roleName, taskName, tier, backend, model string) (resolvedBackend, resolvedModel string) {
	// A pinned backend wins outright (keep the model, empty ⇒ backend default).
	if backend != "" {
		return backend, model
	}
	// A pinned or role-default model on the default backend wins over the router.
	if model != "" {
		return backend, model
	}
	// Nothing pinned: let the router pick. Degrade silently when it can't so the
	// spawn still proceeds on the config default backend.
	if l.Resolver == nil {
		return backend, model
	}
	res, err := l.Resolver.Resolve(ctx, router.ResolveOptions{
		Role:          roleName,
		Task:          taskName,
		Tier:          backendstore.ModelTier(tier),
		AllowFallback: true,
	})
	if err != nil || res == nil || res.BackendID == "" {
		if err != nil {
			slog.Debug("spawn: resolver declined, using request defaults",
				"role", roleName, "task", taskName, "tier", tier, "err", err)
		}
		return backend, model
	}
	return res.BackendID, res.ModelID
}

// launchSuccessor brings up the successor backend b in the retiring agent's existing
// worktree, carrying the handoff forward. It mirrors spawnTyped's launch assembly
// (new tmux session → inject context → build launch → send-keys) minus the worktree
// creation (the worktree already exists) and re-pins a fresh session id for a
// pinning backend (a new backend session is a new conversation, not a resume).
func (l *Lifecycle) launchSuccessor(ctx context.Context, sess *store.Session, b agentbackend.Backend, model, handoffPath string, h handoff.Handoff, req SwapRequest) error {
	// A pinning backend (Claude) needs a fresh warden-minted session id for the new
	// conversation; a non-pinning backend (codex/antigravity) mints its own, so leave
	// the id empty for the poller's discover-then-pin.
	if b.Capabilities().SessionIDControl {
		id, err := store.NewSessionID()
		if err != nil {
			return err
		}
		sess.ClaudeSessionID = id
	} else {
		sess.ClaudeSessionID = ""
	}

	mode := sess.PermissionMode
	if mode == "" {
		mode = l.config().GetDefaultPermissionMode()
	}

	// Recreate the tmux session in the SAME worktree.
	if err := l.newAgentSession(ctx, "", sess.ID, sess.Workdir); err != nil {
		return err
	}

	// The continuation prompt points the successor at the handoff file and restates
	// the goal + next step, so even a backend that cannot read AGENTS.md still picks
	// up the thread. Write it as a prompt file (file-backed positional, like every
	// other launch path) so a multi-line prompt types as one physical line.
	promptFile, err := l.writePromptFile(ctx, sess.ID, continuationPrompt(handoffPath, h, req))
	if err != nil {
		l.killSession(sess.ID)
		return err
	}

	// Inject the handoff into the backend's AGENTS.md rules file (ContextInjector
	// backends, e.g. codex); a flag/positional backend contributes nothing here and
	// relies on the continuation prompt instead. Best-effort: a write failure degrades
	// (the prompt still carries the handoff) rather than aborting the swap.
	persona := personaGuidance(sess.Role)
	if err := l.injectContext(b, sess.Workdir,
		persona,
		handoffGuidance(handoffPath, h),
	); err != nil {
		slog.Warn("hot-swap: context injection failed", "agent", sess.ID, "backend", b.ID(), "err", err)
	}

	base := b.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: sess.ClaudeSessionID, Name: sess.ID, Model: l.launchModel(b, model), Mode: mode,
	})
	hints := l.systemPromptHints(ctx, b, sess.ID,
		hintSpec{persona != "", persona},
		hintSpec{true, handoffGuidance(handoffPath, h)},
	)
	launch := base + hints + l.guardSettings(b, sess.ID) + l.promptArg(b, promptFile) + l.exitSuffix(sess.ID)
	if out, err := l.run.Run(ctx, sess.Workdir, "tmux", "send-keys", "-t", sess.ID, launch, "Enter"); err != nil {
		l.killSession(sess.ID)
		return fmt.Errorf("tmux send-keys: %w: %s", err, out)
	}
	l.seedInteractivePrompt(b, sess.ID, continuationPrompt(handoffPath, h, req))
	return nil
}

// continuationPrompt is the initial task prompt seeded onto the successor: it tells
// the new agent it is picking up an in-progress session, points it at the handoff
// file, and restates the goal + next step inline so it can start even before reading
// the file. req.Prompt (an operator's extra instruction) is appended when set.
func continuationPrompt(handoffPath string, h handoff.Handoff, req SwapRequest) string {
	var b strings.Builder
	b.WriteString("You are taking over an in-progress warden session from a previous agent. ")
	fmt.Fprintf(&b, "A structured handoff has been written to %s — read it first.\n\n", handoffPath)
	if h.Goal != "" {
		fmt.Fprintf(&b, "Goal: %s\n\n", h.Goal)
	}
	if h.NextStep != "" {
		fmt.Fprintf(&b, "Immediate next step: %s\n\n", h.NextStep)
	}
	b.WriteString("Continue the work from where the previous agent left off.")
	if strings.TrimSpace(req.Prompt) != "" {
		fmt.Fprintf(&b, "\n\nAdditional instruction: %s", strings.TrimSpace(req.Prompt))
	}
	return b.String()
}

// handoffGuidance is the raw addendum text delivered to a backend's system prompt /
// AGENTS.md rules file: a pointer to the handoff file plus the goal, so a resumed
// agent that reads its rules file on startup finds the context even if the launch
// prompt scrolled away. Empty only when there is genuinely nothing to say (no goal
// and no path), in which case the injection is skipped.
func handoffGuidance(handoffPath string, h handoff.Handoff) string {
	if handoffPath == "" && h.Goal == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("This session was hot-swapped from a previous agent mid-task. ")
	if handoffPath != "" {
		fmt.Fprintf(&b, "Read the structured handoff at %s before continuing. ", handoffPath)
	}
	if h.Goal != "" {
		fmt.Fprintf(&b, "The session goal is: %s", h.Goal)
	}
	return strings.TrimSpace(b.String())
}

// swapSystemContext renders the System Context block for the handoff: the swap
// direction and the worktree/branch the successor inherits.
func (l *Lifecycle) swapSystemContext(sess *store.Session, fromBackend, fromModel, toBackend, toModel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hot-swap: %s → %s (same worktree, same branch).\n", describeBackend(fromBackend, fromModel), describeBackend(toBackend, toModel))
	if sess.Worktree != "" {
		fmt.Fprintf(&b, "Worktree: %s\n", sess.Worktree)
	}
	if sess.Workdir != "" {
		fmt.Fprintf(&b, "Working directory: %s\n", sess.Workdir)
	}
	if sess.Branch != "" {
		fmt.Fprintf(&b, "Branch: %s\n", sess.Branch)
	}
	if sess.Repo != "" {
		fmt.Fprintf(&b, "Repository: %s\n", sess.Repo)
	}
	return strings.TrimRight(b.String(), "\n")
}

// describeBackend formats a "backend (model)" label, dropping the parenthetical when
// the model is empty.
func describeBackend(backend, model string) string {
	if backend == "" {
		backend = "unknown"
	}
	if model == "" {
		return backend
	}
	return backend + " (" + model + ")"
}

// backendID normalizes an empty session backend to the default (Claude), matching
// backendFor's fallback so the recorded provenance never reads "".
func normalizeBackendID(id string) string {
	if id == "" {
		return agentbackend.DefaultID
	}
	return id
}

// nowUTC returns the current UTC time. A tiny seam so hot-swap timestamps are
// deterministic under test via the same override the resolver uses is unnecessary
// here (the daemon does not inject a clock into lifecycle), so this reads the wall
// clock directly; tests assert on structure, not exact timestamps.
func (l *Lifecycle) nowUTC() time.Time { return time.Now().UTC() }
