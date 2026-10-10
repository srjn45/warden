package daemon

import (
	"context"
	"log/slog"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

// EventRelaunchPermissionMode is the agent event type recorded when a relaunch
// launched with a translated, stepped-down or defaulted permission mode.
const EventRelaunchPermissionMode = "relaunch-permission-mode"

// NewModeNormalizedHook returns the lifecycle OnModeNormalized hook: after a
// relaunch has successfully launched it persists the corrected mode (when the
// resolution says to), appends an agent event and writes an audit record. The
// lifecycle only calls it post-launch, so a failed relaunch leaves the stored
// mode and the event log untouched.
func NewModeNormalizedHook(st *agentstore.Store, a *audit.Writer) func(context.Context, lifecycle.ModeNormalization) {
	return func(ctx context.Context, n lifecycle.ModeNormalization) {
		if n.Persist {
			err := st.Update(ctx, n.AgentID, func(s *agentstore.Agent) error {
				s.PermissionMode = n.To
				return nil
			})
			if err != nil {
				slog.Warn("relaunch: could not persist normalized permission mode", "agent", n.AgentID, "from", n.From, "to", n.To, "err", err)
			}
		}
		detail := n.Rationale.Note()
		if detail == "" {
			detail = "permission_mode " + n.From + " -> " + n.To
		}
		if err := st.AppendEvent(ctx, n.AgentID, store.Event{Type: EventRelaunchPermissionMode, Detail: detail}); err != nil {
			slog.Warn("relaunch: could not record permission-mode event", "agent", n.AgentID, "err", err)
		}
		persisted := "false"
		if n.Persist {
			persisted = "true"
		}
		a.Log(audit.Event{
			Action: audit.ActionRelaunchModeNormalized,
			Actor:  "daemon",
			Target: n.AgentID,
			Detail: map[string]string{
				"path":           n.Path,
				"from_mode":      n.From,
				"to_mode":        n.To,
				"persisted":      persisted,
				"outcome":        string(n.Rationale.Outcome),
				"stored_backend": n.Rationale.StoredBackend,
				"target_backend": n.Rationale.TargetBackend,
				"stored_intent":  string(n.Rationale.StoredIntent),
				"accepted":       string(n.Rationale.AcceptedIntent),
			},
		})
	}
}
