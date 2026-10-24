package ptyruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
)

func seqOwnership() Options {
	return Options{Ownership: ptyevent.OwnershipConfig{HumanQuiet: 50 * time.Millisecond, MaxDefer: 200 * time.Millisecond}}
}

func pasteSubmit(text string, gap time.Duration) []ptyevent.SeqPart {
	return []ptyevent.SeqPart{{Data: []byte(text), Gap: gap}, {Data: []byte("\n")}}
}

// A warden sequence (seed) lands as one unit and is journaled with its origin.
func TestSequenceWardenSeedLandsOnce(t *testing.T) {
	r := startFake(t, seqOwnership())
	r.waitOutput("ready")
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	q := ptyevent.Sequence{Origin: ptyevent.OriginWarden, Reason: "seed", OnceKey: "seed:a", Parts: pasteSubmit("say seeded", 10*time.Millisecond)}
	if err := r.s.WriteSequence(ctx, q); err != nil {
		t.Fatal(err)
	}
	r.waitOutput("seeded\r\n")
	// Replay (a retry, a second seeding path) is refused, never typed twice.
	if err := r.s.WriteSequence(ctx, q); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("replayed once-only sequence: %v, want ErrDuplicate", err)
	}
	n := 0
	for _, ev := range kinds(r.events(), ptyevent.KindInput) {
		if ev.Input.Origin == ptyevent.OriginWarden {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("warden-origin journaled writes = %d, want exactly the 2 parts of one sequence", n)
	}
}

// Human typing during the gap between paste and submit drops the submit: the
// Enter would otherwise submit the human's half-typed line.
func TestSequenceHumanRaceDropsRest(t *testing.T) {
	for _, origin := range []ptyevent.Origin{ptyevent.OriginWarden, ptyevent.OriginOperator} {
		t.Run(string(origin), func(t *testing.T) {
			r := startFake(t, seqOwnership())
			r.waitOutput("ready")
			ctx, cancel := context.WithTimeout(context.Background(), waitFor)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: origin, Reason: "x",
					Parts: pasteSubmit("say bot", 300*time.Millisecond)})
			}()
			time.Sleep(100 * time.Millisecond) // first part is in, gap is running
			if arb, _ := r.s.Input(Write{Origin: ptyevent.OriginHuman, ClientID: "c1", Data: []byte("say hu")}); arb.Verdict != ptyevent.VerdictWrite {
				t.Fatal(arb)
			}
			if err := <-done; !errors.Is(err, ErrPreempted) {
				t.Fatalf("sequence over a typing human: %v, want ErrPreempted", err)
			}
			for _, ev := range kinds(r.events(), ptyevent.KindInput) {
				if ev.Input.Origin != ptyevent.OriginHuman && string(ev.Input.Data) == "\n" {
					t.Fatal("submit written over the human's line")
				}
			}
		})
	}
}

// Concurrent automatic sequences must reserve their whole paste/submit unit:
// without the sequence gate the second paste can land in the first sequence's
// gap and the first Enter submits both lines together.
func TestSequenceAutomaticPartsDoNotInterleave(t *testing.T) {
	r := startFake(t, seqOwnership())
	r.waitOutput("ready")
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	first := make(chan error, 1)
	go func() {
		first <- r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: ptyevent.OriginOperator,
			Parts: pasteSubmit("say first", 200*time.Millisecond)})
	}()
	// Let the first paste land and remain in its deliberate gap before issuing
	// the second sequence. Its paste must wait until first's Enter is written.
	time.Sleep(60 * time.Millisecond)
	second := make(chan error, 1)
	go func() {
		second <- r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: ptyevent.OriginWarden, Reason: "seed",
			Parts: pasteSubmit("say second", 0)})
	}()
	if err := <-first; err != nil {
		t.Fatalf("first sequence: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second sequence: %v", err)
	}
	r.waitOutput("first\r\n")
	r.waitOutput("second\r\n")

	var automatic []string
	for _, ev := range kinds(r.events(), ptyevent.KindInput) {
		if ev.Input.Origin != ptyevent.OriginHuman {
			automatic = append(automatic, string(ev.Input.Data))
		}
	}
	want := []string{"say first", "\n", "say second", "\n"}
	if len(automatic) != len(want) {
		t.Fatalf("automatic inputs = %#v, want %#v", automatic, want)
	}
	for i := range want {
		if automatic[i] != want[i] {
			t.Fatalf("automatic inputs = %#v, want %#v", automatic, want)
		}
	}
}

