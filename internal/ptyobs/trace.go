package ptyobs

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// TraceSchema is the record schema version.
const TraceSchema = 1

// Outcome is the terminal result of one auto-approve attempt.
type Outcome string

const (
	OutcomeApproved    Outcome = "approved"        // option delivered, grant
	OutcomeDenied      Outcome = "denied"          // option delivered, refusal
	OutcomeDryRun      Outcome = "dry_run"         // shadow: decided, not delivered
	OutcomeDeferred    Outcome = "deferred"        // revalidation/arbitration stopped delivery; will re-decide
	OutcomeEscalated   Outcome = "escalated_human" // surfaced for a person
	OutcomeAttention   Outcome = "needs_attention" // unrecognized / unsafe / ambiguous
	OutcomeRoutedBrain Outcome = "routed_brain"    // autopilot strategic decision answered by the brain
	OutcomeError       Outcome = "error"           // delivery failed
)

// Valid reports whether o is a known outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeApproved, OutcomeDenied, OutcomeDryRun, OutcomeDeferred, OutcomeEscalated,
		OutcomeAttention, OutcomeRoutedBrain, OutcomeError:
		return true
	}
	return false
}

// Route is who resolved (or owns) the prompt.
type Route string

const (
	RoutePosture Route = "posture" // Warden's permission engine
	RouteBrain   Route = "brain"   // Tier-1 autopilot manager
	RouteHuman   Route = "human"   // operator inbox
	RouteNone    Route = "none"    // unresolvable → needs_attention
)

// TraceRecord is one append-only decision-trace entry. Every free-text field
// is redacted before it is stored; raw keystrokes are never recorded.
type TraceRecord struct {
	Schema    int       `json:"schema"`
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	SessionID string    `json:"session_id"`
	AgentID   string    `json:"agent_id,omitempty"`

	// CorrelationID joins this record to the journal's DecisionEvent (`dec-…`).
	CorrelationID string `json:"correlation_id,omitempty"`
	// EventSeq is the journal sequence the decision was computed against.
	EventSeq uint64 `json:"event_seq,omitempty"`

	Fingerprint  string `json:"fingerprint,omitempty"`
	ShapeVersion int    `json:"shape_version,omitempty"`
	InstanceKey  string `json:"instance_key,omitempty"`
	Adapter      string `json:"adapter,omitempty"`
	CLIVersion   string `json:"cli_version,omitempty"`

	Class          string  `json:"class,omitempty"`
	Provenance     string  `json:"provenance,omitempty"`
	Confidence     float64 `json:"confidence,omitempty"`
	LearnedShapeID string  `json:"learned_shape_id,omitempty"`

	// Permission is the extracted capability/scope/options with the chosen one.
	Permission PermissionSummary `json:"permission"`

	Posture string `json:"posture,omitempty"`
	Danger  bool   `json:"danger,omitempty"`
	// Policy is the effective policy that produced the action: the decision
	// rule, whether Warden is allowed to act, and any allowlist/breaker note.
	Policy TracePolicy `json:"policy"`

	Action      string `json:"action,omitempty"`
	OptionIndex int    `json:"option_index,omitempty"`
	// Input describes the selected input (never the raw bytes).
	Input string `json:"input,omitempty"`

	Revalidation string  `json:"revalidation,omitempty"`
	Arbitration  string  `json:"arbitration,omitempty"`
	Outcome      Outcome `json:"outcome"`
	Route        Route   `json:"route,omitempty"`
	// Reason is the rejection/escalation/decision reason.
	Reason    string `json:"reason,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

// TracePolicy is the effective policy behind an action.
type TracePolicy struct {
	Rule      string `json:"rule,omitempty"`
	Act       bool   `json:"act"`
	Autopilot bool   `json:"autopilot,omitempty"`
	Downgrade string `json:"downgrade,omitempty"`
}

// TraceConfig bounds the trace. Zero values take the defaults.
type TraceConfig struct {
	// MaxRecords is the per-session retention cap (default 5000).
	MaxRecords int
	// MaxAge drops older records at compaction (default 30 days; <0 disables).
	MaxAge time.Duration
	Clock  func() time.Time
}

func (c *TraceConfig) defaults() {
	if c.MaxRecords <= 0 {
		c.MaxRecords = 5000
	}
	if c.MaxAge == 0 {
		c.MaxAge = 30 * 24 * time.Hour
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Trace is the per-session append-only decision trace store: one JSONL file
// per session. Appends never rewrite; retention compacts a file by atomic
// rename only once it exceeds the cap by 25%.
type Trace struct {
	dir string
	cfg TraceConfig

	mu     sync.Mutex
	counts map[string]int
	seq    uint64

	// Errors counts failed appends (surfaced as a metric).
	errs func()
}

// OpenTrace opens (creating) the trace directory.
func OpenTrace(dir string, cfg TraceConfig) (*Trace, error) {
	cfg.defaults()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Trace{dir: dir, cfg: cfg, counts: map[string]int{}}, nil
}

// SetLimits changes retention live (zero/negative keeps the current value).
func (t *Trace) SetLimits(maxRecords int, maxAge time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if maxRecords > 0 {
		t.cfg.MaxRecords = maxRecords
	}
	if maxAge > 0 {
		t.cfg.MaxAge = maxAge
	}
}

// OnError registers a callback for failed appends.
func (t *Trace) OnError(f func()) { t.errs = f }

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,96}$`)

