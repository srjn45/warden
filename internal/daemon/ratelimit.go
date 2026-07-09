package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
)

// resumeKey is the raw keystroke sent to un-pause a rate-limited Claude pane
// when no rate_limit_resume_prompt is configured. Unlike a textual nudge it
// injects no user turn, so it never pollutes the transcript.
//
// TODO(open-question): confirm the correct un-pause keystroke against a LIVE
// limit hit. This is intentionally fail-closed: a wrong key is a benign no-op
// the next poll re-detects, not a transcript-corrupting action. Keep this in
// sync with the poller's banner constants (claudeLimitBannerRe et al.).
const resumeKey = "Enter"

// resumePaneLines is how many trailing pane rows we capture to re-check the
// banner before resuming; LimitBannerPresent only inspects the last few lines.
const resumePaneLines = 20

// RateLimitScheduler manages scheduled resume attempts for rate-limited agents.
type RateLimitScheduler struct {
	life  Lifecycle
	store store.Store

	retryInterval      time.Duration
	spendRetryInterval time.Duration
	buffer             time.Duration
	enabled            bool
	resumePrompt       string // text to inject on resume; "" = bare keypress only

	// CaptureDir, when non-empty, is where the fixture-capture aid snapshots the
	// trailing pane text on every rate-limit detection (see captureBanner). Left
	// empty in tests and any caller that doesn't want the capture. Set by the
	// daemon after construction.
	CaptureDir string

	mu     sync.Mutex
	timers map[string]*time.Timer
}

// maxRateLimitCaptures bounds how many pane snapshots the fixture-capture aid
// keeps in CaptureDir — newest-N, so the directory can't grow without limit
// while still preserving the most recent real limit hits for parser work.
const maxRateLimitCaptures = 20

// NewRateLimitScheduler creates a new scheduler. The retry interval, spend retry
// interval, buffer, auto-resume toggle, and resume prompt are supplied by the
// caller from config (rate_limit.retry_interval / rate_limit.spend_retry_interval
// / rate_limit.buffer / rate_limit.auto_resume / rate_limit.resume_prompt).
//
// retryInterval is the fallback wait when a resettable limit (session/weekly)
// shows no parseable reset time; spendRetryInterval is the (longer) fallback for
// a monthly spend cap, which carries no reset time and will not clear for hours
// or days. An empty resumePrompt means resume with a bare keypress and no
// injected user turn.
func NewRateLimitScheduler(life Lifecycle, st store.Store, retryInterval, spendRetryInterval, buffer time.Duration, autoResume bool, resumePrompt string) *RateLimitScheduler {
	return &RateLimitScheduler{
		life:               life,
		store:              st,
		timers:             make(map[string]*time.Timer),
		retryInterval:      retryInterval,
		spendRetryInterval: spendRetryInterval,
		buffer:             buffer,
		enabled:            autoResume,
		resumePrompt:       resumePrompt,
	}
}

// clearTimer stops and removes the timer for a session.
func (r *RateLimitScheduler) clearTimer(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.timers[sessionID]; t != nil {
		t.Stop()
	}
	delete(r.timers, sessionID)
}

// OnTransition is wired as a callback on the poller's status-transition hook.
func (r *RateLimitScheduler) OnTransition(sess *store.Session, from, to store.Status) {
	if to != store.StatusRateLimited {
		return
	}

	// Snapshot the pane on every real limit hit, regardless of auto_resume, so the
	// next live limit yields exact bytes to close any parser gap. Cheap and
	// bounded; a capture failure must never block the resume path.
	r.captureBanner(sess)

	if !r.enabled {
		return
	}

	ctx := context.Background()

	// Parse restore time from pane excerpt (already captured by poller)
	restoreTime, ok := poller.ParseRestoreTime(sess.LastPaneExcerpt)

	var scheduleAt time.Time
	switch {
	case ok && restoreTime.After(time.Now()):
		// A resettable limit (session/weekly) told us exactly when it clears:
		// wake just after that, plus a small buffer for clock skew.
		scheduleAt = restoreTime.Add(r.buffer)
	case poller.SpendLimitBannerPresent(sess.LastPaneExcerpt):
		// Monthly spend cap: no reset time in-band, and it will not clear for
		// hours or days — retry on the long spend interval so we keep polling for
		// the auto-resume without hammering a pane that stays capped.
		scheduleAt = time.Now().Add(r.spendRetryInterval)
	default:
		// A resettable limit whose reset time we could not parse: retry soon.
		scheduleAt = time.Now().Add(r.retryInterval)
	}

	// Persist the schedule
	_ = r.store.SetRateLimit(ctx, sess.ID, scheduleAt, 0)

	// Schedule the resume attempt
	r.scheduleResume(sess.ID, scheduleAt)
}

