package daemon

// E2E regression harness: rate-limit recovery + cockpit resilience (wave3).
//
// Three input types are fed through polling and recovery in each scenario:
//
//  1. Confirmed rate-limit banners — via poller.RateLimitObservation (the
//     fresh-excerpt path introduced in PR #547).
//  2. Stale unrelated panes — non-rate-limited status transitions that must not
//     trigger recovery, representing sessions whose pane content is merely old
//     output unrelated to a limit event.
//  3. Terminal/resize sessions (KindTerminal) — handled exclusively by
//     TerminalWatcher; the agent Poller skips them via s.IsTerminal(), so they
//     must never trigger OnRateLimitObservation or any recovery action.
//
// Invariants verified:
//
//   - One eligible switchover at most per recovery generation, even under
//     duplicate concurrent observations.
//   - Correct cooldown: parsed-reset + buffer when parseable; fallbackAt
//     otherwise. The fresh excerpt (not the stale LastPaneExcerpt) drives the
//     parse.
//   - Safe audit metadata: events written by the coordinator and scheduler
//     contain structured identifiers (generation, candidate, phase) — never
//     raw terminal banner text from FreshExcerpt. RateLimitObservation.Fingerprint
//     uses the bounded rl:XXXX format, not the full excerpt.
//   - Cockpit model: terminal-kind sessions (KindTerminal, representing cockpit
//     panes that repaint on resize) are completely orthogonal to recovery;
//     neither OnRateLimitObservation nor OnHardLimit is ever called for them.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// confirmBanner is a Claude-shaped rate-limit banner that matches
// claudeLimitBannerRe (in poller/detect.go) and carries a far-future reset
// time that ParseRestoreTime can parse. UTC avoids local-zone ambiguity.
const confirmBanner = "Claude usage limit reached · resets 11:59pm (UTC)"

// staleUnrelatedPane is output that would appear in a normal working agent pane.
// It does not match any rate-limit detector and must never trigger recovery.
const staleUnrelatedPane = `
> Implementing the feature as requested.
> Running go test ./...
ok      github.com/example/repo/pkg     0.23s
`

// e2eSched builds a RateLimitScheduler for harness scenarios that wire
// OnHardLimit. life is nil (same pattern as backend_recovery_ratelimit_integration_test)
// because HotSwap goes through the coordinator's recoveryLife, not the scheduler.
// auto_resume is false: hard-limit recovery is an independent policy and must
// still claim the session when resume is disabled.
func e2eSched(st store.Store) *RateLimitScheduler {
	return NewRateLimitScheduler(nil, st, 30*time.Minute, 6*time.Hour, time.Minute, false, "")
}

// TestE2EHarness_ConfirmedBannerToSwitchover is the primary integration scenario.
// It feeds a confirmed rate-limit banner via OnRateLimitObservation (fresh-excerpt
// path) and verifies one backend switchover happens, the coordinator claims the
// session, and durable cooldown evidence is written for the original backend/model.
func TestE2EHarness_ConfirmedBannerToSwitchover(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *store.Session, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))

	// Construct the fresh-excerpt observation — this is what the poller emits so
	// the scheduler receives detection-time bytes rather than the stale
	// LastPaneExcerpt on the session snapshot.
	obs := poller.NewRateLimitObservation("agent-1", confirmBanner)
	require.Equal(t, store.StatusRateLimited, obs.ClassifierResult)
	require.True(t, strings.HasPrefix(obs.Fingerprint, "rl:"),
		"Fingerprint must carry the rl: prefix for safe audit exposure")
	require.NotContains(t, obs.Fingerprint, confirmBanner,
		"Fingerprint must not contain raw banner text")

	sched.OnRateLimitObservation(obs)

	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		life.mu.Lock()
		n := len(life.swaps)
		life.mu.Unlock()
		return s != nil && s.BackendRecovery != nil &&
			(s.BackendRecovery.Phase == recoveryStabilizing ||
				s.BackendRecovery.Phase == recoverySwitching) && n == 1
	}, time.Second, 5*time.Millisecond,
		"recovery must reach stabilizing with exactly one HotSwap after confirmed banner")

	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()),
		"original backend must have durable cooldown evidence after switchover")
}

