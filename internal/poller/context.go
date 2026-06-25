package poller

import (
	"context"
	"fmt"
	"time"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/store"
)

// ctxDecision is the outcome of evaluating an agent's context state this tick.
type ctxDecision struct {
	Alert   bool // fire the threshold-crossing notification
	Compact bool // send /compact now
	Suggest bool // surface a "compact before crash" anomaly (critical + can't auto-compact)
}

// decideContext is the pure policy for the context-size guard.
//
//   - Alert fires once per upward crossing into warning or critical (cur != prev
//     and cur is a threshold band), when the warn-alert flag is on.
//   - Compact fires whenever the agent is critical AND idle/waiting AND the
//     auto-compact flag is on AND the cooldown since the last /compact has
//     elapsed. It deliberately does NOT require an edge: that lets a compact that
//     was deferred while the agent was "working" fire on a later tick once it
//     goes idle, while the cooldown prevents re-sending before a just-issued
//     compaction shows up in the transcript.
//   - Suggest fires when the agent is critical but NOT idle/waiting — exactly the
//     case the auto-compact path can't act on (warden won't interrupt a working
//     agent to /compact). Its context only grows from here, so the caller surfaces
//     a "compact before it crashes" anomaly (once per critical episode). It is
//     independent of autoCompact: when auto-compact is off, the human nudge matters
//     even more.
func decideContext(prev, cur ctxtokens.State, status store.Status, sinceCompact, cooldown time.Duration, warnAlert, autoCompact bool) ctxDecision {
	var d ctxDecision
	if warnAlert && cur != prev && (cur == ctxtokens.StateWarning || cur == ctxtokens.StateCritical) {
		d.Alert = true
	}
	idle := status == store.StatusIdle || status == store.StatusWaitingForInput
	if cur == ctxtokens.StateCritical && !idle {
		d.Suggest = true
	}
	if autoCompact && cur == ctxtokens.StateCritical && idle && sinceCompact >= cooldown {
		d.Compact = true
	}
	return d
}

// checkContext reads the agent's context tokens (throttled by the caller),
// classifies them, persists the gauge, and applies decideContext: it fires the
// alert hook and/or sends /compact. It is called only for live, non-terminal
// sessions. A read with ok=false (no model turn yet) is a no-op.
func (p *Poller) checkContext(ctx context.Context, s *store.Session, now time.Time) {
	tokens, ok := p.deps.ContextTokens(ctx, s)
	if !ok {
		return
	}
	cur := ctxtokens.Classify(tokens, p.TokenWarn, p.TokenCrit)
	prev := ctxtokens.State(s.ContextState)
	if err := p.deps.UpdateContext(ctx, s.ID, tokens, string(cur)); err == nil {
		s.ContextState = string(cur) // keep the snapshot coherent for this tick
		s.ContextTokens = tokens
	}

	sinceCompact := p.CompactCooldown // default to "elapsed" when never compacted
	if s.LastCompactAt != nil {
		sinceCompact = now.Sub(*s.LastCompactAt)
	}
	d := decideContext(prev, cur, s.Status, sinceCompact, p.CompactCooldown, p.WarnAlert, p.AutoCompact)

	if d.Alert && p.OnContextAlert != nil {
		p.OnContextAlert(s, cur, tokens)
	}
	// Pre-crash surface: a critical, still-working agent can't be auto-compacted,
	// so its context will only grow toward a crash. Raise the anomaly once per
	// critical episode (cleared when the agent drops out of critical), so the
	// operator gets a single actionable nudge rather than a per-tick stream.
	if cur != ctxtokens.StateCritical {
		delete(p.preCrashFlagged, s.ID)
	} else if d.Suggest && !p.preCrashFlagged[s.ID] {
		p.preCrashFlagged[s.ID] = true
		p.raiseAnomaly(ctx, s, Anomaly{
			Kind:   anomalyPreCrash,
			Detail: fmt.Sprintf("context critical (%dk) while still working — warden can't auto-/compact a busy agent; /compact it now to avoid a crash", tokens/1000),
		})
	}
	if d.Compact {
		if err := p.deps.Compact(ctx, s); err == nil {
			t := now
			s.LastCompactAt = &t
			_ = p.deps.StampCompact(ctx, s.ID)
		}
	}
}
