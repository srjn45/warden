package repl

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// present turns a read tool's raw result (the compact JSON the registry produces
// for the model to read) into something a *human* can read in the deterministic
// `/`-command path. The model still sees the JSON; this only reshapes what the
// REPL prints back to the operator. Known shapes become compact tables; anything
// unrecognised falls back to indented JSON, and a plain scalar (an id, a context
// value, "spawned …") is returned untouched. It never errors — a parse failure
// just yields the original string.
func present(name, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	switch name {
	case "list_agents":
		if out, ok := renderAgents(raw); ok {
			return out
		}
	case "get_agent":
		if out, ok := renderAgent(raw); ok {
			return out
		}
	case "ctx_list":
		if out, ok := renderCtxList(raw); ok {
			return out
		}
	case "pipeline_list":
		if out, ok := renderPipelines(raw); ok {
			return out
		}
	case "read_inbox":
		if out, ok := renderInbox(raw); ok {
			return out
		}
	case "get_collaboration_status":
		if out, ok := renderConflicts(raw); ok {
			return out
		}
	case "list_approvals":
		if out, ok := renderApprovals(raw); ok {
			return out
		}
	}
	// Unknown tool, or a known one whose payload didn't parse: if it's a JSON
	// array/object, indent it; otherwise it's already a readable scalar.
	if looksJSON(raw) {
		return indentJSON(raw)
	}
	return raw
}

func renderAgents(raw string) (string, bool) {
	var ss []*store.Session
	if err := json.Unmarshal([]byte(raw), &ss); err != nil {
		return "", false
	}
	if len(ss) == 0 {
		return "no agents", true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d agent%s\n", len(ss), plural(len(ss)))
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tTYPE\tNAME\tWHAT")
	for _, s := range ss {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.Status, dash(string(s.Type)), dash(s.Name), clip(flatten(agentWhat(s)), 60))
	}
	tw.Flush()
	return strings.TrimRight(b.String(), "\n"), true
}

func renderAgent(raw string) (string, bool) {
	var s store.Session
	if err := json.Unmarshal([]byte(raw), &s); err != nil || s.ID == "" {
		return "", false
	}
	rows := [][2]string{
		{"id", s.ID},
		{"name", dash(s.Name)},
		{"type", dash(string(s.Type))},
		{"status", string(s.Status)},
		{"repo", dash(s.Repo)},
		{"branch", dash(s.Branch)},
		{"worktree", dash(s.Worktree)},
		{"pr", dash(s.PR)},
		{"model", dash(s.Model)},
		{"context", contextLine(s)},
		{"updated", since(s.UpdatedAt)},
		{"what", clip(flatten(agentWhat(&s)), 100)},
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 2, 1, ' ', 0)
	for _, r := range rows {
		if r[1] == "" || r[1] == "-" {
			continue // skip empty fields — keep the block tight
		}
		fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1])
	}
	tw.Flush()
	return strings.TrimRight(b.String(), "\n"), true
}

func renderCtxList(raw string) (string, bool) {
	var es []client.ContextEntry
	if err := json.Unmarshal([]byte(raw), &es); err != nil {
		return "", false
	}
	if len(es) == 0 {
		return "no context keys", true
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE\tBY")
	for _, e := range es {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Key, clip(flatten(e.Value), 50), dash(e.UpdatedBy))
	}
	tw.Flush()
	return strings.TrimRight(b.String(), "\n"), true
}

func renderPipelines(raw string) (string, bool) {
	var ps []*pipeline.Pipeline
	if err := json.Unmarshal([]byte(raw), &ps); err != nil {
		return "", false
	}
	if len(ps) == 0 {
		return "no pipelines", true
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tJOBS\tREPO")
	for _, p := range ps {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", p.ID, p.Status, len(p.Jobs), dash(p.Repo))
	}
	tw.Flush()
	return strings.TrimRight(b.String(), "\n"), true
}

func renderInbox(raw string) (string, bool) {
	var ms []client.Message
	if err := json.Unmarshal([]byte(raw), &ms); err != nil {
		return "", false
	}
	if len(ms) == 0 {
		return "inbox empty", true
	}
	var b strings.Builder
	for i, m := range ms {
		mark := " "
		if !m.Read {
			mark = "•"
		}
		fmt.Fprintf(&b, "%s from %s (%s): %s", mark, dash(m.From), since(m.TS), clip(flatten(m.Body), 80))
		if i < len(ms)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String(), true
}

func renderConflicts(raw string) (string, bool) {
	var cs []client.Conflict
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		return "", false
	}
	if len(cs) == 0 {
		return "no inter-agent file conflicts", true
	}
	var b strings.Builder
	for i, c := range cs {
		ids := make([]string, 0, len(c.Agents))
		for _, a := range c.Agents {
			ids = append(ids, a.ID)
		}
		fmt.Fprintf(&b, "%s — %s", c.File, strings.Join(ids, ", "))
		if i < len(cs)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String(), true
}

func renderApprovals(raw string) (string, bool) {
	var vs []approval.View
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		return "", false
	}
	if len(vs) == 0 {
		return "no pending approvals", true
	}
	var b strings.Builder
	for i, v := range vs {
		fmt.Fprintf(&b, "%s: %s [%s]", dash(v.ID), clip(flatten(v.Question), 80), strings.Join(v.Options, " | "))
		if i < len(vs)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String(), true
}

// agentWhat is the one-line "what is it doing" for an agent: its auto subject if
// present, otherwise the opening of its prompt.
func agentWhat(s *store.Session) string {
	if s.Subject != "" {
		return s.Subject
	}
	return s.Prompt
}

func contextLine(s store.Session) string {
	if s.ContextTokens == 0 {
		return ""
	}
	if s.ContextState != "" {
		return fmt.Sprintf("%d (%s)", s.ContextTokens, s.ContextState)
	}
	return fmt.Sprintf("%d", s.ContextTokens)
}

// flatten collapses newlines and runs of whitespace into single spaces so a
// multi-line prompt fits one table cell.
func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// since renders a timestamp as a compact relative age ("3m", "2h", "5d").
func since(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func looksJSON(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}

func indentJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(b)
}
