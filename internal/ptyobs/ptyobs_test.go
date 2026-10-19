package ptyobs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

// token is built at runtime so no token-shaped literal lives in the repo.
func token() string { return "ghp_" + strings.Repeat("a1B2", 9) }

func testPrompt() *ptyevent.Prompt {
	return &ptyevent.Prompt{
		Fingerprint: "fp1", Class: ptyevent.ClassPermission, Capabilities: []ptyevent.Capability{ptyevent.CapExecute},
		Subject: "Bash", Target: "curl -H 'Authorization: " + token() + "' x", Provenance: ptyevent.ProvParser, Confidence: 0.97,
		Options: []ptyevent.Option{
			{Index: 1, Label: "Yes", Semantic: ptyevent.SemAllowOnce, Default: true},
			{Index: 2, Label: "No", Semantic: ptyevent.SemDeny},
		},
	}
}

func TestSummarizeRedactsAndDescribesChoice(t *testing.T) {
	s := Summarize(testPrompt(), 1)
	if strings.Contains(s.Target, token()) || strings.Contains(s.Text, token()) {
		t.Fatalf("secret leaked: %+v", s)
	}
	if !s.Options[0].Chosen || s.Options[1].Chosen {
		t.Fatalf("chosen flag wrong: %+v", s.Options)
	}
	for _, want := range []string{"execute permission", "Bash", "chose 1", "allow_once"} {
		if !strings.Contains(s.Text, want) {
			t.Errorf("summary %q missing %q", s.Text, want)
		}
	}
	if Summarize(nil, 0).Text != "" {
		t.Error("nil prompt should summarize empty")
	}
	d := Summarize(&ptyevent.Prompt{Class: ptyevent.ClassDecision, Question: "Which approach?"}, 0)
	if !strings.HasPrefix(d.Text, "strategic decision") {
		t.Errorf("decision summary = %q", d.Text)
	}
}

func TestDescribeInputNeverEchoesBytes(t *testing.T) {
	if got := DescribeInput([]byte("2\r"), 2); got != "option 2 + Enter" {
		t.Errorf("got %q", got)
	}
	if got := DescribeInput([]byte("\x1b[B\x1b[B\r"), 3); !strings.Contains(got, "navigate 2") {
		t.Errorf("got %q", got)
	}
	if DescribeInput(nil, 1) != "" {
		t.Error("empty input must describe empty")
	}
}

