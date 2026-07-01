package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/store"
)

// pinnedGitTarget is pinnedWorkdir for strict handlers: it returns the
// authoritative working dir + resolved session, or an apiError carrying the HTTP
// status the hand-written handlers used to write directly.
func (s *Server) pinnedGitTarget(ctx context.Context, session, dir string) (string, *store.Session, error) {
	resolved, sess, status, msg := s.pinnedWorkdir(ctx, session, dir)
	if status != 0 {
		return "", nil, errStatus(status, msg)
	}
	return resolved, sess, nil
}

// GitCommit implements POST /api/v1/git/commit (rail-enforcing stage+commit).
func (s *Server) GitCommit(ctx context.Context, req oapi.GitCommitRequestObject) (oapi.GitCommitResponseObject, error) {
	var b oapi.GitCommitRequest
	if req.Body != nil {
		b = *req.Body
	}
	dir, sess, err := s.pinnedGitTarget(ctx, b.Session, b.Dir)
	if err != nil {
		return nil, err
	}
	meta := plugin.MetaFromSession(sess)
	meta.Workdir = dir
	s.plugins.Dispatch(ctx, plugin.EventPreCommit, meta, map[string]string{"message": b.Message})
	res, err := s.life.Commit(ctx, dir, b.Message)
	if err != nil {
		return nil, errStatus(http.StatusConflict, err.Error())
	}
	if res.Committed && sess != nil {
		s.recordGitEvent(sess.ID, "commit", res.SHA+" on "+res.Branch)
	}
	s.plugins.Dispatch(ctx, plugin.EventPostCommit, meta, map[string]string{
		"sha": res.SHA, "branch": res.Branch, "committed": strconv.FormatBool(res.Committed),
	})
	s.recordGitSavings(sess, res.RawBytes, res.RawSample, res)
	return oapi.GitCommit200JSONResponse(res), nil
}

// GitPush implements POST /api/v1/git/push (protected-branch refusal).
func (s *Server) GitPush(ctx context.Context, req oapi.GitPushRequestObject) (oapi.GitPushResponseObject, error) {
	var b oapi.GitDirRequest
	if req.Body != nil {
		b = *req.Body
	}
	dir, sess, err := s.pinnedGitTarget(ctx, b.Session, b.Dir)
	if err != nil {
		return nil, err
	}
	res, err := s.life.Push(ctx, dir)
	if err != nil {
		return nil, errStatus(http.StatusConflict, err.Error())
	}
	if sess != nil {
		s.recordGitEvent(sess.ID, "push", res.Branch+" -> "+res.Remote)
	}
	s.recordGitSavings(sess, res.RawBytes, res.RawSample, res)
	return oapi.GitPush200JSONResponse(res), nil
}

// GitSync implements POST /api/v1/git/sync (rebase onto origin/base).
func (s *Server) GitSync(ctx context.Context, req oapi.GitSyncRequestObject) (oapi.GitSyncResponseObject, error) {
	var b oapi.GitSyncRequest
	if req.Body != nil {
		b = *req.Body
	}
	dir, sess, err := s.pinnedGitTarget(ctx, b.Session, b.Dir)
	if err != nil {
		return nil, err
	}
	res, err := s.life.Sync(ctx, dir, b.Base)
	if err != nil {
		return nil, errStatus(http.StatusConflict, err.Error())
	}
	if sess != nil && res.Updated {
		s.recordGitEvent(sess.ID, "sync", "rebased onto "+res.Base)
	}
	s.recordGitSavings(sess, res.RawBytes, res.RawSample, res)
	return oapi.GitSync200JSONResponse(res), nil
}

// RunCheck implements POST /api/v1/check. No config / unknown name are
// operator-facing 422s, not server faults.
func (s *Server) RunCheck(ctx context.Context, req oapi.RunCheckRequestObject) (oapi.RunCheckResponseObject, error) {
	var b oapi.CheckRequest
	if req.Body != nil {
		b = *req.Body
	}
	dir, sess, err := s.pinnedGitTarget(ctx, b.Session, b.Dir)
	if err != nil {
		return nil, err
	}
	meta := plugin.MetaFromSession(sess)
	meta.Workdir = dir
	s.plugins.Dispatch(ctx, plugin.EventPreCheck, meta, map[string]string{"name": b.Name})
	res, err := s.life.Check(ctx, dir, b.Name)
	if err != nil {
		return nil, errStatus(http.StatusUnprocessableEntity, err.Error())
	}
	s.plugins.Dispatch(ctx, plugin.EventPostCheck, meta, map[string]string{
		"name": b.Name, "passed": strconv.FormatBool(res.Passed),
	})
	if sess != nil {
		detail := "passed"
		if !res.Passed {
			detail = "failed"
		}
		s.recordGitEvent(sess.ID, "check", detail)
	}
	s.recordCheckSavings(sess, res)
	return oapi.RunCheck200JSONResponse(res), nil
}

