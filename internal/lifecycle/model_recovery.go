package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/router"
	"github.com/srjn45/warden/internal/store"
)

// Bounded startup failure recovery (docs/specs/2026-10-19-aicli-model-catalog-refresh.md §9).
//
// Safety envelope, deliberately narrower than the spec's universal retry:
//   - only an adapter implementing agentbackend.LaunchRejectionDetector takes part
//     (none does in production until a captured, versioned fixture exists);
//   - a rejection counts only when the CLI has EXITED non-zero, no ready marker
//     was seen, no transcript exists, the anchored signature matches, AND (when
//     the launch carries a prompt) the adapter attests via RejectsBeforePrompt
//     that the signature precedes any prompt handling. The lifecycle cannot
//     prove "prompt not accepted" from the pane alone, so absent the attestation
//     it fails closed and does not retry;
//   - recovery only picks another model of the SAME aicli and tier (permission
//     mode, billing and session wiring are unchanged); anything else fails typed;
//   - exact pins and recorded-model relaunches are never substituted.

// maxModelAttempts is the hard cap on distinct pre-task attempts.
const maxModelAttempts = 3

// ModelRecoveryConfig bounds startup recovery. Zero fields take the defaults.
type ModelRecoveryConfig struct {
	MaxAttempts    int           // distinct (aicli, model) attempts including the first (default and hard cap 3)
	Deadline       time.Duration // shared deadline from the first launch (default 3m)
	AttemptTimeout time.Duration // per-attempt observation window (default 60s)
	RejectionTTL   time.Duration // negative-evidence lifetime (default 24h)
}

func (c ModelRecoveryConfig) norm() ModelRecoveryConfig {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.MaxAttempts > maxModelAttempts {
		c.MaxAttempts = maxModelAttempts
	}
	if c.Deadline <= 0 {
		c.Deadline = 3 * time.Minute
	}
	if c.AttemptTimeout <= 0 {
		c.AttemptTimeout = 60 * time.Second
	}
	if c.RejectionTTL <= 0 {
		c.RejectionTTL = 24 * time.Hour
	}
	return c
}

// modelRecoverySettings is the optional ConfigProvider extension carrying the
// hot-reloaded model_recovery.* keys (config.Config implements it). A provider
// without it (test doubles) leaves the compiled defaults.
type modelRecoverySettings interface {
	ModelRecoveryMaxAttempts() int
	ModelRecoveryDeadline() time.Duration
	ModelRecoveryAttemptTimeout() time.Duration
	ModelRecoveryRejectionTTL() time.Duration
}

// modelRecoveryConfig resolves the effective bounds for one launch. An explicit
// non-zero l.ModelRecovery field wins (programmatic/test override); otherwise the
// live config provider supplies it, read per launch so a reload applies from the
// next launch; otherwise the default. The result is always normalised, so the
// hard cap of 3 holds whatever any layer says.
func (l *Lifecycle) modelRecoveryConfig() ModelRecoveryConfig {
	c := l.ModelRecovery
	if s, ok := l.config().(modelRecoverySettings); ok {
		if c.MaxAttempts <= 0 {
			c.MaxAttempts = s.ModelRecoveryMaxAttempts()
		}
		if c.Deadline <= 0 {
			c.Deadline = s.ModelRecoveryDeadline()
		}
		if c.AttemptTimeout <= 0 {
			c.AttemptTimeout = s.ModelRecoveryAttemptTimeout()
		}
		if c.RejectionTTL <= 0 {
			c.RejectionTTL = s.ModelRecoveryRejectionTTL()
		}
	}
	return c.norm()
}

// LaunchEvidenceRecorder persists scope-local launch observations
// (*backendstore.Store implements it).
type LaunchEvidenceRecorder interface {
	RecordLaunchEvidence(aiCli, modelID string, evidence backendstore.LaunchEvidence, rejectFor time.Duration) error
}

