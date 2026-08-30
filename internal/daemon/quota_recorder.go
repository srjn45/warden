package daemon

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/srjn45/warden/internal/spend"
	"github.com/srjn45/warden/internal/store"
)

// quotaRecordInterval is how often the quota recorder samples live agents' token
// usage into the backend registry's rolling quota windows. A modest cadence
// (cheaper than the 5h/daily windows it feeds and gentle on transcript reads),
// following the metrics recorder's plain-const precedent.
const quotaRecordInterval = 60 * time.Second

// runQuotaRecorder periodically samples each live agent's cumulative billed token
// usage (input+output, read from its transcript) and records the per-agent delta
// into the backend registry's rolling quota window
// (backendstore.RecordQuotaUsage). This is the proactive usage feed that keeps
// GetHeadroom current, so the soft auto-handover trigger (DecideHotSwap's quota
// arm, plus the guardian's cost-tier selection) can retire an agent BEFORE it
// hits a hard provider limit rather than only reacting once a limit is tripped.
//
// It reads the same transcript JSONL warden already parses for spend/context —
// the accurate, zero-cost source — rather than spawning a headless CLI `/usage`
// query per backend: `/usage` is an interactive slash command (not a `-p`
// prompt), most wrapped CLIs expose no such command, and measuring quota by
// spending quota is self-defeating. Best-effort and panic-guarded like the
// metrics recorder; a no-op when the registry is unconfigured.
func (s *Server) runQuotaRecorder(ctx context.Context) {
	if s.backends == nil {
		return
	}
	// sessionID -> last cumulative input+output tokens observed this daemon run.
	// Local to this goroutine, so it needs no lock. Reset across restarts: the
	// first observation of a session seeds the baseline and records nothing, so a
	// restart never back-fills a session's whole history as one spurious spike.
	lastTotal := make(map[string]int)
	t := time.NewTicker(quotaRecordInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.recordQuotaOnce(ctx, lastTotal)
		}
	}
}

// recordQuotaOnce samples every live agent once, recording the positive token
// delta since its last sample into its backend's quota window. Panic-guarded so a
// parsing bug can't take down the daemon. lastTotal is mutated in place: fresh
// baselines are seeded and dead sessions are pruned so the map stays bounded.
func (s *Server) recordQuotaOnce(ctx context.Context, lastTotal map[string]int) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("daemon: quota recorder recovered panic", "panic", rec)
		}
	}()
	sessions, err := s.store.List(ctx)
	if err != nil {
		slog.Warn("daemon: quota recorder list failed", "err", err)
		return
	}
	live := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		// Only live AI agents carry attributable token usage; terminals are plain
		// shells and done/errored/orphaned sessions no longer accrue.
		if !liveStatus(sess.Status) || sess.IsTerminal() {
			continue
		}
		if sess.Backend == "" {
			continue // unknown backend: nothing to attribute the usage to.
		}
		live[sess.ID] = true

		total, ok := s.transcriptTotalTokens(sess)
		if !ok {
			continue // no transcript / no usage yet — leave the baseline untouched.
		}
		prev, seen := lastTotal[sess.ID]
		lastTotal[sess.ID] = total
		if !seen {
			continue // first observation this run: seed the baseline, don't back-fill.
		}
		delta := total - prev
		if delta <= 0 {
			continue // no new billed tokens (or a transcript rotation under-count).
		}
		// NOTE: the delta is in tokens. Token-denominated windows (claude/codex 5h,
		// antigravity daily) consume it directly; a request-denominated backend
		// (cursor's monthly request budget) would over-count — aligning per-backend
		// quota units is a modeling follow-up tracked with the backend registry.
		if err := s.backends.RecordQuotaUsage(sess.Backend, float64(delta), sess.Model, time.Now()); err != nil {
			slog.Warn("daemon: quota record failed", "agent", sess.ID, "backend", sess.Backend, "err", err)
		}
	}
	// Drop baselines for sessions that are gone so the map can't grow unbounded.
	for id := range lastTotal {
		if !live[id] {
			delete(lastTotal, id)
		}
	}
}

// transcriptTotalTokens reads an agent's cumulative billed token usage
// (input+output summed over every assistant turn) from its transcript JSONL,
// mirroring the poller's TranscriptUsage read. ok=false when there is no
// transcript path, the file can't be opened, or it carries no usage yet.
func (s *Server) transcriptTotalTokens(sess *store.Session) (int, bool) {
	if s.life == nil {
		return 0, false
	}
	path := s.life.TranscriptPath(sess)
	if path == "" {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	u, ok := spend.GetParser(sess.Backend).ParseUsage(f)
	if !ok {
		return 0, false
	}
	return u.InputTokens + u.OutputTokens, true
}
