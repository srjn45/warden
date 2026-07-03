package memory

import (
	"strings"
	"testing"
)

// TestCanonicalEntryShape: a proposed entry serializes to the design's §4.2 shape,
// with empty metadata parts omitted.
func TestCanonicalEntryShape(t *testing.T) {
	e := Entry{Trust: TrustUnverified, Timestamp: "2026-07-03", Provenance: "agent c3 · sha 04e2", Text: "X lives in foo/"}
	if got, want := e.Canonical(), "- [unverified · 2026-07-03 · agent c3 · sha 04e2] X lives in foo/"; got != want {
		t.Fatalf("Canonical() = %q, want %q", got, want)
	}
	if got, want := (Entry{Text: "plain fact"}).Canonical(), "- plain fact"; got != want {
		t.Fatalf("plain Canonical() = %q, want %q", got, want)
	}
}

// TestSerializeRoundTripPreservesRaw: Parse→Serialize of an unmodified memory
// re-emits the preamble and each entry's verbatim source line, so a curation pass
// that touches nothing produces no diff churn.
func TestSerializeRoundTripPreservesRaw(t *testing.T) {
	src := "<!-- header comment -->\n\n" +
		"- [trusted · 2026-06-01 · agent a1] the daemon API is spec-first\n" +
		"- tests run behind `wd check`\n"
	m := Parse(src)
	out := m.Serialize()
	if !strings.Contains(out, "<!-- header comment -->") {
		t.Errorf("preamble not preserved:\n%s", out)
	}
	if !strings.Contains(out, "- [trusted · 2026-06-01 · agent a1] the daemon API is spec-first") {
		t.Errorf("structured entry Raw not preserved verbatim:\n%s", out)
	}
	if !strings.Contains(out, "- tests run behind `wd check`") {
		t.Errorf("plain entry Raw not preserved verbatim:\n%s", out)
	}
	// Re-parsing the serialized form yields the same entry count (idempotent).
	if got := len(Parse(out).Entries); got != 2 {
		t.Errorf("round-trip entry count = %d, want 2", got)
	}
}

// TestSerializeTombstoneRoundTrips: a struck entry serializes to a strikethrough
// bullet with a reason comment, is excluded from projection, and re-parses back as
// a Tombstone (so a second pass does not resurrect or re-process it).
func TestSerializeTombstoneRoundTrips(t *testing.T) {
	m := &Memory{Entries: []Entry{
		{Trust: TrustTrusted, Text: "X lives in foo/", Tombstone: true, Note: "superseded 2026-07-03 by agent c3"},
		{Trust: TrustUnverified, Timestamp: "2026-07-03", Text: "X lives in bar/"},
	}}
	out := m.Serialize()
	if !strings.Contains(out, "- ~~X lives in foo/~~ <!-- superseded 2026-07-03 by agent c3 -->") {
		t.Fatalf("tombstone not struck with reason:\n%s", out)
	}
	// The tombstone is bookkeeping — never projected.
	if proj := m.RenderDefault(); strings.Contains(proj, "foo/") {
		t.Errorf("tombstoned entry leaked into projection:\n%s", proj)
	}
	if proj := m.RenderDefault(); !strings.Contains(proj, "bar/") {
		t.Errorf("live entry missing from projection:\n%s", proj)
	}
	// Round-trip: the struck line re-parses as a Tombstone and stays struck.
	re := Parse(out)
	var tomb, live int
	for _, e := range re.Entries {
		if e.Tombstone {
			tomb++
		} else if e.Live() {
			live++
		}
	}
	if tomb != 1 || live != 1 {
		t.Errorf("round-trip flags: tomb=%d live=%d, want 1/1\n%s", tomb, live, out)
	}
}

// TestSerializeStaleFlagRoundTrips: a stale-flagged entry keeps its original line
// plus a stale comment, is excluded from projection, and re-parses as Stale.
func TestSerializeStaleFlagRoundTrips(t *testing.T) {
	m := &Memory{Entries: []Entry{
		{Raw: "- [trusted · 2026-06-01] helper lives in internal/gone/x.go", Text: "helper lives in internal/gone/x.go", Trust: TrustTrusted, Stale: true, Note: "internal/gone/x.go"},
	}}
	out := m.Serialize()
	if !strings.Contains(out, staleMarker) || !strings.Contains(out, "internal/gone/x.go") {
		t.Fatalf("stale flag not rendered:\n%s", out)
	}
	if proj := m.RenderDefault(); proj != "" {
		t.Errorf("stale entry projected, want empty:\n%s", proj)
	}
	// Idempotent: re-parsing then re-serializing does not double-append the marker.
	twice := Parse(out).Serialize()
	if got := strings.Count(twice, staleMarker); got != 1 {
		t.Errorf("stale marker count after round-trip = %d, want 1:\n%s", got, twice)
	}
}
