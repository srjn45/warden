package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/store"
)

const (
	// metricsRecordInterval is how often the recorder samples to disk. Cheap
	// (one ps + a few sysctls) and frequent enough to catch a fast memory ramp.
	metricsRecordInterval = 15 * time.Second
	// metricsRetentionDays bounds the on-disk JSONL history.
	metricsRetentionDays = 7
	// metricsHistoryDefaultWindow is the default look-back when no `since` is given.
	metricsHistoryDefaultWindow = 2 * time.Hour
	// metricsHistoryMaxSamples caps a single history response.
	metricsHistoryMaxSamples = 1000
)

// SetMetrics wires the collector + recorder after construction. recorder may be
// nil (recording disabled); collector may be nil (live snapshot returns empty).
// tokenWarn/tokenCrit are the context-token bands the history anomaly detector
// reuses (0 disables the matching check).
func (s *Server) SetMetrics(c *metrics.Collector, r *metrics.Recorder, enabled bool, tokenWarn, tokenCrit int) {
	s.mcollector = c
	s.mrecorder = r
	s.metricsOn = enabled
	s.mTokenWarn = tokenWarn
	s.mTokenCrit = tokenCrit
}

// pressureName returns the cached pressure level name for the collector.
func (s *Server) pressureName() string {
	s.pressMu.RLock()
	lvl := s.pressLevel
	s.pressMu.RUnlock()
	if lvl == 0 {
		return "normal"
	}
	return lvl.String()
}

// PressureName returns the cached pressure level name (for the metrics collector).
func (s *Server) PressureName() string { return s.pressureName() }

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.mcollector == nil {
		writeJSON(w, http.StatusOK, metrics.Sample{})
		return
	}
	sample, err := s.mcollector.Sample(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sample)
}

func (s *Server) handleMetricsHistory(w http.ResponseWriter, r *http.Request) {
	type historyResponse struct {
		Samples   []metrics.Sample       `json:"samples,omitempty"`
		Summaries []metrics.AgentSummary `json:"summaries,omitempty"`
	}
	if s.mrecorder == nil {
		writeJSON(w, http.StatusOK, historyResponse{Samples: []metrics.Sample{}})
		return
	}
	since := time.Now().Add(-metricsHistoryDefaultWindow)
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}
	limit := metricsHistoryMaxSamples
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < limit {
			limit = n
		}
	}
	samples, err := s.mrecorder.History(since, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if samples == nil {
		samples = []metrics.Sample{}
	}
	// summary=true rolls the raw samples into per-agent history + anomaly
	// warnings; agent=<id> narrows it to one agent. Raw samples are the default
	// so existing /metrics/history callers are unaffected.
	if r.URL.Query().Get("summary") == "true" {
		summaries := metrics.SummarizeAgents(samples, metrics.HistoryThresholds{
			ContextWarn: s.mTokenWarn,
			ContextCrit: s.mTokenCrit,
		})
		if want := r.URL.Query().Get("agent"); want != "" {
			filtered := summaries[:0]
			for _, sum := range summaries {
				if sum.ID == want {
					filtered = append(filtered, sum)
				}
			}
			summaries = filtered
		}
		if summaries == nil {
			summaries = []metrics.AgentSummary{}
		}
		writeJSON(w, http.StatusOK, historyResponse{Summaries: summaries})
		return
	}
	writeJSON(w, http.StatusOK, historyResponse{Samples: samples})
}

// runMetricsRecorder samples to disk on a ticker until ctx is done. Best-effort:
// each tick is panic-guarded so a sampling bug can't take down the daemon, and a
// daily prune trims old day-files. No-op when recording is disabled or unwired.
func (s *Server) runMetricsRecorder(ctx context.Context) {
	if !s.metricsOn || s.mrecorder == nil || s.mcollector == nil {
		return
	}
	_ = s.mrecorder.Prune(time.Now(), metricsRetentionDays)
	lastPruneDay := time.Now().Day()
	t := time.NewTicker(metricsRecordInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.recordOnce(ctx)
			if d := time.Now().Day(); d != lastPruneDay {
				_ = s.mrecorder.Prune(time.Now(), metricsRetentionDays)
				lastPruneDay = d
			}
		}
	}
}

// recordOnce samples and appends, recovering from any panic in collection.
func (s *Server) recordOnce(ctx context.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("daemon: metrics recorder recovered panic", "panic", rec)
		}
	}()
	sample, err := s.mcollector.Sample(ctx)
	if err != nil {
		slog.Warn("daemon: metrics sample failed", "err", err)
		return
	}
	if err := s.mrecorder.Record(sample); err != nil {
		slog.Warn("daemon: metrics record failed", "err", err)
	}
}

// storeAgentLister adapts the session store to metrics.Lister, returning only
// live (non-terminal) agents — the ones with a tmux pane worth attributing.
type storeAgentLister struct{ st store.Store }

func (l storeAgentLister) LiveAgents(ctx context.Context) ([]metrics.Agent, error) {
	sessions, err := l.st.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []metrics.Agent
	for _, sess := range sessions {
		if !liveStatus(sess.Status) {
			continue
		}
		out = append(out, metrics.Agent{
			ID:            sess.ID,
			TmuxSession:   sess.TmuxSession,
			Status:        string(sess.Status),
			ContextTokens: sess.ContextTokens,
			CreatedAt:     sess.CreatedAt,
			Workdir:       sess.Workdir,
		})
	}
	return out, nil
}

// NewAgentLister adapts a store into a metrics.Lister of live agents.
func NewAgentLister(st store.Store) metrics.Lister { return storeAgentLister{st: st} }
