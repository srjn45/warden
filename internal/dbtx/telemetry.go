package dbtx

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/audit"
)

// Standard outcome names for telemetry and audit.
const (
	OutcomeSuccess    = "committed"
	OutcomeRolledBack = "rolled_back"
	OutcomeAbandoned  = "abandoned"
	OutcomeNoChange   = "no_change"
	OutcomeOther      = "other"

	maxIntentBuckets     = 64
	maxIntentLen         = 48
	maxTargetLen         = 64
	latencyBucketsMillis = 12
)

// latencyBoundsMs are the histogram upper bounds in milliseconds; the final implicit bucket is +Inf.
var latencyBoundsMs = [latencyBucketsMillis]int64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

var knownIntents = map[string]struct{}{
	"project-child-creation": {},
	"autopilot-startup":      {},
	"pipeline-linkage":       {},
	"archive-restore":        {},
	"project-closure":        {},
	"projection-update":      {},
	"audit-event-append":     {},
	"project":                {},
	"agent":                  {},
	"pipeline":               {},
	"plan":                   {},
	"operation":              {},
	"audit":                  {},
	"migration":              {},
	"sync":                   {},
	"session":                {},
	"test":                   {},
	"seed":                   {},
	"obs":                    {},
	"unspecified":            {},
	"other":                  {},
	"custom":                 {},
}

var knownClasses = map[Class]struct{}{
	ClassConflict:           {},
	ClassExhausted:          {},
	ClassTooLarge:           {},
	ClassDurability:         {},
	ClassOutcomeUnknown:     {},
	ClassCollectionNotFound: {},
	ClassInternal:           {},
}

var dynamicPrefixRe = regexp.MustCompile(`^(op|plan|agent|proj|project|pipe|pipeline|run|job|sess|session|user|tx|wf|workflow)[-_]`)
var hexOrNumericRe = regexp.MustCompile(`^[0-9a-fA-F\-]{8,}$|^[0-9]+$`)

// NormalizeIntent normalizes an intent string to a bounded, low-cardinality label.
// Dynamic identifiers (UUIDs, timestamps, op hashes) are mapped to canonical family
// names to prevent cardinality explosion. Unknown or excessive intents map to "other".
func NormalizeIntent(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unspecified"
	}
	raw = strings.ToLower(raw)
	if _, ok := knownIntents[raw]; ok {
		return raw
	}

	// Split by separator ('/', '@', ':')
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '/' || r == '@' || r == ':'
	})

	if len(parts) > 0 {
		prefix := parts[0]
		if _, ok := knownIntents[prefix]; ok {
			if len(parts) > 1 && isSafeActionSuffix(parts[1]) {
				return prefix + "/" + parts[1]
			}
			return prefix
		}

		if dynamicPrefixRe.MatchString(prefix) {
			cat := dynamicPrefixCategory(prefix)
			if len(parts) > 1 && isSafeActionSuffix(parts[1]) {
				return cat + "/" + parts[1]
			}
			return cat
		}

		if hexOrNumericRe.MatchString(prefix) {
			return "custom"
		}
	}

	// Sanitize characters
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	sanitized := b.String()
	if len(sanitized) > maxIntentLen {
		sanitized = sanitized[:maxIntentLen]
	}
	if hexOrNumericRe.MatchString(sanitized) {
		return "custom"
	}
	return sanitized
}

func isSafeActionSuffix(s string) bool {
	if len(s) == 0 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return !hexOrNumericRe.MatchString(s)
}

func dynamicPrefixCategory(prefix string) string {
	switch {
	case strings.HasPrefix(prefix, "op-") || strings.HasPrefix(prefix, "op_"):
		return "operation"
	case strings.HasPrefix(prefix, "plan-") || strings.HasPrefix(prefix, "plan_"):
		return "plan"
	case strings.HasPrefix(prefix, "agent-") || strings.HasPrefix(prefix, "agent_"):
		return "agent"
	case strings.HasPrefix(prefix, "proj-") || strings.HasPrefix(prefix, "proj_") || strings.HasPrefix(prefix, "project-") || strings.HasPrefix(prefix, "project_"):
		return "project"
	case strings.HasPrefix(prefix, "pipe-") || strings.HasPrefix(prefix, "pipe_") || strings.HasPrefix(prefix, "pipeline-") || strings.HasPrefix(prefix, "pipeline_"):
		return "pipeline"
	case strings.HasPrefix(prefix, "run-") || strings.HasPrefix(prefix, "run_"):
		return "run"
	case strings.HasPrefix(prefix, "job-") || strings.HasPrefix(prefix, "job_"):
		return "job"
	case strings.HasPrefix(prefix, "sess-") || strings.HasPrefix(prefix, "sess_") || strings.HasPrefix(prefix, "session-") || strings.HasPrefix(prefix, "session_"):
		return "session"
	case strings.HasPrefix(prefix, "user-") || strings.HasPrefix(prefix, "user_"):
		return "user"
	case strings.HasPrefix(prefix, "tx-") || strings.HasPrefix(prefix, "tx_"):
		return "tx"
	case strings.HasPrefix(prefix, "wf-") || strings.HasPrefix(prefix, "wf_") || strings.HasPrefix(prefix, "workflow-") || strings.HasPrefix(prefix, "workflow_"):
		return "workflow"
	default:
		return "operation"
	}
}

