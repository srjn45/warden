package ptyobs

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Metric names are a stable, documented contract (docs/PTY_OBSERVABILITY.md).
const (
	MEvents          = "warden_pty_events_total"
	MTransitions     = "warden_pty_state_transitions_total"
	MDetectLatency   = "warden_pty_state_detection_latency_seconds"
	MInvestigations  = "warden_pty_quiescence_investigations_total"
	MPrompts         = "warden_pty_prompts_total"
	MUnrecognized    = "warden_pty_unrecognized_prompts_total"
	MPromptConf      = "warden_pty_prompt_confidence"
	MFallback        = "warden_pty_fallback_total"
	MReconnects      = "warden_pty_reconnects_total"
	MBackpressure    = "warden_pty_backpressure_total"
	MLearnedShape    = "warden_pty_learned_shape_total"
	MDecisions       = "warden_pty_decisions_total"
	MDecisionLatency = "warden_pty_decision_latency_seconds"
	MEscalations     = "warden_pty_escalations_total"
	MRedactions      = "warden_pty_redactions_total"
	MTraceRecords    = "warden_pty_decision_trace_records_total"
	MTraceErrors     = "warden_pty_decision_trace_errors_total"
	MSessions        = "warden_pty_sessions"
)

// Closed label vocabularies. Anything outside collapses to "other".
var (
	eventKinds = []string{"output", "input", "resize", "lifecycle", "state", "prompt", "decision", "marker", "hook", "other"}
	states     = []string{"unknown", "working", "idle", "waiting", "waiting_for_permission", "waiting_for_decision", "needs_attention", "exited", "other"}
	reasons    = []string{"", "startup", "rate_limit", "quota", "auth", "network", "subprocess", "other"}
	invResults = []string{"transition", "no_change", "prompt", "unreadable", "exited"}
	classes    = []string{"permission", "decision", "unknown", "informational"}
	provs      = []string{"parser", "learned", "fastbrain", "none"}
	fallbacks  = []string{"shadow_disagreement", "legacy_poller", "tmux_input", "runtime_off", "parser_failed", "learned_unusable"}
	reconnects = []string{"resumed", "replayed", "reset", "refused"}
	pressure   = []string{"subscriber_cut", "gap", "dropped_output", "journal_trim"}
	shapeOut   = []string{"candidate", "validated", "pinned", "used", "retired", "rejected", "expired", "regression_failed"}
	postures   = []string{"read-only", "read-write", "allowlisted", "dangerously-execute-all", "unset"}
	actions    = []string{"approve", "deny", "ask_human", "route_decision", "needs_attention", "none"}
	routes     = []string{"posture", "brain", "human", "none"}
	escReasons = []string{"unrecognized", "low_confidence", "unsafe_format", "act_disabled", "breaker", "arbiter", "revalidation", "arbitration", "strategic", "brain_failed", "attempts_exhausted", "ambiguous", "other"}
	redKinds   = []string{"anthropic_key", "openai_key", "github_token", "aws_access_key", "google_api_key", "slack_token", "jwt", "bearer_token", "url_credential", "secret_assignment", "private_key", "typed_input", "other"}
)

var (
	adapterRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	versionRE = regexp.MustCompile(`(?:^|[^0-9A-Za-z.])(\d{1,4}\.\d{1,4}(?:\.\d{1,4})?)(?:$|[^0-9A-Za-z])`)
)

type norm func(string) string

func enum(vals []string) norm {
	set := make(map[string]struct{}, len(vals))
	for _, v := range vals {
		set[v] = struct{}{}
	}
	return func(s string) string {
		if _, ok := set[s]; ok {
			return s
		}
		return "other"
	}
}

func adapterNorm(s string) string {
	if adapterRE.MatchString(s) {
		return s
	}
	if s == "" {
		return "unknown"
	}
	return "other"
}

// versionNorm keeps only a leading major.minor[.patch] number from `--version`
// text: no free text ever becomes a label value.
func versionNorm(s string) string {
	if m := versionRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return "unknown"
}

type labelSpec struct {
	name string
	norm norm
}

type family struct {
	name, help, typ string
	labels          []labelSpec
	buckets         []float64
	mu              sync.Mutex
	series          map[string]*seriesData
}

type seriesData struct {
	labels []string
	value  float64
	counts []uint64
	sum    float64
	n      uint64
}

func (f *family) key(vals []string) (string, []string) {
	out := make([]string, len(f.labels))
	for i, l := range f.labels {
		v := ""
		if i < len(vals) {
			v = vals[i]
		}
		out[i] = l.norm(v)
	}
	return strings.Join(out, "\x00"), out
}

