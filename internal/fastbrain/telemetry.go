package fastbrain

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Telemetry and operator controls for the engine (spec §8). Everything here is
// content-free by construction: the only label dimensions are decision kind,
// tier, outcome, provider (AI CLI) and model, each drawn from a bounded
// vocabulary. Prompts, outputs, agent/session ids, paths and raw error strings
// never enter this file's state.

// AllKinds lists every decision kind the engine knows. Unknown kinds are
// reported under KindOther so a caller cannot grow label cardinality.
func AllKinds() []DecisionKind {
	return []DecisionKind{
		KindArbitrateApproval, KindRecognizePrompt, KindReplTurn, KindDiagnoseStall,
		KindDiagnoseFailure, KindClassifyCIFailure, KindRouteTier, KindSummarizeCheck,
		KindCommitMessage, KindPRSummary, KindClassifyTask, KindResolveAgentName,
		KindSummarizeActivity, KindCurateExtract,
	}
}

// KindOther is the bounded label for kinds outside AllKinds.
const KindOther DecisionKind = "other"

// Outcome vocabulary beyond Status values.
const (
	OutcomeCached    = "cached"    // served from the result cache, no runner
	OutcomeCoalesced = "coalesced" // joined an in-flight identical call
	OutcomePaused    = "paused"    // operator pause; failed open without a runner
)

// ShedPaused is the Admission.Reason for a paused kind.
const ShedPaused = "paused"

// Bounds.
const (
	telemetryRecent      = 200
	maxRunnerLabels      = 32
	maxLabelLen          = 48
	DefaultPauseTTL      = time.Hour
	MaxPauseTTL          = 24 * time.Hour
	latencyBucketsMillis = 10
)

// latencyBoundsMs are the histogram upper bounds in milliseconds; the final
// implicit bucket is +Inf.
var latencyBoundsMs = [latencyBucketsMillis]int64{50, 100, 250, 500, 1000, 2000, 5000, 10000, 20000, 60000}

// Histogram is a fixed-bucket latency histogram.
type Histogram struct {
	BoundsMs []int64  `json:"bounds_ms"`
	Counts   []uint64 `json:"counts"` // len(BoundsMs)+1; last is +Inf
	Count    uint64   `json:"count"`
	SumMs    int64    `json:"sum_ms"`
	P50Ms    int64    `json:"p50_ms"`
	P95Ms    int64    `json:"p95_ms"`
}

type hist struct {
	counts [latencyBucketsMillis + 1]uint64
	n      uint64
	sumMs  int64
}

func (h *hist) add(d time.Duration) {
	ms := d.Milliseconds()
	i := 0
	for i < latencyBucketsMillis && ms > latencyBoundsMs[i] {
		i++
	}
	h.counts[i]++
	h.n++
	h.sumMs += ms
}

func (h *hist) snapshot() Histogram {
	out := Histogram{BoundsMs: append([]int64(nil), latencyBoundsMs[:]...), Counts: append([]uint64(nil), h.counts[:]...), Count: h.n, SumMs: h.sumMs}
	out.P50Ms, out.P95Ms = h.quantile(0.5), h.quantile(0.95)
	return out
}

// quantile returns the upper bound of the bucket holding quantile q (an
// estimate; the +Inf bucket reports the last finite bound).
func (h *hist) quantile(q float64) int64 {
	if h.n == 0 {
		return 0
	}
	target := uint64(float64(h.n)*q + 0.5)
	if target < 1 {
		target = 1
	}
	var cum uint64
	for i, c := range h.counts {
		cum += c
		if cum >= target {
			if i >= latencyBucketsMillis {
				return latencyBoundsMs[latencyBucketsMillis-1]
			}
			return latencyBoundsMs[i]
		}
	}
	return latencyBoundsMs[latencyBucketsMillis-1]
}

