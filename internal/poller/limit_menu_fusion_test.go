package poller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

// weeklyResetPostMenuExcerpt is the EXACT regression scenario from the
// usage-api-quota-recovery design record (docs/specs/2026-09-29-usage-api-quota-recovery.md
// §Regression Scenario): after Claude's rate-limit menu safely selects "wait for
// reset", the pane renders this text with NO parseable `resets` banner — so
// claudeLimitBannerRe never matches it and the legacy banner-only path would
// never start recovery. Pane signal fusion (Phase 7) closes exactly this gap by
// treating the confirmed menu selection itself as the trigger.
const weeklyResetPostMenuExcerpt = "Requesting rate limit reset for weekly limit"

// TestTryLimitMenu_ConfirmedSelectionFiresWeeklyResetObservation is the exact
// weekly-reset post-menu fixture: a confirmed "wait for limit to reset"
// selection whose post-menu pane carries no parseable `resets` banner must still
// fire OnLimitMenuSelected with the fresh post-selection excerpt, immediately —
// not conditioned on (and not waiting for) a later banner.
func TestTryLimitMenu_ConfirmedSelectionFiresWeeklyResetObservation(t *testing.T) {
	restore := menuVerifyDelay
	menuVerifyDelay = 0
	defer func() { menuVerifyDelay = restore }()

	// Prove the premise: the exact regression text does NOT match the banner
	// detector — this is the gap that would otherwise leave recovery stalled.
	require.False(t, LimitBannerPresent(weeklyResetPostMenuExcerpt),
		"the weekly-reset post-menu text intentionally carries no parseable `resets` banner")

	menu := sampleLimitMenu // highlighted wait option
	d, p, s := menuSession(t, weeklyResetPostMenuExcerpt)

	var got []RateLimitObservation
	p.OnLimitMenuSelected = func(obs RateLimitObservation) { got = append(got, obs) }

	p.tryLimitMenu(context.Background(), s, menu)

	require.Equal(t, []string{"Enter"}, d.sentSequence("A-1"),
		"a highlighted wait option confirms on bare Enter with no fallback")
	require.Len(t, got, 1, "a confirmed selection must fire exactly one observation")
	require.Equal(t, "A-1", got[0].SessionID)
	require.Equal(t, store.StatusRateLimited, got[0].ClassifierResult)
	require.Equal(t, weeklyResetPostMenuExcerpt, got[0].FreshExcerpt,
		"the observation must carry the FRESH post-selection excerpt, not the triggering menu pane")
	require.NotEmpty(t, got[0].Fingerprint)
}

// TestTryLimitMenu_SelectionFailureNeverFiresObservation proves that a menu
// which never clears — even after the explicit number+Enter fallback — is
// diagnostic only. A selection that cannot be confirmed must never generate a
// confirmed limit observation.
func TestTryLimitMenu_SelectionFailureNeverFiresObservation(t *testing.T) {
	restore := menuVerifyDelay
	menuVerifyDelay = 0
	defer func() { menuVerifyDelay = restore }()

	menu := sampleLimitMenu
	// The stubbed recapture keeps returning the SAME menu forever: neither the
	// first keystroke nor the fallback ever dismisses it.
	d, p, s := menuSession(t, menu)

	called := false
	p.OnLimitMenuSelected = func(RateLimitObservation) { called = true }

	p.tryLimitMenu(context.Background(), s, menu)

	require.Equal(t, []string{"Enter", "1", "Enter"}, d.sentSequence("A-1"),
		"a persistent menu must still get the explicit number+Enter fallback")
	require.False(t, called, "a selection that never confirms must never fire a confirmed observation")
}