// A warden decision records the human fence when it is made, before waiting
// behind another automatic sequence.  A human keystroke while it waits makes
// that decision stale; it must not be re-based to the later queue time.
func TestSequenceQueuedWardenUsesDecisionTimeHumanFence(t *testing.T) {
	r := startFake(t, seqOwnership())
	r.waitOutput("ready")
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	first := make(chan error, 1)
	go func() {
		first <- r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: ptyevent.OriginOperator,
			Parts: pasteSubmit("say first", 300*time.Millisecond)})
	}()
	time.Sleep(60 * time.Millisecond) // first paste is in; its sequence owns seqMu
	warden := make(chan error, 1)
	go func() {
		warden <- r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: ptyevent.OriginWarden, Reason: "resume",
			Parts: pasteSubmit("say stale", 0)})
	}()
	time.Sleep(30 * time.Millisecond) // warden has taken its decision-time fence
	if arb, err := r.s.Input(Write{Origin: ptyevent.OriginHuman, ClientID: "c1", Data: []byte("say human")}); err != nil || arb.Verdict != ptyevent.VerdictWrite {
		t.Fatalf("human preemption = %+v, %v", arb, err)
	}
	if err := <-first; !errors.Is(err, ErrPreempted) {
		t.Fatalf("first sequence after human preemption = %v, want ErrPreempted", err)
	}
	if err := <-warden; !errors.Is(err, ErrPreempted) {
		t.Fatalf("queued stale warden decision = %v, want ErrPreempted", err)
	}
	for _, ev := range kinds(r.events(), ptyevent.KindInput) {
		if string(ev.Input.Data) == "say stale" {
			t.Fatal("stale queued warden input reached the PTY")
		}
	}
}

// A warden sequence issued before a human typed is stale once they have: it is
// dropped even though the human later went quiet. An operator send, being
// explicit, simply waits its turn.
func TestSequenceStaleWardenFencedOperatorWaits(t *testing.T) {
	r := startFake(t, Options{Ownership: ptyevent.OwnershipConfig{HumanQuiet: 150 * time.Millisecond, MaxDefer: 5 * time.Second}})
	r.waitOutput("ready")
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	// Human takes the line, then the automated writers queue behind them.
	r.human("say first")
	r.waitOutput("first\r\n")
	// Issue-time HumanSeq is read inside WriteSequence; the human types again
	// while both are deferred.
	wd := make(chan error, 1)
	op := make(chan error, 1)
	go func() {
		wd <- r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: ptyevent.OriginWarden, Reason: "x", Parts: pasteSubmit("say warden", 0)})
	}()
	go func() {
		op <- r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: ptyevent.OriginOperator, Parts: pasteSubmit("say operator", 0)})
	}()
	time.Sleep(30 * time.Millisecond)
	r.human("say second")
	if err := <-wd; !errors.Is(err, ErrPreempted) {
		t.Fatalf("stale warden sequence: %v, want ErrPreempted", err)
	}
	if err := <-op; err != nil {
		t.Fatalf("operator sequence: %v (must wait for the human, not be dropped)", err)
	}
	r.waitOutput("operator\r\n")
	for _, ev := range kinds(r.events(), ptyevent.KindInput) {
		if string(ev.Input.Data) == "say warden" {
			t.Fatal("stale warden input reached the PTY")
		}
	}
}

// A clean failure before any byte lands releases the once-key (retry allowed);
// shells and malformed origins are rejected.
func TestSequenceKeyReleasedWhenNothingWritten(t *testing.T) {
	r := startFake(t, seqOwnership())
	r.waitOutput("ready")
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	// Human holds the line past MaxDefer: warden abandons without writing.
	if arb, _ := r.s.Input(Write{Origin: ptyevent.OriginHuman, ClientID: "c1", Data: []byte("say ")}); arb.Verdict != ptyevent.VerdictWrite {
		t.Fatal(arb)
	}
	q := ptyevent.Sequence{Origin: ptyevent.OriginWarden, Reason: "resume", OnceKey: "resume:1", Parts: pasteSubmit("say resumed", 0)}
	if err := r.s.WriteSequence(ctx, q); err == nil {
		t.Fatal("warden sequence over an open human line must not write")
	}
	r.human("")
	time.Sleep(80 * time.Millisecond)
	if err := r.s.WriteSequence(ctx, q); err != nil {
		t.Fatalf("retry after a clean abandon: %v (key must be released)", err)
	}
	r.waitOutput("resumed\r\n")
	if err := r.s.WriteSequence(ctx, ptyevent.Sequence{Origin: ptyevent.OriginHuman, Parts: pasteSubmit("x", 0)}); !errors.Is(err, ErrInputRej) {
		t.Fatalf("human origin via Sequence: %v, want ErrInputRej", err)
	}
}