func (f *family) get(vals []string) *seriesData {
	k, lv := f.key(vals)
	s, ok := f.series[k]
	if !ok {
		s = &seriesData{labels: lv}
		if f.typ == "histogram" {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[k] = s
	}
	return s
}

// Metrics is the PTY observability registry. A nil *Metrics is a valid no-op.
type Metrics struct {
	mu       sync.RWMutex
	fams     map[string]*family
	order    []string
	gaugeFns []func() map[string]float64
}

var latencyBuckets = []float64{0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}
var confBuckets = []float64{0.1, 0.3, 0.5, 0.7, 0.8, 0.9, 0.95, 1}

// NewMetrics builds the registry with every documented family declared.
func NewMetrics() *Metrics {
	m := &Metrics{fams: map[string]*family{}}
	ad := labelSpec{"adapter", adapterNorm}
	ver := labelSpec{"version", versionNorm}
	m.decl(MEvents, "Journaled PTY events by kind.", "counter", nil, labelSpec{"kind", enum(eventKinds)})
	m.decl(MTransitions, "Committed state transitions.", "counter", nil, ad, labelSpec{"from", enum(states)}, labelSpec{"to", enum(states)}, labelSpec{"reason", enum(reasons)})
	m.decl(MDetectLatency, "Seconds from last terminal activity to a committed non-working state.", "histogram", latencyBuckets, ad, labelSpec{"to", enum(states)})
	m.decl(MInvestigations, "Quiescence investigations by result.", "counter", nil, ad, labelSpec{"result", enum(invResults)})
	m.decl(MPrompts, "Prompts recognized or seen on screen.", "counter", nil, ad, ver, labelSpec{"classification", enum(classes)}, labelSpec{"provenance", enum(provs)})
	m.decl(MUnrecognized, "Blocking screens no parser or learned shape could classify.", "counter", nil, ad, ver, labelSpec{"classification", enum(classes)})
	m.decl(MPromptConf, "Classification confidence of recognized prompts.", "histogram", confBuckets, ad, labelSpec{"provenance", enum(provs)})
	m.decl(MFallback, "Fallbacks away from the authoritative event path.", "counter", nil, ad, labelSpec{"kind", enum(fallbacks)})
	m.decl(MReconnects, "Stream reconnects by resume result.", "counter", nil, labelSpec{"result", enum(reconnects)})
	m.decl(MBackpressure, "Backpressure events (cut subscribers, journaled gaps, drops).", "counter", nil, labelSpec{"kind", enum(pressure)})
	m.decl(MLearnedShape, "Learned prompt shape lifecycle outcomes.", "counter", nil, ad, labelSpec{"outcome", enum(shapeOut)})
	m.decl(MDecisions, "Auto-approve attempts and their outcomes.", "counter", nil, ad, labelSpec{"posture", enum(postures)}, labelSpec{"action", enum(actions)}, labelSpec{"outcome", enum(outcomeNames())}, labelSpec{"route", enum(routes)})
	m.decl(MDecisionLatency, "Seconds from prompt first seen to decision outcome.", "histogram", latencyBuckets, ad, labelSpec{"outcome", enum(outcomeNames())})
	m.decl(MEscalations, "Prompts escalated to a person or needs_attention, by reason.", "counter", nil, ad, labelSpec{"reason", enum(escReasons)})
	m.decl(MRedactions, "Secret redactions applied in the PTY pipeline, by kind.", "counter", nil, labelSpec{"kind", enum(redKinds)})
	m.decl(MTraceRecords, "Decision-trace records appended.", "counter", nil)
	m.decl(MTraceErrors, "Decision-trace appends that failed.", "counter", nil)
	m.decl(MSessions, "Live PTY-supervised sessions by classifier state.", "gauge", nil, labelSpec{"state", enum(states)})
	return m
}

func outcomeNames() []string {
	return []string{"approved", "denied", "dry_run", "deferred", "escalated_human", "needs_attention", "routed_brain", "error"}
}

func (m *Metrics) decl(name, help, typ string, buckets []float64, labels ...labelSpec) {
	m.fams[name] = &family{name: name, help: help, typ: typ, labels: labels, buckets: buckets, series: map[string]*seriesData{}}
	m.order = append(m.order, name)
}

// GaugeFunc registers a callback that supplies MSessions values (state→count)
// at scrape time.
func (m *Metrics) GaugeFunc(f func() map[string]float64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.gaugeFns = append(m.gaugeFns, f)
	m.mu.Unlock()
}

// Inc adds 1 to a counter series.
func (m *Metrics) Inc(name string, labels ...string) { m.Add(name, 1, labels...) }

// Add adds v to a counter series.
func (m *Metrics) Add(name string, v float64, labels ...string) {
	f := m.family(name)
	if f == nil {
		return
	}
	f.mu.Lock()
	f.get(labels).value += v
	f.mu.Unlock()
}

// Observe records one histogram sample.
func (m *Metrics) Observe(name string, v float64, labels ...string) {
	f := m.family(name)
	if f == nil || f.typ != "histogram" {
		return
	}
	f.mu.Lock()
	s := f.get(labels)
	s.n++
	s.sum += v
	for i, ub := range f.buckets {
		if v <= ub {
			s.counts[i]++
		}
	}
	f.mu.Unlock()
}

// ObserveDuration records d in seconds.
func (m *Metrics) ObserveDuration(name string, d time.Duration, labels ...string) {
	m.Observe(name, d.Seconds(), labels...)
}

func (m *Metrics) family(name string) *family {
	if m == nil {
		return nil
	}
	return m.fams[name]
}

// Bucket is one cumulative histogram bucket.
type Bucket struct {
	LE    float64 `json:"le"`
	Count uint64  `json:"count"`
}

// Series is one labelled sample set.
type Series struct {
	Labels  map[string]string `json:"labels,omitempty"`
	Value   float64           `json:"value"`
	Count   uint64            `json:"count,omitempty"`
	Sum     float64           `json:"sum,omitempty"`
	Buckets []Bucket          `json:"buckets,omitempty"`
}

// Family is one metric family in a snapshot.
type Family struct {
	Name   string   `json:"name"`
	Help   string   `json:"help"`
	Type   string   `json:"type"`
	Series []Series `json:"series"`
}

// Snapshot is a point-in-time copy of every family, sorted for stable output.
type Snapshot struct {
	Now      time.Time `json:"now"`
	Families []Family  `json:"families"`
}

// Snapshot copies the registry (running gauge callbacks first).
func (m *Metrics) Snapshot() Snapshot {
	snap := Snapshot{Now: time.Now().UTC(), Families: []Family{}}
	if m == nil {
		return snap
	}
	m.mu.RLock()
	fns := append([]func() map[string]float64(nil), m.gaugeFns...)
	m.mu.RUnlock()
	if g := m.fams[MSessions]; g != nil {
		g.mu.Lock()
		g.series = map[string]*seriesData{}
		for _, fn := range fns {
			for st, n := range fn() {
				g.get([]string{st}).value += n
			}
		}
		g.mu.Unlock()
	}
	for _, name := range m.order {
		f := m.fams[name]
		f.mu.Lock()
		fam := Family{Name: f.name, Help: f.help, Type: f.typ, Series: []Series{}}
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sd := f.series[k]
			s := Series{Labels: map[string]string{}, Value: sd.value}
			for i, l := range f.labels {
				s.Labels[l.name] = sd.labels[i]
			}
			if f.typ == "histogram" {
				s.Count, s.Sum = sd.n, sd.sum
				for i, ub := range f.buckets {
					s.Buckets = append(s.Buckets, Bucket{LE: ub, Count: sd.counts[i]})
				}
			}
			fam.Series = append(fam.Series, s)
		}
		f.mu.Unlock()
		snap.Families = append(snap.Families, fam)
	}
	return snap
}

