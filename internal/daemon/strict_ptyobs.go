package daemon

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyobs"
)

// effectivePosture is the posture the engine decides under: the session's own,
// or allowlisted (the safe default - never an implicit upgrade) when unset.
func effectivePosture(p ptyevent.Posture) ptyevent.Posture {
	if p.Valid() {
		return p
	}
	return ptyevent.PostureAllowlisted
}

// lastDecision returns the newest trace record for the session, if any.
func (s *Server) lastDecision(tmuxName string) (ptyobs.TraceRecord, bool) {
	if s.ptyTrace == nil || tmuxName == "" {
		return ptyobs.TraceRecord{}, false
	}
	recs := s.ptyTrace.Query(tmuxName, ptyobs.Query{Limit: 1})
	if len(recs) == 0 {
		return ptyobs.TraceRecord{}, false
	}
	return recs[0], true
}

// runtimeView assembles the operator view of one session.
func (s *Server) runtimeView(id, key string, posture ptyevent.Posture) ptyobs.RuntimeView {
	posture = effectivePosture(posture)
	v := ptyobs.RuntimeView{
		SessionID:     id,
		Mode:          "off",
		RequestedMode: "off",
		ActualKind:    "unknown",
		ProcessOwner:  "none",
		Posture:       string(posture),
		Danger:        posture == ptyevent.PostureDangerouslyExecuteAll,
	}
	if v.Danger {
		v.DangerBanner = ptyobs.DangerBanner
	}
	// The decision trace outlives the stream: report the latest decision even
	// when the session is no longer supervised.
	if rec, ok := s.lastDecision(key); ok {
		v.Route, v.LastOutcome = string(rec.Route), string(rec.Outcome)
	}
	if s.pty == nil {
		return v
	}
	v.Mode = string(s.pty.Mode())
	v.RequestedMode = string(s.pty.Mode())

	info := s.pty.Resolve(context.Background(), id)
	v.ActualKind = info.Kind
	v.ProcessOwner = info.ProcessOwner
	if info.RequestedMode != "" {
		v.RequestedMode = info.RequestedMode
	}

	t, ok := s.pty.Lookup(key)
	if !ok {
		return v
	}
	v.Supervised = true
	v.Authoritative = s.pty.IsAuthoritative(key)
	v.Presentation = string(t.Presentation)
	// Input ownership is only arbitrated for a warden-hosted PTY; a mirrored tmux
	// pane has no lease (humans type into tmux directly).
	if t.Direct != nil {
		l := t.Direct.Lease()
		v.InputOwner = string(l.Holder)
		if v.InputOwner == "" {
			v.InputOwner = "free"
		}
		v.HumanLineOpen = l.HumanLineOpen
	} else {
		v.InputOwner = "tmux"
	}
	if s.classifier != nil {
		if st, ok := s.classifier.Status(key); ok {
			v.State = string(st.Snapshot.State)
			v.Reason = string(st.Snapshot.Reason)
			v.Since = st.Snapshot.Since
			v.QuietSeconds = st.Quiet.Seconds()
			for _, tr := range st.Trail {
				e := ptyobs.TrailEntry{At: tr.At, From: string(tr.From), To: string(tr.To), Reason: string(tr.Reason), Seq: tr.Seq}
				for _, ev := range tr.Evidence {
					e.Evidence = append(e.Evidence, string(ev))
				}
				v.Trail = append(v.Trail, e)
			}
			if n := len(v.Trail); n > 0 {
				v.Evidence = v.Trail[n-1].Evidence
			}
			if st.Prompt != nil {
				sum := ptyobs.Summarize(st.Prompt, 0)
				v.Prompt = &sum
			}
		}
	}
	return v
}

// GetSessionRuntime implements GET /api/v1/sessions/{id}/runtime.
func (s *Server) GetSessionRuntime(ctx context.Context, req oapi.GetSessionRuntimeRequestObject) (oapi.GetSessionRuntimeResponseObject, error) {
	sess, err := s.resolveSessionDTO(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return oapi.GetSessionRuntime200JSONResponse(s.runtimeView(sess.ID, sess.RuntimeKey(), sess.PermissionPosture)), nil
}

// ListSessionDecisions implements GET /api/v1/sessions/{id}/decisions.
func (s *Server) ListSessionDecisions(ctx context.Context, req oapi.ListSessionDecisionsRequestObject) (oapi.ListSessionDecisionsResponseObject, error) {
	sess, err := s.resolveSessionDTO(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if o := ptyobs.Outcome(req.Params.Outcome); o != "" && !o.Valid() {
		return nil, errStatus(http.StatusBadRequest, "unknown outcome")
	}
	recs := []ptyobs.TraceRecord{}
	if s.ptyTrace != nil {
		recs = s.ptyTrace.Query(sess.RuntimeKey(), ptyobs.Query{
			Limit:   req.Params.Limit,
			Since:   req.Params.Since,
			Outcome: ptyobs.Outcome(req.Params.Outcome),
		})
		if recs == nil {
			recs = []ptyobs.TraceRecord{}
		}
	}
	out := make([]oapi.PTYDecisionRecord, len(recs))
	for i, r := range recs {
		out[i] = r
	}
	return oapi.ListSessionDecisions200JSONResponse{SessionId: sess.ID, Records: out}, nil
}

// GetPTYMetrics implements GET /api/v1/pty/metrics.
func (s *Server) GetPTYMetrics(_ context.Context, _ oapi.GetPTYMetricsRequestObject) (oapi.GetPTYMetricsResponseObject, error) {
	snap := s.ptyMetrics.Snapshot()
	if snap.Families == nil {
		snap.Families = []ptyobs.Family{}
	}
	if snap.Now.IsZero() {
		snap.Now = time.Now().UTC()
	}
	return oapi.GetPTYMetrics200JSONResponse(snap), nil
}

// GetPTYMetricsPrometheus implements GET /api/v1/pty/metrics.prom.
func (s *Server) GetPTYMetricsPrometheus(_ context.Context, _ oapi.GetPTYMetricsPrometheusRequestObject) (oapi.GetPTYMetricsPrometheusResponseObject, error) {
	var buf bytes.Buffer
	if err := s.ptyMetrics.Snapshot().WritePrometheus(&buf); err != nil {
		return nil, err
	}
	return oapi.GetPTYMetricsPrometheus200TextResponse(buf.String()), nil
}
