package backendstore

// This registry deliberately has a much smaller label space than catalog
// evidence. Model ids, account references, profile paths and command output
// belong in the durable (redacted) audit trail, never in metrics.

import (
	"errors"
	"sync"
	"time"
)

// CatalogRefreshMetric is one bounded refresh counter series.
type CatalogRefreshMetric struct {
	AiCli        string `json:"aicli"`
	Completeness string `json:"completeness"`
	Freshness    string `json:"freshness"`
	Error        string `json:"error"`
	Total        uint64 `json:"total"`
}

// CatalogMetricsSnapshot is safe to return from an operator surface. It has no
// model, account, prompt, profile, or raw provider-output fields.
type CatalogMetricsSnapshot struct {
	Refreshes              []CatalogRefreshMetric `json:"refreshes"`
	Discovered             uint64                 `json:"discovered"`
	Unavailable            uint64                 `json:"unavailable"`
	Unknown                uint64                 `json:"unknown"`
	PartialResults         uint64                 `json:"partial_results"`
	Timeouts               uint64                 `json:"timeouts"`
	ReconciliationConflict uint64                 `json:"reconciliation_conflicts"`
	RefreshDuration        DurationHistogram      `json:"refresh_duration"`
}

// DurationHistogram has fixed bounds to retain only bounded cardinality.
type DurationHistogram struct {
	BoundsSeconds []float64 `json:"bounds_seconds"`
	Counts        []uint64  `json:"counts"`
	Count         uint64    `json:"count"`
	SumSeconds    float64   `json:"sum_seconds"`
}

type catalogMetricKey struct{ ai, completeness, freshness, err string }

// CatalogMetrics records only catalog-level, bounded telemetry.
type CatalogMetrics struct {
	mu                                                             sync.Mutex
	refresh                                                        map[catalogMetricKey]uint64
	discovered, unavailable, unknown, partial, timeouts, conflicts uint64
	durations                                                      DurationHistogram
}

var catalogDurationBounds = []float64{0.01, 0.05, 0.25, 1, 5, 20, 60}

func NewCatalogMetrics() *CatalogMetrics {
	return &CatalogMetrics{refresh: map[catalogMetricKey]uint64{}, durations: DurationHistogram{BoundsSeconds: append([]float64(nil), catalogDurationBounds...), Counts: make([]uint64, len(catalogDurationBounds))}}
}

func catalogAiLabel(v string) string {
	switch v {
	case "codex", "cursor", "antigravity", "opencode", "crush", "claude", "aider", "goose":
		return v
	default:
		return "other"
	}
}
func catalogCompletenessLabel(v Completeness) string {
	switch v {
	case CompletenessComplete, CompletenessPartial, CompletenessEmpty, CompletenessFailed:
		return string(v)
	default:
		return "other"
	}
}
func catalogFreshness(o CatalogObservation, now time.Time) string {
	if o.Source != SourceCLICache {
		return "not_applicable"
	}
	if o.SourceFetchedAt.IsZero() || now.Sub(o.SourceFetchedAt) > 24*time.Hour {
		return "stale"
	}
	return "fresh"
}
func catalogErrorLabel(o CatalogObservation) string {
	if o.Err == nil {
		return "none"
	}
	switch o.Err.Code {
	case "deadline", "cancelled", "partial_result", "not_installed", "command_failed", "malformed", "malformed_or_empty", "identity_mismatch", "scope_unresolved", "stale_cache", "version_mismatch", "empty":
		return o.Err.Code
	default:
		return "other"
	}
}

func (m *CatalogMetrics) observeRefresh(ai string, o CatalogObservation, d time.Duration, changes []ObservationChange) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh[catalogMetricKey{catalogAiLabel(ai), catalogCompletenessLabel(o.Completeness), catalogFreshness(o, time.Now()), catalogErrorLabel(o)}]++
	if o.Completeness == CompletenessPartial {
		m.partial++
	}
	if o.Err != nil && o.Err.Code == "deadline" {
		m.timeouts++
	}
	for _, c := range changes {
		switch c.To {
		case AvailabilityAvailable:
			m.discovered++
		case AvailabilityUnavailable:
			m.unavailable++
		case AvailabilityUnknown:
			m.unknown++
		}
	}
	m.durations.Count++
	m.durations.SumSeconds += d.Seconds()
	for i, b := range m.durations.BoundsSeconds {
		if d.Seconds() <= b {
			m.durations.Counts[i]++
		}
	}
}
func (m *CatalogMetrics) observeConflict(err error) {
	if m != nil && (errorsIsCatalogConflict(err)) {
		m.mu.Lock()
		m.conflicts++
		m.mu.Unlock()
	}
}
func errorsIsCatalogConflict(err error) bool {
	return errors.Is(err, ErrProposalStale) || errors.Is(err, ErrRevisionConflict)
}

// CatalogMetrics returns a copy suitable for concurrent inspection.
func (s *Store) CatalogMetrics() CatalogMetricsSnapshot {
	m := s.catalogMetrics
	if m == nil {
		return CatalogMetricsSnapshot{Refreshes: []CatalogRefreshMetric{}}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := CatalogMetricsSnapshot{Discovered: m.discovered, Unavailable: m.unavailable, Unknown: m.unknown, PartialResults: m.partial, Timeouts: m.timeouts, ReconciliationConflict: m.conflicts, RefreshDuration: DurationHistogram{BoundsSeconds: append([]float64(nil), m.durations.BoundsSeconds...), Counts: append([]uint64(nil), m.durations.Counts...), Count: m.durations.Count, SumSeconds: m.durations.SumSeconds}}
	for k, n := range m.refresh {
		out.Refreshes = append(out.Refreshes, CatalogRefreshMetric{AiCli: k.ai, Completeness: k.completeness, Freshness: k.freshness, Error: k.err, Total: n})
	}
	return out
}