// KindTierMetrics aggregates calls of one (kind, tier).
type KindTierMetrics struct {
	Kind              DecisionKind      `json:"kind"`
	Tier              Tier              `json:"tier"`
	Class             Class             `json:"class"`
	Attempts          uint64            `json:"attempts"`
	Outcomes          map[string]uint64 `json:"outcomes"`
	Shed              map[string]uint64 `json:"shed,omitempty"`
	Cached            uint64            `json:"cached"`
	Coalesced         uint64            `json:"coalesced"`
	Fallbacks         uint64            `json:"fallbacks"`
	FailOpen          uint64            `json:"fail_open"`
	CancelAcknowledge uint64            `json:"cancel_acknowledged"`
	CancelAbandoned   uint64            `json:"cancel_abandoned"`
	QueueWait         Histogram         `json:"queue_wait"`
	RunTime           Histogram         `json:"run_time"`
}

// RunnerMetrics aggregates calls by serving (provider, model).
type RunnerMetrics struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	Tier     Tier   `json:"tier"`
	Calls    uint64 `json:"calls"`
	OK       uint64 `json:"ok"`
	Failed   uint64 `json:"failed"`
	RunSumMs int64  `json:"run_sum_ms"`
}

// TraceCandidate is one redacted selection-trail entry.
type TraceCandidate struct {
	Provider string `json:"provider"`
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason,omitempty"`
}

// Decision is one redacted, bounded trace row for the operator trace view.
type Decision struct {
	Time        time.Time        `json:"time"`
	Kind        DecisionKind     `json:"kind"`
	Tier        Tier             `json:"tier"`
	Class       Class            `json:"class"`
	Outcome     string           `json:"outcome"`
	FinalAction string           `json:"final_action"` // decided | fail_open | cached | deferred
	Reason      string           `json:"reason,omitempty"`
	Provider    string           `json:"provider,omitempty"`
	Model       string           `json:"model,omitempty"`
	QueueWaitMs int64            `json:"queue_wait_ms"`
	RunMs       int64            `json:"run_ms"`
	Coalesced   bool             `json:"coalesced,omitempty"`
	Cached      bool             `json:"cached,omitempty"`
	Fallback    bool             `json:"fallback,omitempty"`
	Attempts    int              `json:"attempts,omitempty"`
	Cancel      string           `json:"cancel,omitempty"`
	Trail       []TraceCandidate `json:"trail,omitempty"`
}

// CircuitInfo is the redacted circuit state of one runner candidate.
type CircuitInfo struct {
	Provider            string       `json:"provider"`
	State               CircuitState `json:"state"`
	ConsecutiveFailures int          `json:"consecutive_failures"`
	LastFailure         string       `json:"last_failure,omitempty"`
	RetryAt             *time.Time   `json:"retry_at,omitempty"`
	Opens               int          `json:"opens"`
}

// KindControl is the operator control state of one decision kind.
type KindControl struct {
	Kind   DecisionKind `json:"kind"`
	Class  Class        `json:"class"`
	Paused bool         `json:"paused"`
	Until  *time.Time   `json:"until,omitempty"` // nil = until resumed/restart
	Since  *time.Time   `json:"since,omitempty"`
	Source string       `json:"source,omitempty"` // "operator" | "config"
}

// QueueDepth is a per-class gauge.
type QueueDepth struct {
	Class  Class `json:"class"`
	Active int   `json:"active"`
	Queued int   `json:"queued"`
}

// TelemetrySnapshot is the full, content-free operator view.
type TelemetrySnapshot struct {
	Now             time.Time         `json:"now"`
	MaxConcurrent   int               `json:"max_concurrent"`
	Active          int               `json:"active"`
	Admitted        uint64            `json:"admitted"`
	Coalesced       uint64            `json:"coalesced"`
	CacheHits       uint64            `json:"cache_hits"`
	CacheItems      int               `json:"cache_items"`
	Preempted       uint64            `json:"preempted"`
	Shed            map[string]uint64 `json:"shed"`
	Queues          []QueueDepth      `json:"queues"`
	Abandoned       int64             `json:"abandoned_total"`
	AbandonedLive   int64             `json:"abandoned_live"`
	Kinds           []KindTierMetrics `json:"kinds"`
	Runners         []RunnerMetrics   `json:"runners"`
	Circuits        []CircuitInfo     `json:"circuits"`
	Controls        []KindControl     `json:"controls"`
	TotalDecisions  uint64            `json:"total_decisions"`
	RecentRetention int               `json:"recent_retention"`
}

