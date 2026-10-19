package ptyobs

import (
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/ptyevent"
)

// OptionView is one offered option as operators see it.
type OptionView struct {
	Index    int    `json:"index"`
	Label    string `json:"label"`
	Semantic string `json:"semantic"`
	Scope    string `json:"scope,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Chosen   bool   `json:"chosen,omitempty"`
}

// PermissionSummary is the human-readable view of a normalized prompt:
// capability, target/scope, offered options and (once decided) the chosen one.
type PermissionSummary struct {
	Class        string       `json:"class"`
	Capabilities []string     `json:"capabilities,omitempty"`
	Subject      string       `json:"subject,omitempty"`
	Target       string       `json:"target,omitempty"`
	Scope        string       `json:"scope,omitempty"`
	Question     string       `json:"question,omitempty"`
	Options      []OptionView `json:"options,omitempty"`
	Chosen       int          `json:"chosen,omitempty"`
	Fingerprint  string       `json:"fingerprint,omitempty"`
	Provenance   string       `json:"provenance,omitempty"`
	Confidence   float64      `json:"confidence,omitempty"`
	// Text is one sentence an operator can read at a glance.
	Text string `json:"text"`
}

// Summarize renders p (nil-safe). chosen is the 1-based option the decision
// picked, or 0 when none has been chosen. Free text is re-redacted.
func Summarize(p *ptyevent.Prompt, chosen int) PermissionSummary {
	if p == nil {
		return PermissionSummary{}
	}
	s := PermissionSummary{
		Class:       string(p.Class),
		Subject:     clean(p.Subject, 120),
		Target:      clean(p.Target, 240),
		Question:    clean(p.Question, 240),
		Chosen:      chosen,
		Fingerprint: p.Fingerprint,
		Provenance:  string(p.Provenance),
		Confidence:  p.Confidence,
	}
	for _, c := range p.Capabilities {
		s.Capabilities = append(s.Capabilities, string(c))
	}
	for _, o := range p.Options {
		s.Options = append(s.Options, OptionView{
			Index: o.Index, Label: clean(o.Label, 120), Semantic: string(o.Semantic),
			Scope: string(o.Scope), Default: o.Default, Chosen: chosen > 0 && o.Index == chosen,
		})
		if chosen > 0 && o.Index == chosen && o.Scope != "" {
			s.Scope = string(o.Scope)
		}
	}
	s.Text = summaryText(s)
	return s
}

func summaryText(s PermissionSummary) string {
	var b strings.Builder
	switch s.Class {
	case string(ptyevent.ClassPermission):
		caps := strings.Join(s.Capabilities, "+")
		if caps == "" {
			caps = "unclassified"
		}
		fmt.Fprintf(&b, "%s permission", caps)
		if s.Subject != "" {
			fmt.Fprintf(&b, " for %s", s.Subject)
		}
		if s.Target != "" {
			fmt.Fprintf(&b, " on %q", s.Target)
		}
	case string(ptyevent.ClassDecision):
		b.WriteString("strategic decision")
		if s.Question != "" {
			fmt.Fprintf(&b, ": %s", s.Question)
		}
	default:
		fmt.Fprintf(&b, "%s prompt", orDefault(s.Class, "unknown"))
	}
	if n := len(s.Options); n > 0 {
		fmt.Fprintf(&b, " (%d options", n)
		for _, o := range s.Options {
			if o.Chosen {
				fmt.Fprintf(&b, "; chose %d %q [%s]", o.Index, o.Label, o.Semantic)
			}
		}
		b.WriteString(")")
	}
	return b.String()
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// clean re-redacts and bounds one free-text field.
func clean(s string, max int) string {
	if s == "" {
		return ""
	}
	red, _ := ptyevent.Redact([]byte(s))
	out := strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\t' || r == 0x7f {
			return ' '
		}
		return r
	}, string(red))
	if r := []rune(out); len(r) > max {
		out = string(r[:max]) + "…"
	}
	return out
}

// DescribeInput renders keystrokes the router chose without recording them
// verbatim: "option N + Enter", arrow moves, or a byte count.
func DescribeInput(data []byte, optionIndex int) string {
	if len(data) == 0 {
		return ""
	}
	moves := strings.Count(string(data), "\x1b[B") + strings.Count(string(data), "\x1b[A")
	switch {
	case moves > 0:
		return fmt.Sprintf("navigate %d + Enter → option %d", moves, optionIndex)
	case optionIndex > 0:
		return fmt.Sprintf("option %d + Enter", optionIndex)
	default:
		return fmt.Sprintf("%d bytes", len(data))
	}
}
