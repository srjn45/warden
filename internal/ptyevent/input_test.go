package ptyevent

import (
	"testing"
	"time"
)

// TestInputOwnership walks a concurrent human/Warden timeline step by step:
// each step is one write attempt against the lease left by the previous ones.
func TestInputOwnership(t *testing.T) {
	cfg := DefaultOwnership
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	type step struct {
		name    string
		req     InputRequest
		want    Verdict
		preempt bool
		release bool // the automatic write completed (otherwise still in flight)
	}
	steps := []step{
		{"warden answers on a free line", InputRequest{Origin: OriginWarden, DecisionID: "d1", At: at(0), Seq: 1, Data: []byte("1")}, VerdictWrite, false, false},
		{"human types while warden holds: human wins and preempts", InputRequest{Origin: OriginHuman, ClientID: "web", At: at(100), Seq: 2, Data: []byte("h")}, VerdictWrite, true, false},
		{"warden deferred inside the quiet window", InputRequest{Origin: OriginWarden, DecisionID: "d2", At: at(1000), Seq: 3, Data: []byte("1")}, VerdictDefer, false, false},
		{"operator also yields to the human", InputRequest{Origin: OriginOperator, At: at(1500), Seq: 3, Data: []byte("hi\r")}, VerdictDefer, false, false},
		{"half-typed line keeps the lease past the quiet window", InputRequest{Origin: OriginWarden, DecisionID: "d2", At: at(10_000), FirstDeferredAt: at(1000), Seq: 3, Data: []byte("1")}, VerdictDefer, false, false},
		{"warden gives up after max defer", InputRequest{Origin: OriginWarden, DecisionID: "d2", At: at(32_000), FirstDeferredAt: at(1000), Seq: 3, Data: []byte("1")}, VerdictAbandon, false, false},
		{"human submits the line", InputRequest{Origin: OriginHuman, ClientID: "web", At: at(33_000), Seq: 4, Data: []byte("ello\r")}, VerdictWrite, false, false},
		{"still quiet window after submit", InputRequest{Origin: OriginWarden, DecisionID: "d3", At: at(34_000), Seq: 5, Data: []byte("1")}, VerdictDefer, false, false},
		{"line free once quiet and submitted", InputRequest{Origin: OriginWarden, DecisionID: "d3", At: at(36_500), Seq: 5, Data: []byte("1")}, VerdictWrite, false, true},
		{"warden without a decision is rejected", InputRequest{Origin: OriginWarden, At: at(40_000), Seq: 6}, VerdictReject, false, false},
		{"warden seed with a reason is allowed", InputRequest{Origin: OriginWarden, Reason: "seed", At: at(40_000), Seq: 6, Data: []byte("task\r")}, VerdictWrite, false, false},
		{"unknown origin rejected", InputRequest{Origin: "ghost", At: at(40_000)}, VerdictReject, false, false},
	}
	var l Lease
	for _, s := range steps {
		var a Arbitration
		l, a = Arbitrate(l, s.req, cfg)
		if a.Verdict != s.want || a.PreemptsWarden != s.preempt {
			t.Fatalf("%s: verdict=%s preempt=%v (%s), want %s preempt=%v", s.name, a.Verdict, a.PreemptsWarden, a.Reason, s.want, s.preempt)
		}
		if a.Verdict == VerdictDefer && a.RetryAfter <= 0 {
			t.Fatalf("%s: defer without RetryAfter", s.name)
		}
		if s.release {
			l = Release(l, s.req.Origin)
			if l.Holder != "" {
				t.Fatalf("%s: release left holder %s", s.name, l.Holder)
			}
		}
	}
	if l.HumanSeq != 4 {
		t.Fatalf("HumanSeq = %d, want 4 (feeds TOCTOU revalidation)", l.HumanSeq)
	}
}

func TestHumanLineOpen(t *testing.T) {
	cases := []struct {
		name string
		open bool
		data string
		want bool
	}{
		{"typing opens", false, "ab", true},
		{"enter closes", true, "\r", false},
		{"ctrl-c clears", true, "\x03", false},
		{"ctrl-u clears", true, "\x15", false},
		{"lone escape clears", true, "\x1b", false},
		{"arrow key does not type", false, "\x1b[A", false},
		{"arrow key keeps open line open", true, "\x1b[D", true},
		{"paste with trailing newline closes", false, "git status\n", false},
	}
	for _, c := range cases {
		if got := humanLineOpen(c.open, []byte(c.data)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
