package lifecycle

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
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
// agent (AiCli/Model/AICLISessionID) and can surface HandoffPath to the operator.
type SwapResult struct {
	Agent        *agentstore.Agent `json:"session"`
	Handoff      handoff.Handoff   `json:"handoff"`
	HandoffPath  string            `json:"handoff_path"`
	FromBackend  string            `json:"from_backend"`
	FromModel    string            `json:"from_model,omitempty"`
	ToBackend    string            `json:"to_backend"`
	ToModel      string            `json:"to_model,omitempty"`
	Reason       SwapReason        `json:"reason"`
	ResolverUsed bool              `json:"resolver_used"` // true when the successor was chosen by the router (vs pinned)
	// FromMode/ToMode are the permission mode before and after the swap; they
	// differ only on a cross-backend swap that had to translate or replace the mode.
	FromMode string `json:"from_mode,omitempty"`
	ToMode   string `json:"to_mode,omitempty"`
	// ModeNote explains a mode change for the agent's event log; "" when unchanged.
	ModeNote string `json:"mode_note,omitempty"`
}

// ErrLaunchFailed is matched (errors.Is) by the error HotSwap returns when the
// successor process exited immediately after launch.
var ErrLaunchFailed = fmt.Errorf("launch_failed")

// LaunchFailedError reports a successor that exited right after launch. The agent
// record is left untouched (previous backend/model/mode) so the swap can be retried
// or the agent restored; Output carries the first lines of the pane.
type LaunchFailedError struct {
	Backend  string
	ExitCode int
	HasExit  bool
	Output   string
}

func (e *LaunchFailedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hot-swap: launch_failed: successor %s exited immediately", e.Backend)
	if e.HasExit {
		fmt.Fprintf(&b, " (exit %d)", e.ExitCode)
	}
	if e.Output != "" {
		b.WriteString(": " + e.Output)
	}
	return b.String()
}

// Is makes errors.Is(err, ErrLaunchFailed) true.
func (e *LaunchFailedError) Is(target error) bool { return target == ErrLaunchFailed }

// ErrNoSwapTarget is returned when a SwapRequest names no successor at all (no
// backend, no model, and no resolvable tier/role) — there is nothing to swap to.
var ErrNoSwapTarget = fmt.Errorf("hot-swap: no successor backend, model, or tier specified")

// ErrNoResolver is returned when a SwapRequest asks for tier/role resolution but no
// Resolver is wired — the swap cannot pick a successor, so it is refused rather than
// guessing.
var ErrNoResolver = fmt.Errorf("hot-swap: tier/role resolution requested but no resolver is configured")