// NormalizeOutcome maps any outcome string to a bounded, closed vocabulary.
func NormalizeOutcome(raw string) string {
	raw = strings.TrimSpace(raw)
	switch raw {
	case OutcomeSuccess:
		return OutcomeSuccess
	case OutcomeRolledBack:
		return OutcomeRolledBack
	case OutcomeAbandoned:
		return OutcomeAbandoned
	case OutcomeNoChange:
		return OutcomeNoChange
	case string(ClassConflict):
		return string(ClassConflict)
	case string(ClassExhausted):
		return string(ClassExhausted)
	case string(ClassDurability):
		return string(ClassDurability)
	case string(ClassTooLarge):
		return string(ClassTooLarge)
	case string(ClassOutcomeUnknown):
		return string(ClassOutcomeUnknown)
	case string(ClassCollectionNotFound):
		return string(ClassCollectionNotFound)
	case string(ClassInternal):
		return string(ClassInternal)
	}
	return OutcomeOther
}

// Histogram is a fixed-bucket latency histogram safe for concurrent JSON export.
type Histogram struct {
	BoundsMs []int64  `json:"bounds_ms"`
	Counts   []uint64 `json:"counts"` // len(BoundsMs)+1; last is +Inf
	Count    uint64   `json:"count"`
	SumMs    int64    `json:"sum_ms"`
	MinMs    int64    `json:"min_ms"`
	MaxMs    int64    `json:"max_ms"`
	P50Ms    int64    `json:"p50_ms"`
	P95Ms    int64    `json:"p95_ms"`
	P99Ms    int64    `json:"p99_ms"`
}

type hist struct {
	counts [latencyBucketsMillis + 1]uint64
	n      uint64
	sumMs  int64
	minMs  int64
	maxMs  int64
}

func (h *hist) add(d time.Duration) {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	i := 0
	for i < latencyBucketsMillis && ms > latencyBoundsMs[i] {
		i++
	}
	h.counts[i]++
	if h.n == 0 || ms < h.minMs {
		h.minMs = ms
	}
	if ms > h.maxMs {
		h.maxMs = ms
	}
	h.n++
	h.sumMs += ms
}

func (h *hist) snapshot() Histogram {
	out := Histogram{
		BoundsMs: append([]int64(nil), latencyBoundsMs[:]...),
		Counts:   append([]uint64(nil), h.counts[:]...),
		Count:    h.n,
		SumMs:    h.sumMs,
		MinMs:    h.minMs,
		MaxMs:    h.maxMs,
	}
	out.P50Ms = h.quantile(0.50)
	out.P95Ms = h.quantile(0.95)
	out.P99Ms = h.quantile(0.99)
	return out
}

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
			if i < latencyBucketsMillis {
				return latencyBoundsMs[i]
			}
			return latencyBoundsMs[latencyBucketsMillis-1]
		}
	}
	return latencyBoundsMs[latencyBucketsMillis-1]
}

// IntentMetrics accumulates counters for one normalized intent.
type IntentMetrics struct {
	Commits    uint64 `json:"commits"`
	Conflicts  uint64 `json:"conflicts"`
	Rollbacks  uint64 `json:"rollbacks"`
	Recoveries uint64 `json:"recoveries"`
	Abandoned  uint64 `json:"abandoned"`
	Errors     uint64 `json:"errors"`
	latency    hist
}

// IntentSummary is an exportable snapshot of one intent's metrics.
type IntentSummary struct {
	Commits    uint64    `json:"commits"`
	Conflicts  uint64    `json:"conflicts"`
	Rollbacks  uint64    `json:"rollbacks"`
	Recoveries uint64    `json:"recoveries"`
	Abandoned  uint64    `json:"abandoned"`
	Errors     uint64    `json:"errors"`
	Latency    Histogram `json:"latency"`
}

