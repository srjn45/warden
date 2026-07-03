package curate

import (
	"context"
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/llm"
	"github.com/srjn45/warden/internal/memory"
)

// maxCandidates caps how many facts one pass proposes, so a chatty model can't bloat
// the memory in a single burst. Curation summarizes to a tight navigational list.
const maxCandidates = 6

// LLMProposer is the production Proposer: it extracts durable facts with warden's
// $0-preferring offload, mirroring digest.ClaudeNarrator's single-Run seam. It tries
// the local LLM FIRST (curation is a fuzzy background task, safe to keep off warden's
// Claude spend); it degrades to headless `claude -p` (Run) only when no local model
// is configured — the "explicitly configured" cloud fallback of §4.2. Because the
// whole pass runs off the debounce timer, never on a request path, even the fallback
// is off any critical path.
type LLMProposer struct {
	// Run is the bounded headless one-shot (lifecycle.RunClaudeP). Required.
	Run func(ctx context.Context, arg string) (string, error)
	// LLM is the optional local model; when non-nil it serves the pass at $0.
	LLM llm.Completer
	// Record, when set, books a fully-offloaded local call to the savings ledger
	// (lifecycle.recordOffload semantics). agent may be "".
	Record func(agent, prompt string)
}

// Propose builds the extraction prompt from the batch and current memory, runs it
// through the offload, and parses the reply's bullet lines into UNVERIFIED candidate
// entries tagged with this batch's provenance and today's date.
func (p LLMProposer) Propose(ctx context.Context, in ProposeInput) ([]memory.Entry, error) {
	prompt := ExtractionPrompt(in)
	var raw string
	if p.LLM != nil {
		out, err := p.LLM.Complete(ctx, prompt)
		if err == nil {
			raw = out
			if p.Record != nil {
				p.Record(provenanceAgent(in.Signals), prompt) // stayed off warden's Claude spend
			}
		}
	}
	if raw == "" {
		if p.Run == nil {
			return nil, nil // no local model and no configured cloud fallback ⇒ skip, $0
		}
		out, err := p.Run(ctx, prompt)
		if err != nil {
			return nil, fmt.Errorf("curate extraction: %w", err)
		}
		raw = out
	}
	return parseCandidates(raw, provenance(in.Signals)), nil
}

// ExtractionPrompt is the §3.2 extraction (NOT dump) instruction: from the batch's
// completion signals + the current memory, surface only DURABLE, reusable facts —
// where things live, how to run X, project invariants — and explicitly discard
// per-task noise ("edited N files on branch Z") and anything already in memory.
func ExtractionPrompt(in ProposeInput) string {
	var b strings.Builder
	b.WriteString("You maintain a project's durable memory for future coding agents. ")
	b.WriteString("From the completed agents' activity below, extract at most ")
	fmt.Fprintf(&b, "%d DURABLE, reusable facts a NEXT agent should not have to rediscover: ", maxCandidates)
	b.WriteString("where things live, how to run something, or a project invariant. ")
	b.WriteString("DISCARD per-task noise (which files changed, branch names, what this run did). ")
	b.WriteString("DISCARD anything already stated in current memory. ")
	b.WriteString("Output ONLY a markdown bullet list, one fact per '- ' line, each a short navigational statement; ")
	b.WriteString("if there is nothing durable to add, output nothing.\n\n")

	if in.Current != nil && len(in.Current.Entries) > 0 {
		b.WriteString("Current memory (do not repeat):\n")
		for _, e := range in.Current.Entries {
			if e.Live() {
				fmt.Fprintf(&b, "- %s\n", e.Text)
			}
		}
		b.WriteString("\n")
	}
	b.WriteString("Completed agent activity:\n")
	for _, s := range in.Signals {
		if s.Task != "" {
			fmt.Fprintf(&b, "- task: %s\n", s.Task)
		}
		if s.Summary != "" {
			fmt.Fprintf(&b, "  did: %s\n", s.Summary)
		}
		if len(s.Files) > 0 {
			fmt.Fprintf(&b, "  files: %s\n", strings.Join(s.Files, ", "))
		}
	}
	return b.String()
}

// parseCandidates turns the model's bullet reply into UNVERIFIED, dated,
// provenance-tagged entries — the enforced proposal shape (§4.2). Non-bullet
// preamble lines are ignored; the count is capped.
func parseCandidates(raw, prov string) []memory.Entry {
	var out []memory.Entry
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "- ") && !strings.HasPrefix(t, "* ") {
			continue
		}
		text := strings.TrimSpace(t[2:])
		// Defensively peel a leaked "[...]" metadata prefix so provenance/trust are
		// set HERE, never taken from model output.
		if strings.HasPrefix(text, "[") {
			if i := strings.IndexByte(text, ']'); i >= 0 {
				text = strings.TrimSpace(text[i+1:])
			}
		}
		if text == "" {
			continue
		}
		out = append(out, memory.Entry{
			Trust:      memory.TrustUnverified,
			Provenance: prov,
			Text:       text,
		})
		if len(out) >= maxCandidates {
			break
		}
	}
	return out
}

// provenance renders a batch's attribution for an entry's metadata: a single agent
// (+sha) reads as "agent X · sha Y"; a coalesced multi-agent batch as "digest batch:
// a, b". Timestamp is stamped by the merge/serialize layer, not here.
func provenance(sigs []Signal) string {
	switch len(sigs) {
	case 0:
		return "digest"
	case 1:
		p := "agent " + sigs[0].Agent
		if sigs[0].Agent == "" {
			p = "digest"
		}
		if sigs[0].Commit != "" {
			p += " · sha " + shortSHA(sigs[0].Commit)
		}
		return p
	default:
		var ids []string
		for _, s := range sigs {
			if s.Agent != "" {
				ids = append(ids, s.Agent)
			}
		}
		if len(ids) == 0 {
			return "digest batch"
		}
		return "digest batch: " + strings.Join(ids, ", ")
	}
}

// provenanceAgent returns a single agent id for the savings attribution, or "".
func provenanceAgent(sigs []Signal) string {
	if len(sigs) == 1 {
		return sigs[0].Agent
	}
	return ""
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
