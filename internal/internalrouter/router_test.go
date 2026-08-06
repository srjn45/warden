package internalrouter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
)

// fixedNow is the injected clock for deterministic LimitedUntil comparisons.
var fixedNow = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// newTestStore opens a fresh on-disk registry seeded with the given rows.
func newTestStore(t *testing.T, rows ...backendstore.Backend) *backendstore.Store {
	t.Helper()
	st, err := backendstore.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for _, b := range rows {
		if err := st.Upsert(b); err != nil {
			t.Fatalf("seed %s: %v", b.ID, err)
		}
	}
	return st
}

// fakeCompleter is a stub local model.
type fakeCompleter struct {
	out   string
	err   error
	calls int
}

func (f *fakeCompleter) Complete(context.Context, string) (string, error) {
	f.calls++
	return f.out, f.err
}

// resp is one free-CLI headless outcome keyed by backend id.
type resp struct {
	out string
	err error
}

// newRouter builds a Router with an injected headless seam and clock, bypassing
// New so tests never shell a real backend binary.
func newRouter(st *backendstore.Store, local *fakeCompleter, responses map[string]resp) (*Router, *[]string) {
	var called []string
	r := &Router{
		store:       st,
		limitTTL:    15 * time.Minute,
		callTimeout: time.Second,
		now:         func() time.Time { return fixedNow },
	}
	if local != nil {
		r.local = local
	}
	r.headless = func(_ context.Context, id, _ string) (string, error) {
		called = append(called, id)
		rr := responses[id]
		return rr.out, rr.err
	}
	return r, &called
}

func ids(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestCandidatesLocalOnly(t *testing.T) {
	st := newTestStore(t,
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree, Default: true},
	)
	if err := st.SetThinkingMode(backendstore.ThinkingModeLocalOnly); err != nil {
		t.Fatal(err)
	}
	r, _ := newRouter(st, nil, nil)
	got, err := r.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{backendstore.IDLocal}
	if diff := ids(got); len(diff) != 1 || diff[0] != want[0] || !got[0].IsLocal {
		t.Fatalf("local_only candidates = %v, want [local] (IsLocal)", diff)
	}
}

func TestCandidatesFreePlusLocalOrdering(t *testing.T) {
	st := newTestStore(t,
		// eligible free, default → must be first.
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree, Default: true},
		// eligible free, non-default → sorts by id after the default.
		backendstore.Backend{ID: "aider", Installed: true, Enabled: true, Tier: backendstore.TierFree},
		backendstore.Backend{ID: "goose", Installed: true, Enabled: true, Tier: backendstore.TierFree},
		// paid — never included.
		backendstore.Backend{ID: "claude", Installed: true, Enabled: true, Tier: backendstore.TierSubscription},
		backendstore.Backend{ID: "cursor", Installed: true, Enabled: true, Tier: backendstore.TierPayPerUse},
		// unclassified — treated as not free.
		backendstore.Backend{ID: "crush", Installed: true, Enabled: true, Tier: backendstore.TierUnclassified},
		// free but not installed / disabled — excluded.
		backendstore.Backend{ID: "opencode", Installed: false, Enabled: true, Tier: backendstore.TierFree},
		backendstore.Backend{ID: "kilo", Installed: true, Enabled: false, Tier: backendstore.TierFree},
		// free but currently limited (future LimitedUntil) — excluded.
		backendstore.Backend{ID: "amp", Installed: true, Enabled: true, Tier: backendstore.TierFree, LimitedUntil: fixedNow.Add(time.Hour)},
		// local row — appended last regardless.
		backendstore.Backend{ID: "local", Installed: true, Enabled: true, Tier: backendstore.TierLocal, IsLocal: true},
	)
	r, _ := newRouter(st, nil, nil)
	got, err := r.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "aider", "goose", "local"}
	g := ids(got)
	if len(g) != len(want) {
		t.Fatalf("candidates = %v, want %v", g, want)
	}
	for i := range want {
		if g[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", g, want)
		}
	}
	if !got[len(got)-1].IsLocal {
		t.Fatalf("last candidate must be the local model, got %+v", got[len(got)-1])
	}
}

