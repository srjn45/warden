package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Action is the operator's decision on a proposed batch of mutating calls.
type Action int

const (
	// Reject is the safe default: nothing runs. Returned on EOF or an
	// unrecognized key so a stray newline never executes a mutation.
	Reject Action = iota
	Approve
	Edit
)

// Decision is the gate's verdict. For Edit, Calls holds the operator-revised
// calls; for Approve, Calls echoes the approved calls.
type Decision struct {
	Action Action
	Calls  []ToolCall
}

// confirmer is the gate seam the loop depends on, so tests can script it.
type confirmer interface {
	Confirm(calls []ToolCall) Decision
}

// Gate renders proposed mutating calls and reads an approve/edit/reject decision
// from a lineReader seam. Confirming before execution is non-negotiable and not
// config-gated — it is what makes a small local model safe in this seat.
type Gate struct {
	in    lineReader
	out   io.Writer
	style *styler
	// reg supplies each tool's parameter schema (field names + types) so the
	// editor can prompt field-by-field. It may be nil — a gate built without a
	// registry (in tests) falls back to editing the call's own args.
	reg *Registry
	// defaults holds the config value warden will substitute for a field the
	// operator leaves blank (e.g. model, permission_mode). Shown in the prompt
	// brackets so the operator can see what an empty answer will actually use.
	defaults map[string]string
	// prefill optionally suggests field values from the operator's query (the
	// hybrid form). nil ⇒ a plain, model-free form. Only the /-command Form path
	// uses it; the model's own [e]dit already carries the model's proposed args.
	prefill prefiller
}

// usePrefiller installs the LLM suggestion seam for the /-command form. NewSession
// wires this when the local model is a Completer; it may be left nil.
func (g *Gate) usePrefiller(p prefiller) { g.prefill = p }

// useRegistry points the editor at the tool schemas so it can prompt one field
// at a time. NewSession wires this when the gate is a *Gate.
func (g *Gate) useRegistry(r *Registry) { g.reg = r }

// UseDefaults records the config defaults warden applies to blank fields, so the
// editor can show "[default: …]" instead of an empty "[]" for fields the model
// omitted (model, permission_mode). The REPL wires this from live config.
func (g *Gate) UseDefaults(d map[string]string) { g.defaults = d }

// NewGate builds a gate over the given reader/writer (stdin/stdout in the REPL,
// scripted buffers in tests). By default it reads through a plain scanner; the
// REPL swaps in its shared interactive lineReader via useReader so the gate's
// approve read and the REPL's line read never race the same terminal.
func NewGate(r io.Reader, w io.Writer) *Gate {
	return &Gate{in: newScannerReader(r, w), out: w, style: newStyler(w)}
}

// useReader points the gate at the REPL's shared lineReader (and matching
// styler) so confirmation prompts get the same interactive editor and colours
// as the main loop.
func (g *Gate) useReader(lr lineReader, st *styler) {
	g.in = lr
	if st != nil {
		g.style = st
	}
}

var _ confirmer = (*Gate)(nil)

// Confirm renders every proposed call (a batch confirms as one unit) and blocks
// for [a]pprove / [e]dit / [r]eject. Reject is the default on EOF or an
// unrecognized key.
func (g *Gate) Confirm(calls []ToolCall) Decision {
	fmt.Fprintln(g.out, g.style.heading.Render("orchestrator wants to:"))
	for i, c := range calls {
		fmt.Fprintf(g.out, "  %d. %s %s\n", i+1, g.style.tool.Render(c.Name), renderArgs(c.Args))
	}
	line, err := g.in.Prompt("[a]pprove [e]dit [r]eject: ")
	if err != nil {
		return Decision{Action: Reject} // EOF / interrupt ⇒ safe default
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "a", "approve", "y", "yes":
		return Decision{Action: Approve, Calls: calls}
	case "e", "edit":
		return Decision{Action: Edit, Calls: g.edit(calls)}
	default:
		return Decision{Action: Reject}
	}
}

// edit walks the operator through each proposed call one field at a time: a
// short prompt per field shows the current value, and a blank line (Enter) keeps
// it. This replaces the old single-line JSON editor, which built a prompt wider
// than the terminal (unusable to hand-edit, and the interactive reader
// mis-renders an over-width prompt — it stacks copies of it). Ctrl-C or EOF
// part-way through stops editing and keeps the remaining fields untouched.
func (g *Gate) edit(calls []ToolCall) []ToolCall {
	edited := make([]ToolCall, len(calls))
	for i, c := range calls {
		edited[i] = g.editOne(c)
	}
	return edited
}

