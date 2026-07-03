package repl

import (
	"context"
	"os"
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/memory"
)

// PR-3 of #53 (backend-neutral project memory): LOCAL grounding of project
// questions. This is the token-REMOVING lever of the whole feature — where
// projection (PR-1) ADDS input tokens per turn, grounding serves "where does X
// live?" / "how do I run Y?" from the canonical .warden/memory.md through the
// LOCAL model, so the answer never makes a cloud round-trip.
//
// It is the SAME SHAPE as Monitor (a bounded call over facts through the existing
// llm.Completer seam, registered as a read-only REPL tool via AddGrounding): read
// the repo's memory, pick the relevant entries, and compose an answer locally —
// citing each entry's trust + provenance so an `unverified` hint reads visibly as
// a hint. Read-only: it uses the memory store's Locate + os.ReadFile discipline
// (never Load/Resolve), so an absent file answers "not in project memory" and is
// NEVER auto-created (PR-2 owns writes).

// maxGroundEntries bounds how many memory entries feed a single grounding answer,
// keeping the local-model prompt (and the degraded verbatim dump) compact.
const maxGroundEntries = 8

// noProjectMemory is the graceful answer when the repo has no usable memory: an
// absent/empty .warden/memory.md, or one with no live (non-tombstoned/stale)
// entries. It never crashes and never auto-creates the file.
const noProjectMemory = "not in project memory — this repo has no .warden/memory.md yet (or it's empty). " +
	"Curate durable facts there with `wd memory`, and warden will answer from them locally."

// Grounder answers a natural-language project question locally from the repo's
// .warden/memory.md. It holds ONLY a local llm.Completer (no Escalator, no cloud
// planner) — so grounding is structurally $0 and can never escalate to a paid
// model. A nil Completer (no local model configured) degrades to returning the
// matching entries verbatim, still $0.
type Grounder struct {
	dir   string        // a path inside the repo whose memory to read (the REPL's cwd)
	store *memory.Store // read source; zero value shells `git rev-parse` for the root
	c     llm.Completer // LOCAL model; nil ⇒ degrade to the raw matched entries ($0)
}

// NewGrounder builds a grounder rooted at dir (a path inside the target repo). A
// nil store defaults to the git-shelling zero value; a nil Completer means "no
// local model" — grounding then degrades to the verbatim matched entries.
func NewGrounder(dir string, store *memory.Store, c llm.Completer) *Grounder {
	if store == nil {
		store = &memory.Store{}
	}
	return &Grounder{dir: dir, store: store, c: c}
}

// Answer resolves a project question from .warden/memory.md, entirely locally. It
// never returns an error to the operator — every failure degrades to a plain
// message — so the REPL loop treats it as a cheap read (auto-execute, no gate).
func (g *Grounder) Answer(ctx context.Context, question string) (string, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return "ask a project question, e.g. \"where does the spawn gate live?\"", nil
	}

	m, err := g.readMemory(ctx)
	if err != nil {
		// A genuine read error (not a missing file) is reported plainly, never as a
		// hard error — memory grounding is additive and must never break the REPL.
		return "couldn't read project memory (" + err.Error() + ")", nil
	}
	if m == nil {
		return noProjectMemory, nil
	}
	live := liveEntries(m)
	if len(live) == 0 {
		return noProjectMemory, nil
	}

	picked := selectRelevant(question, live)

	// No local model configured ⇒ degrade to the matching entries verbatim. Still
	// $0, still grounded, and their trust/provenance is surfaced inline.
	if g.c == nil {
		return groundingFallback(picked), nil
	}
	ans, err := g.c.Complete(ctx, groundingPrompt(question, picked))
	if err != nil || strings.TrimSpace(ans) == "" {
		// The local model faltered — fall back to the verbatim entries rather than
		// escalating to a paid model. Grounding stays local-only, always.
		return groundingFallback(picked), nil
	}
	return strings.TrimSpace(ans) + "\n\n" + citations(picked), nil
}

// readMemory loads the repo's memory READ-ONLY: Locate + os.ReadFile, mirroring
// PR-1's projection discipline (never Load/Resolve, which auto-create). An absent
// file returns (nil, nil) — the graceful "no memory" signal — not an error.
func (g *Grounder) readMemory(ctx context.Context) (*memory.Memory, error) {
	path, err := g.store.Locate(ctx, g.dir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return memory.Parse(string(raw)), nil
}

// liveEntries returns the entries safe to answer from: neither tombstoned
// (superseded/aged-out) nor stale-flagged. Those bookkeeping entries stay in the
// committed file for the diff reviewer but must never be surfaced as facts.
func liveEntries(m *memory.Memory) []memory.Entry {
	var out []memory.Entry
	for _, e := range m.Entries {
		if e.Live() && strings.TrimSpace(e.Text) != "" {
			out = append(out, e)
		}
	}
	return out
}

// selectRelevant ranks entries by keyword overlap with the question and returns
// the top matches (capped). When nothing overlaps it returns the first few live
// entries so the answer is still grounded in real memory rather than empty — the
// local model (or the caller) then honestly reports if none of them answer.
func selectRelevant(question string, entries []memory.Entry) []memory.Entry {
	terms := keywords(question)

	type scored struct {
		e     memory.Entry
		score int
		idx   int
	}
	ranked := make([]scored, 0, len(entries))
	for i, e := range entries {
		ranked = append(ranked, scored{e: e, score: overlap(terms, e.Text), idx: i})
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		if ranked[a].score != ranked[b].score {
			return ranked[a].score > ranked[b].score // higher overlap first
		}
		return ranked[a].idx < ranked[b].idx // stable on authored order
	})

	anyMatch := len(ranked) > 0 && ranked[0].score > 0
	out := make([]memory.Entry, 0, maxGroundEntries)
	for _, r := range ranked {
		if anyMatch && r.score == 0 {
			break // once matches exist, don't pad with unrelated entries
		}
		out = append(out, r.e)
		if len(out) >= maxGroundEntries {
			break
		}
	}
	return out
}