// HotSwap retires the CLI process backing agent and launches a successor backend in
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
// It mutates agent in place (AiCli, Model, AICLISessionID) so the caller persists
// the record, and returns a SwapResult describing the swap. The worktree, branch,
// and agent id are preserved — a hot-swap changes WHO is driving, not WHERE. The
// retiring agent's in-flight turn is discarded (like force-compact / role-switch):
// a swap is an explicit, threshold- or operator-driven action.
func (l *Lifecycle) HotSwap(ctx context.Context, agent *agentstore.Agent, req SwapRequest) (*SwapResult, error) {
	if agent == nil {
		return nil, fmt.Errorf("hot-swap: nil agent")
	}
	if fi, err := os.Stat(agent.Workdir); err != nil || !fi.IsDir() {
		return nil, ErrWorkdirMissing
	}

	fromBackend := normalizeBackendID(agent.AiCli)
	fromModel := agent.Model

	// A handoff built from a transcript that is really another live session's would
	// hand the successor the wrong conversation: refuse instead (the agent keeps
	// running untouched) until this session's conversation is pinned.
	if l.transcriptAmbiguous(agent) {
		return nil, ErrAmbiguousTranscript
	}

	// 1. Extract context from the retiring agent's transcript.
	h := l.extractHandoff(ctx, agent)
	h.SessionID = agent.ID
	h.Backend = fromBackend
	h.Model = fromModel
	if req.Reason == "" {
		req.Reason = SwapReasonManual
	}
	h.Reason = string(req.Reason)
	h.GeneratedAt = l.nowUTC().Format(time.RFC3339)

	// 3. Resolve the successor (before retiring the old CLI, so a resolution failure
	//    leaves the current agent running untouched).
	toBackendID, toModel, resolverUsed, err := l.resolveSuccessor(ctx, agent, req)
	if err != nil {
		return nil, err
	}
	toBackend := l.backendFor(toBackendID)
	h.SuccessorBackend = toBackend.ID()
	h.SuccessorModel = toModel
	h.SystemContext = l.swapSystemContext(agent, fromBackend, fromModel, toBackend.ID(), toModel)

	// 2. Persist the handoff markdown into the worktree.
	handoffPath, err := handoff.Write(agent.Workdir, h)
	if err != nil {
		// A failed handoff write is fatal: without it the successor would launch
		// blind. Leave the current agent running.
		return nil, fmt.Errorf("hot-swap: persist handoff: %w", err)
	}

	// 4. Retire the active CLI (kill the tmux session if it is alive).
	if l.Proc().HasSession(ctx, agent.TmuxSession) {
		l.killSession(agent.TmuxSession)
	}

	// 5. Launch the successor in the same worktree with the handoff injected. The
	//    mode is translated into the successor's vocabulary first; the agent record
	//    is only mutated once the successor is confirmed running, so a failed swap
	//    leaves the previous backend/model/mode intact for retry or restore.
	prevSessionID := agent.AICLISessionID
	fromMode := agent.PermissionMode
	toMode, modeNote := l.successorMode(agent, l.backendFor(fromBackend), toBackend)
	if err := l.launchSuccessor(ctx, agent, toBackend, toModel, toMode, handoffPath, h, req); err != nil {
		agent.AICLISessionID = prevSessionID
		return nil, fmt.Errorf("hot-swap: launch successor %s: %w", toBackend.ID(), err)
	}
	if lerr := l.verifySuccessorRunning(ctx, agent, toBackend); lerr != nil {
		agent.AICLISessionID = prevSessionID
		return nil, lerr
	}

	// Mutate the agent to reflect the new driver (caller persists).
	agent.PermissionMode = toMode
	agent.AiCli = toBackend.ID()
	agent.Model = toModel
	if l.CapacityResolver != nil {
		binding, bindErr := l.CapacityResolver.Resolve(ctx, agent.AiCli, agent.Model)
		if bindErr != nil {
			return nil, fmt.Errorf("hot-swap: resolve capacity binding: %w", bindErr)
		}
		agent.QuotaBinding = binding
	}
	agent.UpdatedAt = l.nowUTC()

	return &SwapResult{
		Agent:        agent,
		Handoff:      h,
		HandoffPath:  handoffPath,
		FromBackend:  fromBackend,
		FromModel:    fromModel,
		ToBackend:    toBackend.ID(),
		ToModel:      toModel,
		Reason:       req.Reason,
		ResolverUsed: resolverUsed,
		FromMode:     fromMode,
		ToMode:       toMode,
		ModeNote:     modeNote,
	}, nil
}

// successorMode returns the permission mode the successor launches with plus a note
// when it differs from the stored one. A same-backend swap keeps the stored mode
// (config default when none). A cross-backend swap translates the stored mode by
// intent through the backends' own tables; with no equivalent it falls back to the
// role default, then the config default for the successor — never a mode the
// successor does not accept, and never more permissive than a known weaker intent.
func (l *Lifecycle) successorMode(agent *agentstore.Agent, from, to agentbackend.Backend) (mode, note string) {
	stored := agent.PermissionMode
	if from.ID() == to.ID() {
		if stored == "" {
			stored = l.config().GetDefaultPermissionMode()
		}
		return stored, ""
	}
	if stored != "" {
		if m, ok := agentbackend.TranslateMode(from, to, stored); ok {
			if m == stored {
				return m, ""
			}
			return m, fmt.Sprintf("permission_mode %q (%s) translated to %q (%s) by intent", stored, from.ID(), m, to.ID())
		}
	}
	// Intent of the stored mode bounds the fallback so it is not more permissive.
	var storedIntent agentbackend.PermissionIntent
	if fm, ok := from.(agentbackend.PermissionMapper); ok && stored != "" {
		storedIntent, _ = fm.ModeIntent(stored)
	}
	m := l.fallbackMode(agent.Role, to, storedIntent)
	if stored == "" {
		return m, ""
	}
	return m, fmt.Sprintf("permission_mode %q (%s) has no equivalent on %s; fell back to %q", stored, from.ID(), to.ID(), m)
}

