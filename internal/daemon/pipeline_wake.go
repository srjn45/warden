package daemon

import (
	"context"

	"github.com/srjn45/warden/internal/mailbox"
)

// pipelineWakeSender stamps a delegated pipeline's push-wake. It reuses the
// reserved "daemon" provenance (same as autopilot's forwarded messages) so the
// wake reads as warden-originated and can't be forged from an agent write path
// (sanitizeSender rejects reserved ids there; daemon-internal appends bypass it).
const pipelineWakeSender = "daemon"

// pipelineWakeNotice is the pane nudge injected into a parked owner so an idle
// orchestrator blocked at its own prompt (not on wait_for_message) still surfaces
// the wake. The durable content is in the inbox; this only prods the agent to read.
const pipelineWakeNotice = "📨 warden: a delegated pipeline you own has an update. Run `warden msg inbox` to read."

// WakePipelineOwner is the executor's OwnerWaker seam (A4 delegated monitoring): it
// lands body durably in the owning orchestrator's inbox, best-effort nudges the
// owner's pane when it is parked, and releases any wait_for_message long-poll
// blocked on that inbox. Best-effort throughout — a delivery failure is swallowed
// so it can never block or fail the reconcile/emit that triggered it. Mirrors
// SendMessage's inbox+wake path but is a trusted daemon-internal write (no
// sanitizeSender gate). No-op when messaging is unconfigured or ownerID is empty.
func (s *Server) WakePipelineOwner(ownerID, body string) {
	if s.mbox == nil || ownerID == "" {
		return
	}
	if _, err := s.mbox.Append(mailbox.Message{To: ownerID, From: pipelineWakeSender, Body: body}); err != nil {
		return
	}
	// Background ctx: this runs off the executor's reconcile/emit path, which has no
	// request context to inherit. A parked (idle / waiting-for-input) owner is nudged
	// so it wakes to read; a working owner is left alone (the message waits, same as
	// SendMessage). The nudge is skipped silently when the session is gone.
	ctx := context.Background()
	if sess, err := s.store.Get(ctx, ownerID); err == nil && sess != nil && parked(sess.Status) {
		_ = s.life.Input(ctx, sess.TmuxSession, pipelineWakeNotice)
	}
	s.notify() // release any wait_for_message long-poll waiting on this inbox
}