// keywords lowercases the question and keeps the content words (length >= 3),
// dropping a handful of common stopwords so "where does the daemon api live"
// keys on daemon/api, not where/does/the/live.
func keywords(q string) map[string]bool {
	stop := map[string]bool{
		"where": true, "does": true, "the": true, "how": true, "can": true,
		"what": true, "which": true, "who": true, "why": true, "and": true,
		"for": true, "you": true, "run": true, "live": true, "lives": true,
		"located": true, "find": true, "get": true, "use": true, "with": true,
		"from": true, "this": true, "that": true, "project": true,
	}
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), notWord) {
		if len(w) >= 3 && !stop[w] {
			out[w] = true
		}
	}
	return out
}

func notWord(r rune) bool {
	return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
}

// overlap counts how many of the question's keywords appear in the entry text.
func overlap(terms map[string]bool, text string) int {
	if len(terms) == 0 {
		return 0
	}
	lt := strings.ToLower(text)
	n := 0
	for term := range terms {
		if strings.Contains(lt, term) {
			n++
		}
	}
	return n
}

// groundingPrompt asks the LOCAL model to answer strictly from the supplied
// facts. It forbids invention, mandates the honest "not in project memory" when
// the facts don't answer, and flags unverified entries as possibly-stale — the
// same grounding discipline PR-1's projection header sets.
func groundingPrompt(question string, entries []memory.Entry) string {
	var b strings.Builder
	b.WriteString("You answer a question about a software project using ONLY the project-memory facts below. ")
	b.WriteString("Never invent anything not stated in them. If the facts do not answer the question, reply exactly: ")
	b.WriteString("not in project memory. Be terse — 1 to 3 sentences. Treat any fact marked 'unverified' as a hint ")
	b.WriteString("that may be stale.\n\nProject memory:\n")
	for _, e := range entries {
		b.WriteString(groundLine(e))
		b.WriteByte('\n')
	}
	b.WriteString("\nQuestion: ")
	b.WriteString(question)
	b.WriteString("\nAnswer:")
	return b.String()
}

// groundingFallback renders the matched entries verbatim — the degraded, $0
// answer when no local model is configured (or it faltered). Their trust and
// provenance are surfaced inline so a stale hint is visibly a hint.
func groundingFallback(entries []memory.Entry) string {
	var b strings.Builder
	b.WriteString("from project memory (.warden/memory.md — no local model wired, showing the matching entries verbatim):\n")
	for _, e := range entries {
		b.WriteString(groundLine(e))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// citations lists the entries an answer was grounded in, with their trust +
// provenance, so the operator can judge and verify a hint.
func citations(entries []memory.Entry) string {
	var b strings.Builder
	b.WriteString("grounded in .warden/memory.md:\n")
	for _, e := range entries {
		b.WriteString(groundLine(e))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// groundLine renders one entry as "- [trust · date · provenance] text". Unlike
// the projection render (which drops metadata to save tokens), grounding KEEPS
// trust/provenance visible: an operator asking a project question needs to know
// whether the answer is a corroborated fact, an unverified hint, or a plain
// human note. A plain hand-authored bullet (TrustNone) is labelled "human".
func groundLine(e memory.Entry) string {
	trust := e.Trust
	if trust == memory.TrustNone {
		trust = "human"
	}
	meta := []string{trust}
	if e.Timestamp != "" {
		meta = append(meta, e.Timestamp)
	}
	if e.Provenance != "" {
		meta = append(meta, e.Provenance)
	}
	return "- [" + strings.Join(meta, " · ") + "] " + strings.TrimSpace(e.Text)
}

// AddGrounding registers the local project-memory grounding verb as a read-only
// (auto-execute, $0) tool, mirroring Monitor.AddMonitoring. The model calls it for
// "where does X live?" / "how do I run Y?" project questions; the answer is
// composed on the local model (or degrades to the matched entries), never touching
// the cloud.
func (r *Registry) AddGrounding(g *Grounder) {
	r.tools = append(r.tools,
		Tool{
			Schema: llm.ToolSchema{Name: "project_memory",
				Description: "Answer a question about THIS project — where something lives, how to run something, a project invariant — from the repo's curated .warden/memory.md. Served LOCALLY at $0 (no cloud call). Prefer this for 'where does X live?' / 'how do I run Y?' questions instead of spawning an agent.",
				Parameters:  objSchema(map[string]any{"question": strProp("the project question to answer from memory")}, "question")},
			invoke: func(ctx context.Context, _ Daemon, a map[string]any) (string, error) {
				q, err := requireStr(a, "question")
				if err != nil {
					return "", err
				}
				return g.Answer(ctx, q)
			},
		},
	)
}
