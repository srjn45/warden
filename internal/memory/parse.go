package memory

import (
	"regexp"
	"strings"
)

// Trust states an entry can carry. PR-0 only PARSES these — it never writes or
// promotes them (that is PR-2's verify-before-trust curation). An entry hand-authored
// without a metadata prefix has trust "" (TrustNone).
const (
	TrustNone       = ""           // plain bullet, no metadata prefix
	TrustUnverified = "unverified" // a learned hint — may be stale, verify before relying
	TrustTrusted    = "trusted"    // corroborated / human-approved
)

// Entry is one memory record: durable text plus the OPTIONAL structured metadata
// (trust / timestamp / provenance) the curation pass will attach. All metadata
// fields are "" for a plain hand-authored bullet — the lenient PR-0 case.
type Entry struct {
	Trust      string // "", "unverified", or "trusted"
	Timestamp  string // absolute YYYY-MM-DD, or "" if absent
	Provenance string // e.g. "agent a1b2 · sha 04e2aed", or "" if absent
	Text       string // the fact itself

	// Raw is the entry's verbatim source line(s) as authored (bullet + any folded
	// continuation lines, joined by "\n"), captured by Parse. Serialize re-emits it
	// unchanged for an untouched entry so a curation pass's diff shows ONLY the
	// lines it added/superseded/flagged — never a reflow of every hand-authored
	// bullet. "" for a synthesized (curation-proposed) entry, which serializes to
	// its Canonical form instead.
	Raw string

	// Tombstone marks an entry struck by supersession or age-out (§4.2): it stays in
	// the committed file as a diff-reviewer breadcrumb but is NEVER projected to an
	// agent. Note carries the reason ("superseded 2026-07-03 by agent c3").
	Tombstone bool

	// Stale marks an entry whose named path no longer exists on the live tree, found
	// by the deterministic staleness check (§4.2). Flagged (not deleted) in the file
	// and excluded from projection. Note carries which path went missing.
	Stale bool

	// Note is the tombstone/stale annotation rendered into the file (an HTML comment)
	// so the human diff reviewer sees WHY an entry was struck or flagged.
	Note string
}

// Unverified reports whether this entry should be projected with the
// "may be stale, verify before relying" caveat (§4.2).
func (e Entry) Unverified() bool { return e.Trust == TrustUnverified }

// Live reports whether this entry should be PROJECTED to an agent: only entries
// that are neither struck (Tombstone) nor flagged missing (Stale). Bookkeeping
// entries stay in the committed file for the diff reviewer but never reach a
// launch.
func (e Entry) Live() bool { return !e.Tombstone && !e.Stale }

// Memory is the parsed .warden/memory.md: any leading freeform/preamble content
// (the commented header, section prose) plus the ordered entries.
type Memory struct {
	Preamble string  // verbatim content before the first bullet (header/comment)
	Entries  []Entry // bullets, in authored order
}

var (
	// bulletRe matches a markdown list bullet ("- " or "* ") and captures the rest.
	bulletRe = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
	// metaRe peels an optional "[ ... ] rest" metadata prefix off a bullet's body.
	metaRe = regexp.MustCompile(`^\[([^\]]*)\]\s*(.*)$`)
	// dateRe identifies an absolute YYYY-MM-DD timestamp among the metadata parts.
	dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// metaSep is the " · " (middle dot) separator between metadata parts, matching the
// design's entry sketch (§4.2).
const metaSep = " · "

// Parse reads .warden/memory.md text into a typed Memory. It is lenient by design
// (PR-0 humans hand-author): a bullet with a "[trust · date · provenance]" prefix
// parses into the structured fields; a plain "- bullet" parses into an entry with
// empty metadata. Indented continuation lines fold into the preceding entry so a
// wrapped multi-line fact stays one entry. Everything before the first bullet is
// preserved verbatim as Preamble.
func Parse(text string) *Memory {
	m := &Memory{}
	var preamble []string
	seenBullet := false
	inComment := false

	for _, line := range strings.Split(text, "\n") {
		// HTML comments (the seeded header, and warden's own injected blocks) are
		// never parsed as entries — an example bullet inside the header comment must
		// not become a phantom fact. Track open/close spanning multiple lines. A
		// PURE comment line (trimmed content begins with "<!--") is skipped; a bullet
		// that merely carries a trailing inline comment — the tombstone/stale
		// breadcrumbs the curation pass writes — must still parse AS a bullet.
		trimmed := strings.TrimSpace(line)
		pureComment := strings.HasPrefix(trimmed, "<!--")
		wasComment := inComment
		if !inComment && pureComment && !strings.Contains(line, "-->") {
			inComment = true
		} else if inComment && strings.Contains(line, "-->") {
			inComment = false
		}
		if wasComment || inComment || (pureComment && strings.Contains(line, "-->")) {
			if !seenBullet {
				preamble = append(preamble, line)
			}
			continue
		}

		if mb := bulletRe.FindStringSubmatch(line); mb != nil {
			seenBullet = true
			e := parseEntry(mb[1])
			e.Raw = line
			markFlags(&e, line)
			m.Entries = append(m.Entries, e)
			continue
		}
		if !seenBullet {
			preamble = append(preamble, line)
			continue
		}
		// After the first bullet: an indented, non-empty line is a continuation of
		// the current entry; blank / unindented prose is a separator we skip.
		if strings.TrimSpace(line) != "" && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(m.Entries) > 0 {
			last := &m.Entries[len(m.Entries)-1]
			last.Text = strings.TrimSpace(last.Text + " " + strings.TrimSpace(line))
			last.Raw += "\n" + line
		}
	}

	m.Preamble = strings.TrimRight(strings.Join(preamble, "\n"), "\n")
	return m
}

// parseEntry splits one bullet body into metadata + text. With no "[...]" prefix it
// returns a plain entry (text only). The metadata parts are classified by CONTENT,
// not position, so order is forgiving: the trust keyword and the YYYY-MM-DD date are
// recognized wherever they sit, and whatever remains is provenance.
func parseEntry(body string) Entry {
	body = strings.TrimSpace(body)
	mm := metaRe.FindStringSubmatch(body)
	if mm == nil {
		return Entry{Text: body}
	}
	e := Entry{Text: strings.TrimSpace(mm[2])}
	var prov []string
	for _, part := range strings.Split(mm[1], metaSep) {
		part = strings.TrimSpace(part)
		switch {
		case part == "":
			// skip
		case part == TrustUnverified || part == TrustTrusted:
			e.Trust = part
		case dateRe.MatchString(part):
			e.Timestamp = part
		default:
			prov = append(prov, part)
		}
	}
	e.Provenance = strings.Join(prov, metaSep)
	return e
}

// markFlags detects an already-serialized tombstone/stale entry on re-parse so a
// curation pass round-trips its own bookkeeping without re-processing it: a struck
// (~~…~~) bullet or one carrying the superseded marker is a Tombstone; one carrying
// the stale marker is Stale. Idempotent — this is what keeps a second pass from
// double-flagging or resurrecting a struck entry.
func markFlags(e *Entry, raw string) {
	if strings.HasPrefix(strings.TrimSpace(e.Text), "~~") || strings.Contains(raw, supersededMarker) {
		e.Tombstone = true
	}
	if strings.Contains(raw, staleMarker) {
		e.Stale = true
	}
}
