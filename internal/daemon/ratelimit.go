package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

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

	// Parse restore time from pane (already captured by poller)
	// NOTE: parseRestoreTime is in internal/poller but not exported yet
	// For now, always fall back to retry interval
	restoreTime := time.Time{}
	ok := false

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

	if err == nil {
		// SUCCESS: transition back to spawning
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
		// Still rate limited - reschedule
		// TODO: Parse new restore time when parseRestoreTime is available
		scheduleAt := time.Now().Add(r.retryInterval)
		_ = r.store.SetRateLimit(ctx, sess.ID, scheduleAt, sess.RateLimitRetryCount+1)
		r.scheduleResume(sess.ID, scheduleAt)

		_ = r.store.AppendEvent(ctx, sess.ID, store.Event{
			TS:     time.Now().UTC(),
			Type:   "rate-limit-retry",
			Detail: "no time parsed, retrying in " + r.retryInterval.String(),
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

// envBool parses a boolean from an environment variable, falling back to def.
func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