func TestCandidatesLimitedBecomesEligibleAfterExpiry(t *testing.T) {
	st := newTestStore(t,
		// LimitedUntil already in the past → eligible again.
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree, LimitedUntil: fixedNow.Add(-time.Minute)},
	)
	r, _ := newRouter(st, nil, nil)
	got, err := r.Candidates()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex", "local"}; ids(got)[0] != want[0] {
		t.Fatalf("candidates = %v, want codex first (limit expired)", ids(got))
	}
}

func TestCompleteLimitSkipContinuation(t *testing.T) {
	st := newTestStore(t,
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree, Default: true},
		backendstore.Backend{ID: "aider", Installed: true, Enabled: true, Tier: backendstore.TierFree},
	)
	// codex hits a rate limit; aider serves.
	r, called := newRouter(st, nil, map[string]resp{
		"codex": {out: "Error: 429 too many requests"},
		"aider": {out: "development"},
	})
	out, err := r.Complete(context.Background(), "classify this")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "development" {
		t.Fatalf("output = %q, want aider's reply", out)
	}
	if len(*called) != 2 || (*called)[0] != "codex" || (*called)[1] != "aider" {
		t.Fatalf("call order = %v, want [codex aider]", *called)
	}
	// codex must have been stamped LimitedUntil = now + TTL.
	b, err := st.Get("codex")
	if err != nil {
		t.Fatal(err)
	}
	if want := fixedNow.Add(15 * time.Minute); !b.LimitedUntil.Equal(want) {
		t.Fatalf("codex LimitedUntil = %v, want %v", b.LimitedUntil, want)
	}
}

func TestCompleteFallsThroughToLocalOnNonLimitError(t *testing.T) {
	st := newTestStore(t,
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree},
	)
	local := &fakeCompleter{out: "local summary"}
	r, called := newRouter(st, local, map[string]resp{
		"codex": {err: errors.New("exec: not found")}, // non-limit error → skip, do NOT stamp
	})
	out, err := r.Complete(context.Background(), "summarize")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out != "local summary" || local.calls != 1 {
		t.Fatalf("output = %q calls = %d, want local to serve", out, local.calls)
	}
	if len(*called) != 1 || (*called)[0] != "codex" {
		t.Fatalf("call order = %v, want [codex]", *called)
	}
	// A non-limit error must NOT stamp LimitedUntil.
	b, _ := st.Get("codex")
	if !b.LimitedUntil.IsZero() {
		t.Fatalf("codex LimitedUntil = %v, want zero (non-limit error)", b.LimitedUntil)
	}
}

func TestCompleteDegradesWhenExhausted(t *testing.T) {
	st := newTestStore(t,
		// A free backend that always limits, and no local model configured.
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree},
	)
	r, _ := newRouter(st, nil, map[string]resp{
		"codex": {out: "usage limit reached"},
	})
	_, err := r.Complete(context.Background(), "name this")
	if !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("Complete err = %v, want ErrNoCandidate (graceful degrade, never paid)", err)
	}
}

func TestCompleteLocalOnlyMode(t *testing.T) {
	st := newTestStore(t,
		backendstore.Backend{ID: "codex", Installed: true, Enabled: true, Tier: backendstore.TierFree, Default: true},
	)
	if err := st.SetThinkingMode(backendstore.ThinkingModeLocalOnly); err != nil {
		t.Fatal(err)
	}
	local := &fakeCompleter{out: "local-only reply"}
	r, called := newRouter(st, local, map[string]resp{"codex": {out: "should not run"}})
	out, err := r.Complete(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if out != "local-only reply" {
		t.Fatalf("output = %q, want local reply", out)
	}
	if len(*called) != 0 {
		t.Fatalf("no free CLI must run in local_only mode, ran %v", *called)
	}
}

func TestIsLimitSignal(t *testing.T) {
	limited := []string{
		"Error: 429 Too Many Requests",
		"you have hit your rate limit",
		"usage limit reached, try again later",
		"monthly spend limit exceeded",
		"model is overloaded",
	}
	for _, s := range limited {
		if !isLimitSignal(s) {
			t.Errorf("isLimitSignal(%q) = false, want true", s)
		}
	}
	ok := []string{"development", "a short summary of the work", "fix-the-login-bug"}
	for _, s := range ok {
		if isLimitSignal(s) {
			t.Errorf("isLimitSignal(%q) = true, want false", s)
		}
	}
}