type kindTierKey struct {
	kind DecisionKind
	tier Tier
}

type runnerKey struct {
	provider, model string
	tier            Tier
}

type pauseState struct {
	since  time.Time
	until  time.Time // zero = no expiry
	source string
}

// AuditFunc receives bounded audit events (action, target = kind or provider,
// detail with bounded values only).
type AuditFunc func(action, target string, detail map[string]string)

// Audit actions emitted through AuditFunc (mirrored in package audit).
const (
	AuditControl   = "fastbrain_control"
	AuditCircuit   = "fastbrain_circuit"
	AuditAbandoned = "fastbrain_cancel_abandoned"
)

type telemetry struct {
	mu       sync.Mutex
	kinds    map[kindTierKey]*ktAgg
	runners  map[runnerKey]*RunnerMetrics
	recent   []Decision // ring, oldest first
	total    uint64
	pauses   map[DecisionKind]pauseState
	circuits map[string]CircuitState
	known    map[DecisionKind]bool
	now      func() time.Time
	audit    AuditFunc
	health   func() []RunnerHealth
}

type ktAgg struct {
	m  KindTierMetrics
	qw hist
	rt hist
}

func newTelemetry(audit AuditFunc, health func() []RunnerHealth, now func() time.Time) *telemetry {
	if now == nil {
		now = time.Now
	}
	t := &telemetry{
		kinds: map[kindTierKey]*ktAgg{}, runners: map[runnerKey]*RunnerMetrics{},
		pauses: map[DecisionKind]pauseState{}, circuits: map[string]CircuitState{},
		known: map[DecisionKind]bool{}, now: now, audit: audit, health: health,
	}
	for _, k := range AllKinds() {
		t.known[k] = true
	}
	return t
}

// boundKind maps any kind to the bounded label vocabulary.
func (t *telemetry) boundKind(k DecisionKind) DecisionKind {
	if t.known[k] {
		return k
	}
	return KindOther
}

// label sanitizes a free-form id/model string to a short [a-z0-9._:/-] label.
func label(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == ':', r == '-':
			b.WriteRune(r)
		case r == '/' || r == ' ':
			b.WriteRune('-')
		}
		if b.Len() >= maxLabelLen {
			break
		}
	}
	return b.String()
}

func outcomeOf(r Response) string {
	switch {
	case r.Admission.Cached:
		return OutcomeCached
	case r.Admission.Reason == ShedPaused:
		return OutcomePaused
	}
	return string(r.Status)
}

func finalAction(r Response) string {
	switch {
	case r.Admission.Cached:
		return "cached"
	case r.Status == StatusOK:
		return "decided"
	case r.Status == StatusDeferred:
		return "deferred"
	}
	return "fail_open"
}