func newTrace(t *testing.T, cfg TraceConfig) *Trace {
	t.Helper()
	tr, err := OpenTrace(filepath.Join(t.TempDir(), "trace"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func rec(session string, o Outcome, at time.Time) TraceRecord {
	return TraceRecord{SessionID: session, Time: at, Outcome: o, Fingerprint: "fp", Class: "permission",
		Reason: "because " + token(), Input: "option 1 + Enter"}
}

func TestTraceAppendRedactsAndQueries(t *testing.T) {
	tr := newTrace(t, TraceConfig{})
	now := time.Now()
	for i, o := range []Outcome{OutcomeApproved, OutcomeEscalated, OutcomeAttention} {
		if err := tr.Append(rec("a1", o, now.Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(tr.path("a1"))
	if bytes.Contains(raw, []byte(token())) {
		t.Fatal("secret persisted in trace file")
	}
	got := tr.Query("a1", Query{})
	if len(got) != 3 || got[0].Outcome != OutcomeAttention {
		t.Fatalf("want newest-first 3, got %+v", got)
	}
	if n := len(tr.Query("a1", Query{Outcome: OutcomeEscalated})); n != 1 {
		t.Errorf("outcome filter = %d", n)
	}
	if n := len(tr.Query("a1", Query{Since: now.Add(1500 * time.Millisecond)})); n != 1 {
		t.Errorf("since filter = %d", n)
	}
	if n := len(tr.Query("a1", Query{Limit: 2})); n != 2 {
		t.Errorf("limit = %d", n)
	}
	if n := len(tr.Query("other", Query{})); n != 0 {
		t.Errorf("sessions leaked into each other: %d", n)
	}
	if err := tr.Append(TraceRecord{SessionID: "a1", Outcome: "bogus"}); err == nil {
		t.Error("invalid outcome accepted")
	}
	if err := tr.Append(TraceRecord{Outcome: OutcomeApproved}); err == nil {
		t.Error("missing session accepted")
	}
}

func TestTraceUnsafeSessionNameStaysInsideDir(t *testing.T) {
	tr := newTrace(t, TraceConfig{})
	evil := "../../etc/passwd"
	if err := tr.Append(rec(evil, OutcomeApproved, time.Now())); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tr.path(evil), tr.dir) || filepath.Dir(tr.path(evil)) != tr.dir {
		t.Fatalf("path escaped: %s", tr.path(evil))
	}
	if len(tr.Query(evil, Query{})) != 1 {
		t.Error("record not retrievable by original id")
	}
	if got := tr.Sessions(); len(got) != 1 || got[0] != evil {
		t.Errorf("sessions = %v", got)
	}
}

func TestTraceRetentionCompacts(t *testing.T) {
	tr := newTrace(t, TraceConfig{MaxRecords: 8, MaxAge: -1})
	now := time.Now()
	for i := 0; i < 40; i++ {
		r := rec("s", OutcomeApproved, now.Add(time.Duration(i)*time.Second))
		r.CorrelationID = fmt.Sprintf("dec-%d", i)
		if err := tr.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	got := tr.Query("s", Query{Limit: 1000})
	if len(got) > 10 || len(got) < 8 {
		t.Fatalf("retained %d, want 8..10", len(got))
	}
	if got[0].CorrelationID != "dec-39" {
		t.Errorf("newest lost: %s", got[0].CorrelationID)
	}
}

func TestTraceAgeRetention(t *testing.T) {
	clock := time.Now()
	tr := newTrace(t, TraceConfig{MaxRecords: 4, MaxAge: time.Hour, Clock: func() time.Time { return clock }})
	for i := 0; i < 3; i++ {
		_ = tr.Append(rec("s", OutcomeApproved, clock.Add(-48*time.Hour)))
	}
	for i := 0; i < 3; i++ {
		_ = tr.Append(rec("s", OutcomeApproved, clock))
	}
	if n := len(tr.Query("s", Query{})); n != 3 {
		t.Fatalf("aged records kept: %d", n)
	}
}

func TestTraceToleratesTornTailAndConcurrency(t *testing.T) {
	tr := newTrace(t, TraceConfig{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = tr.Append(rec("c", OutcomeApproved, time.Now()))
				_ = tr.Query("c", Query{Limit: 5})
			}
		}()
	}
	wg.Wait()
	f, _ := os.OpenFile(tr.path("c"), os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"id":"torn","outcome":"appr`)
	_ = f.Close()
	if n := len(tr.Query("c", Query{Limit: 1000})); n != 200 {
		t.Fatalf("lost records under concurrency / torn tail: %d", n)
	}
	if err := tr.Delete("c"); err != nil || len(tr.Query("c", Query{})) != 0 {
		t.Fatal("delete failed")
	}
}

func TestMetricsLabelsAreBounded(t *testing.T) {
	m := NewMetrics()
	secret := token()
	m.Inc(MUnrecognized, "antigravity", "v"+secret+" 1.2.3", "permission")
	m.Inc(MUnrecognized, "NOT a valid adapter "+secret, "garbage "+secret, secret)
	m.Inc(MPrompts, "claude", "claude-code 2.0.31 (build x)", "decision", "parser")
	m.Inc(MRedactions, "github_token")
	m.Inc(MRedactions, secret)
	m.Observe(MDecisionLatency, 0.2, "claude", "approved")
	var buf bytes.Buffer
	if err := m.Snapshot().WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, secret) || strings.Contains(out, "garbage") || strings.Contains(out, "build x") {
		t.Fatalf("free text became a label:\n%s", out)
	}
	for _, want := range []string{
		`warden_pty_unrecognized_prompts_total{adapter="antigravity",classification="permission",version="1.2.3"} 1`,
		`warden_pty_unrecognized_prompts_total{adapter="other",classification="other",version="unknown"} 1`,
		`warden_pty_prompts_total{adapter="claude",classification="decision",provenance="parser",version="2.0.31"} 1`,
		`warden_pty_redactions_total{kind="other"} 1`,
		`warden_pty_decision_latency_seconds_bucket{adapter="claude",outcome="approved",le="0.25"} 1`,
		`warden_pty_decision_latency_seconds_count{adapter="claude",outcome="approved"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestMetricsNilSafeAndGauges(t *testing.T) {
	var nilm *Metrics
	nilm.Inc(MEvents, "output")
	nilm.Observe(MPromptConf, 1)
	nilm.GaugeFunc(nil)
	if len(nilm.Snapshot().Families) != 0 {
		t.Error("nil snapshot should be empty")
	}
	m := NewMetrics()
	m.GaugeFunc(func() map[string]float64 { return map[string]float64{"working": 2, "idle": 1} })
	m.Inc("not_a_metric")
	for _, f := range m.Snapshot().Families {
		if f.Name == MSessions && len(f.Series) != 2 {
			t.Errorf("gauge series = %+v", f.Series)
		}
	}
}

func TestMetricsRace(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				m.Inc(MEvents, "output")
				m.Observe(MDetectLatency, float64(i)/100, "claude", "idle")
				_ = m.Snapshot()
			}
		}()
	}
	wg.Wait()
	for _, f := range m.Snapshot().Families {
		if f.Name == MEvents && f.Series[0].Value != 1600 {
			t.Fatalf("lost increments: %v", f.Series[0].Value)
		}
	}
}
