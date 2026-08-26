package poller

import (
	"context"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// TestEvalHotSwapSignalsOncePerCriticalEpisode: the poller fires OnHotSwap the first
// tick an agent goes critical, stays quiet while it remains critical, and re-arms
// once it drops out — so the daemon's handler is invoked once per episode.
func TestEvalHotSwapSignalsOncePerCriticalEpisode(t *testing.T) {
	fd := &ctxFakeDeps{tokens: 420000, tokensOK: true}
	p := New(fd, time.Minute)
	p.TokenWarn, p.TokenCrit, p.TokenGuard = 200000, 400000, true
	p.HandoverEnabled = true
	var signals int
	var gotTokens int
	p.OnHotSwap = func(_ *store.Session, tokens int) { signals++; gotTokens = tokens }

	s := &store.Session{ID: "a1", Status: store.StatusWorking}

	// First critical tick: signal.
	p.checkContext(context.Background(), s, time.Now())
	if signals != 1 {
		t.Fatalf("first critical tick: signals=%d, want 1", signals)
	}
	if gotTokens != 420000 {
		t.Fatalf("signalled tokens=%d, want 420000", gotTokens)
	}

	// Second critical tick: no re-fire (edge-triggered per episode).
	p.checkContext(context.Background(), s, time.Now())
	if signals != 1 {
		t.Fatalf("steady critical: signals=%d, want still 1", signals)
	}

	// Drop out of critical, then back in: re-arms and fires again.
	fd.tokens = 100000
	p.checkContext(context.Background(), s, time.Now())
	fd.tokens = 420000
	p.checkContext(context.Background(), s, time.Now())
	if signals != 2 {
		t.Fatalf("re-entered critical: signals=%d, want 2", signals)
	}
}

// TestEvalHotSwapInertWhenDisabled: with handover disabled (the default) the signal
// never fires, even at critical fill.
func TestEvalHotSwapInertWhenDisabled(t *testing.T) {
	fd := &ctxFakeDeps{tokens: 420000, tokensOK: true}
	p := New(fd, time.Minute)
	p.TokenWarn, p.TokenCrit, p.TokenGuard = 200000, 400000, true
	// HandoverEnabled left false.
	var signals int
	p.OnHotSwap = func(*store.Session, int) { signals++ }

	s := &store.Session{ID: "a1", Status: store.StatusWorking}
	p.checkContext(context.Background(), s, time.Now())
	if signals != 0 {
		t.Fatalf("disabled handover fired the signal: signals=%d", signals)
	}
}

// TestEvalHotSwapNoSignalBelowCritical: a warning-band fill does not signal.
func TestEvalHotSwapNoSignalBelowCritical(t *testing.T) {
	fd := &ctxFakeDeps{tokens: 250000, tokensOK: true} // warning, not critical
	p := New(fd, time.Minute)
	p.TokenWarn, p.TokenCrit, p.TokenGuard = 200000, 400000, true
	p.HandoverEnabled = true
	var signals int
	p.OnHotSwap = func(*store.Session, int) { signals++ }

	s := &store.Session{ID: "a1", Status: store.StatusWorking}
	p.checkContext(context.Background(), s, time.Now())
	if signals != 0 {
		t.Fatalf("warning band fired the signal: signals=%d", signals)
	}
}
