package promptengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/decisionrouter"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyobs"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/tmuxproc"
)

// Target is what the engine needs from a runtime-backed session.
type Target struct {
	Stream decisionrouter.Stream
	Writer decisionrouter.InputWriter
}

// Runtime resolves sessions to their stream and input path.
type Runtime interface {
	Target(name string) (Target, bool)
	// Authoritative reports whether the runtime owns this session's decisions
	// (runtime.pty=on). False means shadow: dry-run only.
	Authoritative(name string) bool
}

// Agents reads session records.
type Agents interface {
	Get(ctx context.Context, id string) (*agentstore.Agent, error)
}

// Config tunes the engine. Zero values take safe defaults.
type Config struct {
	// Settle is how long after delivering an answer the same prompt instance is
	// left alone, so a slow redraw is not answered twice.
	Settle time.Duration
	// Retry is the backoff after an attempt that was deferred or dropped by
	// revalidation, before the same prompt instance is re-decided.
	Retry time.Duration
	// MaxAttempts bounds deliveries of one prompt instance. A prompt that is
	// still on screen after this many answers did not take: it escalates.
	MaxAttempts int
	// RecognizeEvery rate-limits Fast-Brain recognition per session.
	RecognizeEvery time.Duration

	// Policy resolves the allow/deny rule set for an agent.
	Policy func(*agentstore.Agent) approval.Policy
	// Backend resolves the agent's adapter.
	Backend func(*agentstore.Agent) agentbackend.Backend
	// CLIVersion reports the installed CLI version for learned-shape pinning.
	CLIVersion func(backendID string) string
	// Trace, when set, receives a redacted record for every attempt/outcome.
	Trace *ptyobs.Trace
	// Metrics, when set, counts prompts, decisions and escalations (bounded labels).
	Metrics *ptyobs.Metrics
	// Notify is called when an outcome should refresh clients.
	Notify func()
	Clock  func() time.Time
}

