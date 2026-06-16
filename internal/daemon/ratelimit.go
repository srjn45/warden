package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/poller"
	"github.com/srjn45/warden/internal/store"
)

// RateLimitScheduler manages scheduled resume attempts for rate-limited agents.
type RateLimitScheduler struct {
	life  Lifecycle
	store store.Store

	retryInterval time.Duration
	buffer        time.Duration
	enabled       bool

	mu     sync.Mutex
	timers map[string]*time.Timer
}

// NewRateLimitScheduler creates a new scheduler with configuration from env vars.
func NewRateLimitScheduler(life Lifecycle, st store.Store) *RateLimitScheduler {
	return &RateLimitScheduler{
		life:          life,
		store:         st,
		timers:        make(map[string]*time.Timer),
		retryInterval: envDuration("WARDEN_RATE_LIMIT_RETRY_INTERVAL", 30*time.Minute),
		buffer:        envDuration("WARDEN_RATE_LIMIT_BUFFER", 1*time.Minute),
		enabled:       envBool("WARDEN_RATE_LIMIT_AUTO_RESUME", true),
	}
}

// OnTransition is wired as a callback on the poller's status-transition hook.
func (r *RateLimitScheduler) OnTransition(sess *store.Session, from, to store.Status) {
	if !r.enabled || to != store.StatusRateLimited {
		return
	}

	ctx := context.Background()

	// Parse restore time from pane excerpt (already captured by poller)
	restoreTime, ok := poller.ParseRestoreTime(sess.LastPaneExcerpt)

	var scheduleAt time.Time
	if ok && restoreTime.After(time.Now()) {
		// Success: use parsed time + buffer
		scheduleAt = restoreTime.Add(r.buffer)
	} else {
		// Fallback: retry in configured interval
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

	// If tmux session already exists, send a resume prompt to Claude
	if err == lifecycle.ErrAlreadyRunning {
		// The tmux session exists - Claude is paused showing rate limit error
		// Send a prompt to resume Claude and continue working
		resumePrompt := "continue"
		sendErr := r.life.Input(ctx, sess.TmuxSession, resumePrompt)
		if sendErr != nil {
			// Input failed - treat as non-rate-limit error
			_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusErrored)
			_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
				TS:     time.Now().UTC(),
				Type:   "rate-limit-resume-failed",
				Detail: "Input failed: " + sendErr.Error(),
			})

			r.mu.Lock()
			delete(r.timers, sess.ID)
			r.mu.Unlock()
			return
		}

		// Input succeeded - transition to spawning and let poller verify
		// If still rate-limited, poller will detect and call OnTransition again
		_, _ = r.store.UpdateStatusIf(ctx, sess.ID, store.StatusRateLimited, store.StatusSpawning)
		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-resumed",
			Detail: "sent resume prompt (tmux session exists)",
		})

		r.mu.Lock()
		delete(r.timers, sess.ID)
		r.mu.Unlock()
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

// envBool parses a boolean from an environment variable, falling back to def.
func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