// fallbackMode picks a valid mode for to: the role default, then the config
// default (translated from the config's Claude vocabulary when needed), then the
// backend's own default posture. A candidate whose intent is skip-all is rejected
// when the stored intent is known and weaker.
func (l *Lifecycle) fallbackMode(role string, to agentbackend.Backend, storedIntent agentbackend.PermissionIntent) string {
	tm, _ := to.(agentbackend.PermissionMapper)
	allowed := func(m string) bool {
		if m == "" || !agentbackend.ModeAccepted(to, m) {
			return false
		}
		if tm != nil && storedIntent != "" && storedIntent != agentbackend.IntentSkipAll {
			if i, ok := tm.ModeIntent(m); ok && i == agentbackend.IntentSkipAll {
				return false
			}
		}
		return true
	}
	if role != "" {
		req := &SpawnRequest{Role: role, Backend: to.ID()}
		applyRoleBackendMode(req)
		if allowed(req.PermissionMode) {
			return req.PermissionMode
		}
	}
	cfg := l.config().GetDefaultPermissionMode()
	if allowed(cfg) {
		return cfg
	}
	if cfg != "" {
		if m, ok := agentbackend.TranslateMode(l.backendFor(""), to, cfg); ok && allowed(m) {
			return m
		}
	}
	if tm != nil {
		if m, ok := tm.ModeForIntent(agentbackend.IntentDefault); ok && allowed(m) {
			return m
		}
	}
	if modes := to.Capabilities().PermissionModes; len(modes) > 0 {
		return modes[0]
	}
	return ""
}

// defaultSwapVerifyWindow bounds the post-launch liveness check: a CLI that rejects
// its arguments exits within a fraction of a second.
const defaultSwapVerifyWindow = time.Second

// verifySuccessorRunning checks, shortly after launch, that the successor did not
// exit at once. The launch line records the CLI's exit status to the agent's
// exit-file (the same signal the poller uses), so its presence — or the tmux
// session vanishing — means the process is gone. On failure it consumes the
// exit-file (so the poller does not finalize the record as errored/orphaned and
// have it reaped), leaves the pane in place and returns a LaunchFailedError.
func (l *Lifecycle) verifySuccessorRunning(ctx context.Context, agent *agentstore.Agent, b agentbackend.Backend) error {
	window := l.SwapVerifyWindow
	if window == 0 {
		window = defaultSwapVerifyWindow
	}
	if window < 0 {
		return nil
	}
	deadline := time.Now().Add(window)
	for {
		code, exited := l.ReadExit(agent.ID)
		if exited || !l.Proc().HasSession(ctx, agent.TmuxSession) {
			pane, _ := l.Proc().CapturePane(ctx, agent.TmuxSession)
			l.ClearExit(agent.ID)
			return &LaunchFailedError{Backend: b.ID(), ExitCode: code, HasExit: exited, Output: firstLines(pane, 8)}
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// firstLines returns up to n non-empty lines of s, trimmed and joined with " | ".
func firstLines(s string, n int) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
			if len(out) == n {
				break
			}
		}
	}
	return strings.Join(out, " | ")
}

// extractHandoff reads the retiring agent's transcript, parses it into neutral
// turns, and distils a Handoff (Goal / Decisions / Modified Files / Next Step),
// enriched with the live working-tree diff. Every step degrades gracefully: an
// unreadable/absent transcript yields an empty (but valid) handoff so a swap still
// proceeds — the successor gets the git-diff and system context even when the
// transcript is gone.
func (l *Lifecycle) extractHandoff(ctx context.Context, agent *agentstore.Agent) handoff.Handoff {
	turns := l.readTurns(agent)
	h := handoff.Extract(turns)
	h.GitDiff = strings.TrimSpace(l.GitNumstat(ctx, agent.Workdir))
	return h
}