// ModelAttempt is one audited startup attempt.
type ModelAttempt struct {
	AiCli   string `json:"aicli"`
	Model   string `json:"model"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

// ModelRecoveryExhaustedError: every allowed pre-task attempt was rejected or no
// further eligible same-aicli/same-tier model exists. Needs operator attention.
type ModelRecoveryExhaustedError struct{ Attempts []ModelAttempt }

func (e *ModelRecoveryExhaustedError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		parts = append(parts, fmt.Sprintf("%s/%s: %s", a.AiCli, a.Model, a.Reason))
	}
	return "model_recovery_exhausted: " + strings.Join(parts, "; ")
}

// ModelRejectedError: a pinned or recorded model was rejected at startup. It is
// reported, never substituted.
type ModelRejectedError struct {
	AiCli, Model, Reason string
	Suggestions          []string
}

func (e *ModelRejectedError) Error() string {
	return fmt.Sprintf("model_rejected: %s/%s: %s", e.AiCli, e.Model, e.Reason)
}

// StartupAmbiguousError: a prompt-withheld launch neither showed a definitive
// model rejection nor a positive input-ready state (timeout, cancel, unexplained
// exit, or a failed delivery). The task prompt is NOT retried and the model is
// NOT substituted: an operator must look at it.
type StartupAmbiguousError struct {
	AiCli, Model, Reason string
	PromptDelivered      bool // true only if delivery was attempted (it may have partially landed)
}

func (e *StartupAmbiguousError) Error() string {
	return fmt.Sprintf("startup_ambiguous: %s/%s: %s", e.AiCli, e.Model, e.Reason)
}

// IsModelRecoveryError reports whether err is one of the typed recovery errors.
func IsModelRecoveryError(err error) bool {
	var a *ModelRecoveryExhaustedError
	var r *ModelRejectedError
	var m *StartupAmbiguousError
	return errors.As(err, &a) || errors.As(err, &r) || errors.As(err, &m)
}

// startupRun describes one launched agent whose startup is to be supervised.
type startupRun struct {
	Agent  *agentstore.Agent
	B      agentbackend.Backend
	Pinned bool // exact pin / recorded model: report, never substitute
	// PromptOnLaunchLine is true when the task prompt rides the launch command
	// (positional arg). The prompt-withheld path (Cursor-style PromptSeeder, resume
	// commands) is false and needs no pre-prompt attestation.
	PromptOnLaunchLine bool
	// WithheldPrompt, when non-empty, is the task prompt held OUT of the launch
	// line. The supervisor delivers it exactly once, only after the adapter's
	// positive InputReady, and never to an attempt that was rejected. Requires the
	// backend to implement both LaunchRejectionDetector and InputReadiness.
	WithheldPrompt string
	Role           string
	Task           string
	Tier           string
	// Relaunch starts the same agent again (fresh pane, same worktree) on model.
	Relaunch func(ctx context.Context, model string) error
}

type startupOutcome int

const (
	outcomeStarted startupOutcome = iota
	outcomeRejected
	outcomeAmbiguous
)

// awaitStartup watches the pane until the attempt resolves. It reports a
// rejection only for an exited, non-zero CLI whose pane carries an anchored
// signature and never showed the ready marker. With ir != nil (prompt-withheld
// launch) it additionally requires a POSITIVE InputReady to report started;
// a timeout, cancel or unexplained exit is outcomeAmbiguous, never "started".
func (l *Lifecycle) awaitStartup(ctx context.Context, agent *agentstore.Agent, id string, det agentbackend.LaunchRejectionDetector, ready string, ir agentbackend.InputReadiness) (startupOutcome, string) {
	poll := l.recoveryPoll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	withheld := ir != nil
	for {
		pane, err := l.run.Run(ctx, "", "tmux", "capture-pane", "-p", "-t", id)
		if err == nil {
			if withheld {
				if code, exited := l.ReadExit(id); exited {
					if code != 0 && l.transcriptPath(agent) == "" {
						if reason, ok := det.DetectModelRejection(pane, agent.Model); ok {
							return outcomeRejected, reason
						}
					}
					return outcomeAmbiguous, fmt.Sprintf("CLI exited (code %d) before input-ready with no model-rejection signature", code)
				}
				if ir.InputReady(pane) {
					return outcomeStarted, ""
				}
			} else {
				if ready != "" && strings.Contains(pane, ready) {
					return outcomeStarted, ""
				}
				if code, exited := l.ReadExit(id); exited {
					if code == 0 || l.transcriptPath(agent) != "" {
						return outcomeStarted, ""
					}
					if reason, ok := det.DetectModelRejection(pane, agent.Model); ok {
						return outcomeRejected, reason
					}
					return outcomeStarted, ""
				}
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			if withheld {
				return outcomeAmbiguous, "no input-ready or model-rejection observed before the attempt window closed"
			}
			return outcomeStarted, ""
		}
	}
}

// withholdPrompt reports whether b takes part in the prompt-withheld startup seam:
// it puts the task prompt on the launch line today (no PromptSeeder) AND exposes
// both a fixture-backed rejection detector and a positive input-ready check. With
// no detector registered (production today) it is false and launch is unchanged.
func withholdPrompt(b agentbackend.Backend) bool {
	if _, ok := b.(agentbackend.PromptSeeder); ok {
		return false
	}
	if _, ok := b.(agentbackend.LaunchRejectionDetector); !ok {
		return false
	}
	_, ok := b.(agentbackend.InputReadiness)
	return ok
}

func (l *Lifecycle) attemptEvent(agent *agentstore.Agent, n int, outcome, reason string) {
	agent.Events = append(agent.Events, store.Event{
		TS:   time.Now().UTC(),
		Type: "model_launch_attempt",
		Detail: fmt.Sprintf("n=%d aicli=%s model=%s outcome=%s reason=%q",
			n, agent.AiCli, agent.Model, outcome, reason),
	})
}

// superviseStartup returns nil when startup is not rejected (or the adapter has
// no detector). On a typed error the caller must clean the failed spawn up.
func (l *Lifecycle) superviseStartup(ctx context.Context, s startupRun) error {
	det, ok := s.B.(agentbackend.LaunchRejectionDetector)
	if !ok {
		return nil
	}
	if s.PromptOnLaunchLine && !det.RejectsBeforePrompt() {
		return nil // cannot prove the prompt was not accepted: fail closed
	}
	cfg := l.modelRecoveryConfig()
	dctx, cancel := context.WithTimeout(ctx, cfg.Deadline)
	defer cancel()
	ready := ""
	if ps, ok := s.B.(agentbackend.PromptSeeder); ok {
		ready = ps.ReadyMarker()
	}
	var ir agentbackend.InputReadiness
	if s.WithheldPrompt != "" {
		var ok bool
		if ir, ok = s.B.(agentbackend.InputReadiness); !ok {
			return &StartupAmbiguousError{AiCli: s.Agent.AiCli, Model: s.Agent.Model, Reason: "prompt withheld but backend has no positive input-ready check"}
		}
	}
	var tried []router.Route
	var attempts []ModelAttempt
	for n := 1; ; n++ {
		a := s.Agent
		cur := router.Route{BackendID: a.AiCli, ModelID: a.Model}
		tried = append(tried, cur)
		actx, acancel := context.WithTimeout(dctx, cfg.AttemptTimeout)
		outcome, reason := l.awaitStartup(actx, a, a.ID, det, ready, ir)
		acancel()
		if outcome == outcomeAmbiguous {
			l.attemptEvent(a, n, "ambiguous", reason)
			l.killSession(a.ID)
			return &StartupAmbiguousError{AiCli: a.AiCli, Model: a.Model, Reason: reason}
		}
		if outcome == outcomeStarted {
			if s.WithheldPrompt != "" {
				// Exactly once, only after a positive input-ready; never retried.
				if err := l.Input(dctx, a.ID, s.WithheldPrompt); err != nil {
					l.attemptEvent(a, n, "ambiguous", "prompt delivery failed: "+err.Error())
					l.killSession(a.ID)
					return &StartupAmbiguousError{AiCli: a.AiCli, Model: a.Model, Reason: "prompt delivery failed: " + err.Error(), PromptDelivered: true}
				}
			}
			l.attemptEvent(a, n, "started", "")
			return nil
		}
		attempts = append(attempts, ModelAttempt{AiCli: a.AiCli, Model: a.Model, Outcome: "rejected", Reason: reason})
		l.attemptEvent(a, n, "rejected", reason)
		if l.Evidence != nil && a.Model != "" {
			if err := l.Evidence.RecordLaunchEvidence(a.AiCli, a.Model, backendstore.LaunchEvidence{
				Kind: backendstore.LaunchFailureInvalidModel, Summary: reason, PreTask: true, ObservedAt: time.Now().UTC(),
			}, cfg.RejectionTTL); err != nil {
				slog.Warn("model recovery: record launch evidence failed", "agent", a.ID, "err", err)
			}
		}
		// Reap the failed child, its PTY and the tmux shell before anything else.
		l.killSession(a.ID)
		if s.Pinned {
			return &ModelRejectedError{AiCli: a.AiCli, Model: a.Model, Reason: reason, Suggestions: l.suggestions(a, s.Tier)}
		}
		if n >= cfg.MaxAttempts || dctx.Err() != nil || l.Resolver == nil || s.Relaunch == nil {
			return &ModelRecoveryExhaustedError{Attempts: attempts}
		}
		res, err := l.Resolver.Resolve(dctx, router.ResolveOptions{
			Role: s.Role, Task: s.Task, Tier: backendstore.ModelTier(s.Tier),
			PreferredBackend: a.AiCli, AllowFallback: false, ExcludeRoutes: tried,
		})
		if err != nil || res == nil || res.ModelID == "" || res.BackendID != a.AiCli || routeSeen(tried, res.BackendID, res.ModelID) {
			attempts = append(attempts, ModelAttempt{AiCli: a.AiCli, Outcome: "no_candidate", Reason: "no further eligible model for this aicli and tier"})
			return &ModelRecoveryExhaustedError{Attempts: attempts}
		}
		a.Model = res.ModelID
		if l.CapacityResolver != nil {
			if b, err := l.CapacityResolver.Resolve(dctx, a.AiCli, a.Model); err == nil {
				a.QuotaBinding = b
			}
		}
		if err := s.Relaunch(dctx, a.Model); err != nil {
			return err
		}
	}
}

func routeSeen(tried []router.Route, backend, model string) bool {
	for _, t := range tried {
		if t.BackendID == backend && t.ModelID == model {
			return true
		}
	}
	return false
}

// suggestions lists same-tier models for the pinned aicli that are not rejected.
func (l *Lifecycle) suggestions(a *agentstore.Agent, tier string) []string {
	if l.Resolver == nil || tier == "" {
		return nil
	}
	var out []string
	var tried []router.Route
	tried = append(tried, router.Route{BackendID: a.AiCli, ModelID: a.Model})
	for i := 0; i < 3; i++ {
		res, err := l.Resolver.Resolve(context.Background(), router.ResolveOptions{
			Tier: backendstore.ModelTier(tier), PreferredBackend: a.AiCli, ExcludeRoutes: tried,
		})
		if err != nil || res == nil || res.ModelID == "" {
			break
		}
		out = append(out, res.ModelID)
		tried = append(tried, router.Route{BackendID: res.BackendID, ModelID: res.ModelID})
	}
	return out
}

// superviseRecorded supervises a relaunch of a RECORDED model (Restore, SwitchRole,
// hot-swap successor). The model is part of the agent's history, so it is treated
// as an exact pin: a detected rejection is recorded as evidence, the failed child
// and its tmux session are reaped, and a typed ModelRejectedError (attention
// needed) is returned. Nothing is ever substituted and the launch is never retried.
// promptOnLaunchLine is true only when the relaunch command carries the task prompt.
// The attempt is judged against model (the one actually launched), which for a
// hot-swap is the successor's, not yet the record's: supervision runs on a copy so
// the record is only mutated by the caller, once the successor is confirmed.
func (l *Lifecycle) superviseRecorded(ctx context.Context, agent *agentstore.Agent, b agentbackend.Backend, model string, promptOnLaunchLine bool, withheld string) error {
	cp := *agent
	cp.AiCli, cp.Model = b.ID(), model
	before := len(cp.Events)
	err := l.superviseStartup(ctx, startupRun{Agent: &cp, B: b, Pinned: true, PromptOnLaunchLine: promptOnLaunchLine, WithheldPrompt: withheld})
	agent.Events = cp.Events
	// Every caller drops the in-memory record on a typed error, so the attempt
	// audit would be lost; hand the new events to the daemon to persist.
	if err != nil && l.OnModelAttemptEvents != nil && len(cp.Events) > before {
		l.OnModelAttemptEvents(ctx, agent.ID, append([]store.Event(nil), cp.Events[before:]...))
	}
	return err
}