// scheduleResume creates a timer for the resume attempt.
func (r *RateLimitScheduler) scheduleResume(sessionID string, at time.Time) {
	delay := time.Until(at)
	if delay < 0 {
		delay = 0 // resume immediately if time already passed
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Cancel existing timer if any
	if existing := r.timers[sessionID]; existing != nil {
		existing.Stop()
	}

	// Create new timer
	r.timers[sessionID] = time.AfterFunc(delay, func() {
		r.attemptResume(sessionID)
	})
}

// attemptResume fires when a scheduled timer triggers.
func (r *RateLimitScheduler) attemptResume(sessionID string) {
	ctx := context.Background()

	sess, err := r.store.Get(ctx, sessionID)
	if err != nil {
		// Session gone (deleted, archived)
		r.mu.Lock()
		delete(r.timers, sessionID)
		r.mu.Unlock()
		return
	}

	// Only resume if still rate limited
	if sess.Status != store.StatusRateLimited {
		// User manually resumed or status changed
		r.mu.Lock()
		delete(r.timers, sessionID)
		r.mu.Unlock()
		return
	}

	// Attempt resume
	err = r.life.Restore(ctx, sess)

	// If the tmux session already exists, Claude is paused showing the limit
	// banner. Un-pause it in place rather than recreating the session.
	if err == lifecycle.ErrAlreadyRunning {
		// Gate: only act if the limit banner is still the trailing pane state. If
		// the agent already moved on, do nothing destructive — clear and exit so a
		// stale schedule can't nudge an agent that has already resumed.
		pane, _ := r.life.Output(ctx, sess.TmuxSession, resumePaneLines)
		if !poller.LimitBannerPresent(pane) {
			_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusSpawning)
			_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
				TS:     time.Now().UTC(),
				Type:   "rate-limit-resumed",
				Detail: "banner cleared before resume; no nudge sent",
			})
			r.clearTimer(sess.ID)
			return
		}

		// Default: a bare un-pause keypress (no injected user turn). A non-empty
		// resumePrompt opts into sending that text instead.
		var sendErr error
		detail := "sent bare resume keypress (tmux session exists)"
		if r.resumePrompt == "" {
			sendErr = r.life.SendKeys(ctx, sess.TmuxSession, resumeKey)
		} else {
			sendErr = r.life.Input(ctx, sess.TmuxSession, r.resumePrompt)
			detail = "sent resume prompt " + strconv.Quote(r.resumePrompt)
		}
		if sendErr != nil {
			// Send failed - treat as non-rate-limit error
			_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusErrored)
			_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
				TS:     time.Now().UTC(),
				Type:   "rate-limit-resume-failed",
				Detail: "resume send failed: " + sendErr.Error(),
			})
			r.clearTimer(sess.ID)
			return
		}

		// Send succeeded - transition to spawning and let poller verify. If still
		// rate-limited, poller will detect and call OnTransition again.
		_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusSpawning)
		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-resumed",
			Detail: detail,
		})
		r.clearTimer(sess.ID)
		return
	}

	if err == nil {
		// SUCCESS: Restore created new session
		_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusSpawning)
		_ = r.store.ClearRateLimit(ctx, sess.ID)

		// Clean up timer
		r.mu.Lock()
		delete(r.timers, sess.ID)
		r.mu.Unlock()

		return
	}

	// FAILURE: check if error indicates still rate limited
	errMsg := err.Error()

	// Try to parse as rate limit error
	// NOTE: This will use detectRateLimit which only checks keywords for now
	isRateLimit := false
	errLower := strings.ToLower(errMsg)
	rateLimitKeywords := []string{"rate limit", "usage limit", "session limit", "quota exceeded"}
	for _, kw := range rateLimitKeywords {
		if strings.Contains(errLower, kw) {
			isRateLimit = true
			break
		}
	}

	if isRateLimit {
		// Still rate limited - reschedule with parsed timestamp if available
		scheduleAt := time.Now().Add(r.retryInterval) // fallback
		detail := "no time parsed, retrying in " + r.retryInterval.String()

		// Try to parse restore time from error message
		if parsedTime, ok := poller.ParseRestoreTime(errMsg); ok {
			scheduleAt = parsedTime.Add(r.buffer)
			detail = "parsed restore time: " + parsedTime.Format(time.RFC3339)
		} else if poller.SpendLimitBannerPresent(sess.LastPaneExcerpt) {
			// Monthly spend cap: use the long spend interval, matching OnTransition.
			scheduleAt = time.Now().Add(r.spendRetryInterval)
			detail = "monthly spend cap, retrying in " + r.spendRetryInterval.String()
		}

		_ = r.store.SetRateLimit(ctx, sess.ID, scheduleAt, sess.RateLimitRetryCount+1)
		r.scheduleResume(sess.ID, scheduleAt)

		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-retry",
			Detail: detail,
		})
	} else {
		// Different error (network, auth, etc.) - transition to errored
		_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusErrored)
		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-resume-failed",
			Detail: "resume failed with non-limit error: " + err.Error(),
		})

		// Clean up timer
		r.mu.Lock()
		delete(r.timers, sess.ID)
		r.mu.Unlock()
	}
}

