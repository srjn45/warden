// Package insights mines warden's own history — completed and active agent
// sessions plus recorded resource metrics — into actionable, operator-facing
// suggestions (#48). The core is a deterministic statistics engine that needs NO
// model: Analyze turns an Input of session records + metric summaries into a
// structured Report (duration outliers, frequently co-edited files, error rates,
// busy periods, and parallelization opportunities). An optional narration layer
// (narrator.go) turns that Report into a short natural-language paragraph via a
// local llm.Completer, degrading to a deterministic template whenever the model
// is off, unreachable, errors, or returns nothing — the same fallback contract
// as internal/digest and the internal/llm Classify/Summarize seams. The engine
// is pure: no filesystem, no subprocess, no network, and it never panics.
package insights

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/store"
)

// SessionRecord is the deterministic, pre-fetched view of one agent session the
// engine reasons over. Files is best-effort — it may be empty when a session's
// touched-file set could not be recovered — and the engine degrades gracefully:
// duration / error-rate / busy-period analysis never needs Files; only the
// co-edit and parallelization heuristics do.
type SessionRecord struct {
	ID     string
	Name   string
	Type   string
	Status string
	Repo   string
	Start  time.Time
	End    time.Time // zero ⇒ still active (open-ended)
	Files  []string
}

// active reports whether the session has not finished (no End recorded).
func (r SessionRecord) active() bool { return r.End.IsZero() }

// duration returns the session's wall-clock span. An active session is measured
// to now; a record with a zero Start, or an End before Start, has no duration.
func (r SessionRecord) duration(now time.Time) time.Duration {
	if r.Start.IsZero() {
		return 0
	}
	end := r.End
	if end.IsZero() {
		end = now
	}
	if end.Before(r.Start) {
		return 0
	}
	return end.Sub(r.Start)
}

// label is the most human handle for a session — its name, else its id.
func (r SessionRecord) label() string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return r.ID
}

// FromSession projects a stored session into a SessionRecord, attaching the
// (best-effort) touched-file set. CreatedAt is the start; a session in a terminal
// status takes UpdatedAt as its end, while a live one is left open-ended (End
// zero). The type is normalized through store.NormalizeType so unknown labels
// collapse to "other".
func FromSession(s *store.Session, files []string) SessionRecord {
	rec := SessionRecord{
		ID:     s.ID,
		Name:   s.Name,
		Type:   string(store.NormalizeType(string(s.Type))),
		Status: string(s.Status),
		Repo:   s.Repo,
		Start:  s.CreatedAt,
		Files:  dedupeSorted(files),
	}
	if isTerminalStatus(s.Status) {
		rec.End = s.UpdatedAt
	}
	return rec
}

func isTerminalStatus(st store.Status) bool {
	switch st {
	case store.StatusDone, store.StatusErrored, store.StatusOrphaned:
		return true
	}
	return false
}

func isErrorStatus(s string) bool {
	switch store.Status(s) {
	case store.StatusErrored, store.StatusOrphaned:
		return true
	}
	return false
}

// Input is everything Analyze needs: the session records (active and archived),
// the per-agent metric summaries (metrics.SummarizeAgents output), and the clock.
type Input struct {
	Sessions []SessionRecord
	Agents   []metrics.AgentSummary
	Now      time.Time
}

// Report is the structured, deterministic output of Analyze. Every slice is
// stably sorted so the same Input always yields byte-identical output (the
// narrator and CLI/MCP renderers depend on this).
type Report struct {
	GeneratedAt    time.Time            `json:"generated_at"`
	Sessions       int                  `json:"sessions"`        // records analyzed
	ActiveSessions int                  `json:"active_sessions"` // open-ended subset
	Durations      []TypeDuration       `json:"durations"`
	CoEdits        []CoEditPair         `json:"co_edits"`
	ErrorRates     []TypeErrorRate      `json:"error_rates"`
	BusiestPeriods []HourBucket         `json:"busiest_periods"`
	Parallelizable []ParallelSuggestion `json:"parallelizable"`
	Anomalies      []AgentAnomaly       `json:"anomalies"`
}

// TypeDuration is the duration distribution for one task type, with the session
// ids that ran far longer than the type's median flagged as outliers.
type TypeDuration struct {
	Type      string   `json:"type"`
	Count     int      `json:"count"`
	MedianSec int64    `json:"median_sec"`
	P90Sec    int64    `json:"p90_sec"`
	MaxSec    int64    `json:"max_sec"`
	Outliers  []string `json:"outliers,omitempty"`
}

// CoEditPair is two files touched together across Count sessions — a hint that
// they form a coupled area worth keeping in one agent's scope.
type CoEditPair struct {
	A     string `json:"a"`
	B     string `json:"b"`
	Count int    `json:"count"`
}

// TypeErrorRate is the error/orphan rate for one task type.
type TypeErrorRate struct {
	Type    string  `json:"type"`
	Total   int     `json:"total"`
	Errored int     `json:"errored"`
	Rate    float64 `json:"rate"`
}

// HourBucket counts sessions started in a given hour-of-day (UTC).
type HourBucket struct {
	Hour  int `json:"hour"`
	Count int `json:"count"`
}

// AgentAnomaly carries forward a live agent's metric warnings (climbing memory,
// pinned CPU, context approaching the limit) from metrics.SummarizeAgents.
type AgentAnomaly struct {
	Agent  string   `json:"agent"`
	Status string   `json:"status"`
	Notes  []string `json:"notes"`
}

const (
	// durationOutlierFactor flags a finished session as a duration outlier when it
	// ran longer than this multiple of its type's median.
	durationOutlierFactor = 2.0
	// coEditMinSessions is the floor for surfacing a co-edited file pair.
	coEditMinSessions = 2
	maxCoEditPairs    = 10
	maxBusyBuckets    = 5
)