// MetricsSnapshot is a point-in-time snapshot of transaction metrics.
type MetricsSnapshot struct {
	Latency         Histogram                `json:"latency"`
	CommitsTotal    uint64                   `json:"commits_total"`
	ConflictsTotal  uint64                   `json:"conflicts_total"`
	RollbacksTotal  uint64                   `json:"rollbacks_total"`
	RecoveriesTotal uint64                   `json:"recoveries_total"`
	AbandonedTotal  uint64                   `json:"abandoned_total"`
	ErrorsTotal     uint64                   `json:"errors_total"`
	ByIntent        map[string]IntentSummary `json:"by_intent"`
	ByOutcome       map[string]uint64        `json:"by_outcome"`
	ByClass         map[string]uint64        `json:"by_class"`
}

// MetricsCollector implements Observer to aggregate bounded transaction metrics in memory.
type MetricsCollector struct {
	mu sync.RWMutex

	latency hist

	commitsTotal    uint64
	conflictsTotal  uint64
	rollbacksTotal  uint64
	recoveriesTotal uint64
	abandonedTotal  uint64
	errorsTotal     uint64

	byIntent  map[string]*IntentMetrics
	byOutcome map[string]uint64
	byClass   map[Class]uint64
}

// NewMetricsCollector returns an initialized MetricsCollector.
func NewMetricsCollector() *MetricsCollector {
	c := &MetricsCollector{
		byIntent:  make(map[string]*IntentMetrics),
		byOutcome: make(map[string]uint64),
		byClass:   make(map[Class]uint64),
	}
	c.byIntent[OutcomeOther] = &IntentMetrics{}
	return c
}

// Observe records a transaction Event with safe label normalization and memory bounds.
func (m *MetricsCollector) Observe(ev Event) {
	if m == nil {
		return
	}
	defer func() { _ = recover() }()

	normIntent := NormalizeIntent(ev.Intent)
	normOutcome := NormalizeOutcome(ev.Outcome)

	m.mu.Lock()
	defer m.mu.Unlock()

	m.latency.add(ev.Duration)

	switch normOutcome {
	case OutcomeSuccess:
		m.commitsTotal++
	case string(ClassConflict):
		m.conflictsTotal++
		m.errorsTotal++
	case OutcomeRolledBack:
		m.rollbacksTotal++
	case OutcomeAbandoned, string(ClassOutcomeUnknown):
		m.abandonedTotal++
		m.errorsTotal++
	default:
		if normOutcome != OutcomeNoChange {
			m.errorsTotal++
		}
	}

	if ev.Resolved {
		m.recoveriesTotal++
	}

	m.byOutcome[normOutcome]++

	if class := Class(normOutcome); isKnownClass(class) {
		m.byClass[class]++
	}

	// Safe cardinality bounding for intent map
	im, ok := m.byIntent[normIntent]
	if !ok {
		if len(m.byIntent) >= maxIntentBuckets {
			normIntent = OutcomeOther
			im = m.byIntent[normIntent]
		}
		if im == nil {
			im = &IntentMetrics{}
			m.byIntent[normIntent] = im
		}
	}

	im.latency.add(ev.Duration)
	switch normOutcome {
	case OutcomeSuccess:
		im.Commits++
	case string(ClassConflict):
		im.Conflicts++
		im.Errors++
	case OutcomeRolledBack:
		im.Rollbacks++
	case OutcomeAbandoned, string(ClassOutcomeUnknown):
		im.Abandoned++
		im.Errors++
	default:
		if normOutcome != OutcomeNoChange {
			im.Errors++
		}
	}

	if ev.Resolved {
		im.Recoveries++
	}
}

func isKnownClass(c Class) bool {
	_, ok := knownClasses[c]
	return ok
}

// Snapshot returns a copy of current metrics.
func (m *MetricsCollector) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	snap := MetricsSnapshot{
		Latency:         m.latency.snapshot(),
		CommitsTotal:    m.commitsTotal,
		ConflictsTotal:  m.conflictsTotal,
		RollbacksTotal:  m.rollbacksTotal,
		RecoveriesTotal: m.recoveriesTotal,
		AbandonedTotal:  m.abandonedTotal,
		ErrorsTotal:     m.errorsTotal,
		ByIntent:        make(map[string]IntentSummary, len(m.byIntent)),
		ByOutcome:       make(map[string]uint64, len(m.byOutcome)),
		ByClass:         make(map[string]uint64, len(m.byClass)),
	}

	for k, v := range m.byOutcome {
		snap.ByOutcome[k] = v
	}
	for k, v := range m.byClass {
		snap.ByClass[string(k)] = v
	}
	for k, im := range m.byIntent {
		if im.Commits == 0 && im.Conflicts == 0 && im.Rollbacks == 0 && im.Recoveries == 0 && im.Abandoned == 0 && im.Errors == 0 && im.latency.n == 0 {
			continue
		}
		snap.ByIntent[k] = IntentSummary{
			Commits:    im.Commits,
			Conflicts:  im.Conflicts,
			Rollbacks:  im.Rollbacks,
			Recoveries: im.Recoveries,
			Abandoned:  im.Abandoned,
			Errors:     im.Errors,
			Latency:    im.latency.snapshot(),
		}
	}

	return snap
}