// editOne prompts for each editable field of a single call. It starts from a
// copy of the proposed args, so untouched fields (and fields the operator skips)
// keep exactly what the model proposed. The model's own proposal is the
// pre-selection, so no extra suggestions are layered on.
func (g *Gate) editOne(c ToolCall) ToolCall {
	return g.collect(c, g.fieldsFor(c), nil, "editing")
}

// Form collects arguments for a /-command-initiated call. query is the
// operator's original words, fed to the LLM pre-fill so each field opens with a
// suggested value the operator can accept with Enter; fields are the schema
// fields to offer (all of them for a `+` form, just the required ones when a
// command auto-opens for a missing argument). The completed call still rides the
// confirm gate afterwards for any mutation — the form fills, it does not approve.
func (g *Gate) Form(ctx context.Context, query string, c ToolCall, fields []fieldSpec) ToolCall {
	var suggest map[string]string
	if g.prefill != nil {
		suggest = g.prefill.Prefill(ctx, c.Name, query, fields)
	}
	return g.collect(c, fields, suggest, "filling")
}

// collect is the shared per-field walk behind both [e]dit and the /-command
// form. For each field it shows a pick-list (enum fields), a y/n hint (booleans),
// or a free-text bracket, with the value Enter will accept already selected:
// whatever the operator/model already set, else an LLM suggestion, else the
// config default. Enter keeps it; "-" clears it back to the config default;
// Ctrl-C/EOF stops and keeps the rest untouched.
func (g *Gate) collect(c ToolCall, fields []fieldSpec, suggest map[string]string, verb string) ToolCall {
	args := cloneArgs(c.Args)
	if len(fields) == 0 {
		return ToolCall{Name: c.Name, Args: args}
	}
	// Seed suggestions into blank fields so the existing "Enter keeps current"
	// path doubles as "Enter accepts the suggestion".
	for _, f := range fields {
		if argStr(args, f.name) == "" {
			if v, ok := suggest[f.name]; ok {
				args[f.name] = v
			}
		}
	}
	fmt.Fprintln(g.out, g.style.hint.Render(
		fmt.Sprintf("%s %s — Enter accepts the value in [ ], type a new one, or \"-\" to clear (Ctrl-C to finish):", verb, c.Name)))
	for _, f := range fields {
		if opts := fieldOptions(f.name); len(opts) > 0 {
			fmt.Fprintln(g.out, g.style.hint.Render(renderOptions(opts, g.selected(f, args))))
		}
		line, err := g.in.Prompt(g.fieldPrompt(f, args))
		if err != nil {
			break // Ctrl-C / EOF → keep the remaining fields as proposed
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue // Enter keeps the current/suggested value
		}
		if line == "-" {
			delete(args, f.name) // clear → fall back to the config default
			continue
		}
		setField(args, f, line, g.out)
	}
	return ToolCall{Name: c.Name, Args: args}
}

// selected is the value a bare Enter will keep for a field: the current arg if
// set, otherwise the config default warden would substitute. Used to mark the
// pick-list so the pre-selection is visible.
func (g *Gate) selected(f fieldSpec, args map[string]any) string {
	if cur := argStr(args, f.name); cur != "" {
		return cur
	}
	return g.defaults[f.name]
}

// formFields returns the schema fields a /-command form should offer for a tool,
// optionally narrowed to a set of names (the required-only auto-open case). An
// empty `only` means "every fillable field".
func (g *Gate) formFields(tool string, only ...string) []fieldSpec {
	all := g.fieldsFor(ToolCall{Name: tool})
	if len(only) == 0 {
		return all
	}
	want := make(map[string]bool, len(only))
	for _, n := range only {
		want[n] = true
	}
	out := all[:0:0]
	for _, f := range all {
		if want[f.name] {
			out = append(out, f)
		}
	}
	return out
}

// fieldSpec is one editable argument: its key and JSON-Schema type.
type fieldSpec struct {
	name string
	kind string // "string" | "boolean" | "integer"
}

