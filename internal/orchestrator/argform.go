package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/llm"
)

// argform is the option-aware interactive argument collector shared by the gate's
// [e]dit flow and the `/`-command forms. It upgrades the old free-text-only
// field editor with two things the operator asked for: a numbered pick-list for
// fields whose domain is known (model, permission_mode, type, booleans), and an
// optional LLM pre-fill that suggests a value for each field from the operator's
// own words. The structure (which fields, which options) is always deterministic
// — derived from warden's own enums below — so the form keeps working with no
// model in the loop; the model can only ever nudge a default, never invent an
// out-of-range option.

// fieldOptions returns the allowed values for a constrained field, or nil for a
// free-text field. These are warden's own closed enums — kept in sync with
// internal/store (task types), internal/config (permission modes), and the
// Claude model set — so a pick-list can never offer a value the daemon rejects.
func fieldOptions(name string) []string {
	switch name {
	case "model":
		return []string{"sonnet", "opus", "haiku", "fable"}
	case "permission_mode":
		return []string{"auto", "default", "acceptEdits", "bypassPermissions", "dontAsk", "plan"}
	case "type":
		return []string{"development", "analysis", "spike", "pr-review", "code", "docs", "website", "debug-ci", "tests", "other"}
	}
	return nil
}

// resolveOption maps an operator's entry for an enum field to a canonical value.
// It accepts a 1-based menu number ("3") or the value itself, case-insensitively
// ("pr-review", "PR-Review"). The bool is false when the entry matches neither,
// so a typo is reported and the current value kept rather than written blind.
func resolveOption(line string, opts []string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", false
	}
	if n, err := strconv.Atoi(line); err == nil {
		if n >= 1 && n <= len(opts) {
			return opts[n-1], true
		}
		return "", false
	}
	for _, o := range opts {
		if strings.EqualFold(line, o) {
			return o, true
		}
	}
	return "", false
}

// renderOptions builds the numbered menu shown above an enum field's prompt. The
// value warden will use if the operator just presses Enter (suggested if the
// model offered one, else the config default) is marked, so the pre-selection is
// always visible.
func renderOptions(opts []string, selected string) string {
	parts := make([]string, 0, len(opts))
	for i, o := range opts {
		s := fmt.Sprintf("%d) %s", i+1, o)
		if o == selected {
			s = "[" + s + " ←]"
		}
		parts = append(parts, s)
	}
	return "    " + strings.Join(parts, "  ")
}

// prefiller suggests argument values for a tool call from the operator's query.
// It is the hybrid half of the form: structure stays deterministic, but the
// model may pre-select a sensible value the operator can accept with one keypress
// or override. A nil prefiller (or any error/timeout) simply yields no
// suggestions — the form falls back to a plain, model-free pick-list.
type prefiller interface {
	Prefill(ctx context.Context, tool, query string, fields []fieldSpec) map[string]string
}

// llmPrefiller drives the local model (the same Completer the monitor condenser
// uses) for a single structured suggestion turn. It validates every suggestion
// against the field's enum and drops anything out-of-range or unknown, so the
// model can never push an invalid value into the form.
type llmPrefiller struct{ comp llm.Completer }

func (p llmPrefiller) Prefill(ctx context.Context, tool, query string, fields []fieldSpec) map[string]string {
	if p.comp == nil || strings.TrimSpace(query) == "" || len(fields) == 0 {
		return nil
	}
	raw, err := p.comp.Complete(ctx, prefillPrompt(tool, query, fields))
	if err != nil {
		return nil
	}
	return parsePrefill(raw, fields)
}

// prefillPrompt asks the model for a JSON object of field→value suggestions,
// constraining enum fields to their allowed values so a well-behaved model stays
// in range (parsePrefill enforces it regardless).
func prefillPrompt(tool, query string, fields []fieldSpec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are filling arguments for warden's %q action.\n", tool)
	fmt.Fprintf(&b, "The operator's request: %q\n\n", query)
	b.WriteString("Suggest a value for each field below, or omit a field you can't infer. ")
	b.WriteString("Reply with ONLY a JSON object mapping field name to value — no prose.\n\nFields:\n")
	for _, f := range fields {
		if opts := fieldOptions(f.name); len(opts) > 0 {
			fmt.Fprintf(&b, "- %s: one of [%s]\n", f.name, strings.Join(opts, ", "))
		} else if f.kind == "boolean" {
			fmt.Fprintf(&b, "- %s: true or false\n", f.name)
		} else {
			fmt.Fprintf(&b, "- %s: free text\n", f.name)
		}
	}
	return b.String()
}

// parsePrefill decodes the model's JSON object and keeps only valid suggestions:
// a known field, and — for an enum field — an in-range value. Everything else is
// dropped. A non-JSON reply yields no suggestions rather than an error.
func parsePrefill(raw string, fields []fieldSpec) map[string]string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexByte(raw, '{'); i > 0 {
		raw = raw[i:] // tolerate a chatty prefix before the object
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	known := make(map[string]fieldSpec, len(fields))
	for _, f := range fields {
		known[f.name] = f
	}
	out := map[string]string{}
	for k, v := range m {
		f, ok := known[k]
		if !ok {
			continue
		}
		val := strings.TrimSpace(fmt.Sprintf("%v", v))
		if val == "" || strings.EqualFold(val, "null") {
			continue
		}
		if opts := fieldOptions(f.name); len(opts) > 0 {
			if canon, ok := resolveOption(val, opts); ok {
				out[k] = canon
			}
			continue // never accept an out-of-range enum suggestion
		}
		out[k] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