// readTurns opens the retiring agent's transcript and parses it into warden's
// neutral turns via its backend adapter. It returns nil on any failure (no
// transcript path, unopenable file, parse error) — the caller treats an empty slice
// as "no recoverable transcript context", never as a hard error.
func (l *Lifecycle) readTurns(agent *agentstore.Agent) []agentbackend.Turn {
	path := l.transcriptPath(agent)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Debug("hot-swap: open transcript failed", "agent", agent.ID, "path", path, "err", err)
		return nil
	}
	defer f.Close()
	turns, err := l.backendFor(agent.AiCli).ParseTranscript(f)
	if err != nil {
		slog.Debug("hot-swap: parse transcript failed", "agent", agent.ID, "path", path, "err", err)
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
func (l *Lifecycle) resolveSuccessor(ctx context.Context, agent *agentstore.Agent, req SwapRequest) (backendID, model string, resolverUsed bool, err error) {
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
		return normalizeBackendID(agent.AiCli), req.Model, false, nil
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

// ErrModelRequiresAiCli is returned when a spawn pins a model without also
// pinning an AI CLI. A model is only meaningful relative to an AI CLI, so the
// pair must be specified together (or neither — then the router picks both).
var ErrModelRequiresAiCli = fmt.Errorf("model requires an explicit aicli; cannot pin --model without --aicli")

// resolveSpawnTarget decides the AI CLI+model for a FIRST spawn via the
// deterministic Role → Tier → (AICLI, Model) matrix:
//
//	a) Exact pin: both aicli and model set → validate the AI CLI and use them
//	   directly (tier is ignored).
//	b) Invalid: model set without aicli → ErrModelRequiresAiCli.
//	c) AICLI pin without model → resolve the optimal model for that AI CLI from
//	   the requested tier (or role/task-mapped tier) via PreferredBackend.
//	d) Neither pinned → resolve the optimal (aicli, model) from the requested
//	   tier (or role/task-mapped tier).
//	e) Model is never left empty — when the router is absent or declines, the
//	   config/default model fills in so every spawn carries an explicit model.
//
// Unlike a hot-swap (which refuses with ErrNoResolver when nothing is wired), a
// first spawn DEGRADES gracefully on resolver absence/errors — except for the
// hard validation in (b). It never fails the spawn because the resolver is
// absent or empty.
func (l *Lifecycle) resolveSpawnTarget(ctx context.Context, roleName, taskName, tier, backend, model string) (resolvedBackend, resolvedModel string, err error) {
	backend = strings.TrimSpace(backend)
	model = strings.TrimSpace(model)

	// b) Model without AI CLI is always invalid.
	if model != "" && backend == "" {
		return "", "", fmt.Errorf("%w (got model %q)", ErrModelRequiresAiCli, model)
	}

	// a) Exact pin: both set — validate AI CLI, ignore tier, use as-is.
	if backend != "" && model != "" {
		if _, err := agentbackend.Get(backend); err != nil {
			return "", "", err
		}
		return backend, model, nil
	}

	// c) AICLI pin without model — pick the optimal model for that AI CLI.
	if backend != "" {
		if _, err := agentbackend.Get(backend); err != nil {
			return "", "", err
		}
		if l.Resolver == nil {
			return backend, l.resolveDefaultModel(), nil
		}
		res, resErr := l.Resolver.Resolve(ctx, router.ResolveOptions{
			Role:             roleName,
			Task:             taskName,
			Tier:             backendstore.ModelTier(tier),
			PreferredBackend: backend,
			AllowFallback:    true,
		})
		if resErr != nil || res == nil || res.BackendID == "" || res.ModelID == "" {
			if resErr != nil {
				slog.Debug("spawn: resolver declined preferred aicli, using default model",
					"aicli", backend, "role", roleName, "task", taskName, "tier", tier, "err", resErr)
			}
			return backend, l.resolveDefaultModel(), nil
		}
		return res.BackendID, res.ModelID, nil
	}

	// d) Neither pinned — let the router pick both. Degrade to defaults when it
	// can't so the spawn still proceeds (e: model is never empty).
	if l.Resolver == nil {
		return "", l.resolveDefaultModel(), nil
	}
	res, resErr := l.Resolver.Resolve(ctx, router.ResolveOptions{
		Role:          roleName,
		Task:          taskName,
		Tier:          backendstore.ModelTier(tier),
		AllowFallback: true,
	})
	if resErr != nil || res == nil || res.BackendID == "" || res.ModelID == "" {
		if resErr != nil {
			slog.Debug("spawn: resolver declined, using request defaults",
				"role", roleName, "task", taskName, "tier", tier, "err", resErr)
		}
		return "", l.resolveDefaultModel(), nil
	}
	return res.BackendID, res.ModelID, nil
}

// launchSuccessor brings up the successor backend b in the retiring agent's existing
// worktree, carrying the handoff forward. It mirrors spawnTyped's launch assembly
// (new tmux session → inject context → build launch → send-keys) minus the worktree
// creation (the worktree already exists) and re-pins a fresh session id for a
// pinning backend (a new backend session is a new conversation, not a resume).
func (l *Lifecycle) launchSuccessor(ctx context.Context, agent *agentstore.Agent, b agentbackend.Backend, model, mode, handoffPath string, h handoff.Handoff, req SwapRequest) error {
	// A pinning backend (Claude) needs a fresh warden-minted session id for the new
	// conversation; a non-pinning backend (codex/antigravity) mints its own, so leave
	// the id empty for the poller's discover-then-pin.
	if b.Capabilities().SessionIDControl {
		id, err := store.NewSessionID()
		if err != nil {
			return err
		}
		agent.AICLISessionID = id
	} else {
		agent.AICLISessionID = ""
	}

	// Recreate the tmux session in the SAME worktree.
	if err := l.newAgentSession(ctx, "", agent.ID, agent.Workdir); err != nil {
		return err
	}

	// The continuation prompt points the successor at the handoff file and restates
	// the goal + next step, so even a backend that cannot read AGENTS.md still picks
	// up the thread. Write it as a prompt file (file-backed positional, like every
	// other launch path) so a multi-line prompt types as one physical line.
	promptFile, err := l.writePromptFile(ctx, agent.ID, continuationPrompt(handoffPath, h, req))
	if err != nil {
		l.killSession(agent.ID)
		return err
	}

	// Inject the handoff into the backend's AGENTS.md rules file (ContextInjector
	// backends, e.g. codex); a flag/positional backend contributes nothing here and
	// relies on the continuation prompt instead. Best-effort: a write failure degrades
	// (the prompt still carries the handoff) rather than aborting the swap.
	persona := personaGuidance(agent.Role)
	if err := l.injectContext(b, agent.Workdir,
		persona,
		handoffGuidance(handoffPath, h),
	); err != nil {
		slog.Warn("hot-swap: context injection failed", "agent", agent.ID, "backend", b.ID(), "err", err)
	}

	base := b.LaunchCmd(agentbackend.LaunchOpts{
		SessionID: agent.AICLISessionID, Name: agent.ID, Model: l.launchModel(b, model), Mode: mode, Network: launchNetwork(agent),
		LogFile: l.sessionLogFile(b, agent.ID),
	})
	hints := l.systemPromptHints(ctx, b, agent.ID,
		hintSpec{persona != "", persona},
		hintSpec{true, handoffGuidance(handoffPath, h)},
	)
	launch := base + hints + l.guardSettings(b, agent.ID) + l.promptArg(b, promptFile) + l.exitSuffix(agent.ID)
	if out, err := l.run.Run(ctx, agent.Workdir, "tmux", "send-keys", "-t", agent.ID, launch, "Enter"); err != nil {
		l.killSession(agent.ID)
		return fmt.Errorf("tmux send-keys: %w: %s", err, out)
	}
	l.seedInteractivePrompt(b, agent.ID, continuationPrompt(handoffPath, h, req))
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
func (l *Lifecycle) swapSystemContext(agent *agentstore.Agent, fromBackend, fromModel, toBackend, toModel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hot-swap: %s → %s (same worktree, same branch).\n", describeBackend(fromBackend, fromModel), describeBackend(toBackend, toModel))
	if agent.Worktree != "" {
		fmt.Fprintf(&b, "Worktree: %s\n", agent.Worktree)
	}
	if agent.Workdir != "" {
		fmt.Fprintf(&b, "Working directory: %s\n", agent.Workdir)
	}
	if agent.Branch != "" {
		fmt.Fprintf(&b, "Branch: %s\n", agent.Branch)
	}
	if agent.Repo != "" {
		fmt.Fprintf(&b, "Repository: %s\n", agent.Repo)
	}
	fmt.Fprintf(&b, "Execution profile: network=%s\n", agent.ExecutionProfile.EffectiveNetwork())
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
