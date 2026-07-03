package memory

import (
	"strings"
	"testing"
)

// TestParseStructuredEntry: a "[trust · date · provenance] text" bullet splits into
// the structured fields, classified by content so order is forgiving.
func TestParseStructuredEntry(t *testing.T) {
	m := Parse("- [unverified · 2026-06-30 · agent a1b2 · sha 04e2aed] The daemon API is spec-first.\n")
	if len(m.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(m.Entries))
	}
	e := m.Entries[0]
	if e.Trust != TrustUnverified {
		t.Errorf("Trust = %q, want unverified", e.Trust)
	}
	if e.Timestamp != "2026-06-30" {
		t.Errorf("Timestamp = %q, want 2026-06-30", e.Timestamp)
	}
	if e.Provenance != "agent a1b2 · sha 04e2aed" {
		t.Errorf("Provenance = %q", e.Provenance)
	}
	if e.Text != "The daemon API is spec-first." {
		t.Errorf("Text = %q", e.Text)
	}
	if !e.Unverified() {
		t.Errorf("Unverified() = false, want true")
	}
}

// TestParsePlainBullet: a bullet with no "[...]" prefix parses as an entry with empty
// metadata (the lenient PR-0 human-authored case).
func TestParsePlainBullet(t *testing.T) {
	m := Parse("- tests run behind `wd check`\n")
	if len(m.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(m.Entries))
	}
	e := m.Entries[0]
	if e.Trust != TrustNone || e.Timestamp != "" || e.Provenance != "" {
		t.Errorf("plain bullet carried metadata: %+v", e)
	}
	if e.Text != "tests run behind `wd check`" {
		t.Errorf("Text = %q", e.Text)
	}
	if e.Unverified() {
		t.Errorf("plain bullet must not be flagged unverified")
	}
}

// TestParsePreambleAndContinuation: content before the first bullet is preserved as
// preamble; an indented continuation line folds into the preceding entry.
func TestParsePreambleAndContinuation(t *testing.T) {
	src := "<!-- header comment -->\nsome prose\n\n" +
		"- [trusted · 2026-06-30] first fact\n" +
		"  wrapped onto a second line\n" +
		"- second fact\n"
	m := Parse(src)
	if !strings.Contains(m.Preamble, "header comment") || !strings.Contains(m.Preamble, "some prose") {
		t.Errorf("preamble = %q", m.Preamble)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(m.Entries))
	}
	if m.Entries[0].Trust != TrustTrusted {
		t.Errorf("Trust = %q, want trusted", m.Entries[0].Trust)
	}
	if m.Entries[0].Text != "first fact wrapped onto a second line" {
		t.Errorf("continuation not folded: %q", m.Entries[0].Text)
	}
	if m.Entries[1].Text != "second fact" {
		t.Errorf("Text[1] = %q", m.Entries[1].Text)
	}
}

// TestParseForgivingMetaOrder: trust and date are recognized regardless of order, and
// a star bullet is accepted.
func TestParseForgivingMetaOrder(t *testing.T) {
	m := Parse("* [2026-01-02 · trusted · agent z9] reordered metadata\n")
	if len(m.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(m.Entries))
	}
	e := m.Entries[0]
	if e.Trust != TrustTrusted || e.Timestamp != "2026-01-02" || e.Provenance != "agent z9" {
		t.Errorf("forgiving parse failed: %+v", e)
	}
}