// observe records one finished Decide call.
func (t *telemetry) observe(r Response) {
	kind := t.boundKind(r.Kind)
	key := kindTierKey{kind, r.Tier}
	cls := ClassOf(r.Kind)
	outcome := outcomeOf(r)
	d := Decision{
		Time: t.now(), Kind: kind, Tier: r.Tier, Class: cls, Outcome: outcome,
		FinalAction: finalAction(r), Reason: label(r.Admission.Reason),
		QueueWaitMs: r.Admission.QueueWait.Milliseconds(),
		Coalesced:   r.Admission.Coalesced, Cached: r.Admission.Cached,
		Fallback: r.Selection.Fallback, Attempts: r.Selection.Attempts,
		Cancel: string(r.Cancel),
	}
	ran := !r.Admission.Cached && !r.Admission.Coalesced && r.Admission.Reason == "" &&
		r.Status != StatusNoRunner
	if ran && r.Duration > r.Admission.QueueWait {
		d.RunMs = (r.Duration - r.Admission.QueueWait).Milliseconds()
	}
	provider := label(r.Selection.Runner)
	model := label(r.Selection.Model)

	t.mu.Lock()
	if provider != "" && !t.runnerSlotLocked(runnerKey{provider, model, r.Tier}) {
		provider, model = "other", ""
	}
	d.Provider, d.Model = provider, model
	for i, c := range r.Selection.Trail {
		if i >= maxTrail {
			break
		}
		d.Trail = append(d.Trail, TraceCandidate{Provider: label(c.ID), Verdict: label(string(c.Verdict)), Reason: label(c.Reason)})
	}
	a := t.kinds[key]
	if a == nil {
		a = &ktAgg{m: KindTierMetrics{Kind: kind, Tier: r.Tier, Class: cls, Outcomes: map[string]uint64{}, Shed: map[string]uint64{}}}
		t.kinds[key] = a
	}
	a.m.Attempts++
	a.m.Outcomes[outcome]++
	if r.Admission.Reason != "" {
		a.m.Shed[label(r.Admission.Reason)]++
	}
	if r.Admission.Cached {
		a.m.Cached++
	}
	if r.Admission.Coalesced {
		a.m.Coalesced++
	}
	if r.Selection.Fallback {
		a.m.Fallbacks++
	}
	if d.FinalAction == "fail_open" {
		a.m.FailOpen++
	}
	switch r.Cancel {
	case CancelAcknowledged:
		a.m.CancelAcknowledge++
	case CancelAbandoned:
		a.m.CancelAbandoned++
	}
	if !r.Admission.Cached && !r.Admission.Coalesced {
		a.qw.add(r.Admission.QueueWait)
	}
	if ran {
		a.rt.add(time.Duration(d.RunMs) * time.Millisecond)
		if provider != "" {
			rm := t.runners[runnerKey{provider, model, r.Tier}]
			if rm != nil {
				rm.Calls++
				if r.Status == StatusOK {
					rm.OK++
				} else {
					rm.Failed++
				}
				rm.RunSumMs += d.RunMs
			}
		}
	}
	t.total++
	if len(t.recent) >= telemetryRecent {
		copy(t.recent, t.recent[1:])
		t.recent = t.recent[:telemetryRecent-1]
	}
	t.recent = append(t.recent, d)
	events := t.circuitEventsLocked()
	if r.Cancel == CancelAbandoned && t.audit != nil {
		events = append(events, auditEvent{AuditAbandoned, string(kind), map[string]string{"tier": string(r.Tier), "provider": provider}})
	}
	t.mu.Unlock()
	t.emit(events)
}

// runnerSlotLocked reserves a runner row, refusing new labels past the bound.
func (t *telemetry) runnerSlotLocked(k runnerKey) bool {
	if _, ok := t.runners[k]; ok {
		return true
	}
	if len(t.runners) >= maxRunnerLabels {
		return false
	}
	t.runners[k] = &RunnerMetrics{Provider: k.provider, Model: k.model, Tier: k.tier}
	return true
}

type auditEvent struct {
	action, target string
	detail         map[string]string
}

func (t *telemetry) emit(evs []auditEvent) {
	if t.audit == nil {
		return
	}
	for _, e := range evs {
		t.audit(e.action, e.target, e.detail)
	}
}

// circuitEventsLocked diffs the health snapshot against the last seen states
// and returns audit events for transitions.
func (t *telemetry) circuitEventsLocked() []auditEvent {
	if t.health == nil || t.audit == nil {
		return nil
	}
	var evs []auditEvent
	for _, h := range t.health() {
		id := label(h.ID)
		prev, seen := t.circuits[id]
		if seen && prev == h.State {
			continue
		}
		t.circuits[id] = h.State
		if !seen && h.State == CircuitClosed {
			continue
		}
		evs = append(evs, auditEvent{AuditCircuit, id, map[string]string{
			"from": string(prev), "to": string(h.State), "last_failure": label(string(h.LastFailure)),
		}})
	}
	return evs
}