// TestTryLimitMenu_FalsePositiveNeverSendsKeysOrFiresObservation proves that an
// unrelated prompt — one that is NOT Claude's recognized "wait for limit to
// reset" menu — never triggers a keystroke or a confirmed observation. This
// guards against a generic numbered menu (e.g. an ordinary approval prompt)
// being misread as rate-limit evidence.
func TestTryLimitMenu_FalsePositiveNeverSendsKeysOrFiresObservation(t *testing.T) {
	restore := menuVerifyDelay
	menuVerifyDelay = 0
	defer func() { menuVerifyDelay = restore }()

	unrelated := "Do you want to proceed?\n❯ 1. Yes\n  2. No\nEnter to confirm"
	d, p, s := menuSession(t, unrelated)

	called := false
	p.OnLimitMenuSelected = func(RateLimitObservation) { called = true }

	p.tryLimitMenu(context.Background(), s, unrelated)

	require.Empty(t, d.sentSequence("A-1"), "an unrelated menu must never be auto-answered")
	require.False(t, called, "an unrelated menu must never fire a confirmed limit observation")
}

// TestTryLimitMenu_RecaptureErrorNeverFiresObservation proves that a transient
// pane-capture failure after the selection keystroke never fabricates a
// confirmed observation (nor sends a spurious fallback keystroke into a pane
// whose real state could not be read).
func TestTryLimitMenu_RecaptureErrorNeverFiresObservation(t *testing.T) {
	restore := menuVerifyDelay
	menuVerifyDelay = 0
	defer func() { menuVerifyDelay = restore }()

	s := &agentstore.Agent{ID: "A-1", TmuxSession: "A-1", Status: store.StatusRateLimited}
	d := &stubDeps{
		sessions:   []*agentstore.Agent{s},
		alive:      map[string]bool{"A-1": true},
		updates:    map[string]store.Status{},
		captureErr: errors.New("tmux capture-pane: no such session"),
	}
	p := New(d, 5*time.Minute)
	p.RateLimitAutoResume = true

	called := false
	p.OnLimitMenuSelected = func(RateLimitObservation) { called = true }

	p.tryLimitMenu(context.Background(), s, sampleLimitMenu)

	require.Equal(t, []string{"Enter"}, d.sentSequence("A-1"),
		"the first keystroke must still be sent even though the recapture will fail")
	require.False(t, called, "a failed recapture must never fire a confirmed limit observation")
}

// TestTryLimitMenu_NoopWithoutHook proves that a nil OnLimitMenuSelected (the
// default) never panics and leaves the menu-answering behavior unchanged — the
// fused-observation step is purely additive.
func TestTryLimitMenu_NoopWithoutHook(t *testing.T) {
	restore := menuVerifyDelay
	menuVerifyDelay = 0
	defer func() { menuVerifyDelay = restore }()

	d, p, s := menuSession(t, "the agent has moved on, working...")
	require.Nil(t, p.OnLimitMenuSelected)

	require.NotPanics(t, func() { p.tryLimitMenu(context.Background(), s, sampleLimitMenu) })
	require.Equal(t, []string{"Enter"}, d.sentSequence("A-1"))
}

// TestTryLimitMenu_UsesFreshRecaptureNotStaleTriggeringPane proves the
// stale-pane guard: the observation's FreshExcerpt comes from the FRESH
// post-selection recapture, never from the (now-stale) menu pane that
// triggered the selection in the first place.
func TestTryLimitMenu_UsesFreshRecaptureNotStaleTriggeringPane(t *testing.T) {
	restore := menuVerifyDelay
	menuVerifyDelay = 0
	defer func() { menuVerifyDelay = restore }()

	triggeringPane := sampleLimitMenu
	freshPostSelectionPane := "Requesting rate limit reset for weekly limit\n(this is NOT the stale menu text)"
	d, p, s := menuSession(t, freshPostSelectionPane)

	var got RateLimitObservation
	p.OnLimitMenuSelected = func(obs RateLimitObservation) { got = obs }

	p.tryLimitMenu(context.Background(), s, triggeringPane)

	require.Equal(t, []string{"Enter"}, d.sentSequence("A-1"))
	require.Equal(t, freshPostSelectionPane, got.FreshExcerpt,
		"the observation must reflect the fresh recapture, not the stale triggering menu pane")
	require.NotContains(t, got.FreshExcerpt, "Stop and wait",
		"the stale menu text must not leak into the fused observation")
}
