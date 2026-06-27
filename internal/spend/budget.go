package spend

import "fmt"

// BudgetVerdict is the cost gate's decision, mirroring pressure.Verdict: a soft
// warning the spawn path returns as a 428 so the caller can re-submit with force.
// Over is true when current spend has reached a configured cap; Reason names which
// cap (daily and/or weekly) and the figures, so a screenshot of the warning is
// self-describing.
type BudgetVerdict struct {
	Over      bool    `json:"over"`
	DailyUSD  float64 `json:"daily_usd"`
	WeeklyUSD float64 `json:"weekly_usd"`
	CapDaily  float64 `json:"cap_daily"`
	CapWeekly float64 `json:"cap_weekly"`
	Reason    string  `json:"reason"`
}

// EvaluateBudget decides whether a spawn should warn on cost: the gate trips when
// already-observed daily spend has reached capDaily, OR weekly spend has reached
// capWeekly. A cap <= 0 disables that axis (so an operator can set just a daily or
// just a weekly limit). The check is on CURRENT spend at/over the cap rather than
// a projection — a new agent only adds spend, so spawning while already at the cap
// is exactly what the gate is meant to stop. Pure and unit-testable.
func EvaluateBudget(dailyUSD, weeklyUSD, capDaily, capWeekly float64) BudgetVerdict {
	v := BudgetVerdict{DailyUSD: dailyUSD, WeeklyUSD: weeklyUSD, CapDaily: capDaily, CapWeekly: capWeekly}
	byDaily := capDaily > 0 && dailyUSD >= capDaily
	byWeekly := capWeekly > 0 && weeklyUSD >= capWeekly
	v.Over = byDaily || byWeekly
	switch {
	case byDaily && byWeekly:
		v.Reason = fmt.Sprintf("daily spend $%.2f ≥ $%.2f cap · weekly $%.2f ≥ $%.2f cap", dailyUSD, capDaily, weeklyUSD, capWeekly)
	case byDaily:
		v.Reason = fmt.Sprintf("daily spend $%.2f ≥ $%.2f cap", dailyUSD, capDaily)
	case byWeekly:
		v.Reason = fmt.Sprintf("weekly spend $%.2f ≥ $%.2f cap", weeklyUSD, capWeekly)
	}
	return v
}