// WritePrometheus renders the snapshot in the Prometheus text format.
func (s Snapshot) WritePrometheus(w io.Writer) error {
	for _, f := range s.Families {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.Name, f.Help, f.Name, f.Type); err != nil {
			return err
		}
		for _, se := range f.Series {
			lbl := labelString(se.Labels, "")
			if f.Type != "histogram" {
				if _, err := fmt.Fprintf(w, "%s%s %g\n", f.Name, lbl, se.Value); err != nil {
					return err
				}
				continue
			}
			for _, b := range se.Buckets {
				fmt.Fprintf(w, "%s_bucket%s %d\n", f.Name, labelString(se.Labels, fmt.Sprintf("%g", b.LE)), b.Count)
			}
			fmt.Fprintf(w, "%s_bucket%s %d\n", f.Name, labelString(se.Labels, "+Inf"), se.Count)
			fmt.Fprintf(w, "%s_sum%s %g\n%s_count%s %d\n", f.Name, lbl, se.Sum, f.Name, lbl, se.Count)
		}
	}
	return nil
}

func labelString(l map[string]string, le string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf(`%s=%q`, k, l[k]))
	}
	if le != "" {
		parts = append(parts, fmt.Sprintf(`le=%q`, le))
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Total sums a counter/gauge family's series whose labels contain every given
// key/value pair (counts for histograms), e.g. Total(MEvents, "kind", "output").
func (s Snapshot) Total(name string, kv ...string) float64 {
	var sum float64
	for _, f := range s.Families {
		if f.Name != name {
			continue
		}
	series:
		for _, se := range f.Series {
			for i := 0; i+1 < len(kv); i += 2 {
				if se.Labels[kv[i]] != kv[i+1] {
					continue series
				}
			}
			if f.Type == "histogram" {
				sum += float64(se.Count)
			} else {
				sum += se.Value
			}
		}
	}
	return sum
}
