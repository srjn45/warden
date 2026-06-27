package spend

import (
	"sort"
	"time"
)

// Bucket is one rollup row — an agent, a repo, or a day — with the billed tokens
// behind it and the priced dollar figure. It crosses the wire to the CLI/MCP/web.
type Bucket struct {
	Key    string  `json:"key"`
	Input  int     `json:"input_tokens"`
	Output int     `json:"output_tokens"`
	USD    float64 `json:"usd"`
}

// Report is the cost-governance rollup the `wd spend` command, the `spend` MCP
// tool, the `wd ls` cost column, and the web Metrics tab all read. It prices each
// session by its own model, then aggregates three ways (per-agent, per-repo,
// per-day) and surfaces the daily/weekly totals the budget gate compares to its
// caps. Empty everywhere when no spend has been observed yet.
type Report struct {
	TotalUSD     float64  `json:"total_usd"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	ByAgent      []Bucket `json:"by_agent"`
	ByRepo       []Bucket `json:"by_repo"`
	ByDay        []Bucket `json:"by_day"`
	// DailyUSD / WeeklyUSD are the windows the budget gate enforces: spend whose
	// first-seen day is today, and within the trailing 7 days, respectively.
	DailyUSD  float64 `json:"daily_usd"`
	WeeklyUSD float64 `json:"weekly_usd"`
}

// AgentUSD returns a map of agent id → priced spend, the join the `wd ls` cost
// column and the web Metrics tab use to put a dollar figure next to each agent.
func (r Report) AgentUSD() map[string]float64 {
	out := make(map[string]float64, len(r.ByAgent))
	for _, b := range r.ByAgent {
		out[b.Key] = b.USD
	}
	return out
}

// BuildReport prices and aggregates the per-session spend into the three rollups
// plus the daily/weekly windows, all relative to now. It is pure (no I/O) so the
// pricing + bucketing math is unit-testable; the store supplies the sessions. A
// session with an empty repo is bucketed under "—" so unattributed spend still
// shows. By-agent and by-repo are sorted biggest-$-first (the rows a cost report
// leads with); by-day is chronological.
func BuildReport(sessions []SessionSpend, now time.Time) Report {
	var r Report
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -6).Format("2006-01-02") // trailing 7 days incl. today

	agent := map[string]*Bucket{}
	repo := map[string]*Bucket{}
	day := map[string]*Bucket{}

	add := func(m map[string]*Bucket, key string, in, out int, usd float64) {
		b := m[key]
		if b == nil {
			b = &Bucket{Key: key}
			m[key] = b
		}
		b.Input += in
		b.Output += out
		b.USD += usd
	}

	for _, s := range sessions {
		usd := Cost(s.Model, s.Input, s.Output)
		r.TotalUSD += usd
		r.InputTokens += s.Input
		r.OutputTokens += s.Output

		repoKey := s.Repo
		if repoKey == "" {
			repoKey = "—"
		}
		add(agent, s.Session, s.Input, s.Output, usd)
		add(repo, repoKey, s.Input, s.Output, usd)
		if s.Day != "" {
			add(day, s.Day, s.Input, s.Output, usd)
		}
		if s.Day == today {
			r.DailyUSD += usd
		}
		if s.Day >= weekAgo { // lexicographic compare is correct for YYYY-MM-DD
			r.WeeklyUSD += usd
		}
	}

	r.ByAgent = sortedByUSD(agent)
	r.ByRepo = sortedByUSD(repo)
	r.ByDay = sortedByKey(day)
	return r
}

// sortedByUSD flattens a bucket map biggest-dollar-first (ties broken by key for
// determinism) — the order a cost rollup reads best in.
func sortedByUSD(m map[string]*Bucket) []Bucket {
	out := make([]Bucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].USD != out[j].USD {
			return out[i].USD > out[j].USD
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// sortedByKey flattens a bucket map in ascending key order — chronological for
// the YYYY-MM-DD day buckets.
func sortedByKey(m map[string]*Bucket) []Bucket {
	out := make([]Bucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