// TestE2EHarness_StaleUnrelatedPanesNoSwitchover feeds non-rate-limited status
// transitions through the scheduler and coordinator, verifying that stale or
// unrelated panes never trigger a backend switchover.  A session whose pane shows
// normal working output must never be misread as a hard limit.
func TestE2EHarness_StaleUnrelatedPanesNoSwitchover(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *store.Session, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	sess := st.snapSession("agent-1")
	require.NotNil(t, sess)
	sess.LastPaneExcerpt = staleUnrelatedPane

	// Simulate the transitions a normal working agent produces — none of these
	// should trigger rate-limit handling in the scheduler.
	for _, to := range []store.Status{
		store.StatusIdle,
		store.StatusWorking,
		store.StatusWaitingForInput,
	} {
		sched.OnTransition(sess, store.StatusWorking, to)
	}

	time.Sleep(20 * time.Millisecond)

	s := st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery,
		"stale unrelated panes must not start a recovery generation")

	life.mu.Lock()
	swaps := len(life.swaps)
	life.mu.Unlock()
	require.Zero(t, swaps, "stale unrelated panes must produce no HotSwap calls")
}

// TestE2EHarness_TerminalSessionIndependentOfRecovery verifies the cockpit
// resilience invariant: terminal-kind sessions (KindTerminal), which represent
// cockpit panes that repaint on every terminal resize (SIGWINCH), are completely
// orthogonal to agent recovery.
//
// The Poller skips KindTerminal sessions at the start of its tick loop via the
// s.IsTerminal() guard, so OnRateLimitObservation is never fired for them.  The
// scheduler's OnTransition also exits immediately for any to≠StatusRateLimited
// transition, so the coordinator's OnHardLimit is never reached.
func TestE2EHarness_TerminalSessionIndependentOfRecovery(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	onHardLimitCalled := 0
	sched.OnHardLimit = func(sess *store.Session, until time.Time) bool {
		onHardLimitCalled++
		return c.OnHardLimit(sess, until)
	}

	// Terminal session whose pane contains rate-limit-shaped text — simulates a
	// cockpit pane that was resized and repainted alongside a rate-limited agent.
	termSess := &store.Session{
		ID:              "term-1",
		TmuxSession:     "warden-term-abc",
		Kind:            store.KindTerminal,
		Status:          store.StatusWorking,
		LastPaneExcerpt: confirmBanner,
	}

	// Transitions fired by the TerminalWatcher: these must not reach the
	// rate-limit path because the scheduler exits on to≠StatusRateLimited.
	sched.OnTransition(termSess, store.StatusWorking, store.StatusOrphaned)
	sched.OnTransition(termSess, store.StatusOrphaned, store.StatusWorking)

	// Coordinator OnTransition: skips sessions with no active BackendRecovery
	// (store.Get returns ErrNotFound for "term-1" since it was never inserted).
	c.OnTransition(termSess, store.StatusWorking, store.StatusWorking)
	c.OnTransition(termSess, store.StatusWorking, store.StatusIdle)

	time.Sleep(20 * time.Millisecond)

	require.Zero(t, onHardLimitCalled,
		"OnHardLimit must never be called for terminal-kind session transitions")

	s := st.snapSession("agent-1")
	require.Nil(t, s.BackendRecovery,
		"agent recovery must not be started by terminal session events")

	life.mu.Lock()
	swaps := len(life.swaps)
	life.mu.Unlock()
	require.Zero(t, swaps, "no HotSwap must occur for terminal session events")
}

