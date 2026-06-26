package poller

import (
	"context"
	"fmt"
	"time"

	"github.com/srjn45/warden/internal/ctxtokens"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/store"
)

// compactLandWindow bounds how long a pending /compact waits for its reclaim to
// appear before warden abandons it. Past this, the compaction is presumed to
// have failed to land (or the agent never processed it), so the parked
// pre-compact reading is discarded rather than later credited to an unrelated
// context drop. Var (not const) so tests can shrink it.
var compactLandWindow = 15 * time.Minute

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

	// Read the agent's cumulative billed usage from the same transcript: it feeds
	// the real-spend denominator (OnSpend) and the net compact cost (the output
	// delta straddling a compaction). Best-effort — an unreadable usage block just
	// leaves spend unknown for this tick.
	inUsage, outUsage, usageOK := p.deps.TranscriptUsage(ctx, s)
	if usageOK && p.OnSpend != nil {
		p.OnSpend(s.ID, inUsage, outUsage)
	}

	// Reconcile any /compact still awaiting its reclaim. A compaction lands a few
	// ticks after it is sent; when this reading falls below the parked pre-compact
	// level, the difference is the context warden reclaimed — record it (net of the
	// summary-generation output cost measured from outUsage).
	p.reconcileCompact(s, tokens, outUsage, usageOK, now)

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
			// Park the pre-compact reading so the reclaim can be measured once the
			// compaction lands (reconcileCompact on a later tick). preOut is the
			// cumulative billed output now, before the summary is generated; the
			// output the transcript bills by the landing tick is the generation cost.
			// Overwrites any prior marker — the most recent send is the one to credit.
			p.pendingCompact[s.ID] = compactPending{pre: tokens, preOut: outUsage, outOK: usageOK, at: now}
		}
	}
}

// reconcileCompact checks whether a /compact parked for s has landed (this
// reading dropped below the pre-compact level) and, if so, records the reclaimed
// context as a FeatureCompact saving via OnSaving. Context tokens only fall on
// compaction, so any decrease is the signal; raw is the pre-compact reading and
// kept is what survived. The summary warden told the agent to generate bills a
// one-time output cost that the kept-context side does NOT capture, so the output
// the transcript billed between the /compact send and this landing (curOut-preOut)
// is passed as cost, making the recorded Saved a true NET reclaim. When the usage
// delta is unmeasurable on either side, cost is 0 — warden never guesses the cost
// upward, which could only understate a real saving. A marker older than
// compactLandWindow is abandoned (the compaction never visibly landed) so it can't
// later be credited to an unrelated drop. Tick goroutine only.
func (p *Poller) reconcileCompact(s *store.Session, tokens, curOut int, curOutOK bool, now time.Time) {
	pc, ok := p.pendingCompact[s.ID]
	if !ok {
		return
	}
	if compactLanded(pc.pre, tokens) {
		delete(p.pendingCompact, s.ID)
		if p.OnSaving != nil {
			p.OnSaving(savings.FeatureCompact, s.ID, pc.pre, tokens, compactCost(pc, curOut, curOutOK))
		}
		return
	}
	if now.Sub(pc.at) >= compactLandWindow {
		delete(p.pendingCompact, s.ID)
	}
}

// compactCost is the one-time billed output cost of generating a compaction's
// summary: the cumulative billed output the transcript grew by between the
// /compact send (pc.preOut) and the landing reading (curOut). It is 0 unless both
// readings were measurable and the delta is positive — an unmeasurable or
// non-increasing usage delta yields no (conservative) cost rather than a guess.
func compactCost(pc compactPending, curOut int, curOutOK bool) int {
	if !pc.outOK || !curOutOK {
		return 0
	}
	if d := curOut - pc.preOut; d > 0 {
		return d
	}
	return 0
}

// compactLanded reports whether a context reading taken after a /compact shows
// the compaction has landed — i.e. the reading fell below the pre-compact level.
// Context-window occupancy only decreases on compaction, so any drop is the cue.
func compactLanded(pre, cur int) bool { return cur < pre }