// captureBanner snapshots the agent's trailing pane text (the banner and/or
// menu that tripped rate-limit detection) to CaptureDir as a fixture-capture
// aid, then prunes the directory to the newest maxRateLimitCaptures files. It is
// a permanent, cheap diagnostic: the next real limit hit leaves the exact bytes
// on disk so a parser gap (e.g. an unhandled weekly-banner format) can be fixed
// from ground truth instead of guessed. Best-effort — any error is logged and
// swallowed so it never interferes with the resume path. A no-op when CaptureDir
// is empty or the pane excerpt is blank.
func (r *RateLimitScheduler) captureBanner(sess *store.Session) {
	if r.CaptureDir == "" || strings.TrimSpace(sess.LastPaneExcerpt) == "" {
		return
	}
	if err := os.MkdirAll(r.CaptureDir, 0o700); err != nil {
		slog.Warn("rate-limit capture: mkdir failed", "dir", r.CaptureDir, "err", err)
		return
	}
	// Timestamped, id-tagged name so captures sort chronologically and never
	// collide; the raw excerpt is the file body for verbatim fixture extraction.
	name := time.Now().UTC().Format("20060102T150405.000Z") + "-" + sess.ID + ".txt"
	if err := os.WriteFile(filepath.Join(r.CaptureDir, name), []byte(sess.LastPaneExcerpt), 0o600); err != nil {
		slog.Warn("rate-limit capture: write failed", "dir", r.CaptureDir, "err", err)
		return
	}
	r.pruneCaptures()
}

// pruneCaptures keeps only the newest maxRateLimitCaptures .txt files in
// CaptureDir, deleting the oldest. Names are timestamp-prefixed so a lexical
// sort is chronological.
func (r *RateLimitScheduler) pruneCaptures() {
	entries, err := os.ReadDir(r.CaptureDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".txt") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= maxRateLimitCaptures {
		return
	}
	sort.Strings(names) // oldest first
	for _, old := range names[:len(names)-maxRateLimitCaptures] {
		_ = os.Remove(filepath.Join(r.CaptureDir, old))
	}
}

// ReconstructTimers rebuilds active timers from session state on daemon startup.
func (r *RateLimitScheduler) ReconstructTimers(ctx context.Context) error {
	sessions, err := r.store.List(ctx)
	if err != nil {
		return err
	}

	for _, sess := range sessions {
		if sess.Status == store.StatusRateLimited && sess.RateLimitRestoreAt != nil {
			r.scheduleResume(sess.ID, *sess.RateLimitRestoreAt)
		}
	}

	return nil
}

// CancelTimer stops and removes the timer for a session.
func (r *RateLimitScheduler) CancelTimer(sessionID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if timer := r.timers[sessionID]; timer != nil {
		timer.Stop()
		delete(r.timers, sessionID)
	}
}
