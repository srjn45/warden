package daemon

import (
	"context"
	"fmt"

	"github.com/srjn45/warden/internal/groupstore"
	"github.com/srjn45/warden/internal/mailbox"
)

// groupIntroSender stamps warden-brokered introduction messages. It reuses the
// reserved "daemon" provenance (same as the pipeline wake and autopilot's
// forwarded approvals) so an intro reads as warden-originated and can't be forged
// from an agent write path (sanitizeSender rejects reserved ids there;
// daemon-internal appends bypass it).
const groupIntroSender = "daemon"

// groupIntroNotice is the pane nudge injected into a parked recipient so an idle
// agent (not blocked on wait_for_message) still surfaces the introduction. The
// durable descriptor is in the inbox; this only prods the agent to read.
const groupIntroNotice = "📨 warden: a new collaboration-group introduction arrived. Run `warden msg inbox` to read."

// composeIntro builds the uniform, token-free introduction descriptor for one
// roster member (design §3.2). `lead` is the connecting verb phrase — "joined"
// for the announcement of a fresh joiner, "is in" for a reciprocal roster entry —
// so both directions share one template. name is the member's human-friendly
// alias; it falls back to the agent id when unset. A not-yet-resolved summary
// (B5) renders as a placeholder so the sentence stays well-formed.
func composeIntro(lead, groupName string, m groupstore.Member, name string) string {
	who := name
	if who == "" {
		who = m.AgentID
	}
	summary := m.Summary
	if summary == "" {
		summary = "summary pending"
	}
	return fmt.Sprintf("Agent %s (%s) %s group %q; it orchestrates project %s — %s. Contact it for changes to that project.",
		who, m.AgentID, lead, groupName, m.ProjectKey, summary)
}

// brokerIntroductions delivers warden-composed introductions in BOTH directions
// when `joiner` seats into `grp` (design §3.2). Each existing member receives one
// message announcing the joiner; the joiner receives one message per existing
// member (the reciprocal roster). Warden composes and delivers every message, so
// no agent turn is spent — zero agent tokens.
//
// Delivery reuses SendMessage's inbox+wake path as a trusted daemon-internal
// write (no sanitizeSender gate) and is best-effort throughout: a delivery
// failure is swallowed so it can never fail the join it follows. No-op when
// messaging is unconfigured.
func (s *Server) brokerIntroductions(ctx context.Context, grp *groupstore.Group, joiner groupstore.Member, joinerName string) {
	if s.mbox == nil || grp == nil {
		return
	}
	joinerIntro := composeIntro("joined", grp.Name, joiner, joinerName)
	delivered := false
	for _, m := range grp.Members {
		if m.AgentID == joiner.AgentID {
			continue
		}
		// Existing member ← announcement of the joiner.
		s.deliverIntro(ctx, m.AgentID, joinerIntro)
		// Joiner ← reciprocal roster entry describing this existing member.
		s.deliverIntro(ctx, joiner.AgentID, composeIntro("is in", grp.Name, m, s.sessionName(ctx, m.AgentID)))
		delivered = true
	}
	if delivered {
		s.notify() // release any wait_for_message long-poll blocked on these inboxes
	}
}

// deliverIntro lands one introduction durably in a recipient's inbox and, if the
// recipient is parked (idle / waiting-for-input, not working), nudges its pane so
// it wakes to read. Mirrors WakePipelineOwner; best-effort — every failure is
// swallowed.
func (s *Server) deliverIntro(ctx context.Context, to, body string) {
	if to == "" {
		return
	}
	if _, err := s.mbox.Append(mailbox.Message{To: to, From: groupIntroSender, Body: body}); err != nil {
		return
	}
	if sess, err := s.store.Get(ctx, to); err == nil && sess != nil && parked(sess.Status) {
		_ = s.life.Input(ctx, sess.TmuxSession, groupIntroNotice)
	}
}

// sessionName resolves an agent's human-friendly alias for the intro descriptor,
// returning "" (which composeIntro renders as the bare id) when the session is
// gone or unnamed.
func (s *Server) sessionName(ctx context.Context, id string) string {
	if sess, err := s.store.Get(ctx, id); err == nil && sess != nil {
		return sess.Name
	}
	return ""
}
