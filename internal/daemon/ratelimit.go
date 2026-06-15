package daemon

import (
	"context"
	"os"
	"strconv"
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

// attemptResume is called when the timer fires (placeholder for now).
func (r *RateLimitScheduler) attemptResume(sessionID string) {
	// Will implement in next task
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
