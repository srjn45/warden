package memory

import "strings"

// Marker comments the curation pass writes into .warden/memory.md so a struck or
// flagged entry stays visible and re-parseable (markFlags reads them back). They
// are HTML comments so they never project as facts and read as breadcrumbs in the
// committed diff.
const (
	supersededMarker = "<!-- superseded" // opens a tombstone note; closed by " -->"
	staleMarker      = "<!-- stale:"     // opens a staleness note; closed by " -->"
)

// Canonical renders an entry as ONE bullet line in the design's entry shape (§4.2):
//
//   - [unverified · 2026-06-30 · agent a1b2 · sha 04e2aed] the fact
//
// Empty metadata parts are omitted; an entry with no metadata at all renders as a
// plain "- the fact". This is the form Serialize uses for a freshly proposed
// (curation-authored) entry — an untouched entry re-emits its verbatim Raw instead.
func (e Entry) Canonical() string {
	var meta []string
	if e.Trust != "" {
		meta = append(meta, e.Trust)
	}
	if e.Timestamp != "" {
		meta = append(meta, e.Timestamp)
	}
	if e.Provenance != "" {
		meta = append(meta, e.Provenance)
	}
	if len(meta) == 0 {
		return "- " + e.Text
	}
	return "- [" + strings.Join(meta, metaSep) + "] " + e.Text
}

// serializeLine renders one entry for the committed file. An untouched entry
// re-emits its verbatim Raw (zero diff churn); a proposed entry renders Canonical;
// a tombstoned/stale entry renders its struck/flagged form with the reason comment.
func serializeLine(e Entry) string {
	switch {
	case e.Tombstone:
		return tombstoneLine(e)
	case e.Stale:
		return staleLine(e)
	case e.Raw != "":
		return e.Raw
	default:
		return e.Canonical()
	}
}

// tombstoneLine renders a struck entry: strikethrough over its text plus a comment
// carrying the reason. If it is ALREADY a serialized tombstone (Raw carries the
// marker), the Raw is re-emitted verbatim so a second pass is idempotent.
func tombstoneLine(e Entry) string {
	if e.Raw != "" && (strings.Contains(e.Raw, supersededMarker) || strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(e.Raw, "-")), "~~")) {
		return e.Raw
	}
	note := e.Note
	if note == "" {
		note = "superseded"
	}
	return "- ~~" + e.Text + "~~ <!-- " + note + " -->"
}

// staleLine renders a flagged (path-missing) entry: its original line preserved,
// plus a stale comment. Idempotent — an already-flagged Raw is re-emitted verbatim.
func staleLine(e Entry) string {
	base := e.Raw
	if base == "" {
		base = e.Canonical()
	}
	if strings.Contains(base, staleMarker) {
		return base
	}
	note := e.Note
	if note == "" {
		note = "named path no longer exists"
	}
	return base + " <!-- stale: " + note + " -->"
}

// Serialize renders the whole Memory back to .warden/memory.md text (the additive
// WRITE helper PR-0 deferred). It preserves the Preamble verbatim and re-emits each
// untouched entry's Raw line unchanged, so a curation pass produces a MINIMAL,
// reviewable diff — only the added/superseded/flagged lines move. It is the inverse
// of Parse for an unmodified Memory (modulo inter-bullet blank lines, which a
// compact list drops).
func (m *Memory) Serialize() string {
	var b strings.Builder
	if p := strings.TrimRight(m.Preamble, "\n"); p != "" {
		b.WriteString(p)
		b.WriteString("\n")
		if len(m.Entries) > 0 {
			b.WriteString("\n") // blank line between header and the list
		}
	}
	for i, e := range m.Entries {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(serializeLine(e))
	}
	b.WriteString("\n")
	return b.String()
}
