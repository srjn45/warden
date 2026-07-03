package memory

import (
	"strings"
	"testing"
)

// TestRenderEmptyIsEmpty: a memory with no entries renders "" so PR-1 projects
// nothing (keeping Claude's launch byte-identical when memory is empty).
func TestRenderEmptyIsEmpty(t *testing.T) {
	if got := (&Memory{Preamble: "just a header"}).RenderDefault(); got != "" {
		t.Fatalf("empty memory render = %q, want empty", got)
	}
}

// TestRenderHeaderAndCaveat: the rendered block leads with the header, lists each
// entry as a bullet, and flags ONLY unverified entries with the stale caveat.
func TestRenderHeaderAndCaveat(t *testing.T) {
	m := &Memory{Entries: []Entry{
		{Trust: TrustTrusted, Text: "daemon API is spec-first"},
		{Trust: TrustUnverified, Text: "cache lives in ~/.warden"},
		{Text: "plain human fact"},
	}}
	out := m.RenderDefault()
	if !strings.HasPrefix(out, projectionHeader) {
		t.Errorf("render missing header:\n%s", out)
	}
	if !strings.Contains(out, "- daemon API is spec-first\n") {
		t.Errorf("trusted entry missing/plain:\n%s", out)
	}
	if !strings.Contains(out, "- cache lives in ~/.warden"+unverifiedCaveat) {
		t.Errorf("unverified entry missing caveat:\n%s", out)
	}
	// The caveat must appear exactly once — only the unverified entry carries it.
	if n := strings.Count(out, unverifiedCaveat); n != 1 {
		t.Errorf("caveat count = %d, want 1:\n%s", n, out)
	}
}

// TestRenderBudgetIsHardCap: over-budget memory is trimmed to fit the byte cap, and
// the lowest-value entries (unverified, then oldest) drop first while kept entries
// stay in authored order.
func TestRenderBudgetIsHardCap(t *testing.T) {
	big := strings.Repeat("x", 200)
	m := &Memory{Entries: []Entry{
		{Trust: TrustTrusted, Timestamp: "2026-06-01", Text: "KEEP-trusted " + big},
		{Trust: TrustUnverified, Timestamp: "2026-06-02", Text: "DROP-unverified " + big},
		{Trust: TrustNone, Timestamp: "2026-06-03", Text: "KEEP-plain " + big},
	}}
	// Budget fits the header + 2 of the ~215-byte lines + the trim note, but not the
	// third (unverified, +caveat) line.
	budget := len(projectionHeader) + 560
	out := m.Render(budget)
	if len(out) > budget {
		t.Fatalf("render exceeded hard budget: %d > %d", len(out), budget)
	}
	if !strings.Contains(out, "KEEP-trusted") {
		t.Errorf("dropped the highest-trust entry:\n%s", out)
	}
	if strings.Contains(out, "DROP-unverified") {
		t.Errorf("kept the lowest-value (unverified) entry over a plain one:\n%s", out)
	}
	if !strings.Contains(out, "KEEP-plain") {
		t.Errorf("dropped a plain entry before the unverified one:\n%s", out)
	}
	if !strings.Contains(out, "trimmed to fit") {
		t.Errorf("missing trim note:\n%s", out)
	}
	// Kept entries render in authored order: trusted (idx 0) before plain (idx 2).
	if strings.Index(out, "KEEP-trusted") > strings.Index(out, "KEEP-plain") {
		t.Errorf("kept entries out of authored order:\n%s", out)
	}
}