// fieldsFor returns the editable fields for a call, ordered for prompting. It
// prefers the tool's declared schema (so the operator can fill in a field the
// model omitted, e.g. branch); with no registry it falls back to the keys the
// call already carries.
func (g *Gate) fieldsFor(c ToolCall) []fieldSpec {
	props := map[string]any{}
	var required []string
	if g.reg != nil {
		if tl, ok := g.reg.Lookup(c.Name); ok {
			if p, ok := tl.Schema.Parameters["properties"].(map[string]any); ok {
				props = p
			}
			required, _ = tl.Schema.Parameters["required"].([]string)
		}
	}
	if len(props) == 0 {
		for k, v := range c.Args {
			props[k] = map[string]any{"type": kindOf(v)}
		}
	}
	return orderFields(props, required)
}

// preferredFieldOrder lists known argument keys in the order an operator most
// naturally fills them. Unlisted keys follow, alphabetically. Required keys
// always lead, in their schema order.
var preferredFieldOrder = []string{
	"prompt", "name", "type", "repo", "branch", "worktree", "in_repo",
	"model", "permission_mode", "agent", "ticket", "to", "body", "text",
	"message", "key", "value", "spec", "id", "option", "dir", "base",
	"hard", "unread", "prefix", "lines",
}

func orderFields(props map[string]any, required []string) []fieldSpec {
	seen := map[string]bool{}
	var out []fieldSpec
	add := func(name string) {
		if seen[name] {
			return
		}
		p, ok := props[name]
		if !ok {
			return
		}
		seen[name] = true
		out = append(out, fieldSpec{name: name, kind: kindFromProp(p)})
	}
	for _, n := range required {
		add(n)
	}
	for _, n := range preferredFieldOrder {
		add(n)
	}
	rest := make([]string, 0, len(props))
	for n := range props {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	for _, n := range rest {
		add(n)
	}
	return out
}

// fieldPrompt renders one short prompt: the field name and its current value
// (truncated so the line never exceeds the terminal width). Booleans show a y/n
// hint. When the field is blank but warden has a config default for it, the
// bracket shows "default: <value>" so the operator knows what an empty answer
// will use (e.g. model, permission_mode).
func (g *Gate) fieldPrompt(f fieldSpec, args map[string]any) string {
	if f.kind == "boolean" {
		return fmt.Sprintf("  %s [%v] (y/n): ", f.name, argBool(args, f.name))
	}
	cur := argStr(args, f.name)
	if cur == "" {
		if d := g.defaults[f.name]; d != "" {
			return fmt.Sprintf("  %s [default: %s]: ", f.name, truncateVal(d, 20))
		}
	}
	return fmt.Sprintf("  %s [%s]: ", f.name, truncateVal(cur, 28))
}

// setField applies a non-blank operator entry to args, coercing by type. A
// malformed bool/int is reported and the current value kept, so a typo never
// silently writes garbage.
func setField(args map[string]any, f fieldSpec, line string, out io.Writer) {
	switch f.kind {
	case "boolean":
		b, ok := parseBoolInput(line)
		if !ok {
			fmt.Fprintln(out, "  (enter y or n — keeping current)")
			return
		}
		args[f.name] = b
	case "integer":
		n, err := strconv.Atoi(line)
		if err != nil {
			fmt.Fprintln(out, "  (enter a whole number — keeping current)")
			return
		}
		args[f.name] = n
	default:
		if opts := fieldOptions(f.name); len(opts) > 0 {
			v, ok := resolveOption(line, opts)
			if !ok {
				fmt.Fprintf(out, "  (pick 1-%d or a listed value — keeping current)\n", len(opts))
				return
			}
			args[f.name] = v
			return
		}
		args[f.name] = line
	}
}

func parseBoolInput(s string) (val, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes", "true", "1":
		return true, true
	case "n", "no", "false", "0":
		return false, true
	}
	return false, false
}

func kindFromProp(p any) string {
	if m, ok := p.(map[string]any); ok {
		if t, ok := m["type"].(string); ok {
			return t
		}
	}
	return "string"
}

func kindOf(v any) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case float64, int:
		return "integer"
	default:
		return "string"
	}
}

func cloneArgs(a map[string]any) map[string]any {
	out := make(map[string]any, len(a))
	for k, v := range a {
		out[k] = v
	}
	return out
}

// truncateVal keeps a current-value preview short (and single-line) so the
// readline prompt stays well under the terminal width.
func truncateVal(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

func renderArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(b)
}
