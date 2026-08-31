package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/spend"
)

// GetSpend implements GET /api/v1/spend: the cost rollup. Gated by the same
// `savings` switch the ledger uses — spend is the cost half of the same measured
// data — returning 403 when disabled so the surface points at the toggle.
func (s *Server) GetSpend(_ context.Context, _ oapi.GetSpendRequestObject) (oapi.GetSpendResponseObject, error) {
	if !s.savingsOn || s.spend == nil {
		return nil, errStatus(http.StatusForbidden, "spend tracking is disabled — enable it with `savings: true` in the config file")
	}
	return oapi.GetSpend200JSONResponse(s.spendReport()), nil
}

// spendReport prices and aggregates the per-session spend tracker into the
// cost-governance rollup served by GET /api/v1/spend, surfaced in `wd spend`, the
// `wd ls` cost column, and the web Metrics tab. Fail-open: a nil tracker or a read
// error yields an empty report rather than an error, so cost reporting never
// breaks the surface it feeds.
func (s *Server) spendReport() spend.Report {
	if s.spend == nil {
		return spend.Report{}
	}
	sessions, err := s.spend.Sessions()
	if err != nil {
		return spend.Report{}
	}
	return spend.BuildReportWithCost(sessions, time.Now(), func(session spend.SessionSpend) float64 {
		backendID := session.Backend
		if backendID == "" {
			backendID = agentbackend.DefaultID // legacy entries predate backend stamping
		}
		backend, err := agentbackend.Get(backendID)
		if err != nil {
			return 0
		}
		pricing, ok := backend.Pricing()
		if !ok {
			return 0
		}
		return pricing.Cost(session.Model, session.Input, session.Output)
	})
}

// budgetVerdict evaluates the cost gate against the current daily/weekly spend.
// It reuses pressure.Verdict so the spawn path returns the SAME 428
// ConfirmationResponse the memory-pressure gate uses — the client already decodes
// that shape; only the Reason differs. ok=false (the second return) means the gate
// is off or spend is under the caps, so the spawn proceeds untouched.
func (s *Server) budgetVerdict() (pressure.Verdict, bool) {
	s.pressMu.RLock()
	on, daily, weekly := s.budgetGate, s.budgetDailyUSD, s.budgetWeeklyUSD
	s.pressMu.RUnlock()
	if !on || (daily <= 0 && weekly <= 0) {
		return pressure.Verdict{}, false
	}
	rep := s.spendReport()
	bv := spend.EvaluateBudget(rep.DailyUSD, rep.WeeklyUSD, daily, weekly)
	if !bv.Over {
		return pressure.Verdict{}, false
	}
	return pressure.Verdict{Elevated: true, Reason: "budget — " + bv.Reason}, true
}