// Analyze runs the full deterministic statistics core over an Input. It is the
// single entry point; every sub-analysis is a pure helper exercised directly in
// tests. A zero Now defaults to time.Now (so active-session durations resolve).
func Analyze(in Input) Report {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	r := Report{
		GeneratedAt: now,
		Sessions:    len(in.Sessions),
	}
	for _, s := range in.Sessions {
		if s.active() {
			r.ActiveSessions++
		}
	}
	r.Durations = durationStats(in.Sessions, now)
	r.CoEdits = coEdits(in.Sessions)
	r.ErrorRates = errorRates(in.Sessions)
	r.BusiestPeriods = busiestPeriods(in.Sessions)
	r.Parallelizable = SuggestParallelization(in.Sessions, now)
	r.Anomalies = anomalies(in.Agents)
	return r
}

// durationStats groups finished sessions by type and summarizes each type's
// wall-clock distribution (median / p90 / max) plus the session ids that ran
// past durationOutlierFactor × median. Active sessions are excluded so a single
// long-running agent can't skew the historical baseline.
func durationStats(sessions []SessionRecord, now time.Time) []TypeDuration {
	byType := map[string][]SessionRecord{}
	for _, s := range sessions {
		if s.active() || s.duration(now) <= 0 {
			continue
		}
		byType[s.Type] = append(byType[s.Type], s)
	}
	var out []TypeDuration
	for typ, recs := range byType {
		secs := make([]int64, 0, len(recs))
		for _, r := range recs {
			secs = append(secs, int64(r.duration(now).Seconds()))
		}
		sort.Slice(secs, func(i, j int) bool { return secs[i] < secs[j] })
		med := percentile(secs, 50)
		td := TypeDuration{
			Type:      typ,
			Count:     len(recs),
			MedianSec: med,
			P90Sec:    percentile(secs, 90),
			MaxSec:    secs[len(secs)-1],
		}
		if med > 0 {
			thresh := int64(float64(med) * durationOutlierFactor)
			outliers := make([]SessionRecord, 0)
			for _, r := range recs {
				if int64(r.duration(now).Seconds()) > thresh {
					outliers = append(outliers, r)
				}
			}
			sort.Slice(outliers, func(i, j int) bool {
				di, dj := outliers[i].duration(now), outliers[j].duration(now)
				if di != dj {
					return di > dj
				}
				return outliers[i].ID < outliers[j].ID
			})
			for _, r := range outliers {
				td.Outliers = append(td.Outliers, r.ID)
			}
		}
		out = append(out, td)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// coEdits counts unordered file pairs that appear together within a session, and
// surfaces those co-edited across at least coEditMinSessions sessions — sorted by
// frequency (desc) then path, capped at maxCoEditPairs.
func coEdits(sessions []SessionRecord) []CoEditPair {
	counts := map[[2]string]int{}
	for _, s := range sessions {
		files := dedupeSorted(s.Files)
		for i := 0; i < len(files); i++ {
			for j := i + 1; j < len(files); j++ {
				counts[[2]string{files[i], files[j]}]++
			}
		}
	}
	out := make([]CoEditPair, 0, len(counts))
	for k, c := range counts {
		if c < coEditMinSessions {
			continue
		}
		out = append(out, CoEditPair{A: k[0], B: k[1], Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	if len(out) > maxCoEditPairs {
		out = out[:maxCoEditPairs]
	}
	return out
}

// errorRates groups every session by type and reports the errored/orphaned share,
// sorted worst-first.
func errorRates(sessions []SessionRecord) []TypeErrorRate {
	type acc struct{ total, errored int }
	m := map[string]*acc{}
	for _, s := range sessions {
		a := m[s.Type]
		if a == nil {
			a = &acc{}
			m[s.Type] = a
		}
		a.total++
		if isErrorStatus(s.Status) {
			a.errored++
		}
	}
	out := make([]TypeErrorRate, 0, len(m))
	for typ, a := range m {
		rate := 0.0
		if a.total > 0 {
			rate = float64(a.errored) / float64(a.total)
		}
		out = append(out, TypeErrorRate{Type: typ, Total: a.total, Errored: a.errored, Rate: rate})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rate != out[j].Rate {
			return out[i].Rate > out[j].Rate
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// busiestPeriods buckets session starts by hour-of-day (UTC) and returns the
// busiest, sorted by count (desc) then hour.
func busiestPeriods(sessions []SessionRecord) []HourBucket {
	counts := map[int]int{}
	for _, s := range sessions {
		if s.Start.IsZero() {
			continue
		}
		counts[s.Start.UTC().Hour()]++
	}
	out := make([]HourBucket, 0, len(counts))
	for h, c := range counts {
		out = append(out, HourBucket{Hour: h, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Hour < out[j].Hour
	})
	if len(out) > maxBusyBuckets {
		out = out[:maxBusyBuckets]
	}
	return out
}

// anomalies carries forward the metric warnings metrics.SummarizeAgents already
// computed, sorted by agent id.
func anomalies(agents []metrics.AgentSummary) []AgentAnomaly {
	out := make([]AgentAnomaly, 0)
	for _, a := range agents {
		if len(a.Anomalies) == 0 {
			continue
		}
		out = append(out, AgentAnomaly{
			Agent:  a.ID,
			Status: a.Status,
			Notes:  append([]string(nil), a.Anomalies...),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// percentile returns the nearest-rank percentile of an ascending-sorted slice.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	rank := int(math.Ceil(float64(p)/100*float64(len(sorted)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

// dedupeSorted trims, drops blanks, de-duplicates, and sorts a path list so the
// pair/disjointness logic is order-independent.
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}
