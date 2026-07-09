package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
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

// autopilotApprovals implements poller.AutopilotApprovals over the daemon's
// autopilot Controller (which run is active + its brain) and mailbox. It is the
// §8 approval-routing seam: an autopilot-owned worker's unanswerable prompt goes
// to its run's brain instead of a human.
type autopilotApprovals struct{ s *Server }

// BrainFor returns the brain owning worker session s while its run is active, so
// the poller can forward s's unanswerable prompt there. ok=false when s is not an
// autopilot-owned worker, carries no run tag, its run has no live brain, or s IS
// the brain (a brain never forwards its own prompt to itself).
func (a autopilotApprovals) BrainFor(s *store.Session) (string, bool) {
	if a.s == nil || a.s.autopilot == nil || s == nil || !s.HasTag(autopilotOwnershipTag) {
		return "", false
	}
	runID := runIDFromTags(s.Tags)
	if runID == "" {
		return "", false
	}
	brainID, ok := a.s.autopilot.ActiveBrainForRun(runID)
	if !ok || brainID == s.ID {
		return "", false
	}
	return brainID, true
}

// Forward delivers a worker's unanswerable prompt to the brain's mailbox and
// mirrors a non-blocking copy to the human inbox (visibility + audit). Both are
// best-effort: a mailbox error is logged, never propagated — the worker's
// progress must not hinge on the notification landing.
func (a autopilotApprovals) Forward(ctx context.Context, brainID string, worker *store.Session, reason string) {
	if a.s == nil || a.s.mbox == nil || worker == nil {
		return
	}
	label := worker.ID
	if worker.Name != "" {
		label = worker.Name + " (" + worker.ID + ")"
	}
	brainBody := fmt.Sprintf("autopilot: worker %s needs a decision — %s. Read its pane/transcript and answer via approve or send_to_agent.", label, reason)
	if _, err := a.s.mbox.Append(mailbox.Message{To: brainID, From: autopilotForwardSender, Body: brainBody}); err != nil {
		slog.Warn("autopilot: forward approval to brain failed", "brain", brainID, "worker", worker.ID, "err", err)
	}
	mirror := fmt.Sprintf("autopilot routed a worker approval to the brain %s (no action needed): worker %s — %s", brainID, label, reason)
	if _, err := a.s.mbox.Append(mailbox.Message{To: humanRecipient, From: autopilotForwardSender, Body: mirror}); err != nil {
		slog.Warn("autopilot: mirror approval to human inbox failed", "worker", worker.ID, "err", err)
	}
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