// TestE2EHarness_OneSwitchoverPerGeneration verifies that duplicate
// OnRateLimitObservation calls for the same session while a recovery generation
// is already active produce exactly one HotSwap — not one per observation.  This
// guards the at-most-one invariant when the poller detects the banner on multiple
// consecutive ticks before the session transitions away.
func TestE2EHarness_OneSwitchoverPerGeneration(t *testing.T) {
	c, st, life := recoveryFixture(t, map[string][]backendusage.Limit{
		"codex":  {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(100)}},
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := e2eSched(st)
	sched.OnHardLimit = func(sess *store.Session, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))

	obs := poller.NewRateLimitObservation("agent-1", confirmBanner)

	// Fire the same observation twice in quick succession — simulates two
	// consecutive poller ticks before the status transitions away.
	sched.OnRateLimitObservation(obs)
	sched.OnRateLimitObservation(obs)

	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		life.mu.Lock()
		n := len(life.swaps)
		life.mu.Unlock()
		return s != nil && s.BackendRecovery != nil &&
			(s.BackendRecovery.Phase == recoveryStabilizing ||
				s.BackendRecovery.Phase == recoverySwitching) && n == 1
	}, time.Second, 5*time.Millisecond)

	time.Sleep(30 * time.Millisecond) // allow any spurious second advance() to complete

	life.mu.Lock()
	swapCount := len(life.swaps)
	life.mu.Unlock()
	require.Equal(t, 1, swapCount,
		"duplicate observations must not trigger more than one HotSwap per generation")
}

// TestE2EHarness_FreshExcerptDrivesResetParse demonstrates that the
// OnRateLimitObservation path (fresh excerpt) correctly drives
// limitClearsAtExcerpt independently of the stale LastPaneExcerpt stored on the
// session.  When the session's stored excerpt is a spend-cap banner (no parseable
// reset time) but the observation carries a clock-time banner, the schedule must
// reflect the parsed reset time — not the 6-hour spend retry interval.
func TestE2EHarness_FreshExcerptDrivesResetParse(t *testing.T) {
	_, st, _ := recoveryFixture(t, nil)

	// No OnHardLimit wired → falls through to the pause-and-resume schedule path.
	// auto_resume must be ON so SetRateLimit is called; life stays nil because
	// we assert the persisted restore time before any resume timer fires.
	sched := NewRateLimitScheduler(nil, st,
		30*time.Minute, // retryInterval   (fallback for no-parse)
		6*time.Hour,    // spendRetryInterval
		0,              // buffer = 0 for deterministic assertion
		true,           // auto_resume so SetRateLimit is called
		"",
	)

	// Seed the session with a spend-cap banner as the stale stored excerpt.
	require.NoError(t, st.Update(context.Background(), "agent-1", func(s *store.Session) error {
		s.LastPaneExcerpt = "You have hit your monthly spend limit. Adjust your monthly spend limit at claude.ai."
		s.Status = store.StatusRateLimited
		return nil
	}))

	expected, ok := poller.ParseRestoreTime(confirmBanner)
	require.True(t, ok, "confirmBanner must parse to a clock-time reset")

	// Fire an observation with a CLOCK-TIME banner (parseable reset at 11:59 PM UTC).
	// The scheduler must use FreshExcerpt, not sess.LastPaneExcerpt.
	obs := poller.NewRateLimitObservation("agent-1", confirmBanner)
	sched.OnRateLimitObservation(obs)

	s := st.snapSession("agent-1")
	require.NotNil(t, s.RateLimitRestoreAt,
		"RateLimitRestoreAt must be set after observation")
	require.WithinDuration(t, expected, *s.RateLimitRestoreAt, time.Second,
		"restore time must come from FreshExcerpt clock parse, not the stale spend-cap excerpt (6h fallback)")
}

