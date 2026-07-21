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

// forceInterruptWindow bounds how long the force-compact machine waits for a
// busy agent to drop to idle after warden sent Escape. Past this the interrupt
// is presumed not to have taken (the agent ignored it or was mid-tool), so the
// machine gives up for this episode and falls back to the human "compact before
// crash" nudge rather than re-interrupting forever. Var (not const) for tests.
var forceInterruptWindow = 45 * time.Second

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
//     auto-compact flag is on AND no /compact is already in flight AND the
//     cooldown since the last /compact has elapsed. It deliberately does NOT
//     require an edge: that lets a compact that was deferred while the agent was
//     "working" fire on a later tick once it goes idle. The in-flight guard
//     (compactPending) is what actually stops a re-send storm: a /compact that
//     hasn't been followed by a fresh prompt leaves the transcript's last
//     assistant turn still reporting the pre-compact (critical) fill, so the
//     cooldown alone would re-fire /compact every cooldown until the land window
//     abandons the marker. Holding off while a send is still parked caps that at
//     one /compact per episode (re-armed only when the marker lands or goes
//     stale). The cooldown still guards the gap before the marker is parked and
//     any re-fire after a landed compaction left the agent critical.
//   - Suggest fires when the agent is critical but NOT idle/waiting — exactly the
//     case the auto-compact path can't act on (warden won't interrupt a working
//     agent to /compact). Its context only grows from here, so the caller surfaces
//     a "compact before it crashes" anomaly (once per critical episode). It is
//     independent of autoCompact: when auto-compact is off, the human nudge matters
//     even more.
func decideContext(prev, cur ctxtokens.State, status store.Status, sinceCompact, cooldown time.Duration, warnAlert, autoCompact, compactPending bool) ctxDecision {
	var d ctxDecision
	if warnAlert && cur != prev && (cur == ctxtokens.StateWarning || cur == ctxtokens.StateCritical) {
		d.Alert = true
	}
	idle := status == store.StatusIdle || status == store.StatusWaitingForInput
	if cur == ctxtokens.StateCritical && !idle {
		d.Suggest = true
	}
	if autoCompact && cur == ctxtokens.StateCritical && idle && !compactPending && sinceCompact >= cooldown {
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
	// Snapshot the hot-reloadable guard knobs once for this tick (a live config
	// reload may swap them concurrently via SetContextGuard).
	g := p.ctxGuard()
	cur := ctxtokens.Classify(tokens, g.Warn, g.Crit)
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
		p.OnSpend(s, inUsage, outUsage)
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
	// A /compact already parked for this agent (reconcileCompact above ran first,
	// so a landed or stale marker is gone by now) means a previous send hasn't
	// shown its reclaim yet — don't pile another on top while it's still in flight.
	_, compactInFlight := p.pendingCompact[s.ID]
	d := decideContext(prev, cur, s.Status, sinceCompact, p.CompactCooldown, g.WarnAlert, g.AutoCompact, compactInFlight)

	if d.Alert && p.OnContextAlert != nil {
		p.OnContextAlert(s, cur, tokens)
	}

	// Force-compact machine: for agents it is enabled on, warden interrupts a busy
	// critical agent (Escape), /compact-s it once idle, then resumes it. When it is
	// actively driving this agent it owns the compaction (so skip the normal
	// idle-compact below) and the busy-critical case is handled (so suppress the
	// human pre-crash nudge). It compacts via the same park-pendingCompact path, so
	// savings + the in-flight guard still apply.
	forceHandling := p.stepForceCompact(ctx, s, cur, tokens, outUsage, usageOK, sinceCompact, now)

	// Pre-crash surface: a critical, still-working agent can't be auto-compacted,
	// so its context will only grow toward a crash. Raise the anomaly once per
	// critical episode (cleared when the agent drops out of critical), so the
	// operator gets a single actionable nudge rather than a per-tick stream. The
	// force machine, when driving, will interrupt+compact instead, so don't also
	// nag the operator.
	if cur != ctxtokens.StateCritical {
		delete(p.preCrashFlagged, s.ID)
	} else if d.Suggest && !forceHandling && !p.preCrashFlagged[s.ID] {
		p.preCrashFlagged[s.ID] = true
		p.raiseAnomaly(ctx, s, Anomaly{
			Kind:   anomalyPreCrash,
			Detail: fmt.Sprintf("context critical (%dk) while still working — warden can't auto-/compact a busy agent; /compact it now to avoid a crash", tokens/1000),
		})
	}
	if d.Compact && !forceHandling {
		p.sendCompact(ctx, s, tokens, outUsage, usageOK, now)
	}
}

// sendCompact issues /compact to s and parks the pre-compact reading so the
// reclaim can be measured once the compaction lands (reconcileCompact on a later
// tick). preOut is the cumulative billed output now, before the summary is
// generated; the output the transcript bills by the landing tick is the
// generation cost. Overwrites any prior marker — the most recent send is the one
// to credit. Shared by the idle auto-compact path and the force-compact machine.
func (p *Poller) sendCompact(ctx context.Context, s *store.Session, tokens, outUsage int, usageOK bool, now time.Time) bool {
	if err := p.deps.Compact(ctx, s); err != nil {
		return false
	}
	t := now
	s.LastCompactAt = &t
	_ = p.deps.StampCompact(ctx, s.ID)
	p.pendingCompact[s.ID] = compactPending{pre: tokens, preOut: outUsage, outOK: usageOK, at: now}
	return true
}

// stepForceCompact advances the per-agent force-compact machine one tick and
// reports whether it is actively driving this agent (the caller then skips the
// normal idle-compact and suppresses the busy-critical nudge).
//
// The flow mirrors the operator's manual fix — Escape to interrupt → /compact →
// resume prompt — as a state machine across ticks:
//
//   - awaiting land: a /compact this machine sent has cleared from pendingCompact
//     (reconcileCompact ran earlier this tick), meaning it landed (context
//     dropped) or was abandoned. On a real landing (no longer critical) send the
//     configured resume prompt; on abandonment (still critical) just clear and let
//     the normal paths retry. This is checked first because a landing drops the
//     agent out of critical, so it happens outside the critical guard below.
//   - not yet enabled / not critical: clear any stale state and bow out.
//   - critical + idle, not started: /compact directly (no interrupt needed).
//   - critical + busy, not started: send Escape and wait for idle.
//   - critical + busy, interrupting: /compact once idle, else abandon past
//     forceInterruptWindow (the interrupt never took).
//
// The compact itself is gated by the cooldown so a failed/slow landing can't
// storm /compact. Tick goroutine only.
func (p *Poller) stepForceCompact(ctx context.Context, s *store.Session, cur ctxtokens.State, tokens, outUsage int, usageOK bool, sinceCompact time.Duration, now time.Time) bool {
	g := p.ctxGuard() // hot-reloadable: force-compact default + resume prompt
	st, active := p.forceCompact[s.ID]

	// Resume / cleanup: a force-compaction we were awaiting has resolved.
	if active && st.phase == fcAwaitLand {
		if _, pending := p.pendingCompact[s.ID]; pending {
			return true // still waiting for the compaction to land
		}
		delete(p.forceCompact, s.ID)
		// Resume only on a real landing — context fell out of critical. If it is
		// still critical the compaction was abandoned (never visibly landed); don't
		// resume into an un-compacted context, just let the normal paths re-drive.
		if cur != ctxtokens.StateCritical && g.CompactResume != "" {
			_ = p.deps.Resume(ctx, s, g.CompactResume)
		}
		return false
	}

	eff := g.ForceCompact
	if s.ForceCompact != nil {
		eff = *s.ForceCompact
	}
	if cur != ctxtokens.StateCritical || !eff {
		if active { // no longer critical or disabled mid-flight: drop stale state
			delete(p.forceCompact, s.ID)
		}
		return false
	}

	idle := s.Status == store.StatusIdle || s.Status == store.StatusWaitingForInput

	if !active {
		if idle {
			// Already idle — no turn to interrupt; compact straight away.
			if sinceCompact >= p.CompactCooldown {
				p.sendCompact(ctx, s, tokens, outUsage, usageOK, now)
				p.forceCompact[s.ID] = fcState{phase: fcAwaitLand, at: now}
			}
			return true
		}
		_ = p.deps.Interrupt(ctx, s)
		p.forceCompact[s.ID] = fcState{phase: fcInterrupting, at: now}
		return true
	}

	// active && fcInterrupting: the Escape was sent; wait for the agent to go idle.
	if idle {
		if sinceCompact >= p.CompactCooldown {
			p.sendCompact(ctx, s, tokens, outUsage, usageOK, now)
			p.forceCompact[s.ID] = fcState{phase: fcAwaitLand, at: now}
		}
		return true
	}
	if now.Sub(st.at) >= forceInterruptWindow {
		delete(p.forceCompact, s.ID) // interrupt never took; fall back to the nudge
		return false
	}
	return true
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
