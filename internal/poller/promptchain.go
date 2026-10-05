package poller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
)

// PromptQuestion is what stage 3 (the tier-1 brain) is asked.
type PromptQuestion struct {
	Question    string
	Options     []string
	PaneTail    string
	Destructive bool
	Marker      string
}

// PromptAnswerKind mirrors brainconsult.AnswerKind without importing it.
type PromptAnswerKind string

const (
	PromptApprove      PromptAnswerKind = "approve"
	PromptReject       PromptAnswerKind = "reject"
	PromptSelectOption PromptAnswerKind = "select_option"
	PromptType         PromptAnswerKind = "type"
)

// PromptAnswer is the brain's decision.
type PromptAnswer struct {
	Kind   PromptAnswerKind
	Option int // 1-based
	Text   string
	Reason string
}

// promptChain is the in-flight stage-3 consult for one agent's current prompt.
type promptChain struct {
	sig    string
	cancel context.CancelFunc
}

const (
	promptTailLines = 80
	// promptConsultAttempts is the stage-3 try count: one retry with a fresh brain.
	promptConsultAttempts = 2
)

// endPromptChain cancels the agent's in-flight consult and forgets its prompt.
func (p *Poller) endPromptChain(id string) {
	p.fwdMu.Lock()
	c := p.chains[id]
	delete(p.chains, id)
	delete(p.lastForward, id)
	p.fwdMu.Unlock()
	if c != nil {
		c.cancel()
	}
}

// WaitPromptChains blocks until every stage-3 consult goroutine has returned.
// Tests use it to await the asynchronous brain stage.
func (p *Poller) WaitPromptChains() { p.chainWG.Wait() }

// routeToBrain is the stage-3 entry of the daemon-owned prompt chain (§I): for an
// autopilot run agent it starts ONE asynchronous tier-1 brain consult for this
// prompt (a prompt that changes or disappears cancels it) and returns true so the
// caller suppresses the human path — no run agent ever waits on a human. It is a
// no-op returning false for every ordinary agent.
func (p *Poller) routeToBrain(ctx context.Context, s *agentstore.Agent, ap *agentbackend.Approval, pane, sig, reason string, destructive bool) bool {
	if p.Autopilot == nil || !p.Autopilot.OwnsAgent(s) {
		return false
	}
	p.fwdMu.Lock()
	if cur := p.chains[s.ID]; cur != nil && cur.sig == sig {
		p.fwdMu.Unlock()
		return true // same prompt already in flight (or already handled)
	}
	if p.lastForward[s.ID] == sig {
		p.fwdMu.Unlock()
		return true // consult for this prompt already finished
	}
	if old := p.chains[s.ID]; old != nil {
		old.cancel() // the prompt changed
	}
	cctx, cancel := context.WithCancel(ctx)
	c := &promptChain{sig: sig, cancel: cancel}
	p.chains[s.ID] = c
	p.lastForward[s.ID] = sig
	p.fwdMu.Unlock()

	slog.Info("autopilot: prompt chain → brain", "agent", s.ID, "reason", reason, "destructive", destructive)
	marker := ""
	if destructive {
		marker = reason
	}
	q := PromptQuestion{
		Question: ap.Question, Options: ap.Options, Destructive: destructive, Marker: marker,
		PaneTail: lastLines(pane, promptTailLines),
	}
	p.chainWG.Add(1)
	go func() {
		defer p.chainWG.Done()
		defer cancel()
		p.runConsult(cctx, s, ap, q, reason)
	}()
	return true
}

// runConsult drives stage 3: consult (retrying once with a fresh brain), deliver
// the answer, audit, and on exhaustion hand the agent to the guardian.
func (p *Poller) runConsult(ctx context.Context, s *agentstore.Agent, ap *agentbackend.Approval, q PromptQuestion, reason string) {
	var ans PromptAnswer
	var err error
	for attempt := 1; attempt <= promptConsultAttempts; attempt++ {
		ans, err = p.Autopilot.ConsultPrompt(ctx, s, q)
		if err == nil || ctx.Err() != nil {
			break
		}
		slog.Warn("autopilot: prompt consult failed", "agent", s.ID, "attempt", attempt, "err", err)
	}
	if ctx.Err() != nil {
		return // prompt changed or disappeared — superseded
	}
	if err != nil {
		p.exhaustConsult(ctx, s, ap, q, err)
		return
	}
	decision, ok := p.deliver(ctx, s, ap, q.Destructive, ans)
	if !ok {
		p.exhaustConsult(ctx, s, ap, q, fmt.Errorf("could not deliver brain answer %q", ans.Kind))
		return
	}
	p.Autopilot.Audit(ctx, "autopilot_prompt_resolved", s, map[string]string{
		"stage": "brain", "decision": decision, "reason": ans.Reason, "category": categoryOf(q)})
	if brainID, ok := p.Autopilot.BrainFor(s); ok {
		p.Autopilot.Forward(ctx, brainID, s, fmt.Sprintf("answered a prompt on its own: %s (%s) — no action needed", decision, ans.Reason))
	} else {
		p.Autopilot.Forward(ctx, "", s, fmt.Sprintf("answered a prompt on its own: %s (%s)", decision, ans.Reason))
	}
	if p.OnChange != nil {
		p.OnChange()
	}
}