// CreatePR implements POST /api/v1/sessions/{id}/create-pr. Idempotent: an
// already-open PR comes back as a non-error result.
func (s *Server) CreatePR(ctx context.Context, req oapi.CreatePRRequestObject) (oapi.CreatePRResponseObject, error) {
	sess, err := s.resolveSession(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	base := ""
	if req.Body != nil {
		base = req.Body.Base
	}
	dir := sess.Workdir
	if dir == "" {
		return nil, errStatus(http.StatusConflict, "agent has no working directory — cannot open a PR")
	}
	if _, err := s.life.Push(ctx, dir); err != nil {
		return nil, errStatus(http.StatusConflict, "push failed: "+err.Error())
	}
	d := s.buildDigest(ctx, sess)
	res, err := s.life.CreatePR(ctx, dir, prTitle(sess, d), digest.Markdown(&d), base)
	if err != nil {
		return nil, errStatus(http.StatusConflict, err.Error())
	}
	s.recordGitEvent(sess.ID, "pr", res.URL)
	return oapi.CreatePR200JSONResponse(res), nil
}

// GetPressure implements GET /api/v1/pressure (gauge + UI gating).
func (s *Server) GetPressure(ctx context.Context, _ oapi.GetPressureRequestObject) (oapi.GetPressureResponseObject, error) {
	s.pressMu.RLock()
	lvl, gate, max := s.pressLevel, s.spawnGate, s.spawnGateMax
	s.pressMu.RUnlock()
	if lvl == 0 {
		lvl = pressure.Normal
	}
	v := pressure.Evaluate(lvl, s.liveAgentCount(ctx), max)
	return oapi.GetPressure200JSONResponse{
		Level:       int(lvl),
		LevelName:   lvl.String(),
		AgentCount:  v.AgentCount,
		MaxAgents:   max,
		Elevated:    v.Elevated,
		GateEnabled: gate,
	}, nil
}

// GetDigest implements GET /api/v1/sessions/{id}/digest. A reaped pipeline job's
// snapshot is served directly; otherwise the digest is rebuilt live.
func (s *Server) GetDigest(ctx context.Context, req oapi.GetDigestRequestObject) (oapi.GetDigestResponseObject, error) {
	sess, err := s.resolveSession(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	if s.exec != nil && sess.PipelineID != "" && sess.JobID != "" {
		if snap := s.exec.JobDigest(sess.PipelineID, sess.JobID); snap != nil {
			return oapi.GetDigest200JSONResponse(*snap), nil
		}
	}
	d := s.buildDigest(ctx, sess)
	return oapi.GetDigest200JSONResponse(d), nil
}

// GetSavings implements GET /api/v1/savings. Gated by the savings config; off ⇒
// 403 so the CLI can print a friendly "enable savings" message.
func (s *Server) GetSavings(_ context.Context, req oapi.GetSavingsRequestObject) (oapi.GetSavingsResponseObject, error) {
	if !s.savingsOn || s.savings == nil {
		return nil, errStatus(http.StatusForbidden, "savings ledger disabled (set savings: true in the config file)")
	}
	var since time.Time
	if q := req.Params.Since; q != "" {
		t, err := parseSinceParam(q)
		if err != nil {
			return nil, errStatus(http.StatusBadRequest, err.Error())
		}
		since = t
	}
	// Optional projections, off by default: ?bucket=day|hour attaches the
	// zero-filled saved-tokens trend (an unknown value yields no trend, not a
	// 400); ?samples=true attaches the retained provenance pairs.
	bucket := req.Params.Bucket
	if bucket != savings.GranularityHour && bucket != savings.GranularityDay {
		bucket = ""
	}
	samples := req.Params.Samples
	sum, err := s.savings.Report(since, bucket, samples)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "read savings ledger: "+err.Error())
	}
	if s.spend != nil {
		if total, terr := s.spend.Total(); terr != nil {
			slog.Warn("savings: read spend total failed", "err", terr)
		} else {
			sum.MeasuredSpend = total
		}
	}
	if cal, ok, cerr := s.savings.Calibration(); cerr != nil {
		slog.Warn("savings: read calibration failed", "err", cerr)
	} else if ok {
		sum.Calibrated = true
		sum.CalibratedBytesPerToken = cal.BytesPerToken
		sum.CalibrationSamples = cal.Samples
		savings.SetCalibration(cal.BytesPerToken)
	}
	return oapi.GetSavings200JSONResponse(sum), nil
}
