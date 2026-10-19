package daemon

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyobs"
)

// SetTraceLimits applies the runtime.trace_max_records / trace_max_age_days
// retention to the decision trace. It is safe before the trace is opened (the
// values are remembered and used when it is) and on every config reload.
func (s *Server) SetTraceLimits(maxRecords, ageDays int) {
	s.traceMax, s.traceAgeDays = maxRecords, ageDays
	if s.ptyTrace != nil {
		s.ptyTrace.SetLimits(maxRecords, traceMaxAge(ageDays))
	}
}

func traceMaxAge(days int) time.Duration {
	if days <= 0 {
		return -1 // retention by age disabled
	}
	return time.Duration(days) * 24 * time.Hour
}

// openDecisionTrace opens the per-session decision trace under the PTY data dir
// and hooks its failures into the metrics. A failure leaves the trace nil: the
// engine still decides and answers, it just records nothing (and says so).
func (s *Server) openDecisionTrace() *ptyobs.Trace {
	if s.pty == nil {
		return nil
	}
	tr, err := ptyobs.OpenTrace(filepath.Join(s.pty.Dir(), "trace"), ptyobs.TraceConfig{
		MaxRecords: s.traceMax,
		MaxAge:     traceMaxAge(s.traceAgeDays),
	})
	if err != nil {
		slog.Warn("ptyobs: decision trace unavailable; decisions will not be recorded", "err", err)
		return nil
	}
	tr.OnError(func() { s.ptyMetrics.Inc(ptyobs.MTraceErrors) })
	s.ptyTrace = tr
	return tr
}

// countPTYEvent feeds the stream-level metrics from every journaled event:
// volume by kind, redactions by secret kind (never the secret), journaled gaps
// as backpressure, and runtime re-adoption as a reconnect.
func (s *Server) countPTYEvent(ev ptyevent.Event) {
	m := s.ptyMetrics
	if m == nil {
		return
	}
	m.Inc(ptyobs.MEvents, string(ev.Kind))
	for _, r := range ev.Redactions {
		m.Inc(ptyobs.MRedactions, string(r.Kind))
	}
	switch ev.Kind {
	case ptyevent.KindMarker:
		if ev.Marker != nil && ev.Marker.Type == ptyevent.MarkerGap {
			m.Inc(ptyobs.MBackpressure, "gap")
		}
	case ptyevent.KindLifecycle:
		if ev.Lifecycle != nil && ev.Lifecycle.Phase == ptyevent.PhaseRecovered {
			m.Inc(ptyobs.MReconnects, "resumed")
		}
	}
}

// decorateApproval adds the operator view to an approval: effective posture and
// danger banner (always), the route of the latest decision, and - when the
// runtime has the prompt normalised - the permission summary.
func (s *Server) decorateApproval(v *approval.View, sess *agentstore.Agent) {
	posture := effectivePosture(sess.PermissionPosture)
	v.Posture = string(posture)
	if posture == ptyevent.PostureDangerouslyExecuteAll {
		v.Danger, v.DangerBanner = true, ptyobs.DangerBanner
	}
	if rec, ok := s.lastDecision(sess.TmuxSession); ok {
		v.Route = string(rec.Route)
	}
	if s.classifier == nil {
		return
	}
	if st, ok := s.classifier.Status(sess.TmuxSession); ok && st.Prompt != nil {
		sum := ptyobs.Summarize(st.Prompt, 0)
		v.Summary = &sum
	}
}
