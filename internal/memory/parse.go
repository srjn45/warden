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
}

// Unverified reports whether this entry should be projected with the
// "may be stale, verify before relying" caveat (§4.2).
func (e Entry) Unverified() bool { return e.Trust == TrustUnverified }

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
		// not become a phantom fact. Track open/close spanning multiple lines.
		wasComment := inComment
		if !inComment && strings.Contains(line, "<!--") && !strings.Contains(line, "-->") {
			inComment = true
		} else if inComment && strings.Contains(line, "-->") {
			inComment = false
		}
		if wasComment || inComment || (strings.Contains(line, "<!--") && strings.Contains(line, "-->")) {
			if !seenBullet {
				preamble = append(preamble, line)
			}
			continue
		}

		if mb := bulletRe.FindStringSubmatch(line); mb != nil {
			seenBullet = true
			m.Entries = append(m.Entries, parseEntry(mb[1]))
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
