package lifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/ptyevent"
)

// authoredStubHost is a direct host that also exposes the shared input authority.
type authoredStubHost struct {
	directStubHost
	seqs []ptyevent.Sequence
}

func (h *authoredStubHost) AuthoredInput(_ context.Context, _ string, q ptyevent.Sequence) error {
	h.seqs = append(h.seqs, q)
	return nil
}

func TestDirectInputGoesThroughSharedAuthority(t *testing.T) {
	h := &authoredStubHost{directStubHost: directStubHost{plan: true, launched: true}}
	l := New(nil, nil)
	l.SetProcessHost(h)
	ctx := context.Background()

	// An operator send is one fenced unit (paste then submit), operator origin.
	if err := l.Input(ctx, "a1", "do it"); err != nil {
		t.Fatal(err)
	}
	q := h.seqs[0]
	if q.Origin != ptyevent.OriginOperator || len(q.Parts) != 2 || !strings.Contains(string(q.Parts[0].Data), "do it") || string(q.Parts[1].Data) != "\r" {
		t.Fatalf("operator sequence = %+v", q)
	}
	if len(h.sent) != 0 {
		t.Fatalf("direct input bypassed the authority via SendKeys: %v", h.sent)
	}

	// Automatic input carries its origin, reason and once-key through.
	if err := l.InputAs(ctx, "a1", "continue", InputAuthority{Origin: ptyevent.OriginWarden, Reason: "resume", OnceKey: "resume:1"}); err != nil {
		t.Fatal(err)
	}
	if q := h.seqs[1]; q.Origin != ptyevent.OriginWarden || q.Reason != "resume" || q.OnceKey != "resume:1" {
		t.Fatalf("warden sequence = %+v", q)
	}
	if err := l.SendKeysAs(ctx, "a1", "Enter", InputAuthority{Origin: ptyevent.OriginWarden, Reason: "resume"}); err != nil {
		t.Fatal(err)
	}
	if q := h.seqs[2]; q.Origin != ptyevent.OriginWarden || string(q.Parts[0].Data) != "\r" {
		t.Fatalf("keys sequence = %+v", q)
	}
}

// A runtime without the authority can still take an explicit operator send
// (legacy behaviour) but never automatic input.
func TestDirectAutomaticInputFailsClosedWithoutAuthority(t *testing.T) {
	h := &directStubHost{plan: true, launched: true}
	l := New(nil, nil)
	l.SetProcessHost(h)
	err := l.InputAs(context.Background(), "a1", "x", InputAuthority{Origin: ptyevent.OriginWarden, Reason: "seed"})
	if err == nil || len(h.sent) != 0 {
		t.Fatalf("automatic input without authority: err=%v sent=%v", err, h.sent)
	}
	if err := l.SendKeysAs(context.Background(), "a1", "2", InputAuthority{Origin: ptyevent.OriginWarden, Reason: "x"}); err == nil || len(h.sent) != 0 {
		t.Fatalf("automatic keys without authority: err=%v sent=%v", err, h.sent)
	}
}

// A direct agent is seeded on positive readiness only: no ready marker means no
// timing-guess typing, and nothing is sent.
func TestDirectSeedRefusesTimingGuess(t *testing.T) {
	h := &authoredStubHost{directStubHost: directStubHost{plan: true, launched: true}}
	l := New(nil, nil)
	l.SetProcessHost(h)
	if err := l.deliverSeed("a1", "task", ""); err == nil {
		t.Fatal("markerless direct seed must fail closed")
	}
	if len(h.seqs) != 0 || len(h.sent) != 0 {
		t.Fatalf("seed typed without readiness evidence: %v %v", h.seqs, h.sent)
	}
}