func (c *Config) defaults() {
	if c.Settle <= 0 {
		c.Settle = 3 * time.Second
	}
	if c.Retry <= 0 {
		c.Retry = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.RecognizeEvery <= 0 {
		c.RecognizeEvery = 30 * time.Second
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Engine drives prompts to decisions for runtime-backed sessions.
type Engine struct {
	cfg     Config
	rt      Runtime
	agents  Agents
	router  *decisionrouter.Router
	classif *decisionrouter.PromptClassifier
	learner *decisionrouter.Learner

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	busy bool
	// last is the outcome for the prompt instance currently being handled.
	last struct {
		key       string
		at        time.Time
		attempts  int
		final     bool // no further automatic action for this instance
		delivered bool
	}
	recognizedAt  time.Time
	unreadableKey string
}

// New builds an Engine. classif may be nil (unreadable screens then escalate
// without recognition); learner may be nil (nothing is learned).
func New(cfg Config, rt Runtime, agents Agents, router *decisionrouter.Router,
	classif *decisionrouter.PromptClassifier, learner *decisionrouter.Learner) *Engine {
	cfg.defaults()
	return &Engine{cfg: cfg, rt: rt, agents: agents, router: router, classif: classif, learner: learner,
		sessions: make(map[string]*session)}
}

func (e *Engine) sess(name string) *session {
	s, ok := e.sessions[name]
	if !ok {
		s = &session{}
		e.sessions[name] = s
	}
	return s
}

// OnPrompt is the stateclassifier hook for a parsed prompt on screen. It never
// blocks: the work runs on its own goroutine, one at a time per session.
func (e *Engine) OnPrompt(name string, p *ptyevent.Prompt) {
	if p == nil {
		return
	}
	e.mu.Lock()
	s := e.sess(name)
	if s.busy {
		e.mu.Unlock()
		return
	}
	now := e.cfg.Clock()
	if s.last.key == p.InstanceKey {
		wait := e.cfg.Retry
		if s.last.delivered {
			wait = e.cfg.Settle
		}
		if s.last.final || now.Sub(s.last.at) < wait {
			e.mu.Unlock()
			return
		}
	}
	s.busy = true
	e.mu.Unlock()
	go func() {
		defer e.release(name)
		e.handle(context.Background(), name, p)
	}()
}

func (e *Engine) release(name string) {
	e.mu.Lock()
	e.sess(name).busy = false
	e.mu.Unlock()
}

// handle runs one prompt through the router and records the outcome.
func (e *Engine) handle(ctx context.Context, name string, p *ptyevent.Prompt) {
	tgt, ok := e.rt.Target(name)
	if !ok {
		return
	}
	ag, err := e.agents.Get(ctx, name)
	if err != nil || ag == nil {
		return
	}
	if e.learner != nil {
		e.learner.ParserAgreement(p)
	}
	req := e.routeRequest(ag, name, tgt, p)
	started := e.cfg.Clock()
	e.countPrompt(req.BackendID, req.CLIVersion, p)
	res, err := e.router.Route(ctx, req)
	if err != nil {
		slog.Warn("promptengine: route failed", "session", name, "err", err)
	}
	e.observe(attempt{session: name, ag: ag, req: req, started: started, prompt: p}, res, err)
	e.record(name, p, res, err)
	if e.cfg.Notify != nil {
		e.cfg.Notify()
	}
}

func (e *Engine) routeRequest(ag *agentstore.Agent, name string, tgt Target, p *ptyevent.Prompt) decisionrouter.RouteRequest {
	posture := ag.PermissionPosture
	if !posture.Valid() {
		posture = ptyevent.PostureAllowlisted // the safe default; never an implicit upgrade
	}
	var b agentbackend.Backend
	if e.cfg.Backend != nil {
		b = e.cfg.Backend(ag)
	}
	backendID := ag.AiCli
	if b != nil {
		backendID = b.ID()
	}
	var pol approval.Policy
	if e.cfg.Policy != nil {
		pol = e.cfg.Policy(ag)
	}
	version := ""
	if e.cfg.CLIVersion != nil {
		version = e.cfg.CLIVersion(backendID)
	}
	return decisionrouter.RouteRequest{
		SessionID:       name,
		AgentID:         ag.ID,
		Name:            ag.Name,
		RunID:           ag.AutopilotRunID,
		IsAutopilot:     ag.AutopilotRunID != "",
		Workdir:         ag.Workdir,
		BackendID:       backendID,
		CLIVersion:      version,
		Backend:         b,
		Posture:         posture,
		AllowlistPolicy: pol,
		// Whether warden WRITES a decision is separate from what the posture
		// permits (§10.3): the per-session opt-in or autopilot ownership.
		Act:         ag.AutoApprove || ag.AutopilotRunID != "",
		Screen:      tgt.Stream.Screen(),
		LivePrompt:  p,
		Stream:      tgt.Stream,
		InputWriter: tgt.Writer,
		DryRun:      !e.rt.Authoritative(name),
	}
}

// record remembers the outcome so the same instance is not decided twice.
func (e *Engine) record(name string, p *ptyevent.Prompt, res decisionrouter.RouteResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sess(name)
	now := e.cfg.Clock()
	if s.last.key != p.InstanceKey {
		s.last.attempts = 0
	}
	s.last.key, s.last.at = p.InstanceKey, now
	s.last.delivered = res.Delivered
	switch {
	case res.Delivered:
		s.last.attempts++
		// A prompt still on screen after MaxAttempts answers did not take;
		// stop pressing keys and leave it to a human.
		s.last.final = s.last.attempts >= e.cfg.MaxAttempts
	case err != nil:
		s.last.final = false
	case res.RevalidationCode != "" && res.RevalidationCode != ptyevent.RevalOK:
		// Dropped by TOCTOU or deferred: re-decide from fresh evidence later.
		s.last.final = false
	default:
		// ask_human / needs_attention / route_decision / downgraded: a human or
		// the brain owns it now; do not re-run on every investigation.
		s.last.final = true
	}
}

// OnUnreadable is the stateclassifier hook for a blocking screen no parser
// could read. It tries Fast-Brain recognition (redacted input, verified
// against the screen), feeds the learner, and routes the reading: a confident
// reading from a usable learned shape can be answered by posture; anything
// else journals needs_attention and waits for a human. Never answered blind.
func (e *Engine) OnUnreadable(name, screen string) {
	if e.classif == nil {
		return
	}
	key := screenKey(screen)
	e.mu.Lock()
	s := e.sess(name)
	now := e.cfg.Clock()
	if s.busy || (s.unreadableKey == key && now.Sub(s.recognizedAt) < e.cfg.RecognizeEvery) {
		e.mu.Unlock()
		return
	}
	s.busy, s.unreadableKey, s.recognizedAt = true, key, now
	e.mu.Unlock()
	go func() {
		defer e.release(name)
		e.handleUnreadable(context.Background(), name, screen)
	}()
}

func (e *Engine) handleUnreadable(ctx context.Context, name, screen string) {
	tgt, ok := e.rt.Target(name)
	if !ok {
		return
	}
	ag, err := e.agents.Get(ctx, name)
	if err != nil || ag == nil {
		return
	}
	var b agentbackend.Backend
	if e.cfg.Backend != nil {
		b = e.cfg.Backend(ag)
	}
	req := e.routeRequest(ag, name, tgt, nil)
	creq := decisionrouter.ClassifyRequest{
		Screen: screen, Backend: b, BackendID: req.BackendID, CLIVersion: req.CLIVersion,
		SessionID: name, ProjectID: ag.Workdir, Workdir: ag.Workdir, AgentID: ag.ID, Now: e.cfg.Clock(),
	}
	started := e.cfg.Clock()
	cres, err := e.classif.Classify(ctx, creq)
	if err != nil || cres.IsInformational || cres.Prompt == nil {
		if err == nil && cres.IsInformational {
			return
		}
		// A blocking screen nothing could read: measured, traced, escalated.
		reason := "no parser or learned shape recognized the prompt format"
		if err != nil {
			reason = "prompt recognition failed"
		}
		e.countUnrecognized(req.BackendID, req.CLIVersion)
		e.observe(attempt{session: name, ag: ag, req: req, started: started, unreadable: reason},
			decisionrouter.RouteResult{}, nil)
		return
	}
	e.countPrompt(req.BackendID, req.CLIVersion, cres.Prompt)
	if e.learner != nil {
		if lerr := e.learner.Observe(creq, cres); lerr != nil {
			slog.Warn("promptengine: learner observe failed", "session", name, "err", lerr)
		}
	}
	req.LivePrompt = cres.Prompt
	req.Screen = screen
	// A fastbrain/learned reading is not state-machine evidence yet, so the
	// router's live check (labels re-located, target visible) is the guard.
	res, rerr := e.router.Route(ctx, req)
	e.observe(attempt{session: name, ag: ag, req: req, started: started, prompt: cres.Prompt}, res, rerr)
	e.record(name, cres.Prompt, res, rerr)
	if e.cfg.Notify != nil {
		e.cfg.Notify()
	}
}

func screenKey(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > 30 {
		lines = lines[len(lines)-30:]
	}
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:8])
}

// HumanNotifier is the router's human-inbox seam. Understood-but-unanswered
// prompts already surface through the waiting_for_input status and the
// approvals queue, so this only refreshes clients.
type HumanNotifier struct{ Notify func() }

// NotifyHumanApproval implements decisionrouter.HumanInboxNotifier.
func (h HumanNotifier) NotifyHumanApproval(context.Context, string, string, *ptyevent.Prompt, []string) error {
	if h.Notify != nil {
		h.Notify()
	}
	return nil
}

// DirectWriter delivers router keystrokes to a warden-owned PTY, through the
// input lease (a human who is typing always wins; the write defers).
type DirectWriter struct{ S *ptyruntime.Session }

// WriteInput implements decisionrouter.InputWriter.
func (w DirectWriter) WriteInput(ctx context.Context, data []byte, origin ptyevent.Origin, decisionID string) error {
	return w.S.WriteWait(ctx, ptyruntime.Write{Origin: origin, Data: data, DecisionID: decisionID})
}

// Lease implements decisionrouter.InputWriter.
func (w DirectWriter) Lease() ptyevent.Lease { return w.S.Lease() }

// TmuxWriter delivers router keystrokes to a tmux-hosted pane. A mirror is
// observe-only, so there is no input lease to consult; the router's live screen
// re-read immediately before the write is the TOCTOU guard.
type TmuxWriter struct {
	Host tmuxproc.Host
	Name string
}

// WriteInput implements decisionrouter.InputWriter.
func (w TmuxWriter) WriteInput(ctx context.Context, data []byte, _ ptyevent.Origin, _ string) error {
	keys, err := tmuxKeys(data)
	if err != nil {
		return err
	}
	return w.Host.SendKeys(ctx, w.Name, keys...)
}

// Lease implements decisionrouter.InputWriter.
func (TmuxWriter) Lease() ptyevent.Lease { return ptyevent.Lease{} }
