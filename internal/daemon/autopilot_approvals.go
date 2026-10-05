package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/poller"
)

// humanRecipient is the mailbox recipient id for the operator's own inbox. The
// approval router mirrors a copy of every brain-forward here so a human still
// sees (and can audit) what autopilot handled, without ever blocking on it.
const humanRecipient = "human"

// autopilotForwardSender stamps forwarded/mirrored approval messages. "daemon" is
// a reserved provenance id (sanitizeSender blocks agents from forging it), and
// daemon-internal writes call Append directly, so this is trusted by construction
// — matching the collab monitor's daemon-origin warnings.
const autopilotForwardSender = "daemon"

// promptBrainTimeout bounds one stage-3 consult (spec §I.3 prompts.brain_timeout).
// Var so tests can shrink it.
var promptBrainTimeout = 5 * time.Minute

// promptBrainTier pins stage-3 brains to tier-1 so a user's role-tier override
// can never route a prompt answer to an expensive model (spec §I.2).
const promptBrainTier = "tier-1"

// autopilotApprovals implements poller.AutopilotApprovals: the daemon-owned
// prompt chain for run agents (spec §I). Stage 3 spawns a short-lived tier-1
// brain through internal/brainconsult; the manager's mailbox is informational
// only and nothing waits on it or the human inbox.
type autopilotApprovals struct{ s *Server }

var _ poller.AutopilotApprovals = autopilotApprovals{}

// OwnsAgent reports whether s is a worker, manager or resolver of an active run.
func (a autopilotApprovals) OwnsAgent(s *agentstore.Agent) bool {
	if a.s == nil || a.s.autopilot == nil || s == nil || !s.HasTag(autopilotOwnershipTag) {
		return false
	}
	runID := runIDFromTags(s.Tags)
	if runID == "" {
		return false
	}
	_, ok := a.s.autopilot.ActiveBrainForRun(runID)
	return ok
}

// BrainFor returns the run's manager id for the informational note; ok=false when
// s is not a run agent, has no live manager, or IS the manager.
func (a autopilotApprovals) BrainFor(s *agentstore.Agent) (string, bool) {
	if !a.OwnsAgent(s) {
		return "", false
	}
	brainID, ok := a.s.autopilot.ActiveBrainForRun(runIDFromTags(s.Tags))
	if !ok || brainID == s.ID {
		return "", false
	}
	return brainID, true
}

// Forward is informational only: a short note to the manager (when brainID is
// set) and a mirror to the human inbox. Best-effort — a mailbox error is logged,
// never propagated, and nothing waits on either recipient.
func (a autopilotApprovals) Forward(ctx context.Context, brainID string, worker *agentstore.Agent, reason string) {
	if a.s == nil || a.s.mbox == nil || worker == nil {
		return
	}
	label := worker.ID
	if worker.Name != "" {
		label = worker.Name + " (" + worker.ID + ")"
	}
	if brainID != "" {
		body := fmt.Sprintf("autopilot (info, no action needed): worker %s — %s.", label, reason)
		if _, err := a.s.mbox.Append(mailbox.Message{To: brainID, From: autopilotForwardSender, Body: body}); err != nil {
			slog.Warn("autopilot: info note to manager failed", "brain", brainID, "worker", worker.ID, "err", err)
		}
	}
	mirror := fmt.Sprintf("autopilot handled a run-agent prompt (no action needed): %s — %s", label, reason)
	if _, err := a.s.mbox.Append(mailbox.Message{To: humanRecipient, From: autopilotForwardSender, Body: mirror}); err != nil {
		slog.Warn("autopilot: mirror prompt to human inbox failed", "worker", worker.ID, "err", err)
	}
}

// ConsultPrompt is stage 3: a tier-1 brain agent is given the prompt, its
// options, the agent's pane tail, the task and the plan constraints, and returns
// the answer. The brain never touches the stuck agent.
func (a autopilotApprovals) ConsultPrompt(ctx context.Context, s *agentstore.Agent, q poller.PromptQuestion) (poller.PromptAnswer, error) {
	c := a.s.promptConsultor
	if c == nil {
		if a.s.life == nil {
			return poller.PromptAnswer{}, fmt.Errorf("prompt consult: lifecycle not configured")
		}
		c = brainconsult.New(
			brainConsultSpawner{life: a.s.life, store: a.s.store},
			a.s.audit,
			brainconsult.Options{Timeout: promptBrainTimeout, Role: autopilotBrainRole, ModelTier: promptBrainTier},
		)
	}
	task := strings.TrimSpace(s.Task)
	if task == "" {
		task = strings.TrimSpace(s.Subject)
	}
	if task == "" {
		task = promptTaskLine(s.Prompt, 300)
	}
	res, err := c.Consult(ctx, brainconsult.Request{
		Intent: "answer run agent prompt",
		Mode:   brainconsult.ModeAnswer,
		RunID:  runIDFromTags(s.Tags),
		Repo:   s.Repo,
		Answer: &brainconsult.AnswerContext{
			Question: q.Question, Options: q.Options, PaneTail: q.PaneTail,
			Task:        task,
			Constraints: "Stay within the run's plan and the agent's own worktree and run-owned branches; never touch main, force-push, or delete data outside the worktree.",
			Destructive: q.Destructive, Marker: q.Marker,
		},
	})
	if err != nil {
		return poller.PromptAnswer{}, err
	}
	return poller.PromptAnswer{Kind: poller.PromptAnswerKind(res.Answer), Option: res.Option, Text: res.Text, Reason: res.Reason}, nil
}

// TypeText delivers text to the agent through the same input path as
// send_to_agent / POST /sessions/{id}/input.
func (a autopilotApprovals) TypeText(ctx context.Context, s *agentstore.Agent, text string) error {
	if a.s == nil || a.s.life == nil {
		return fmt.Errorf("lifecycle not configured")
	}
	return a.s.life.Input(ctx, s.TmuxSession, text)
}

// HandOff gives an unanswerable prompt to the guardian as a blocker: the owner
// is notified through the escalation notifier instead of the agent waiting.
func (a autopilotApprovals) HandOff(ctx context.Context, s *agentstore.Agent, reason string) {
	runID := runIDFromTags(s.Tags)
	autopilotRuntime{s: a.s}.NotifyEscalation(runID, "autopilot blocker: agent "+s.ID+" is stuck on a prompt", reason)
}

// Audit records a chain outcome in the audit log.
func (a autopilotApprovals) Audit(ctx context.Context, action string, s *agentstore.Agent, detail map[string]string) {
	if a.s == nil || s == nil {
		return
	}
	d := map[string]string{"agent": s.ID, "run": runIDFromTags(s.Tags)}
	for k, v := range detail {
		d[k] = v
	}
	a.s.recordAuditCtx(ctx, action, s.ID, d)
}

func promptTaskLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// runIDFromTags returns the run id from a `run:<run_id>` tag, or "" when none is
// present.
func runIDFromTags(tags []string) string {
	for _, t := range tags {
		if id, ok := strings.CutPrefix(t, runTagPrefix); ok && id != "" {
			return id
		}
	}
	return ""
}