func categoryOf(q PromptQuestion) string {
	if q.Destructive {
		return "destructive"
	}
	if len(q.Options) > 2 {
		return "multi_option"
	}
	return "yes_no"
}

// exhaustConsult handles stage-3 failure: a destructive prompt is rejected (the
// safe default), then the agent is handed to the guardian as a blocker rather
// than left waiting.
func (p *Poller) exhaustConsult(ctx context.Context, s *agentstore.Agent, ap *agentbackend.Approval, q PromptQuestion, cause error) {
	detail := map[string]string{"stage": "brain", "error": cause.Error(), "category": categoryOf(q)}
	if q.Destructive {
		if d, ok := p.deliver(ctx, s, ap, true, PromptAnswer{Kind: PromptReject,
			Text: "rejected: destructive action not permitted unattended; choose a non-destructive alternative"}); ok {
			detail["default"] = d
		}
	}
	p.Autopilot.Audit(ctx, "autopilot_prompt_escalated", s, detail)
	p.Autopilot.HandOff(ctx, s, fmt.Sprintf("prompt unanswered after %d brain attempts: %s — %v", promptConsultAttempts, strings.TrimSpace(q.Question), cause))
}

// deliver applies a brain answer through the existing send path. A destructive
// prompt never auto-selects the affirmative/destructive option: approve and any
// non-negative selection are downgraded to a reject. It returns a short decision
// label and whether anything was delivered.
func (p *Poller) deliver(ctx context.Context, s *agentstore.Agent, ap *agentbackend.Approval, destructive bool, ans PromptAnswer) (string, bool) {
	kind := ans.Kind
	if destructive {
		switch kind {
		case PromptApprove:
			kind = PromptReject
		case PromptSelectOption:
			if ans.Option < 1 || ans.Option > len(ap.Options) || !isNegative(ap.Options[ans.Option-1]) {
				kind = PromptReject
			}
		case PromptType:
			// Free text is not a selection; fall through.
		}
	}
	switch kind {
	case PromptApprove:
		if ap.AffirmativeIdx == 0 {
			return "", false
		}
		return "approve", p.sendOption(ctx, s, ap, ap.AffirmativeIdx)
	case PromptSelectOption:
		if ans.Option < 1 || ans.Option > len(ap.Options) {
			return "", false
		}
		return "select_option:" + strconv.Itoa(ans.Option), p.sendOption(ctx, s, ap, ans.Option)
	case PromptReject:
		neg := 0
		for i, o := range ap.Options {
			if isNegative(o) {
				neg = i + 1
				break
			}
		}
		if neg == 0 {
			if !p.sendKey(ctx, s, "Escape") {
				return "", false
			}
		} else if !p.sendOption(ctx, s, ap, neg) {
			return "", false
		}
		if ans.Text != "" {
			_ = p.Autopilot.TypeText(ctx, s, ans.Text)
		}
		return "reject", true
	case PromptType:
		if err := p.Autopilot.TypeText(ctx, s, ans.Text); err != nil {
			slog.Warn("autopilot: typing brain answer failed", "agent", s.ID, "err", err)
			return "", false
		}
		return "type", true
	}
	return "", false
}

// sendOption selects option idx of the prompt with the keystrokes its menu takes
// (see answer).
func (p *Poller) sendOption(ctx context.Context, s *agentstore.Agent, ap *agentbackend.Approval, idx int) bool {
	if err := p.answer(ctx, s, ap, idx); err != nil {
		slog.Warn("autopilot: failed to send brain answer", "agent", s.ID, "option", idx, "err", err)
		return false
	}
	return true
}

func (p *Poller) sendKey(ctx context.Context, s *agentstore.Agent, key string) bool {
	if err := p.deps.SendKeys(ctx, s.TmuxSession, key); err != nil {
		slog.Warn("autopilot: failed to send brain answer", "agent", s.ID, "key", key, "err", err)
		return false
	}
	return true
}

// isNegative reports whether an option label is a refusal ("No", "Reject", "Deny"…).
func isNegative(label string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	for _, pre := range []string{"no", "reject", "deny", "cancel", "don't", "do not", "skip"} {
		if l == pre || strings.HasPrefix(l, pre+" ") || strings.HasPrefix(l, pre+",") {
			return true
		}
	}
	return false
}