func (t *Trace) path(session string) string {
	name := session
	if !safeID.MatchString(session) || strings.HasPrefix(session, ".") {
		h := sha256.Sum256([]byte(session))
		name = "s-" + hex.EncodeToString(h[:8])
	}
	return filepath.Join(t.dir, name+".jsonl")
}

// Append redacts and appends r. It never returns the record's raw secrets;
// an error means the record was not stored.
func (t *Trace) Append(r TraceRecord) error {
	if r.SessionID == "" {
		return errors.New("ptyobs: trace record has no session")
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("ptyobs: invalid outcome %q", r.Outcome)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	r.Schema = TraceSchema
	if r.Time.IsZero() {
		r.Time = t.cfg.Clock()
	}
	r.Time = r.Time.UTC()
	if r.ID == "" {
		r.ID = fmt.Sprintf("tr-%d-%d", r.Time.UnixNano(), t.seq)
	}
	r.Reason = clean(r.Reason, 400)
	r.Policy.Rule = clean(r.Policy.Rule, 120)
	r.Policy.Downgrade = clean(r.Policy.Downgrade, 120)
	r.Input = clean(r.Input, 120)
	r.CLIVersion = clean(r.CLIVersion, 64)
	b, err := json.Marshal(r)
	if err != nil {
		return t.fail(err)
	}
	p := t.path(r.SessionID)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return t.fail(err)
	}
	_, werr := f.Write(append(b, '\n'))
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return t.fail(errors.Join(werr, cerr))
	}
	n, ok := t.counts[p]
	if !ok {
		n = t.countLines(p)
	} else {
		n++
	}
	t.counts[p] = n
	if n > t.cfg.MaxRecords+t.cfg.MaxRecords/4 {
		t.compactLocked(p)
	}
	return nil
}

func (t *Trace) fail(err error) error {
	if t.errs != nil {
		t.errs()
	}
	return err
}

func (t *Trace) countLines(p string) int {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		n++
	}
	_ = sc.Err() // an over-long line only under-counts; compaction re-reads and checks
	return n
}

// compactLocked applies retention: keep the newest MaxRecords within MaxAge.
func (t *Trace) compactLocked(p string) {
	recs, err := readFileErr(p)
	if err != nil {
		t.fail(err) // keep the file as is rather than truncate it from a partial read
		return
	}
	if t.cfg.MaxAge > 0 {
		cut := t.cfg.Clock().Add(-t.cfg.MaxAge)
		kept := recs[:0]
		for _, r := range recs {
			if !r.Time.Before(cut) {
				kept = append(kept, r)
			}
		}
		recs = kept
	}
	if len(recs) > t.cfg.MaxRecords {
		recs = recs[len(recs)-t.cfg.MaxRecords:]
	}
	var sb strings.Builder
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return
	}
	t.counts[p] = len(recs)
}

// Query filters a session's trace. Zero fields match everything.
type Query struct {
	Since       time.Time
	Until       time.Time
	Outcome     Outcome
	Class       string
	Fingerprint string
	Correlation string
	// Limit caps the result (newest first). Default 100, max 1000.
	Limit int
}

// Query returns the session's records, newest first. A corrupt line is
// skipped (the file is append-only; a torn tail must not hide history).
func (t *Trace) Query(session string, q Query) []TraceRecord {
	t.mu.Lock()
	recs := readFile(t.path(session))
	t.mu.Unlock()
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	out := make([]TraceRecord, 0, limit)
	for i := len(recs) - 1; i >= 0 && len(out) < limit; i-- {
		r := recs[i]
		if r.SessionID != session {
			continue
		}
		switch {
		case !q.Since.IsZero() && r.Time.Before(q.Since):
		case !q.Until.IsZero() && r.Time.After(q.Until):
		case q.Outcome != "" && r.Outcome != q.Outcome:
		case q.Class != "" && r.Class != q.Class:
		case q.Fingerprint != "" && r.Fingerprint != q.Fingerprint:
		case q.Correlation != "" && r.CorrelationID != q.Correlation:
		default:
			out = append(out, r)
		}
	}
	return out
}

// Sessions lists sessions that have a trace file (hashed names are returned
// from the records inside).
func (t *Trace) Sessions() []string {
	ents, err := os.ReadDir(t.dir)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		recs := readFile(filepath.Join(t.dir, e.Name()))
		if len(recs) > 0 {
			seen[recs[len(recs)-1].SessionID] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Delete removes a session's trace (agent deletion).
func (t *Trace) Delete(session string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.path(session)
	delete(t.counts, p)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readFile(p string) []TraceRecord {
	recs, _ := readFileErr(p)
	return recs
}

// readFileErr is readFile plus whether the scan stopped early (an unreadable or
// over-long line), so compaction never rewrites a file from a partial read.
func readFileErr(p string) ([]TraceRecord, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []TraceRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r TraceRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
