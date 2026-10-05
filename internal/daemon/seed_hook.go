package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/store"
)

// NewSeedOutcomeHook returns the lifecycle.OnSeed callback: it persists the final
// seed outcome (seed_status / seed_error) on the session and, on a failure, writes
// one audit event and sends one operator notification naming the agent and the
// saved prompt file. a and n may be nil (audit / notifications off).
func NewSeedOutcomeHook(st *agentstore.Store, a *audit.Writer, n notify.Notifier) func(lifecycle.SeedOutcome) {
	return func(o lifecycle.SeedOutcome) {
		err := st.Update(context.Background(), o.AgentID, func(s *agentstore.Agent) error {
			s.SeedStatus, s.SeedError = o.Status, o.Err
			return nil
		})
		if err != nil {
			slog.Warn("prompt seed: could not persist outcome", "agent", o.AgentID, "status", o.Status, "err", err)
		}
		if o.Status != store.SeedFailed {
			return
		}
		a.Log(audit.Event{
			Action: audit.ActionPromptSeedFailed,
			Actor:  "daemon",
			Target: o.AgentID,
			Detail: map[string]string{"backend": o.Backend, "error": o.Err, "prompt_file": o.PromptFile},
		})
		if n != nil {
			title, body := SeedFailureMessage(o)
			n.Notify(title, body)
		}
	}
}

// SeedFailureMessage builds the operator notification for an undelivered initial
// prompt: it names the agent and says where the prompt was saved.
func SeedFailureMessage(o lifecycle.SeedOutcome) (title, body string) {
	body = fmt.Sprintf("%s never received its initial prompt (%s)", o.AgentID, o.Err)
	if o.PromptFile != "" {
		body += fmt.Sprintf(" — the prompt is saved at %s", o.PromptFile)
	}
	return "warden — prompt not delivered", body
}
