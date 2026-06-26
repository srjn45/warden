package daemon

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/store"
)

// errBadSince is the operator-facing message for an unparseable ?since value.
var errBadSince = errors.New("invalid since value — use a duration (168h) or an RFC3339 timestamp")

func (s *Server) registerSavingsRoutes(r chi.Router) {
	r.Get("/savings", s.handleSavings)
}

// parseSinceParam resolves the ?since query value to an absolute time. The CLI
// already expands human windows ("7d"/"2w") to an RFC3339 timestamp before
// sending, so that is the primary form; a bare Go duration ("168h") is also
// accepted for direct API callers. An unparseable value is an error the handler
// surfaces as a 400.
func parseSinceParam(q string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, q); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(q)
	if err != nil {
		return time.Time{}, errBadSince
	}
	return time.Now().Add(-d), nil
}

// handleSavings returns the aggregated savings summary over an optional ?since
// window (a duration like "7d"/"24h" or an RFC3339 timestamp; absent ⇒ all time).
// Gated by the `savings` config setting: when off (or the store is unconfigured)
// it returns 403 so the CLI can print a friendly "enable savings" message rather
// than an empty report that looks like zero savings.
func (s *Server) handleSavings(w http.ResponseWriter, r *http.Request) {
	if !s.savingsOn || s.savings == nil {
		writeErr(w, http.StatusForbidden, "savings ledger disabled (set savings: true in the config file)")
		return
	}
	var since time.Time
	if q := r.URL.Query().Get("since"); q != "" {
		t, err := parseSinceParam(q)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		since = t
	}
	sum, err := s.savings.Summary(since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read savings ledger: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

// recordCheckSavings records one savings event for a completed `wd check` run:
// the raw combined output the checks would have spilled into the transcript
// (RawBytes, captured on pass and fail) minus what `wd check` actually returned
// (the truncated/condensed failure Output). Fail-open by contract — a nil store,
// the feature being off, or a write error must never fail the check that ran, so
// every error path only logs. sess may be nil (a human-run check); the agent id
// is then empty, which the ledger records as an unattributed saving.
func (s *Server) recordCheckSavings(sess *store.Session, res lifecycle.CheckResult) {
	if !s.savingsOn || s.savings == nil {
		return
	}
	var rawBytes, keptBytes int
	for _, c := range res.Checks {
		rawBytes += c.RawBytes
		keptBytes += len(c.Output)
	}
	if rawBytes == 0 {
		return // nothing ran, or every check produced no output — no saving to claim
	}
	var agent string
	if sess != nil {
		agent = sess.ID
	}
	ev := savings.NewEvent(savings.FeatureCheck, agent,
		savings.EstimateTokensLen(rawBytes), savings.EstimateTokensLen(keptBytes))
	if err := s.savings.Record(ev); err != nil {
		slog.Warn("savings: failed to record check event", "err", err)
	}
}

// RecordLifecycleSaving is the lifecycle.Lifecycle.SavingsHook seam: it records a
// saving emitted from deep inside a lifecycle feature (an LLM offload — a
// Classify/Summarize call served by the local model instead of warden's own
// Claude) that the request handler never observes. Gate-aware and fail-open like
// the other record helpers; a zero raw figure is a no-op.
func (s *Server) RecordLifecycleSaving(feature, agent string, rawTokens, keptTokens int) {
	if !s.savingsOn || s.savings == nil || rawTokens == 0 {
		return
	}
	if err := s.savings.Record(savings.NewEvent(feature, agent, rawTokens, keptTokens)); err != nil {
		slog.Warn("savings: failed to record lifecycle event", "feature", feature, "err", err)
	}
}

// recordGitSavings records one FeatureCommit event for a completed wd
// commit/push/sync: rawBytes is the git output warden ran on the agent's behalf
// (CommitResult.RawBytes et al.), and the kept side is the compact struct the
// agent actually receives — measured by marshaling the same value writeJSON
// sends, so the json:"-" RawBytes field is correctly excluded. Fail-open like
// recordCheckSavings: a nil store, the feature being off, or a write error only
// logs. sess may be nil (a human-run git action), recorded unattributed.
func (s *Server) recordGitSavings(sess *store.Session, rawBytes int, result any) {
	if !s.savingsOn || s.savings == nil {
		return
	}
	if rawBytes == 0 {
		return // nothing the agent would have read (e.g. a clean-tree commit)
	}
	keptBytes := 0
	if b, err := json.Marshal(result); err == nil {
		keptBytes = len(b)
	}
	var agent string
	if sess != nil {
		agent = sess.ID
	}
	ev := savings.NewEvent(savings.FeatureCommit, agent,
		savings.EstimateTokensLen(rawBytes), savings.EstimateTokensLen(keptBytes))
	if err := s.savings.Record(ev); err != nil {
		slog.Warn("savings: failed to record git event", "err", err)
	}
}