// TestE2EHarness_FingerprintFormatAndEventSafety verifies the safe-audit-metadata
// invariant.
//
// RateLimitObservation.Fingerprint must:
//   - Use the "rl:" prefix + exactly 8 lowercase hex digits (bounded, opaque).
//   - Not contain any raw banner text from FreshExcerpt.
//
// Events emitted by the coordinator (backend_recovery_started, etc.) must contain
// structured identifiers (generation, candidate) only — never raw terminal banner
// text or pane menu options from FreshExcerpt.
func TestE2EHarness_FingerprintFormatAndEventSafety(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	rawExcerpt := "Claude usage limit reached · resets 11:59pm (UTC)\n❯ 1. Stop and wait for limit to reset\n  2. Upgrade your plan"
	obs := poller.NewRateLimitObservation("agent-1", rawExcerpt)

	// Fingerprint: "rl:" + exactly 8 lowercase hex chars (4-byte SHA-256 prefix).
	require.Regexp(t, `^rl:[0-9a-f]{8}$`, obs.Fingerprint,
		"Fingerprint must be rl:<8hex> — safe to surface in audit events")
	require.NotContains(t, obs.Fingerprint, "Claude",
		"Fingerprint must not contain raw banner text")
	require.NotContains(t, obs.Fingerprint, "resets",
		"Fingerprint must not contain raw banner text")

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))
	require.True(t, c.OnHardLimit(st.snapSession("agent-1"), time.Now().Add(time.Hour)))

	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		if s == nil || s.BackendRecovery == nil {
			return false
		}
		for _, ev := range s.Events {
			if ev.Type == "backend_recovery_started" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond)

	s := st.snapSession("agent-1")
	for _, ev := range s.Events {
		require.NotContains(t, ev.Detail, "Claude usage limit reached",
			"event %q detail must not contain raw terminal banner text", ev.Type)
		require.NotContains(t, ev.Detail, "Stop and wait",
			"event %q detail must not expose raw terminal menu options", ev.Type)
		if ev.Type == "backend_recovery_started" {
			require.Contains(t, ev.Detail, "generation=",
				"backend_recovery_started event must carry generation identifier")
		}
	}
}

// TestE2EHarness_CooldownFromParsedReset verifies that when the fresh excerpt
// contains a parseable reset time, the durable cooldown for the original backend
// lasts well beyond the retryInterval fallback — proving it used the parsed clock
// time rather than the default 30-minute fallback.
func TestE2EHarness_CooldownFromParsedReset(t *testing.T) {
	c, st, _ := recoveryFixture(t, map[string][]backendusage.Limit{
		"claude": {{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: used(20)}},
	})

	sched := NewRateLimitScheduler(nil, st, 30*time.Minute, 6*time.Hour, 0, false, "")
	sched.OnHardLimit = func(sess *store.Session, until time.Time) bool {
		return c.OnHardLimit(sess, until)
	}

	require.NoError(t, st.UpdateStatus(context.Background(), "agent-1", store.StatusRateLimited))
	obs := poller.NewRateLimitObservation("agent-1", confirmBanner)
	sched.OnRateLimitObservation(obs)

	require.Eventually(t, func() bool {
		s := st.snapSession("agent-1")
		return s != nil && s.BackendRecovery != nil
	}, time.Second, 5*time.Millisecond)

	// Original backend must have a durable cooldown.
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()),
		"codex must have durable cooldown after confirmed hard limit")

	// The cooldown must survive past 30 minutes (proving it came from the parsed
	// clock time, not the 30m retryInterval fallback).
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now().Add(31*time.Minute)),
		"cooldown from parsed reset time must exceed the 30m retryInterval fallback")
}

// TestE2EHarness_CooldownFallbackWhenNoParsedReset verifies that when the
// coordinator receives a fallbackAt time (no parseable reset from the banner),
// the durable cooldown expiry matches that time exactly — not a fabricated value.
func TestE2EHarness_CooldownFallbackWhenNoParsedReset(t *testing.T) {
	c, _, _ := recoveryFixture(t, nil)

	fallback := time.Now().Add(45 * time.Minute)
	require.True(t, c.OnHardLimit(&store.Session{ID: "agent-1"}, fallback))

	// Cooldown is stamped synchronously inside OnHardLimit, before advance() runs.
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", time.Now()),
		"codex must be in cooldown immediately after OnHardLimit")

	// One minute before fallback: still active.
	require.True(t, c.backends.IsRLCoolingDown("codex", "codex-model", fallback.Add(-time.Minute)),
		"cooldown must still be active one minute before the fallback expiry")

	// One second after fallback: expired.
	require.False(t, c.backends.IsRLCoolingDown("codex", "codex-model", fallback.Add(time.Second)),
		"cooldown must have expired after the fallback time")
}