// paused reports whether kind is currently paused, expiring stale pauses.
func (t *telemetry) paused(kind DecisionKind) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.pauses[kind]
	if !ok {
		return false
	}
	if !p.until.IsZero() && !t.now().Before(p.until) {
		delete(t.pauses, kind)
		return false
	}
	return true
}

// setPaused pauses (ttl bounded to MaxPauseTTL; zero = DefaultPauseTTL) or
// resumes a kind. source is "operator" or "config" (config pauses never expire).
func (t *telemetry) setPaused(kind DecisionKind, paused bool, ttl time.Duration, source string) error {
	if !t.known[kind] {
		return ErrUnknownKind
	}
	t.mu.Lock()
	now := t.now()
	if paused {
		p := pauseState{since: now, source: source}
		if source != "config" {
			if ttl <= 0 {
				ttl = DefaultPauseTTL
			}
			if ttl > MaxPauseTTL {
				ttl = MaxPauseTTL
			}
			p.until = now.Add(ttl)
		}
		t.pauses[kind] = p
	} else {
		delete(t.pauses, kind)
	}
	t.mu.Unlock()
	detail := map[string]string{"paused": boolStr(paused), "source": label(source)}
	if paused && ttl > 0 && source != "config" {
		detail["ttl"] = ttl.String()
	}
	t.emit([]auditEvent{{AuditControl, string(kind), detail}})
	return nil
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func (t *telemetry) controlsLocked() []KindControl {
	now := t.now()
	out := make([]KindControl, 0, len(t.known))
	for _, k := range AllKinds() {
		c := KindControl{Kind: k, Class: ClassOf(k)}
		if p, ok := t.pauses[k]; ok && (p.until.IsZero() || now.Before(p.until)) {
			c.Paused, c.Source = true, p.source
			since := p.since
			c.Since = &since
			if !p.until.IsZero() {
				u := p.until
				c.Until = &u
			}
		}
		out = append(out, c)
	}
	return out
}

func (t *telemetry) snapshot() TelemetrySnapshot {
	t.mu.Lock()
	s := TelemetrySnapshot{Now: t.now(), TotalDecisions: t.total, RecentRetention: telemetryRecent, Controls: t.controlsLocked()}
	for _, a := range t.kinds {
		m := a.m
		m.Outcomes = copyCounts(a.m.Outcomes)
		m.Shed = copyCounts(a.m.Shed)
		m.QueueWait, m.RunTime = a.qw.snapshot(), a.rt.snapshot()
		s.Kinds = append(s.Kinds, m)
	}
	for _, r := range t.runners {
		s.Runners = append(s.Runners, *r)
	}
	t.mu.Unlock()
	sort.Slice(s.Kinds, func(i, j int) bool {
		if s.Kinds[i].Kind != s.Kinds[j].Kind {
			return s.Kinds[i].Kind < s.Kinds[j].Kind
		}
		return s.Kinds[i].Tier < s.Kinds[j].Tier
	})
	sort.Slice(s.Runners, func(i, j int) bool {
		a, b := s.Runners[i], s.Runners[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Tier < b.Tier
	})
	if t.health != nil {
		for _, h := range t.health() {
			c := CircuitInfo{Provider: label(h.ID), State: h.State, ConsecutiveFailures: h.ConsecutiveFailures,
				LastFailure: label(string(h.LastFailure)), Opens: h.Opens}
			if !h.RetryAt.IsZero() {
				ra := h.RetryAt
				c.RetryAt = &ra
			}
			s.Circuits = append(s.Circuits, c)
		}
		sort.Slice(s.Circuits, func(i, j int) bool { return s.Circuits[i].Provider < s.Circuits[j].Provider })
	}
	return s
}

func (t *telemetry) decisions(limit int) []Decision {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(t.recent)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]Decision, limit)
	// newest first
	for i := 0; i < limit; i++ {
		out[i] = t.recent[n-1-i]
	}
	return out
}

func copyCounts(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
