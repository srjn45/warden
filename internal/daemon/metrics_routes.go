package daemon

import (
	"context"
	"log"
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
func (s *Server) SetMetrics(c *metrics.Collector, r *metrics.Recorder, enabled bool) {
	s.mcollector = c
	s.mrecorder = r
	s.metricsOn = enabled
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
		Samples []metrics.Sample `json:"samples"`
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
			log.Printf("daemon: metrics recorder recovered panic: %v", rec)
		}
	}()
	sample, err := s.mcollector.Sample(ctx)
	if err != nil {
		log.Printf("daemon: metrics sample failed: %v", err)
		return
	}
	if err := s.mrecorder.Record(sample); err != nil {
		log.Printf("daemon: metrics record failed: %v", err)
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
		out = append(out, metrics.Agent{ID: sess.ID, TmuxSession: sess.TmuxSession, Status: string(sess.Status)})
	}
	return out, nil
}

// NewAgentLister adapts a store into a metrics.Lister of live agents.
func NewAgentLister(st store.Store) metrics.Lister { return storeAgentLister{st: st} }
