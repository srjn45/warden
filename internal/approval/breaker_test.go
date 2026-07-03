package approval

import (
	"testing"
	"time"
)

func newTestBreaker(t0 time.Time) (*Breaker, *time.Time) {
	now := t0
	b := NewBreaker()
	b.now = func() time.Time { return now }
	return b, &now
}

func TestBreakerAllowsUpToMaxThenTripsOnce(t *testing.T) {
	b, _ := newTestBreaker(time.Unix(0, 0))
	for i := 1; i <= 3; i++ {
		ok, tripped := b.Allow("a1", "sig", 3)
		if !ok || tripped {
			t.Fatalf("approval %d: got ok=%v tripped=%v, want ok=true tripped=false", i, ok, tripped)
		}
	}
	// 4th identical approval is denied, and only the FIRST denial reports the trip.
	ok, tripped := b.Allow("a1", "sig", 3)
	if ok || !tripped {
		t.Fatalf("first denial: got ok=%v tripped=%v, want ok=false tripped=true", ok, tripped)
	}
	ok, tripped = b.Allow("a1", "sig", 3)
	if ok || tripped {
		t.Fatalf("second denial: got ok=%v tripped=%v, want ok=false tripped=false", ok, tripped)
	}
}

func TestBreakerDifferentPromptResetsRun(t *testing.T) {
	b, _ := newTestBreaker(time.Unix(0, 0))
	for i := 0; i < 5; i++ {
		b.Allow("a1", "sig-A", 2)
	}
	if ok, _ := b.Allow("a1", "sig-B", 2); !ok {
		t.Fatal("a different prompt must start a fresh run")
	}
	// And the old signature also starts fresh (run is replaced, not archived).
	if ok, _ := b.Allow("a1", "sig-A", 2); !ok {
		t.Fatal("returning to the old prompt after a reset must start a fresh run")
	}
}

func TestBreakerPerAgentIsolation(t *testing.T) {
	b, _ := newTestBreaker(time.Unix(0, 0))
	for i := 0; i < 3; i++ {
		b.Allow("a1", "sig", 2)
	}
	if ok, _ := b.Allow("a2", "sig", 2); !ok {
		t.Fatal("agent a2 must not inherit a1's tripped run")
	}
}

func TestBreakerCooldownReArms(t *testing.T) {
	b, now := newTestBreaker(time.Unix(0, 0))
	for i := 0; i < 3; i++ {
		b.Allow("a1", "sig", 2) // 2 allowed + 1 denied -> tripped
	}
	// A live loop refreshes the run: still denied just under the cooldown.
	*now = now.Add(breakerCooldown - time.Second)
	if ok, _ := b.Allow("a1", "sig", 2); ok {
		t.Fatal("within cooldown the tripped run must stay blocked")
	}
	// Quiet past the cooldown: the run re-arms.
	*now = now.Add(breakerCooldown + time.Second)
	ok, tripped := b.Allow("a1", "sig", 2)
	if !ok || tripped {
		t.Fatalf("after cooldown: got ok=%v tripped=%v, want ok=true tripped=false", ok, tripped)
	}
}

func TestBreakerDisabledAndReset(t *testing.T) {
	b, _ := newTestBreaker(time.Unix(0, 0))
	for i := 0; i < 100; i++ {
		if ok, tripped := b.Allow("a1", "sig", 0); !ok || tripped {
			t.Fatal("max<=0 must disable the breaker")
		}
	}
	for i := 0; i < 3; i++ {
		b.Allow("a1", "sig", 2)
	}
	b.Reset("a1")
	if ok, _ := b.Allow("a1", "sig", 2); !ok {
		t.Fatal("Reset must clear the tripped run")
	}
}

func TestEffectiveMaxRepeats(t *testing.T) {
	if got := (Policy{}).EffectiveMaxRepeats(); got != DefaultMaxRepeats {
		t.Fatalf("unset: got %d, want default %d", got, DefaultMaxRepeats)
	}
	if got := (Policy{MaxRepeats: 3}).EffectiveMaxRepeats(); got != 3 {
		t.Fatalf("explicit: got %d, want 3", got)
	}
	if got := (Policy{MaxRepeats: -1}).EffectiveMaxRepeats(); got != 0 {
		t.Fatalf("negative (disabled): got %d, want 0", got)
	}
}

func TestForInheritsMaxRepeats(t *testing.T) {
	p := Policy{Enabled: true, MaxRepeats: 7, Agents: map[string]Policy{
		"kept":     {},
		"override": {MaxRepeats: 2},
		"disabled": {MaxRepeats: -1},
	}}
	if got := p.For("kept").MaxRepeats; got != 7 {
		t.Fatalf("unset override must inherit the default's MaxRepeats: got %d, want 7", got)
	}
	if got := p.For("override").MaxRepeats; got != 2 {
		t.Fatalf("explicit override: got %d, want 2", got)
	}
	if got := p.For("disabled").MaxRepeats; got != -1 {
		t.Fatalf("negative override must stick (per-agent disable): got %d, want -1", got)
	}
}