// CommitsTotal returns total successful commits recorded.
func (m *MetricsCollector) CommitsTotal() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.commitsTotal
}

// ConflictsTotal returns total conflicts recorded.
func (m *MetricsCollector) ConflictsTotal() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.conflictsTotal
}

// RollbacksTotal returns total rollbacks recorded.
func (m *MetricsCollector) RollbacksTotal() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rollbacksTotal
}

// RecoveriesTotal returns total recoveries recorded.
func (m *MetricsCollector) RecoveriesTotal() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.recoveriesTotal
}

// AbandonedTotal returns total abandoned operations recorded.
func (m *MetricsCollector) AbandonedTotal() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.abandonedTotal
}

// ErrorsTotal returns total errors recorded across all classes.
func (m *MetricsCollector) ErrorsTotal() uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.errorsTotal
}

// Reset clears all counters and latency distributions (useful in tests).
func (m *MetricsCollector) Reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latency = hist{}
	m.commitsTotal = 0
	m.conflictsTotal = 0
	m.rollbacksTotal = 0
	m.recoveriesTotal = 0
	m.abandonedTotal = 0
	m.errorsTotal = 0
	m.byIntent = make(map[string]*IntentMetrics)
	m.byIntent[OutcomeOther] = &IntentMetrics{}
	m.byOutcome = make(map[string]uint64)
	m.byClass = make(map[Class]uint64)
}

// AuditObserver logs significant cross-collection transaction events (recovery, abandoned operations,
// outcome resolution) to an audit.Writer in best-effort mode.
type AuditObserver struct {
	writer *audit.Writer
}

// NewAuditObserver builds an Observer that forwards significant events to the audit trail.
func NewAuditObserver(w *audit.Writer) *AuditObserver {
	return &AuditObserver{writer: w}
}

// Observe logs significant events to the audit trail. Never blocks, errors, or panics.
func (a *AuditObserver) Observe(ev Event) {
	if a == nil || a.writer == nil {
		return
	}
	defer func() { _ = recover() }()

	normIntent := NormalizeIntent(ev.Intent)
	normOutcome := NormalizeOutcome(ev.Outcome)

	var action string
	switch {
	case ev.Resolved:
		action = audit.ActionTxRecovery
	case normOutcome == OutcomeAbandoned || normOutcome == string(ClassOutcomeUnknown):
		action = audit.ActionTxAbandoned
	case normOutcome == string(ClassDurability) || normOutcome == string(ClassTooLarge):
		action = audit.ActionTxOutcome
	default:
		// Normal commits, standard rollbacks, and expected retried conflicts do not spam audit.jsonl
		return
	}

	target := ev.TxID
	if target == "" {
		target = normIntent
	}
	if len(target) > maxTargetLen {
		target = target[:maxTargetLen]
	}

	detail := map[string]string{
		"intent":   normIntent,
		"outcome":  normOutcome,
		"duration": ev.Duration.String(),
		"ops":      fmt.Sprintf("%d", ev.Ops),
	}
	if ev.TxID != "" {
		detail["tx_id"] = ev.TxID
	}
	if ev.Resolved {
		detail["resolved"] = "true"
	}

	a.writer.Log(audit.Event{
		Time:   time.Now(),
		Action: action,
		Target: target,
		Detail: audit.SafeDetail(detail),
	})
}

func safeObserve(obs Observer, ev Event) {
	if obs == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	obs.Observe(ev)
}

// MultiObserver combines multiple Observers into one. Failures or panics in one observer
// do not affect others or the caller.
func MultiObserver(observers ...Observer) Observer {
	var valid []Observer
	for _, o := range observers {
		if o != nil {
			valid = append(valid, o)
		}
	}
	if len(valid) == 0 {
		return nil
	}
	if len(valid) == 1 {
		return valid[0]
	}
	return multiObs(valid)
}

type multiObs []Observer

func (m multiObs) Observe(ev Event) {
	for _, o := range m {
		safeObserve(o, ev)
	}
}

// ObserverFunc is a functional adapter for Observer.
type ObserverFunc func(Event)

func (f ObserverFunc) Observe(ev Event) {
	if f != nil {
		f(ev)
	}
}

// DefaultObserver creates a new MetricsCollector and, if w != nil, pairs it with an AuditObserver.
func DefaultObserver(w *audit.Writer) (*MetricsCollector, Observer) {
	collector := NewMetricsCollector()
	if w == nil {
		return collector, collector
	}
	return collector, MultiObserver(collector, NewAuditObserver(w))
}
