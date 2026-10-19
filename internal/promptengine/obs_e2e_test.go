package promptengine

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyobs"
)

// obsRig is a scripted rig with a real decision trace and metrics attached.
func obsRig(t *testing.T, posture ptyevent.Posture, act bool) (*rig, *ptyobs.Trace, *ptyobs.Metrics, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "trace")
	tr, err := ptyobs.OpenTrace(dir, ptyobs.TraceConfig{})
	if err != nil {
		t.Fatal(err)
	}
	m := ptyobs.NewMetrics()
	r := newRigCfg(t, posture, act, "/w", func(c *Config) { c.Trace, c.Metrics = tr, m })
	return r, tr, m, dir
}

func records(t *testing.T, tr *ptyobs.Trace) []ptyobs.TraceRecord {
	t.Helper()
	var all []ptyobs.TraceRecord
	for _, s := range tr.Sessions() {
		all = append(all, tr.Query(s, ptyobs.Query{Limit: 1000})...)
	}
	return all
}

// TestE2EReleaseGateTraceMetricsAndRedaction replays the deterministic fixture
// panes through the real classifier + engine and checks the audit surfaces:
// an approved permission, a drifted menu (escalated), a TOCTOU-stale prompt
// (rejected) each leave exactly the trace record, journaled decision and
// metrics they should; state transitions are counted; and a secret painted on
// screen reaches neither the trace file nor the Prometheus text.
func TestE2EReleaseGateTraceMetricsAndRedaction(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	withSecret := strings.ReplaceAll(cmdPane(t), "git rev-parse HEAD origin/main", "curl -H 'Authorization: "+secret+"' x")

	// 1. approved permission, with a secret in the command line.
	r, tr, m, dir := obsRig(t, ptyevent.PostureDangerouslyExecuteAll, true)
	r.look(withSecret)
	d := r.waitDecision(1)[0]
	if d.Action != ptyevent.ActionApprove {
		t.Fatalf("action = %s (%s)", d.Action, d.Reason)
	}
	r.settle()
	if got := r.w.all(); len(got) != 1 {
		t.Fatalf("keystrokes = %q", got)
	}
	recs := records(t, tr)
	if len(recs) == 0 {
		t.Fatal("approved decision left no trace record")
	}
	last := recs[0] // newest first
	if !last.Danger || last.Posture != string(ptyevent.PostureDangerouslyExecuteAll) {
		t.Errorf("trace lost posture/danger: %+v", last)
	}
	if last.Outcome != ptyobs.OutcomeApproved {
		t.Errorf("outcome = %q, want approved", last.Outcome)
	}
	if last.CorrelationID == "" || last.CorrelationID != d.ID {
		t.Errorf("trace correlation %q does not join journal decision %q", last.CorrelationID, d.ID)
	}

	// 2. a state transition and a decision were counted; no label leaks.
	snap := m.Snapshot()
	if snap.Total(ptyobs.MTransitions) == 0 {
		t.Error("no state transition counted")
	}
	if snap.Total(ptyobs.MDecisions) == 0 {
		t.Error("no decision counted")
	}
	var prom bytes.Buffer
	if err := snap.WritePrometheus(&prom); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secret, "curl", "Authorization", "/w"} {
		if strings.Contains(prom.String(), leak) {
			t.Errorf("metrics text leaked %q", leak)
		}
	}

	// 3. the trace on disk never holds the secret.
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("trace file %s holds the raw secret", p)
			}
		}
		return nil
	})

	// 4. drifted wording escalates and writes nothing.
	r2, tr2, _, _ := obsRig(t, ptyevent.PostureDangerouslyExecuteAll, true)
	drift := strings.NewReplacer("Yes, run command", "Go ahead", "No, cancel", "Stop").Replace(cmdPane(t))
	r2.look(drift)
	r2.waitDecision(1)
	r2.settle()
	if got := r2.w.all(); len(got) != 0 {
		t.Fatalf("drifted menu received %q", got)
	}
	for _, rec := range records(t, tr2) {
		if rec.Outcome == ptyobs.OutcomeApproved {
			t.Errorf("drifted menu traced as approved: %+v", rec)
		}
	}

	// 5. stale prompt (TOCTOU) is rejected and recorded as such.
	r3, tr3, m3, _ := obsRig(t, ptyevent.PostureDangerouslyExecuteAll, true)
	other := strings.ReplaceAll(cmdPane(t), "git rev-parse HEAD origin/main", "rm -rf /")
	r3.look(cmdPane(t), cmdPane(t), other)
	if d := r3.waitDecision(1)[0]; d.Revalidation != ptyevent.RevalTargetChanged {
		t.Fatalf("revalidation = %q", d.Revalidation)
	}
	r3.settle()
	if got := r3.w.all(); len(got) != 0 {
		t.Fatalf("stale prompt answered: %q", got)
	}
	var stale bool
	for _, rec := range records(t, tr3) {
		if rec.Revalidation != "" && rec.Outcome != ptyobs.OutcomeApproved {
			stale = true
		}
	}
	if !stale {
		t.Errorf("stale prompt left no non-approved trace record: %+v", records(t, tr3))
	}
	if m3.Snapshot().Total(ptyobs.MDecisions) == 0 {
		t.Error("rejected decision was not counted")
	}
}
